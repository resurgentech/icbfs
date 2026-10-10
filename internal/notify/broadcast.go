package notify

import "sync"

// Broadcaster fans out one Source's signals to any number of
// independent subscribers, each receiving every signal — needed as
// soon as more than one consumer needs to observe the same underlying
// Source. Source.Signals() itself is a single-consumption channel (a
// value read by one consumer is gone for any other), which task E3's
// FUSE watch dispatch already treats as its own exclusive channel; B10
// (locking's blocking-acquisition accelerator) needs to observe the
// exact same signals independently, without stealing them from E3's
// dispatch or vice versa — this is what makes that possible.
type Broadcaster struct {
	mu     sync.Mutex
	subs   []chan Signal
	closed bool
}

// bufferedSubscriberCapacity bounds how many unconsumed signals a
// subscriber can fall behind by before this Broadcaster starts
// dropping new ones for it. A dropped signal is never a correctness
// problem for this feature (ARCHITECTURE.md's governing principle: a
// signal is always just a "go check current state" hint) — a slow or
// stalled subscriber just gets fewer wake-ups, never corrupted ones.
const bufferedSubscriberCapacity = 32

// NewBroadcaster starts forwarding every signal from source to every
// current and future Subscribe caller. The underlying source is
// consumed by exactly one internal goroutine, owned by the
// Broadcaster — callers never read source.Signals() themselves once
// it's handed to NewBroadcaster.
func NewBroadcaster(source Source) *Broadcaster {
	b := &Broadcaster{}
	go func() {
		for sig := range source.Signals() {
			b.mu.Lock()
			for _, ch := range b.subs {
				select {
				case ch <- sig:
				default:
				}
			}
			b.mu.Unlock()
		}
		b.mu.Lock()
		b.closed = true
		for _, ch := range b.subs {
			close(ch)
		}
		b.mu.Unlock()
	}()
	return b
}

// Subscribe returns a channel that receives every signal broadcast
// from here on. The returned channel is closed when the underlying
// Source's own channel closes (e.g. Close was called on it). Each
// subscription is independent — reading from one never consumes
// signals meant for another.
func (b *Broadcaster) Subscribe() <-chan Signal {
	ch := make(chan Signal, bufferedSubscriberCapacity)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs = append(b.subs, ch)
	return ch
}
