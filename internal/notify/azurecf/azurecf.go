// Package azurecf is the checkpoint/filter logic behind the Azure
// Blob Storage Change Feed backend adapter for Change notifications
// (ARCHITECTURE.md's Change notifications section, ROADMAP.md's task
// E5).
//
// This package deliberately does NOT depend on the real Azure SDK for
// Go or talk to a real Storage Account — stated plainly, not silently
// absent, per task E5's own instruction: this environment has no way
// to provision a real Azure Storage Account (ARCHITECTURE.md's Testing
// reality note), and there is no way to verify a real Azure SDK
// integration's API shape against actual installed source the way
// every other backend API assumption in this project has been
// checked first (go-fuse's Notify* methods, MinIO's
// ListenBucketNotification, both confirmed by reading the installed
// package directly). Writing "real" Azure SDK integration code
// without that verification would be exactly the kind of unverified
// assumption this project has otherwise avoided throughout — so
// instead, the part that *can* be built and genuinely tested here is:
// the pull-and-resume checkpoint/cursor logic and the client-side
// prefix filtering Azure's Change Feed needs (it has no server-side
// filtering at all, unlike MinIO/S3 — ARCHITECTURE.md). Reader is the
// minimal seam a real Azure SDK-backed implementation would plug into;
// providing that implementation is the explicitly-documented gap this
// task leaves for whoever has a real Storage Account to build and
// verify it against.
package azurecf

import (
	"context"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/notify"
)

// Event is one Change Feed entry — the minimal field this package's
// filter logic needs. A real Reader implementation would translate
// whatever the actual Azure SDK's change-feed record type looks like
// into this.
type Event struct {
	Key string // the blob name (object key) this event is about
}

// Cursor is an opaque "I've read up to here" checkpoint. Change Feed
// is a pull-and-resume append-only log (ARCHITECTURE.md), so resuming
// a poll after a restart needs to remember where the last one left
// off — treated as opaque bytes here since the real shape depends on
// whatever continuation-token representation the actual Azure SDK
// uses, not verifiable in this environment (see package doc comment).
type Cursor []byte

// Page is one page of events read from the feed, plus the cursor to
// resume from for the next page.
type Page struct {
	Events []Event
	Next   Cursor
}

// Reader reads one page of the Change Feed starting from cursor (nil
// for "from the beginning of the feed"). A real Azure SDK-backed
// implementation of this interface is the documented gap this task
// leaves open — see package doc comment.
type Reader interface {
	ReadPage(ctx context.Context, cursor Cursor) (Page, error)
}

// pollInterval bounds how often Source re-polls once it has caught up
// to the current end of the feed (ReadPage returning zero events).
// ARCHITECTURE.md gives Azure Change Feed's own latency as "within an
// order of a few minutes" (Microsoft's own wording) — a tight poll
// loop here would just be wasted API calls against a feed that can't
// have changed meaningfully faster than that anyway.
const pollInterval = 30 * time.Second

// Source polls a Reader, filters each event's key against prefix
// client-side (task E5: Azure's Change Feed has no server-side
// filtering, unlike MinIO/S3 — every consumer account-wide reads every
// other tenant's raw change events too before discarding what doesn't
// match, per ARCHITECTURE.md's cross-tenant-visibility note), and
// persists its cursor via persist after every successfully-processed
// page, so a later restart resumes from there instead of re-reading
// the whole feed from scratch.
type Source struct {
	cancel context.CancelFunc
	ch     chan notify.Signal
	done   chan struct{}
}

var _ notify.Source = (*Source)(nil)

// New starts polling reader for events under prefix, beginning at
// initial (nil/empty to start from the beginning of the feed), calling
// persist (if non-nil) with each new cursor as pages are consumed.
func New(ctx context.Context, reader Reader, prefix string, initial Cursor, persist func(Cursor) error) *Source {
	pollCtx, cancel := context.WithCancel(ctx)
	s := &Source{
		cancel: cancel,
		ch:     make(chan notify.Signal),
		done:   make(chan struct{}),
	}
	go s.run(pollCtx, reader, prefix, initial, persist)
	return s
}

func (s *Source) run(ctx context.Context, reader Reader, prefix string, cursor Cursor, persist func(Cursor) error) {
	defer close(s.ch)
	defer close(s.done)
	for {
		page, err := reader.ReadPage(ctx, cursor)
		if err != nil {
			// Per ARCHITECTURE.md's governing principle, a failed
			// poll is never a correctness problem for this feature —
			// just a slower wake-up next time around. Retry after the
			// same backoff a caught-up/empty page uses.
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
				continue
			}
		}

		for _, ev := range page.Events {
			if !strings.HasPrefix(ev.Key, prefix) {
				continue
			}
			select {
			case s.ch <- notify.Signal{Key: ev.Key}:
			case <-ctx.Done():
				return
			}
		}

		cursor = page.Next
		if persist != nil {
			// Best-effort: a failed checkpoint save just risks
			// re-delivering already-seen events after a restart,
			// which this feature's "just a wake-up hint" governing
			// principle already tolerates — not treated as fatal.
			_ = persist(cursor)
		}

		if len(page.Events) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
			}
		}
	}
}

func (s *Source) Signals() <-chan notify.Signal { return s.ch }

// Close stops polling and waits for the internal goroutine to exit.
// Safe to call more than once.
func (s *Source) Close() error {
	s.cancel()
	<-s.done
	return nil
}
