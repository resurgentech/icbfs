package azurecf

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeReader implements Reader over a canned sequence of pages, for
// exercising the checkpoint/filter logic without a real Azure Storage
// Account — task E5's explicitly-chosen test strategy (ARCHITECTURE.md's
// Testing reality note: no real account is provisionable here).
type fakeReader struct {
	mu    sync.Mutex
	pages map[string]Page // keyed by the cursor that reads it
	calls []Cursor        // every cursor ReadPage was called with, in order
}

func newFakeReader(pages map[string]Page) *fakeReader {
	return &fakeReader{pages: pages}
}

func (r *fakeReader) ReadPage(ctx context.Context, cursor Cursor) (Page, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cursor)
	page, ok := r.pages[string(cursor)]
	if !ok {
		// Once we've walked off the end of the canned pages, report
		// "caught up" (empty page, same cursor) rather than erroring —
		// matches a real feed's steady-state behavior once a consumer
		// has read everything currently available.
		return Page{Next: cursor}, nil
	}
	return page, nil
}

func (r *fakeReader) callsSoFar() []Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Cursor(nil), r.calls...)
}

// TestSourceFiltersByPrefixAndResumesFromCheckpoint covers ROADMAP.md's
// task E5 "Done when": the checkpoint/filter logic has a passing test
// against a fake feed. Two pages, each mixing a matching and a
// non-matching key, confirm (a) only prefix-matching events are
// forwarded as signals and (b) the reader is queried with each page's
// own continuation cursor in turn, not re-read from the beginning —
// the actual pull-and-resume behavior this task exists to implement.
func TestSourceFiltersByPrefixAndResumesFromCheckpoint(t *testing.T) {
	pages := map[string]Page{
		"": {
			Events: []Event{
				{Key: "0000-match-1"},
				{Key: "9999-no-match-1"},
			},
			Next: Cursor("cursor-1"),
		},
		"cursor-1": {
			Events: []Event{
				{Key: "9999-no-match-2"},
				{Key: "0000-match-2"},
			},
			Next: Cursor("cursor-2"),
		},
	}
	reader := newFakeReader(pages)

	var persistMu sync.Mutex
	var persisted []Cursor
	persist := func(c Cursor) error {
		persistMu.Lock()
		defer persistMu.Unlock()
		persisted = append(persisted, c)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := New(ctx, reader, "0000-", nil, persist)
	defer src.Close()

	got := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case sig := <-src.Signals():
			got = append(got, sig.Key)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for signal %d", i+1)
		}
	}

	want := []string{"0000-match-1", "0000-match-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got signals %v, want %v (non-matching keys should never be forwarded)", got, want)
	}

	// Give the reader's third call (cursor-2 -> empty/caught-up) a
	// moment to actually happen before inspecting call history.
	time.Sleep(100 * time.Millisecond)
	calls := reader.callsSoFar()
	wantCalls := []Cursor{nil, Cursor("cursor-1"), Cursor("cursor-2")}
	if len(calls) < len(wantCalls) {
		t.Fatalf("got %d ReadPage calls, want at least %d: %v", len(calls), len(wantCalls), calls)
	}
	for i, want := range wantCalls {
		if string(calls[i]) != string(want) {
			t.Fatalf("ReadPage call %d used cursor %q, want %q — not resuming from the checkpoint", i, calls[i], want)
		}
	}

	persistMu.Lock()
	gotPersisted := append([]Cursor(nil), persisted...)
	persistMu.Unlock()
	wantPersisted := []Cursor{Cursor("cursor-1"), Cursor("cursor-2")}
	if len(gotPersisted) < len(wantPersisted) {
		t.Fatalf("got %d persisted cursors, want at least %d: %v", len(gotPersisted), len(wantPersisted), gotPersisted)
	}
	for i, want := range wantPersisted {
		if string(gotPersisted[i]) != string(want) {
			t.Fatalf("persisted cursor %d = %q, want %q", i, gotPersisted[i], want)
		}
	}
}

// TestSourceRestartsFromPersistedCursor confirms the other half of
// "pull-and-resume": a Source started with a non-nil initial cursor
// (as if resuming after a restart that loaded a previously-persisted
// checkpoint) reads from there, not from the beginning of the feed.
func TestSourceRestartsFromPersistedCursor(t *testing.T) {
	pages := map[string]Page{
		"cursor-1": {
			Events: []Event{{Key: "0000-after-restart"}},
			Next:   Cursor("cursor-2"),
		},
	}
	reader := newFakeReader(pages)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := New(ctx, reader, "0000-", Cursor("cursor-1"), nil)
	defer src.Close()

	select {
	case sig := <-src.Signals():
		if sig.Key != "0000-after-restart" {
			t.Fatalf("got signal %q, want %q", sig.Key, "0000-after-restart")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
	}

	calls := reader.callsSoFar()
	if len(calls) == 0 || string(calls[0]) != "cursor-1" {
		t.Fatalf("first ReadPage call used cursor %v, want %q (should resume, not start from the beginning)", calls, "cursor-1")
	}
}
