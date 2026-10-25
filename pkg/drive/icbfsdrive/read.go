package icbfsdrive

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// List lists a directory, Depth levels down, sorted, a page at a time.
func (d *Drive) List(ctx context.Context, ref drive.ItemRef, opts drive.ListOptions) (drive.Page, error) {
	fsys, err := d.at(ctx, opts.At)
	if err != nil {
		return drive.Page{}, err
	}
	dir, err := d.resolve(ctx, fsys, ref)
	if err != nil {
		return drive.Page{}, err
	}
	if dir.typ != icbfs.TypeDir {
		return drive.Page{}, drive.ErrNotDir
	}
	depth := opts.Depth
	if depth < 1 {
		depth = 1
	}
	var nodes []*node
	var walk func(dir *node, level int) error
	walk = func(dir *node, level int) error {
		if ok, err := d.allowed(ctx, dir, needRead); err != nil {
			return err
		} else if !ok {
			if level == 1 {
				return fmt.Errorf("%s: %w", dir.path, drive.ErrForbidden)
			}
			return nil // a subdirectory the profile can't list: left out
		}
		entries, err := fsys.ReadDir(ctx, dir.key)
		if err != nil {
			return mapErr(err)
		}
		children := make([]*node, len(entries))
		chain := append(append([]ancestor(nil), dir.chain...), ancestor{key: dir.key, path: dir.path, attr: dir.attr})
		var wg sync.WaitGroup
		sem := make(chan struct{}, 16)
		for i, e := range entries {
			if !visible(e.Type) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, name, key string, typ icbfs.EntryType) {
				defer wg.Done()
				defer func() { <-sem }()
				a, err := fsys.Stat(ctx, key, typ)
				if err != nil {
					return
				}
				children[i] = &node{fsys: fsys, key: key, typ: typ, name: name, path: joinPath(dir.path, name), attr: a, chain: chain}
			}(i, e.Name, e.UUID, e.Type)
		}
		wg.Wait()
		for _, c := range children {
			if c == nil {
				continue
			}
			nodes = append(nodes, c)
			if c.typ == icbfs.TypeDir && level < depth {
				if err := walk(c, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(dir, 1); err != nil {
		return drive.Page{}, err
	}

	items := make([]drive.Item, len(nodes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, n *node) {
			defer wg.Done()
			defer func() { <-sem }()
			items[i] = d.item(ctx, n, true)
		}(i, n)
	}
	wg.Wait()
	sortItems(items, opts.Sort)

	start := 0
	if opts.Cursor != "" {
		for i, it := range items {
			if it.Path == opts.Cursor {
				start = i + 1
				break
			}
		}
	}
	items = items[start:]
	page := drive.Page{Items: items}
	if opts.Limit > 0 && len(items) > opts.Limit {
		page.Items = items[:opts.Limit]
		page.Next = page.Items[opts.Limit-1].Path
	}
	if page.Items == nil {
		page.Items = []drive.Item{}
	}
	return page, nil
}

func sortItems(items []drive.Item, by string) {
	less := func(a, b drive.Item) bool {
		return strings.ToLower(a.Path) < strings.ToLower(b.Path) || strings.EqualFold(a.Path, b.Path) && a.Path < b.Path
	}
	switch by {
	case "modified":
		sort.SliceStable(items, func(i, j int) bool {
			if !items[i].Modified.Equal(items[j].Modified) {
				return items[i].Modified.After(items[j].Modified)
			}
			return less(items[i], items[j])
		})
	case "size":
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].Size != items[j].Size {
				return items[i].Size > items[j].Size
			}
			return less(items[i], items[j])
		})
	default:
		sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
	}
}

// Stat describes one item, with every path to it and the profile's
// rights on it. At.Version describes that version of a file.
func (d *Drive) Stat(ctx context.Context, ref drive.ItemRef, at drive.At) (drive.Item, error) {
	fsys, err := d.at(ctx, drive.At{Time: at.Time, Name: at.Name})
	if err != nil {
		return drive.Item{}, err
	}
	n, err := d.resolve(ctx, fsys, ref)
	if err != nil {
		if at.Version != "" && ref.ID != "" {
			return d.statTrashed(ctx, ref.ID, at.Version)
		}
		return drive.Item{}, err
	}
	it := d.item(ctx, n, true)
	if at.Version != "" {
		if n.typ != icbfs.TypeFile {
			return drive.Item{}, drive.ErrIsDir
		}
		obj, err := fsys.Store().HeadVersion(ctx, n.key, at.Version)
		if err != nil {
			return drive.Item{}, mapErr(err)
		}
		it.Size = obj.Size
		it.Version = &drive.Version{ID: obj.VersionID, Time: obj.LastModified, Size: obj.Size, Current: it.Version != nil && it.Version.ID == obj.VersionID}
	}
	if len(n.chain) > 0 {
		it.ParentID = n.parentKey()
	}
	if n.typ == icbfs.TypeFile && n.attr.Nlink > 1 {
		it.Paths, _ = d.findPaths(ctx, fsys, n.key, 0)
	} else {
		it.Paths = []string{n.path}
	}
	if d.windows {
		if sd, _ := sdOf(n); sd != nil {
			it.Owner = sd.Owner
		}
	} else {
		it.Owner = fmt.Sprintf("uid:%d", n.attr.Uid)
	}
	it.Rights = d.rights(ctx, n)
	return it, nil
}

// statTrashed describes a version of a deleted file, from the trash.
func (d *Drive) statTrashed(ctx context.Context, id, version string) (drive.Item, error) {
	e, err := d.trashedItem(ctx, id)
	if err != nil {
		return drive.Item{}, err
	}
	obj, err := d.fs.Store().HeadVersion(ctx, id, version)
	if err != nil {
		return drive.Item{}, mapErr(err)
	}
	return drive.Item{ID: id, Name: e.Name, Path: e.Path, Type: e.Type, Size: obj.Size,
		ContentType: contentType(e.Name, icbfs.Attr{}), Modified: obj.LastModified,
		Version: &drive.Version{ID: obj.VersionID, Time: obj.LastModified, Size: obj.Size}}, nil
}

// Read streams a file's content: now, as of a time or name, or one
// version by id (a deleted file's too, from the trash).
func (d *Drive) Read(ctx context.Context, ref drive.ItemRef, at drive.At, rng *drive.Range) (io.ReadCloser, drive.Item, error) {
	it, err := d.Stat(ctx, ref, at)
	if err != nil {
		return nil, drive.Item{}, err
	}
	if it.Type != drive.TypeFile {
		return nil, drive.Item{}, drive.ErrIsDir
	}
	if it.Rights != nil && !it.Rights.Read {
		return nil, drive.Item{}, fmt.Errorf("%s: %w", it.Path, drive.ErrForbidden)
	}
	if it.Rights == nil { // a trashed file: the drive's root decides
		root, err := d.rootNode(ctx, d.fs)
		if err != nil {
			return nil, drive.Item{}, err
		}
		if err := d.check(ctx, root, needRead); err != nil {
			return nil, drive.Item{}, err
		}
	}
	fsys, err := d.at(ctx, drive.At{Time: at.Time, Name: at.Name})
	if err != nil {
		return nil, drive.Item{}, err
	}
	store := fsys.Store()
	var body io.ReadCloser
	if it.Version != nil && it.Version.ID != "" {
		body, _, err = store.GetVersion(ctx, it.ID, it.Version.ID)
	} else {
		body, _, err = store.Get(ctx, it.ID)
	}
	if err != nil {
		return nil, drive.Item{}, mapErr(err)
	}
	if rng != nil {
		if rng.Offset > 0 {
			if _, err := io.CopyN(io.Discard, body, rng.Offset); err != nil {
				body.Close()
				return nil, drive.Item{}, fmt.Errorf("range past the end: %w", drive.ErrInvalid)
			}
		}
		if rng.Length >= 0 {
			body = limitedReadCloser{io.LimitReader(body, rng.Length), body}
		}
	}
	return body, it, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// Versions lists a file's versions, newest first, with the names each was
// current at. A deleted file's versions come from the trash.
func (d *Drive) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	id := ref.ID
	n, err := d.resolve(ctx, d.fs, ref)
	switch {
	case err == nil:
		if n.typ != icbfs.TypeFile {
			return nil, drive.ErrIsDir
		}
		if err := d.check(ctx, n, needRead); err != nil {
			return nil, err
		}
		id = n.key
	case ref.ID != "":
		if _, terr := d.trashedItem(ctx, ref.ID); terr != nil {
			return nil, err
		}
		root, rerr := d.rootNode(ctx, d.fs)
		if rerr != nil {
			return nil, rerr
		}
		if err := d.check(ctx, root, needRead); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	objs, err := d.fs.Store().History(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	dates, err := objstore.ReadDates(ctx, d.fs.Store(), id)
	if err != nil {
		return nil, mapErr(err)
	}
	// Newest first by effective date (a version written late sorts where
	// it belongs); the store's order breaks ties.
	current := ""
	if len(objs) > 0 {
		current = objs[0].VersionID
	}
	sort.SliceStable(objs, func(i, j int) bool { return dates.Time(&objs[i]).After(dates.Time(&objs[j])) })
	var sums map[string]icbfs.Checksums
	if n != nil {
		sums = n.attr.Checksums
	} else if attr, err := d.fs.Stat(ctx, id, icbfs.TypeFile); err == nil {
		sums = attr.Checksums
	}
	names, _ := d.Names(ctx)
	var out []drive.Version
	for i, o := range objs {
		if o.Deleted {
			continue
		}
		v := drive.Version{ID: o.VersionID, Time: dates.Time(&o), Stored: o.LastModified, Size: o.Size, Current: o.VersionID == current,
			Checksums: toChecksums(sums[o.ETag])}
		// Current at a name's time: written at or before it, and the next
		// version (or delete) came after it.
		until := time.Now().Add(time.Hour)
		if i > 0 {
			until = dates.Time(&objs[i-1])
		}
		for _, nm := range names {
			if !nm.Time.Before(dates.Time(&o)) && nm.Time.Before(until) {
				v.Names = append(v.Names, nm.Name)
			}
		}
		out = append(out, v)
	}
	if out == nil {
		out = []drive.Version{}
	}
	return out, nil
}

var _ drive.Checksummer = (*Drive)(nil)

// Checksums are a file version's ("" for the current one), computed and
// kept if they weren't: by its path or id, or by id once it's in the
// trash.
func (d *Drive) Checksums(ctx context.Context, ref drive.ItemRef, version string) (drive.Checksums, error) {
	id := ref.ID
	n, err := d.resolve(ctx, d.fs, ref)
	switch {
	case err == nil:
		if n.typ != icbfs.TypeFile {
			return drive.Checksums{}, drive.ErrIsDir
		}
		if err := d.check(ctx, n, needRead); err != nil {
			return drive.Checksums{}, err
		}
		id = n.key
	case ref.ID != "":
		if _, terr := d.trashedItem(ctx, ref.ID); terr != nil {
			return drive.Checksums{}, err
		}
		root, rerr := d.rootNode(ctx, d.fs)
		if rerr != nil {
			return drive.Checksums{}, rerr
		}
		if err := d.check(ctx, root, needRead); err != nil {
			return drive.Checksums{}, err
		}
	default:
		return drive.Checksums{}, err
	}
	c, err := d.fs.VersionChecksums(ctx, id, version)
	if err != nil {
		return drive.Checksums{}, mapErr(err)
	}
	return *toChecksums(c), nil
}
