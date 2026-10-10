package notify

import "sync"

// FakeSource is a trivial Source for testing a consumer independent of
// any real backend (task E1's own "Done when") — tests call Emit to
// push a signal as if a real backend had delivered one.
type FakeSource struct {
	ch chan Signal

	mu     sync.Mutex
	closed bool
}

var _ Source = (*FakeSource)(nil)

// NewFakeSource returns a FakeSource with the given signal-channel
// buffer size (0 for unbuffered).
func NewFakeSource(buffer int) *FakeSource {
	return &FakeSource{ch: make(chan Signal, buffer)}
}

func (f *FakeSource) Signals() <-chan Signal { return f.ch }

// Emit pushes sig onto the signal channel, as if a real backend had
// just delivered it. Panics if called after Close, same as sending on
// any closed channel would — tests control both ends, so this isn't
// expected to need graceful handling.
func (f *FakeSource) Emit(sig Signal) {
	f.ch <- sig
}

func (f *FakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.ch)
	return nil
}
