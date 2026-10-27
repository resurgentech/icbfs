package icbfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// A directory's history is its blocks' versions: its root block, under
// the directory's key, and the blocks a split put under keys of their
// own. Normally it's only ever added to, in order. ReplaceDirHistory
// rewrites it: for a caller (a mirror) that learned of a change late,
// after the directory had moved on, and puts the directory's past right.

// DirState is a directory's entries from At until the next state.
type DirState struct {
	At      time.Time
	Entries []block.Entry
}

// ErrDirChanged is ReplaceDirHistory's refusal when the directory isn't
// the version the caller read (DirVersion): it has changed since.
var ErrDirChanged = errors.New("the directory has changed since its history was read")

// dirBlockHistory is every version of every block directory rootKey has
// had, by key (rootKey first): every version of its root block names the
// blocks it had then, and theirs theirs.
func (f *Filesystem) dirBlockHistory(ctx context.Context, rootKey string) ([]string, map[string][]objstore.Object, error) {
	keys := []string{rootKey}
	versions := map[string][]objstore.Object{}
	seen := map[string]bool{rootKey: true}
	for i := 0; i < len(keys); i++ {
		key := keys[i]
		history, err := f.store.History(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		versions[key] = history
		for _, v := range history {
			if v.Deleted {
				continue
			}
			body, _, err := f.store.GetVersion(objstore.CacheContent(ctx), key, v.VersionID)
			if err != nil {
				continue
			}
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil {
				return nil, nil, err
			}
			blk, err := block.Decode(data)
			if err != nil {
				continue
			}
			next := []string{blk.Right}
			for _, c := range blk.Children {
				next = append(next, c.UUID)
			}
			for _, k := range next {
				if k != "" && !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
	}
	return keys, versions, nil
}

// DirHistory is every version of every block directory dirKey has had,
// newest first per key: what the times of its states are read from.
func (f *Filesystem) DirHistory(ctx context.Context, dirKey string) ([]objstore.Object, error) {
	keys, versions, err := f.dirBlockHistory(ctx, dirKey)
	if err != nil {
		return nil, err
	}
	var all []objstore.Object
	for _, k := range keys {
		all = append(all, versions[k]...)
	}
	return all, nil
}

// DirVersion names directory dirKey's current state: it changes whenever
// any of its blocks does.
func (f *Filesystem) DirVersion(ctx context.Context, dirKey string) (string, error) {
	h := sha256.New()
	seen := map[string]bool{}
	queue := []string{dirKey}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		blk, obj, err := f.readBlock(ctx, key)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %s\n", key, obj.ETag)
		if blk.Right != "" {
			queue = append(queue, blk.Right)
		}
		for _, c := range blk.Children {
			queue = append(queue, c.UUID)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// ReplaceDirHistory erases directory dirKey's history (its blocks' and
// its .metadata's) and writes states in its place, oldest first, each
// dated at its time (At, strictly increasing and not in the future); the
// last is the directory now. The items themselves, their links and the
// trash are left as they are: an entry names an item that exists. ifMatch,
// if set, is the DirVersion the caller read: ErrDirChanged if the
// directory has changed since. A change to the directory made while this
// runs can be lost: the caller stops its own writes to it meanwhile.
func (f *Filesystem) ReplaceDirHistory(ctx context.Context, dirKey string, states []DirState, ifMatch string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	ctx = undeclared(ctx)
	if len(states) == 0 {
		return fmt.Errorf("a directory's history needs its state now: %w", ErrInvalidArgument)
	}
	now := time.Now()
	for i, s := range states {
		if s.At.After(now) || i > 0 && !s.At.After(states[i-1].At) {
			return fmt.Errorf("state %d at %s: states go oldest first, none in the future: %w", i, s.At.Format(time.RFC3339Nano), ErrInvalidArgument)
		}
	}
	sorted := make([][]block.Entry, len(states))
	for i, s := range states {
		var err error
		if sorted[i], err = f.sortedEntries(s.Entries); err != nil {
			return fmt.Errorf("state %d: %w", i, err)
		}
	}
	if ifMatch != "" {
		cur, err := f.DirVersion(ctx, dirKey)
		if err != nil {
			return err
		}
		if cur != ifMatch {
			return ErrDirChanged
		}
	}
	oldKeys, oldVersions, err := f.dirBlockHistory(ctx, dirKey)
	if err != nil {
		return err
	}
	oldMeta, err := f.store.History(ctx, metadataObjectKey(dirKey))
	if err != nil {
		return err
	}
	attr, err := f.statDir(ctx, dirKey)
	if err != nil {
		return err
	}

	// The new history, written in order and recorded: each state's blocks
	// below the root are new, and a state's go once the next one is
	// written (a deletion now, which views of the past see past); its
	// .metadata is the directory's attributes now with the state's time as
	// mtime, the directory's mtime being its entries'.
	rec := &objstore.Recorder{}
	var superseded []string
	for i, s := range states {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.At), rec)
		below, err := f.writeDirTree(sctx, dirKey, sorted[i])
		if err != nil {
			return err
		}
		ma := attr
		ma.Mtime = s.At
		if ma.Ctime.IsZero() || ma.Ctime.Before(s.At) {
			ma.Ctime = s.At
		}
		data, err := f.encodeMetadata(sctx, ma)
		if err != nil {
			return err
		}
		if _, err := f.store.Put(sctx, metadataObjectKey(dirKey), bytes.NewReader(data), nil, ""); err != nil {
			return err
		}
		for _, k := range superseded {
			_ = f.store.Delete(ctx, k)
		}
		superseded = below
	}
	// The record: the new versions' dates, and nothing else (the old ones
	// are going).
	byBase := rec.Dates()
	if err := objstore.SetDates(ctx, f.store, dirKey, byBase[dirKey]); err != nil {
		return err
	}
	for base, dates := range byBase {
		if base != dirKey {
			if err := objstore.AddDates(ctx, f.store, base, dates); err != nil {
				return err
			}
		}
	}
	// Then the old history goes: every version of the root block and of
	// .metadata before the new ones, and every other block it had, whole.
	for _, v := range oldVersions[dirKey] {
		if err := f.store.Erase(ctx, dirKey, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	for _, v := range oldMeta {
		if err := f.store.Erase(ctx, metadataObjectKey(dirKey), v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	for _, k := range oldKeys[1:] {
		if err := f.store.EraseAll(ctx, k); err != nil {
			return err
		}
	}
	return f.markPast(ctx)
}

// sortedEntries is entries in this filesystem's collation, refusing two
// with one name (one name in two cases, on a primary-Windows filesystem)
// and an invalid name.
func (f *Filesystem) sortedEntries(entries []block.Entry) ([]block.Entry, error) {
	c := f.collation()
	out := append([]block.Entry(nil), entries...)
	sort.Slice(out, func(i, j int) bool { return c.Compare(out[i].Name, out[j].Name) < 0 })
	for i, e := range out {
		if err := f.checkName(e.Name); err != nil {
			return nil, err
		}
		if e.UUID == "" {
			return nil, fmt.Errorf("%q names no item: %w", e.Name, ErrInvalidArgument)
		}
		if i > 0 && (out[i-1].Name == e.Name || f.primaryWindows && block.EqualFold(out[i-1].Name, e.Name)) {
			return nil, fmt.Errorf("%q twice: %w", e.Name, ErrExists)
		}
	}
	return out, nil
}

// sameEntries reports whether a and b (in the same order) are the same.
func sameEntries(a, b []block.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeDirTree writes entries (sorted) as directory dirKey's tree: its
// root block, and the blocks below it under new keys, which it returns.
// Blocks are full but the last of each level, linked right as splits
// leave them.
func (f *Filesystem) writeDirTree(ctx context.Context, dirKey string, entries []block.Entry) ([]string, error) {
	n, c := f.dirBlockEntries, f.collation()
	if len(entries) <= n {
		return nil, f.putBlock(ctx, dirKey, dirKey, &block.Block{Kind: block.Leaf, Collation: c, Entries: entries}, nil, "")
	}
	// A level: blocks and, for the level above, each one's least name.
	type node struct {
		blk *block.Block
		min string
	}
	var level []node
	for i := 0; i < len(entries); i += n {
		chunk := entries[i:min(i+n, len(entries))]
		level = append(level, node{&block.Block{Kind: block.Leaf, Collation: c, Entries: chunk}, chunk[0].Name})
	}
	var below []string
	for {
		keys := make([]string, len(level))
		for i := range level {
			k, err := f.newKey(ctx)
			if err != nil {
				return nil, err
			}
			keys[i] = k
		}
		if len(level) == 1 {
			// The top: it goes under the directory's own key instead.
			return below, f.putBlock(ctx, dirKey, dirKey, level[0].blk, nil, "")
		}
		// Written right to left, so a block's right sibling is there first.
		for i := len(level) - 1; i >= 0; i-- {
			if i+1 < len(level) {
				level[i].blk.HighKey, level[i].blk.Right = level[i+1].min, keys[i+1]
			}
			if err := f.putBlock(ctx, dirKey, keys[i], level[i].blk, nil, ""); err != nil {
				return nil, err
			}
		}
		below = append(below, keys...)
		var up []node
		for i := 0; i < len(level); i += n {
			var children []block.Child
			for j := i; j < min(i+n, len(level)); j++ {
				minKey := level[j].min
				if j == 0 {
					minKey = "" // the first child of a level covers everything below
				}
				children = append(children, block.Child{MinKey: minKey, UUID: keys[j]})
			}
			up = append(up, node{&block.Block{Kind: block.Internal, Collation: c, Children: children}, level[i].min})
		}
		level = up
	}
}
