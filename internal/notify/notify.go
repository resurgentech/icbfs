// Package notify defines the internal event-source abstraction behind
// icbfs's optional Change notifications feature (ARCHITECTURE.md's
// Change notifications section, ROADMAP.md's Part E): a minimal,
// backend-independent "something changed, go check current state"
// wake-up signal, never an authoritative payload of what changed —
// per ARCHITECTURE.md's governing design principle, this holds
// regardless of which backend (MinIO, Azure, AWS S3) is in use, and
// the interface is deliberately too thin to tempt a consumer into
// trusting it as more than that.
package notify

// Signal is a single wake-up notification: the key or prefix that
// changed. Consumers (task E3's FUSE wiring, task B10's lock-polling
// accelerator) treat this purely as a hint to re-check current state,
// never as proof of what the change was — a signal can be coarser
// than the actual writes that produced it (e.g. several rapid writes
// to the same key coalesced into one signal), and a consumer that
// assumes otherwise is the one at fault, not this package.
type Signal struct {
	// Key is the object key (or, for a prefix-scoped subscription,
	// the key that matched it) that changed.
	Key string
}

// Source is the minimal interface every backend notification adapter
// (tasks E4-E6) implements.
type Source interface {
	// Signals returns the channel wake-up signals arrive on. The
	// channel is closed when the source is done — ctx (passed to
	// whatever constructed the Source) was cancelled, or the
	// underlying backend connection ended for any other reason. A
	// closed channel is not itself an error; callers that need to
	// distinguish "done cleanly" from "failed" should check context
	// error state or a backend-specific accessor, not infer it from
	// channel closure alone.
	Signals() <-chan Signal

	// Close stops the source and releases any underlying resources
	// (connections, goroutines, subscriptions). Safe to call more than
	// once.
	Close() error
}
