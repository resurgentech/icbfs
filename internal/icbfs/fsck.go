package icbfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
)

// CheckProblem is one inconsistency Check found: what (Kind), on which
// object (Key), at which path if it has one, and the details.
type CheckProblem struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

// CheckReport is what Check found: counts of what it walked, the
// problems, and the objects nothing reaches.
type CheckReport struct {
	Directories int            `json:"directories"`
	Files       int            `json:"files"` // regular files, symlinks and special files
	Blocks      int            `json:"blocks"`
	Problems    []CheckProblem `json:"problems"`

	// Unreachable counts current objects of the filesystem that no name
	// leads to, and lists up to maxUnreachableListed of them. Some are
	// expected, so they aren't problems: the right half of a split that
	// lost its race, a file deleted while open whose last close hasn't
	// come, a lock object left after its file went.
	Unreachable       int      `json:"unreachable"`
	UnreachableListed []string `json:"unreachable_listed,omitempty"`
}

// OK reports whether Check found no problems.
func (r CheckReport) OK() bool { return len(r.Problems) == 0 }

const maxUnreachableListed = 50

type checkedItem struct {
	typ   EntryType
	paths []string
}

// Check walks the whole filesystem and reports inconsistencies, changing
// nothing (fsck, report only). It's meant for a quiet filesystem: a
// directory or file changing while it's walked can show as a problem that
// isn't one.
//
// It checks, for every directory, that each block decodes, its entries are
// in order and inside the block's range, its children and right links make
// a tree with no block reached twice, and no name is listed twice; and for
// every item a name leads to, that it has its objects, that a directory
// has one name, that an item isn't listed with two types, and that a
// file's link count is its number of names.
func (f *Filesystem) Check(ctx context.Context) (CheckReport, error) {
	var r CheckReport
	problem := func(kind, key, p, format string, args ...any) {
		r.Problems = append(r.Problems, CheckProblem{Kind: kind, Key: key, Path: p, Detail: fmt.Sprintf(format, args...)})
	}
	items := map[string]*checkedItem{}
	blocks := map[string]bool{}

	type dirToWalk struct{ key, path string }
	queue := []dirToWalk{{f.RootKey(), "/"}}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		r.Directories++
		names := map[string]bool{}
		err := f.checkTree(ctx, d.key, "", "", blocks, &r, problem, d.path, func(e block.Entry) {
			p := path.Join(d.path, e.Name)
			if names[e.Name] {
				problem("duplicate-name", d.key, p, "the name is listed twice in its directory")
			}
			names[e.Name] = true
			it := items[e.UUID]
			if it == nil {
				it = &checkedItem{typ: e.Type}
				items[e.UUID] = it
			} else if it.typ != e.Type {
				problem("type-mismatch", e.UUID, p, "listed as a %s here and a %s at %s", typeName(e.Type), typeName(it.typ), it.paths[0])
			}
			it.paths = append(it.paths, p)
			if e.Type == TypeDir {
				if len(it.paths) > 1 {
					problem("directory-linked", e.UUID, p, "a directory with a second name (also %s)", it.paths[0])
					return
				}
				queue = append(queue, dirToWalk{e.UUID, p})
			}
		})
		if err != nil {
			return r, err
		}
	}

	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		it := items[key]
		if it.typ == TypeDir {
			continue // its tree was walked, top block and all
		}
		r.Files++
		a, err := f.statFile(ctx, key)
		switch {
		case errors.Is(err, ErrNotFound):
			problem("missing-object", key, it.paths[0], "the content or .metadata object is missing")
			continue
		case err != nil:
			problem("unreadable", key, it.paths[0], "%v", err)
			continue
		}
		if a.Nlink == 0 {
			problem("deleted-but-listed", key, it.paths[0], "the link count is 0 (marked deleted)")
		} else if int(a.Nlink) != len(it.paths) {
			problem("link-count", key, it.paths[0], "link count %d, but %d names: %s", a.Nlink, len(it.paths), strings.Join(it.paths, ", "))
		}
	}

	if err := f.checkUnreachable(ctx, items, blocks, &r); err != nil {
		return r, err
	}
	return r, nil
}

// checkTree walks the directory tree at key, as walkChain does, checking
// each block and calling entry for each entry. lower and upper bound the
// names the walk covers ("" for none).
func (f *Filesystem) checkTree(ctx context.Context, key, lower, upper string, blocks map[string]bool, r *CheckReport,
	problem func(kind, key, path, format string, args ...any), dirPath string, entry func(block.Entry)) error {
	for {
		blk, _, err := f.readBlock(ctx, key)
		if errors.Is(err, ErrNotFound) {
			problem("missing-block", key, dirPath, "a directory block is missing")
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			problem("unreadable-block", key, dirPath, "%v", err)
			return nil
		}
		if blocks[key] {
			problem("block-reached-twice", key, dirPath, "the directory's tree reaches this block twice")
			return nil
		}
		blocks[key] = true
		r.Blocks++
		c := blk.Collation
		if blk.Kind == block.Leaf {
			for i, e := range blk.Entries {
				if i > 0 && c.Compare(blk.Entries[i-1].Name, e.Name) >= 0 {
					problem("out-of-order", key, dirPath, "%q is listed after %q", e.Name, blk.Entries[i-1].Name)
				}
				if lower != "" && c.Compare(e.Name, lower) < 0 {
					problem("outside-range", key, dirPath, "%q is below the block's range, from %q", e.Name, lower)
				}
				if blk.HighKey != "" && c.Compare(e.Name, blk.HighKey) >= 0 {
					problem("outside-range", key, dirPath, "%q is past the block's bound %q", e.Name, blk.HighKey)
				}
				entry(e)
			}
		} else {
			for i, ch := range blk.Children {
				if i > 0 && c.Compare(blk.Children[i-1].MinKey, ch.MinKey) >= 0 {
					problem("out-of-order", key, dirPath, "child from %q is listed after child from %q", ch.MinKey, blk.Children[i-1].MinKey)
				}
				childLower := ch.MinKey
				if i == 0 {
					childLower = lower
				}
				if err := f.checkTree(ctx, ch.UUID, childLower, childUpper(blk, i, upper), blocks, r, problem, dirPath, entry); err != nil {
					return err
				}
			}
		}
		if !followRight(blk, upper) {
			return nil
		}
		key, lower = blk.Right, blk.HighKey
	}
}

// checkUnreachable counts the filesystem's current objects that nothing
// reaches: not the root or a block of a directory walked, not an item a
// name leads to or one of its objects (.metadata, .lock, streams), not a
// trash entry (or the pointer to one), a named version, an origin record
// or the past-changed mark.
func (f *Filesystem) checkUnreachable(ctx context.Context, items map[string]*checkedItem, blocks map[string]bool, r *CheckReport) error {
	versions, err := f.store.HistoryUnder(ctx, f.id+"-")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range versions {
		if seen[v.Key] {
			continue // only the newest version says whether it's current
		}
		seen[v.Key] = true
		if v.Deleted {
			continue
		}
		k := v.Key
		if blocks[k] || k == f.pastKey() || strings.HasPrefix(k, f.trashPrefix()) || strings.HasPrefix(k, nameKey(f.id, "")) ||
			strings.HasPrefix(k, f.id+"-trashby-") || strings.HasPrefix(k, f.id+"-origin-") || strings.HasPrefix(k, f.appPrefix()) {
			continue
		}
		base := k
		for _, suffix := range []string{".metadata", ".lock", ".version"} {
			base = strings.TrimSuffix(base, suffix)
		}
		if blocks[base] {
			continue // a split block's .version record
		}
		if i := strings.Index(base, streamKeyInfix); i >= 0 {
			base = base[:i]
		}
		if items[base] != nil || base == f.RootKey() {
			continue
		}
		r.Unreachable++
		if len(r.UnreachableListed) < maxUnreachableListed {
			r.UnreachableListed = append(r.UnreachableListed, k)
		}
	}
	return nil
}

func typeName(t EntryType) string {
	switch t {
	case TypeFile:
		return "file"
	case TypeDir:
		return "directory"
	case TypeSymlink:
		return "symlink"
	case TypeFifo:
		return "fifo"
	case TypeCharDevice:
		return "character device"
	case TypeBlockDevice:
		return "block device"
	case TypeSocket:
		return "socket"
	}
	return fmt.Sprintf("type %d", t)
}
