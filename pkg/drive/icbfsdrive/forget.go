package icbfsdrive

import (
	"context"
	"io"
	"sync"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// A dated change forgets what the history cache kept of the keys it
// wrote, and nothing else: the past of everything else is as it was, and
// views of it stay warm through a mirror's pass. The keys come from the
// drive's store (noteWrites), which sees every write the change makes,
// dated or not.

// writtenKeys is the keys the writes under one change wrote.
type writtenKeys struct {
	mu       sync.Mutex
	keys     map[string]bool
	prefixes []string // EraseAll's
}

type writtenKeysKey struct{}

func withWrittenKeys(ctx context.Context, w *writtenKeys) context.Context {
	return context.WithValue(ctx, writtenKeysKey{}, w)
}

func noteWritten(ctx context.Context, key string) {
	if w, _ := ctx.Value(writtenKeysKey{}).(*writtenKeys); w != nil {
		w.mu.Lock()
		if w.keys == nil {
			w.keys = map[string]bool{}
		}
		w.keys[key] = true
		w.mu.Unlock()
	}
}

func noteErasedUnder(ctx context.Context, prefix string) {
	if w, _ := ctx.Value(writtenKeysKey{}).(*writtenKeys); w != nil {
		w.mu.Lock()
		w.prefixes = append(w.prefixes, prefix)
		w.mu.Unlock()
	}
}

// forgettable is the keys to forget: each written, and its item's
// .version (the dates of what was written) and .metadata (a content
// version put back on top rewrites its references).
func (w *writtenKeys) forgettable() (keys, prefixes []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range w.keys {
		base := objstore.BaseKey(k)
		add(k)
		add(base + objstore.VersionSuffix)
		add(base + ".metadata")
	}
	return keys, append([]string(nil), w.prefixes...)
}

// noteWrites is the drive's store as its filesystem writes through it:
// each write's key goes to its change's writtenKeys.
type noteWrites struct{ objstore.Store }

func (s *noteWrites) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	defer noteWritten(ctx, key)
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *noteWrites) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	defer noteWritten(ctx, key)
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

func (s *noteWrites) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	defer noteWritten(ctx, key)
	return s.Store.PutParts(ctx, key, parts, md, ifMatch)
}

func (s *noteWrites) Delete(ctx context.Context, key string) error {
	defer noteWritten(ctx, key)
	return s.Store.Delete(ctx, key)
}

func (s *noteWrites) Undelete(ctx context.Context, key string) error {
	defer noteWritten(ctx, key)
	return s.Store.Undelete(ctx, key)
}

func (s *noteWrites) Erase(ctx context.Context, key, versionID string) error {
	defer noteWritten(ctx, key)
	return s.Store.Erase(ctx, key, versionID)
}

func (s *noteWrites) EraseAll(ctx context.Context, prefix string) error {
	defer noteErasedUnder(ctx, prefix)
	return s.Store.EraseAll(ctx, prefix)
}

// AbortCopy, ListPages and ListPagesAfter pass the store's through, as
// dropOnWrite does (objstore.CopyAborter, PageLister, AfterLister): an
// abort ends no version, a listing writes nothing. Without them the
// filesystem would never end a store copy, and would list a prefix
// whole (the trash among them).
func (s *noteWrites) AbortCopy(ctx context.Context, key, id string) error {
	return objstore.AbortCopy(ctx, s.Store, key, id)
}

func (s *noteWrites) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	return objstore.ListEach(ctx, s.Store, prefix, fn)
}

func (s *noteWrites) ListPagesAfter(ctx context.Context, prefix, after string, fn func([]objstore.Object) error) error {
	return objstore.ListEachAfter(ctx, s.Store, prefix, after, fn)
}
