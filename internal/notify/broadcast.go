package notify

import "sync"

// Broadcaster gives every subscriber every signal of one Source. A Source's
// channel can be read by only one consumer, and the FUSE watch dispatch and
// locking's acquire accelerator each need all of them.
type Broadcaster struct {
	mu     sync.Mutex
	subs   []chan Signal
	closed bool
}

// bufferedSubscriberCapacity is how far a subscriber can fall behind before
// new signals are dropped for it. A slow subscriber only gets fewer
// wake-ups.
const bufferedSubscriberCapacity = 32

// NewBroadcaster forwards every signal from source to every subscriber. Its
// goroutine is source's only reader; callers don't read it themselves after
// this.
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

// Subscribe returns a channel that receives every signal from now on,
// closed when the Source's channel closes. Subscriptions are independent.
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
