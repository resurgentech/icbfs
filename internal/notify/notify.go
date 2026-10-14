// Package notify is the change-notification source behind the optional
// change notifications (docs/architecture.md): a "something changed, go
// look" signal from any backend, never an authoritative account of what
// changed.
package notify

// Signal is one wake-up: the key that changed. Consumers (the FUSE watch
// dispatch, locking's acquire accelerator) only re-check state on it. A
// signal can stand for several writes, coalesced.
type Signal struct {
	// Key is the object key (or, for a prefix-scoped subscription,
	// the key that matched it) that changed.
	Key string
}

// Source is the minimal interface every backend notification adapter
// implements.
type Source interface {
	// Signals is the channel signals arrive on. It's closed when the source
	// ends (its ctx was cancelled, or the backend connection ended);
	// closing says nothing about why.
	Signals() <-chan Signal

	// Close stops the source and releases any underlying resources
	// (connections, goroutines, subscriptions). Safe to call more than
	// once.
	Close() error
}
