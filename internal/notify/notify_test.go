package notify

import "testing"

// TestFakeSourceDeliversEmittedSignals covers ROADMAP.md's task E1
// "Done when": a trivial fake implementation exists and is usable to
// test a consumer independent of any real backend.
func TestFakeSourceDeliversEmittedSignals(t *testing.T) {
	src := NewFakeSource(4)
	defer src.Close()

	src.Emit(Signal{Key: "0000-some-uuid"})
	src.Emit(Signal{Key: "0000-some-uuid.metadata"})

	got := <-src.Signals()
	if got.Key != "0000-some-uuid" {
		t.Fatalf("got %q, want %q", got.Key, "0000-some-uuid")
	}
	got = <-src.Signals()
	if got.Key != "0000-some-uuid.metadata" {
		t.Fatalf("got %q, want %q", got.Key, "0000-some-uuid.metadata")
	}
}

func TestFakeSourceCloseClosesSignalsChannel(t *testing.T) {
	src := NewFakeSource(0)
	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A second Close must not panic (double-close safety).
	if err := src.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, ok := <-src.Signals(); ok {
		t.Fatal("expected Signals() to be closed after Close")
	}
}
