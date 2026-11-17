package icbfstest

// A store that fails, loses the answer to, or holds the n-th write of a
// named kind (T82-10 round 6, the rows of interloc's docs/STATES.md that
// are a store error or a cut on one write): one wrapper for every such
// test in icbfs-git.

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// The kinds of write a Fault names.
const (
	WritePut         = "put"         // Put (a part, a note), with or without ifMatch
	WritePutIfAbsent = "putifabsent" // PutIfAbsent (a record, an outcome, a mark)
	WritePutParts    = "putparts"    // PutParts (an assembly's copy)
	WriteEraseAll    = "eraseall"    // EraseAll (an upload's parts, an upload)
)

// What a Fault does at its write.
const (
	// Fail: the write is not made; the caller gets ErrInjected.
	Fail = iota
	// Lost: the write is made and its answer lost; the caller gets
	// ErrInjected (a store that committed and a connection that dropped,
	// or a process that went right after).
	Lost
	// Hold: the write waits until Release is closed or its context ends
	// (then ErrInjected wrapping the context's error, the write not made).
	Hold
)

// ErrInjected is the error a Fault gives.
var ErrInjected = errors.New("icbfstest: an injected store fault")

// Fault is the n-th write (from 1) of Kind whose key contains Match (for
// EraseAll, its prefix), in Start's stores, met with Do.
type Fault struct {
	Kind  string
	Match string
	N     int
	Do    int
	// Release ends a Hold.
	Release chan struct{}
	// Reached, if not nil, is closed when the fault's write is reached.
	Reached chan struct{}
}

type armed struct {
	f     Fault
	seen  int
	fired int
}

var (
	faultMu sync.Mutex
	faults  []*armed
)

// InjectFault arms f until the test ends; fired says how many times its
// write was met (0 or 1).
func InjectFault(t interface{ Cleanup(func()) }, f Fault) (fired func() int) {
	a := &armed{f: f}
	faultMu.Lock()
	faults = append(faults, a)
	faultMu.Unlock()
	t.Cleanup(func() {
		faultMu.Lock()
		defer faultMu.Unlock()
		for i, b := range faults {
			if b == a {
				faults = append(faults[:i], faults[i+1:]...)
				return
			}
		}
	})
	return func() int {
		faultMu.Lock()
		defer faultMu.Unlock()
		return a.fired
	}
}

// faultAt is the fault this write meets, if any (counted once).
func faultAt(kind, key string) *Fault {
	faultMu.Lock()
	defer faultMu.Unlock()
	for _, a := range faults {
		if a.f.Kind != kind || !strings.Contains(key, a.f.Match) || a.fired > 0 {
			continue
		}
		a.seen++
		if a.seen == a.f.N {
			a.fired++
			f := a.f
			return &f
		}
	}
	return nil
}

// write runs do under the fault this write meets.
func write[T any](ctx context.Context, kind, key string, do func() (T, error)) (T, error) {
	var zero T
	f := faultAt(kind, key)
	if f == nil {
		return do()
	}
	if f.Reached != nil {
		close(f.Reached)
	}
	switch f.Do {
	case Fail:
		return zero, ErrInjected
	case Lost:
		if _, err := do(); err != nil {
			return zero, err
		}
		return zero, ErrInjected
	default:
		select {
		case <-f.Release:
			return do()
		case <-ctx.Done():
			return zero, errors.Join(ErrInjected, ctx.Err())
		}
	}
}

func (s r3Store) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return write(ctx, WritePut, key, func() (*objstore.Object, error) { return s.Store.Put(ctx, key, body, md, ifMatch) })
}

func (s r3Store) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	return write(ctx, WritePutIfAbsent, key, func() (*objstore.Object, error) { return s.Store.PutIfAbsent(ctx, key, body, md) })
}

func (s r3Store) EraseAll(ctx context.Context, prefix string) error {
	_, err := write(ctx, WriteEraseAll, prefix, func() (struct{}, error) { return struct{}{}, s.Store.EraseAll(ctx, prefix) })
	return err
}
