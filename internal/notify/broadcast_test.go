package notify

import (
	"testing"
	"time"
)

func TestBroadcasterDeliversToEverySubscriberIndependently(t *testing.T) {
	src := NewFakeSource(4)
	b := NewBroadcaster(src)

	sub1 := b.Subscribe()
	sub2 := b.Subscribe()

	src.Emit(Signal{Key: "a"})

	for i, ch := range []<-chan Signal{sub1, sub2} {
		select {
		case sig := <-ch:
			if sig.Key != "a" {
				t.Fatalf("subscriber %d got %q, want %q", i, sig.Key, "a")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d: timed out waiting for the broadcast signal", i)
		}
	}
}

func TestBroadcasterSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	src := NewFakeSource(0)
	b := NewBroadcaster(src)
	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Give the broadcaster's forwarding goroutine a moment to observe
	// the closed source and mark itself closed.
	time.Sleep(100 * time.Millisecond)

	sub := b.Subscribe()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("expected a closed channel (no signal), got one with data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the post-close subscription to be closed")
	}
}

func TestBroadcasterExistingSubscribersCloseWhenSourceCloses(t *testing.T) {
	src := NewFakeSource(0)
	b := NewBroadcaster(src)
	sub := b.Subscribe()

	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("expected the subscription to close, got a signal instead")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the subscription to close")
	}
}
