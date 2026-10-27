package icbfsdrive

import (
	"context"
	"sync"
	"time"
)

// pastMarkEvery is how often, at most, the drive tells other readers'
// history caches that the filesystem's past has changed
// (icbfs.MarkPastChanged): a mirror catching up changes the past many
// times a second, and only the last mark matters to a reader.
const pastMarkEvery = time.Second

// pastMarker sends the drive's past-changed marks, at most one per
// pastMarkEvery. A change within the interval of the last mark isn't
// dropped but put off to the interval's end, so the last change is
// always marked; Close sends one still put off.
type pastMarker struct {
	mark func(context.Context) error

	mu      sync.Mutex
	last    time.Time   // when the last mark was sent
	pending *time.Timer // a mark put off, if one is
}

// changed marks the past changed now, or at the interval's end.
func (p *pastMarker) changed(ctx context.Context) {
	p.mu.Lock()
	if p.pending != nil {
		p.mu.Unlock()
		return // the mark put off is after this change
	}
	if wait := pastMarkEvery - time.Since(p.last); wait > 0 {
		p.pending = time.AfterFunc(wait, p.flush)
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	_ = p.mark(context.WithoutCancel(ctx))
}

// flush sends a mark put off, if there is one.
func (p *pastMarker) flush() {
	p.mu.Lock()
	if p.pending == nil {
		p.mu.Unlock()
		return
	}
	p.pending.Stop()
	p.pending, p.last = nil, time.Now()
	p.mu.Unlock()
	_ = p.mark(context.Background())
}
