package icbfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// CheckProblem is one inconsistency Check found: what (Kind), on which
// object (Key), at which path if it has one, and the details; and, for
// CheckRepair, whether it was repaired.
type CheckProblem struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Path     string `json:"path,omitempty"`
	Detail   string `json:"detail"`
	Repaired bool   `json:"repaired,omitempty"`
}

// CheckReport is what Check found: counts of what it walked, the
// problems, and the objects nothing reaches.
type CheckReport struct {
	Directories int            `json:"directories"`
	Files       int            `json:"files"` // regular files, symlinks and special files
	Blocks      int            `json:"blocks"`
	Problems    []CheckProblem `json:"problems"`

	// RetiredHolders counts the retired holders: what purges left of
	// items stubs still name (a key behind a delete marker, with a
	// retired .refs and no .metadata). They are neither unreachable nor
	// missing.
	RetiredHolders int `json:"retired_holders"`

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

// Unrepaired is how many of the problems weren't repaired.
func (r CheckReport) Unrepaired() int {
	n := 0
	for _, p := range r.Problems {
		if !p.Repaired {
			n++
		}
	}
	return n
}

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
//
// And stubs and their targets' .refs (checkRefs): ref-target-missing, a
// stub whose target version is gone; ref-to-ref, a stub naming a stub;
// ref-entry-dead, a .refs entry naming no live stub; ref-entry-pending,
// an entry never finalized; ref-holder-dead, a retired holder none of
// whose entries is live.
func (f *Filesystem) Check(ctx context.Context) (CheckReport, error) {
	return f.check(ctx, false)
}

// CheckRepair is Check, then repairs what it can of .refs: a dead entry
// is dropped; a pending one is finalized if its stub is there, dropped if
// not; a dead holder is erased. Each problem repaired says so. A stub
// whose target is gone or is a stub is reported, not repaired.
func (f *Filesystem) CheckRepair(ctx context.Context) (CheckReport, error) {
	if err := f.checkWritable(); err != nil {
		return CheckReport{}, err
	}
	return f.check(ctx, true)
}

func (f *Filesystem) check(ctx context.Context, repair bool) (CheckReport, error) {
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
	stubbed := map[string]map[string]Ref{} // each named file's .metadata refs
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
		if len(a.Refs) > 0 {
			stubbed[key] = a.Refs
		}
		if a.Nlink == 0 {
			problem("deleted-but-listed", key, it.paths[0], "the link count is 0 (marked deleted)")
		} else if int(a.Nlink) != len(it.paths) {
			problem("link-count", key, it.paths[0], "link count %d, but %d names: %s", a.Nlink, len(it.paths), strings.Join(it.paths, ", "))
		}
	}

	if err := f.checkRefs(ctx, items, stubbed, &r, problem, repair); err != nil {
		return r, err
	}
	if err := f.checkUnreachable(ctx, items, blocks, &r); err != nil {
		return r, err
	}
	return r, nil
}

// checkRefs checks stubs and .refs. The stubs looked at are the named
// files' versions their .metadata's refs list (the cache every stub write
// keeps; a stub no writer recorded isn't found here) and the stubs .refs
// entries name. Each .refs is a target's, or a retired holder's (retired,
// and its item has no .metadata; a trashed target's has none either, but
// isn't retired). With repair, each .refs is put right by one finalize
// and one dropRef, which erases a holder left with nothing live; a dead
// holder with no entry to drop is erased directly.
func (f *Filesystem) checkRefs(ctx context.Context, items map[string]*checkedItem, stubbed map[string]map[string]Ref,
	r *CheckReport, problem func(kind, key, path, format string, args ...any), repair bool) error {
	pathOf := func(key string) string {
		if it := items[key]; it != nil {
			return it.paths[0]
		}
		return ""
	}
	checked := map[string]bool{}
	// checkStub looks at the stub version, if it is one, of key, and
	// reports a target that is gone or is a stub.
	checkStub := func(key, version string) error {
		if checked[key+"@"+version] {
			return nil
		}
		checked[key+"@"+version] = true
		obj, err := f.store.HeadVersion(ctx, key, version)
		if objstore.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		ref, ok, err := RefOf(obj)
		if !ok {
			return nil
		}
		if err != nil {
			problem("unreadable", key, pathOf(key), "stub version %s: %v", version, err)
			return nil
		}
		target, err := f.store.HeadVersion(ctx, ref.Key, ref.VersionID)
		switch {
		case objstore.IsNotFound(err) || err == nil && target.Deleted:
			problem("ref-target-missing", key, pathOf(key), "stub version %s names %s version %s, which is gone", version, ref.Key, ref.VersionID)
		case err != nil:
			return err
		case IsRef(target):
			problem("ref-to-ref", key, pathOf(key), "stub version %s names %s version %s, itself a stub", version, ref.Key, ref.VersionID)
		}
		return nil
	}

	stubKeys := make([]string, 0, len(stubbed))
	for k := range stubbed {
		stubKeys = append(stubKeys, k)
	}
	sort.Strings(stubKeys)
	for _, key := range stubKeys {
		history, err := f.store.History(ctx, key)
		if err != nil {
			return err
		}
		for _, v := range history {
			if _, ok := stubbed[key][v.ETag]; ok && !v.Deleted {
				if err := checkStub(key, v.VersionID); err != nil {
					return err
				}
			}
		}
	}

	objs, err := f.store.List(ctx, f.prefix())
	if err != nil {
		return err
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, refsSuffix) {
			continue
		}
		target := strings.TrimSuffix(o.Key, refsSuffix)
		refs, _, err := loadRefs(ctx, f.store, target)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			problem("unreadable", o.Key, "", "%v", err)
			continue
		}
		holder := false
		if refs.Retired {
			if _, err := f.store.Head(ctx, metadataObjectKey(target)); objstore.IsNotFound(err) {
				holder = true
				r.RetiredHolders++
			} else if err != nil {
				return err
			}
		}
		var finalize []*pb.Referrer // pending entries whose stubs are there, with their versions
		var drop []*pb.Referrer
		var reported []int // indexes into r.Problems this .refs repairs
		note := func(kind, format string, args ...any) {
			problem(kind, target, pathOf(target), format, args...)
			reported = append(reported, len(r.Problems)-1)
		}
		registered := map[string]bool{}
		for _, e := range refs.Referrers {
			if !e.Pending {
				registered[e.ReferrerKey+"@"+e.ReferrerVersion] = true
			}
		}
		live := 0
		for _, e := range refs.Referrers {
			if e.Pending {
				v, err := f.findStub(ctx, e.ReferrerKey, target, e.TargetVersion, registered)
				if err != nil {
					return err
				}
				if v != "" {
					note("ref-entry-pending", "%s's entry for version %s is pending; its stub is version %s", e.ReferrerKey, e.TargetVersion, v)
					registered[e.ReferrerKey+"@"+v] = true
					finalize = append(finalize, &pb.Referrer{ReferrerKey: e.ReferrerKey, ReferrerVersion: v, TargetVersion: e.TargetVersion})
					live++
				} else {
					note("ref-entry-pending", "%s's entry for version %s is pending, and no stub of it names that version", e.ReferrerKey, e.TargetVersion)
					drop = append(drop, e)
				}
				continue
			}
			ok, err := f.referrerLive(ctx, target, e)
			if err != nil {
				return err
			}
			if !ok {
				note("ref-entry-dead", "%s version %s, its entry for version %s, isn't a stub naming it", e.ReferrerKey, e.ReferrerVersion, e.TargetVersion)
				drop = append(drop, e)
				continue
			}
			live++
			if err := checkStub(e.ReferrerKey, e.ReferrerVersion); err != nil {
				return err
			}
		}
		if holder && live == 0 {
			note("ref-holder-dead", "a retired holder none of whose %d entries is live", len(refs.Referrers))
		}
		if !repair || len(reported) == 0 {
			continue
		}
		if err := f.repairRefs(ctx, target, finalize, drop, holder && live == 0); err != nil {
			return fmt.Errorf("repair %s: %w", o.Key, err)
		}
		for _, i := range reported {
			r.Problems[i].Repaired = true
		}
	}
	return nil
}

// findStub is the newest version of key that is a stub naming target's
// version targetVersion and isn't in registered (key@version): what a
// pending entry was for.
func (f *Filesystem) findStub(ctx context.Context, key, target, targetVersion string, registered map[string]bool) (string, error) {
	history, err := f.store.History(ctx, key)
	if objstore.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, v := range history {
		if v.Deleted || v.Size == 0 || v.Size > maxRefRecordSize || registered[key+"@"+v.VersionID] {
			continue
		}
		obj, err := f.store.HeadVersion(ctx, key, v.VersionID)
		if objstore.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if ref, ok, err := RefOf(obj); ok && err == nil && ref.Key == target && ref.VersionID == targetVersion {
			return v.VersionID, nil
		}
	}
	return "", nil
}

// repairRefs finalizes target's pending entries named in finalize (by
// referrer and target version) with their stubs' versions, then drops
// the entries in drop (dropRef, which erases a holder left with nothing
// live); a dead holder (deadHolder) with nothing to drop is erased
// directly.
func (f *Filesystem) repairRefs(ctx context.Context, target string, finalize, drop []*pb.Referrer, deadHolder bool) error {
	if len(finalize) > 0 {
		err := f.updateRefs(ctx, target, func(refs *pb.Refs) (bool, error) {
			changed := false
			for _, want := range finalize {
				for _, e := range refs.Referrers {
					if e.Pending && e.ReferrerKey == want.ReferrerKey && e.TargetVersion == want.TargetVersion {
						e.Pending, e.ReferrerVersion, changed = false, want.ReferrerVersion, true
						break
					}
				}
			}
			return changed, nil
		})
		if err != nil {
			return err
		}
	}
	if len(drop) > 0 {
		return f.dropRef(ctx, target, func(e *pb.Referrer) bool {
			for _, d := range drop {
				if sameReferrer(e, d) {
					return true
				}
			}
			return false
		})
	}
	if deadHolder {
		return f.settleHolder(ctx, target)
	}
	return nil
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
// or the past-changed mark. Nor is a .refs: its referrers' stubs reach it,
// whether or not its own item is still named (a trashed target's). A
// retired holder's key is current only as a delete marker, so its kept
// versions aren't counted either: it is neither unreachable nor missing.
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
			strings.HasPrefix(k, f.id+"-trashby-") || strings.HasPrefix(k, f.id+"-origin-") || strings.HasPrefix(k, f.appPrefix()) ||
			strings.HasSuffix(k, refsSuffix) {
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
