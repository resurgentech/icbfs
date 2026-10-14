// Package azurecf is the checkpoint and filter logic of an Azure Blob
// Storage Change Feed source (docs/architecture.md's Change notifications
// section): resuming the feed from a cursor, and filtering keys
// client-side, since the Change Feed has no server-side filter.
//
// It doesn't use the Azure SDK: with no Storage Account to test against,
// there's no client. Reader is where one would plug in.
package azurecf

import (
	"context"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/notify"
)

// Event is one Change Feed entry: the key, all the filter needs. A Reader
// translates the SDK's record type into it.
type Event struct {
	Key string // the blob name (object key) this event is about
}

// Cursor is an opaque checkpoint: how far the feed has been read, so a
// restart resumes there. Its form would be whatever the SDK's continuation
// token is.
type Cursor []byte

// Page is one page of events read from the feed, plus the cursor to
// resume from for the next page.
type Page struct {
	Events []Event
	Next   Cursor
}

// Reader reads one page of the Change Feed from cursor (nil for the
// beginning). There's no implementation yet; see the package comment.
type Reader interface {
	ReadPage(ctx context.Context, cursor Cursor) (Page, error)
}

// pollInterval is how often Source polls once it has caught up. The Change
// Feed's latency is minutes, so polling faster would only cost API calls.
const pollInterval = 30 * time.Second

// Source polls a Reader, forwards events whose keys start with prefix
// (filtering client-side: every reader of an account's feed sees every
// tenant's events), and saves its cursor with persist after each page, so a
// restart resumes from there.
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
			// A failed poll only delays the next wake-up: retry after the
			// usual interval.
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
			// Best effort: a lost checkpoint only means events seen again
			// after a restart, which hints tolerate.
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
