package icbfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// App objects are data a tool keeps beside a filesystem, not about any
// one item and not in the tree: a mirror's table of the ids its app keys
// use, say. One object each, <id>-app-<tool>.<name>, named as app keys
// are; versioned by the store like everything else, so a view of the past
// reads the one current then. No mount shows them; fsck knows them; a
// purge leaves them; DeleteAppObject is the only way one goes (short of
// the whole filesystem).

// MaxAppObjectSize is the largest app object: they're small (tables as
// JSON), and written in one piece.
const MaxAppObjectSize = 4 << 20

func (f *Filesystem) appPrefix() string { return f.id + "-app-" }

// appObjectKey is the key of app object name, checked: "<tool>.<name>",
// a tool of lowercase letters and digits.
func (f *Filesystem) appObjectKey(name string) (string, error) {
	tool, rest, ok := strings.Cut(name, ".")
	if !ok || tool == "" || rest == "" || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("app object %q: want <tool>.<name>: %w", name, ErrInvalidArgument)
	}
	for _, r := range tool {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return "", fmt.Errorf("app object %q: the tool is lowercase letters and digits: %w", name, ErrInvalidArgument)
		}
	}
	return f.appPrefix() + name, nil
}

// GetAppObject is app object name's content, and its version.
func (f *Filesystem) GetAppObject(ctx context.Context, name string) ([]byte, *objstore.Object, error) {
	key, err := f.appObjectKey(name)
	if err != nil {
		return nil, nil, err
	}
	body, obj, err := f.store.Get(ctx, key)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	return data, obj, nil
}

// HeadAppObject is app object name's current version, without its
// content; none is ErrNotFound.
func (f *Filesystem) HeadAppObject(ctx context.Context, name string) (*objstore.Object, error) {
	key, err := f.appObjectKey(name)
	if err != nil {
		return nil, err
	}
	obj, err := f.store.Head(ctx, key)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return obj, nil
}

// PutAppObject writes app object name, as a new version.
func (f *Filesystem) PutAppObject(ctx context.Context, name string, data []byte) (*objstore.Object, error) {
	return f.PutAppObjectIf(ctx, name, data, "", false)
}

// PutAppObjectIf is PutAppObject conditioned as the store conditions a
// write: on the current version's ETag (ifMatch), or on there being no
// object yet (ifAbsent); neither means always. A condition that doesn't
// hold is an error objstore.IsPreconditionFailed recognizes, and nothing
// is written.
func (f *Filesystem) PutAppObjectIf(ctx context.Context, name string, data []byte, ifMatch string, ifAbsent bool) (*objstore.Object, error) {
	if err := f.checkWritable(); err != nil {
		return nil, err
	}
	key, err := f.appObjectKey(name)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxAppObjectSize {
		return nil, fmt.Errorf("app object %q: %d bytes, the most is %d: %w", name, len(data), MaxAppObjectSize, ErrFileTooLarge)
	}
	if ifAbsent {
		return f.store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil)
	}
	return f.store.Put(ctx, key, bytes.NewReader(data), nil, ifMatch)
}

// DeleteAppObject deletes app object name; none is ErrNotFound.
func (f *Filesystem) DeleteAppObject(ctx context.Context, name string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	key, err := f.appObjectKey(name)
	if err != nil {
		return err
	}
	if _, err := f.store.Head(ctx, key); err != nil {
		return mapNotFound(err)
	}
	return f.store.Delete(ctx, key)
}

// ListAppObjects lists the app objects whose names start with prefix
// ("" for all), sorted.
func (f *Filesystem) ListAppObjects(ctx context.Context, prefix string) ([]string, error) {
	objs, err := f.store.List(ctx, f.appPrefix()+prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		names = append(names, strings.TrimPrefix(o.Key, f.appPrefix()))
	}
	sort.Strings(names)
	return names, nil
}
