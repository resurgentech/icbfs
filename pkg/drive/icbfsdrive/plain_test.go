package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// plainDrive is an icbfs drive behind drive.Drive alone: every optional
// interface hidden (InfoSetter, TimeSetter, FileWriter, KeyVersioner,
// Walker, ...), so a mirror on it takes the paths any backend gets. It
// is a mirror's destination (an OriginFinder) by an index of its own,
// kept through every change made through it: writes, folders made,
// moves, origins set, deletes into the trash, restores. A lookup follows
// the rules drive.Mirror's old whole-drive search followed (oldSearch):
// of the live items whose origin is the source item, the newest copy
// (Origin.Copied; of equal ones, the first in a listing); else the first
// item it's an alias on; else the trash's first entry for it. No lookup
// lists the drive: a change re-reads what it touched (an item, or a
// moved, deleted or restored folder's items), and a lookup reads the
// folder of the item it found, for the item as a listing gives it. Each
// answer is also checked against oldSearch's, so a test on a plainDrive
// proves the index right as it goes.
type plainDrive struct {
	drive.Drive
	idrv *Drive
	t    testing.TB
	ix   *originIndex
}

// originKey is a source item: the source drive and its id there.
type originKey struct{ drive, id string }

// originIndex is plainDrive's index.
type originIndex struct {
	mu      sync.Mutex
	live    map[string]drive.Item          // by item id: its path and origin
	claims  map[originKey]map[string]bool  // the live items whose origin it is
	aliases map[originKey]map[string]bool  // the live items it's an alias on
	trash   map[originKey]drive.TrashEntry // the trash's first entry for it
}

// plain is d behind plainDrive, its index loaded with what d holds now:
// one listing of the drive and of its trash, at the start only.
func plain(t testing.TB, d *Drive) plainDrive {
	t.Helper()
	p := plainDrive{Drive: d, idrv: d, t: t, ix: &originIndex{live: map[string]drive.Item{},
		claims: map[originKey]map[string]bool{}, aliases: map[originKey]map[string]bool{}, trash: map[originKey]drive.TrashEntry{}}}
	ctx := system()
	items, err := p.under(ctx, drive.ByPath("/"))
	if err != nil {
		t.Fatalf("plainDrive: list the drive: %v", err)
	}
	for _, it := range items {
		p.ix.add(it)
	}
	var keys []originKey
	for cursor := ""; ; {
		page, err := d.Trash(ctx, drive.TrashOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("plainDrive: list the trash: %v", err)
		}
		for _, e := range page.Entries {
			if e.Origin != nil {
				keys = append(keys, originKey{e.Origin.Drive, e.Origin.ID})
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	p.refreshTrash(ctx, keys)
	return p
}

// under is the item at ref and, if it's a folder, everything below it,
// as a listing gives them.
func (p plainDrive) under(ctx context.Context, ref drive.ItemRef) ([]drive.Item, error) {
	var items []drive.Item
	if ref.Path != "/" {
		it, err := p.listed(ctx, ref)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
		if it.Type != drive.TypeDir {
			return items, nil
		}
		ref = drive.ItemRef{ID: it.ID, Path: it.Path}
	}
	for cursor := ""; ; {
		page, err := p.idrv.List(ctx, ref, drive.ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Next == "" {
			return items, nil
		}
		cursor = page.Next
	}
}

// listed is the item at ref as a listing of its folder gives it.
func (p plainDrive) listed(ctx context.Context, ref drive.ItemRef) (drive.Item, error) {
	it, err := p.idrv.Stat(ctx, ref, drive.At{})
	if err != nil {
		return drive.Item{}, err
	}
	dir := path.Dir(it.Path)
	for cursor := ""; ; {
		page, err := p.idrv.List(ctx, drive.ByPath(dir), drive.ListOptions{Cursor: cursor})
		if err != nil {
			return drive.Item{}, err
		}
		for _, c := range page.Items {
			if c.ID == it.ID {
				return c, nil
			}
		}
		if page.Next == "" {
			return drive.Item{}, fmt.Errorf("%s isn't in its folder's listing: %w", it.Path, drive.ErrNotFound)
		}
		cursor = page.Next
	}
}

// add indexes it as it is now (forgetting what it was).
func (ix *originIndex) add(it drive.Item) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.forget(it.ID)
	ix.live[it.ID] = drive.Item{ID: it.ID, Path: it.Path, Origin: it.Origin}
	if it.Origin == nil {
		return
	}
	put := func(m map[originKey]map[string]bool, k originKey) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][it.ID] = true
	}
	put(ix.claims, originKey{it.Origin.Drive, it.Origin.ID})
	for _, a := range it.Origin.Aliases {
		put(ix.aliases, originKey{it.Origin.Drive, a.ID})
	}
}

// forget drops the item id from the index; the caller holds mu.
func (ix *originIndex) forget(id string) {
	old, ok := ix.live[id]
	if !ok {
		return
	}
	delete(ix.live, id)
	if old.Origin == nil {
		return
	}
	drop := func(m map[originKey]map[string]bool, k originKey) {
		delete(m[k], id)
		if len(m[k]) == 0 {
			delete(m, k)
		}
	}
	drop(ix.claims, originKey{old.Origin.Drive, old.Origin.ID})
	for _, a := range old.Origin.Aliases {
		drop(ix.aliases, originKey{old.Origin.Drive, a.ID})
	}
}

// refreshTrash reads the trash's first entry for each source item whose
// entries a change may have added or taken away.
func (p plainDrive) refreshTrash(ctx context.Context, keys []originKey) {
	for _, k := range keys {
		page, err := p.idrv.Trash(ctx, drive.TrashOptions{OriginDrive: k.drive, OriginID: k.id, Limit: 1})
		if err != nil {
			p.t.Errorf("plainDrive: the trash of %v: %v", k, err)
			continue
		}
		p.ix.mu.Lock()
		if len(page.Entries) > 0 {
			p.ix.trash[k] = page.Entries[0]
		} else {
			delete(p.ix.trash, k)
		}
		p.ix.mu.Unlock()
	}
}

// keysOf is the source items items are copies of.
func keysOf(items []drive.Item) []originKey {
	var keys []originKey
	for _, it := range items {
		if it.Origin != nil {
			keys = append(keys, originKey{it.Origin.Drive, it.Origin.ID})
		}
	}
	return keys
}

// reread indexes what's at ref now: the item, and a folder's items.
func (p plainDrive) reread(ctx context.Context, ref drive.ItemRef) []drive.Item {
	items, err := p.under(ctx, ref)
	if err != nil {
		p.t.Errorf("plainDrive: read %+v after a change: %v", ref, err)
		return nil
	}
	for _, it := range items {
		p.ix.add(it)
	}
	return items
}

func (p plainDrive) Write(ctx context.Context, pth string, body io.Reader, cond drive.Condition, opts drive.WriteOptions) (drive.Item, error) {
	it, err := p.Drive.Write(ctx, pth, body, cond, opts)
	if err == nil {
		p.reread(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
	}
	return it, err
}

func (p plainDrive) Mkdir(ctx context.Context, pth string, parents bool) (drive.Item, error) {
	it, err := p.Drive.Mkdir(ctx, pth, parents)
	if err == nil {
		p.reread(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
	}
	return it, err
}

func (p plainDrive) SetOrigin(ctx context.Context, ref drive.ItemRef, o drive.Origin) error {
	err := p.Drive.SetOrigin(ctx, ref, o)
	if err == nil {
		p.reread(ctx, ref)
	}
	return err
}

func (p plainDrive) Move(ctx context.Context, from drive.ItemRef, to string, replace bool) (drive.Item, error) {
	var gone []drive.Item
	if replace {
		gone, _ = p.under(ctx, drive.ByPath(to))
	}
	it, err := p.Drive.Move(ctx, from, to, replace)
	if err == nil {
		p.ix.mu.Lock()
		for _, g := range gone {
			p.ix.forget(g.ID)
		}
		p.ix.mu.Unlock()
		p.reread(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
		p.refreshTrash(ctx, keysOf(gone))
	}
	return it, err
}

func (p plainDrive) Delete(ctx context.Context, ref drive.ItemRef, recursive bool, cond drive.Condition) error {
	gone, _ := p.under(ctx, ref)
	err := p.Drive.Delete(ctx, ref, recursive, cond)
	if err == nil {
		p.ix.mu.Lock()
		for _, g := range gone {
			p.ix.forget(g.ID)
		}
		p.ix.mu.Unlock()
		p.refreshTrash(ctx, keysOf(gone))
	}
	return err
}

func (p plainDrive) Restore(ctx context.Context, entry string, opts drive.RestoreOptions) (drive.Item, error) {
	it, err := p.Drive.Restore(ctx, entry, opts)
	if err == nil {
		p.refreshTrash(ctx, keysOf(p.reread(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})))
	}
	return it, err
}

// Purge isn't kept in the index: refused.
func (p plainDrive) Purge(ctx context.Context, ref drive.ItemRef, allNames bool) error {
	return fmt.Errorf("plainDrive keeps no index through a purge: %w", drive.ErrNotSupported)
}

// listingLess is the order a listing gives items in (sortItems).
func listingLess(a, b string) int {
	if la, lb := strings.ToLower(a), strings.ToLower(b); la != lb {
		return strings.Compare(la, lb)
	}
	return strings.Compare(a, b)
}

// find is the index's answer, by the old search's rules.
func (ix *originIndex) find(originDrive, originID string) (id, alias string, te *drive.TrashEntry) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	k := originKey{originDrive, originID}
	inOrder := func(set map[string]bool) []drive.Item {
		var its []drive.Item
		for id := range set {
			its = append(its, ix.live[id])
		}
		slices.SortFunc(its, func(a, b drive.Item) int { return listingLess(a.Path, b.Path) })
		return its
	}
	var found *drive.Item
	for _, it := range inOrder(ix.claims[k]) {
		if found == nil || it.Origin.Copied.After(found.Origin.Copied) {
			found = &it // the newest claim wins
		}
	}
	if found != nil {
		return found.ID, "", nil
	}
	for _, it := range inOrder(ix.aliases[k]) {
		if it.Origin.ID != originID {
			return it.ID, originID, nil // folded into another's item
		}
	}
	if e, ok := ix.trash[k]; ok {
		return "", "", &e
	}
	return "", "", nil
}

func (p plainDrive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	it, te, err := p.lookup(ctx, originDrive, originID)
	if got, want := foundOf(it, te, err), foundOf(oldSearch(ctx, p.idrv, originDrive, originID)); got != want {
		p.t.Errorf("plainDrive.FindByOrigin(%s, %s) = %+v; the old search: %+v", originDrive, originID, got, want)
	}
	return it, te, err
}

// lookup is FindByOrigin's answer from the index alone.
func (p plainDrive) lookup(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	id, alias, te := p.ix.find(originDrive, originID)
	switch {
	case te != nil:
		return drive.Item{}, te, nil
	case id == "":
		return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, drive.ErrNotFound)
	}
	p.ix.mu.Lock()
	ref := drive.ItemRef{ID: id, Path: p.ix.live[id].Path}
	p.ix.mu.Unlock()
	it, err := p.listed(ctx, ref)
	if errors.Is(err, drive.ErrNotFound) {
		p.t.Errorf("plainDrive: the index has %s at %s, the drive doesn't", id, ref.Path)
	}
	if err != nil {
		return drive.Item{}, nil, err
	}
	it.Alias = alias
	return it, nil, nil
}
