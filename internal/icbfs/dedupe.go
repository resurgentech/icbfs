package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// FileMetadata is the .metadata of the file at key, as one GET reads it:
// its checksums and stubs' refs by content ETag (Attr.Checksums,
// Attr.Refs), without the Head of the content that Stat adds. For a
// reader that has the content's versions from a listing already (dedupe).
func (f *Filesystem) FileMetadata(ctx context.Context, key string) (Attr, error) {
	body, _, err := f.store.Get(objstore.CacheContent(ctx), metadataObjectKey(key))
	if err != nil {
		return Attr{}, mapNotFound(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return Attr{}, err
	}
	return decodeFileMetadata(data)
}

// ReferrerCounts is how many stubs elsewhere name each version of the
// item at key, by version id, as its .refs lists them: every entry, a
// pending one too, without judging whether its stub is still there (that
// would be a Head each, and dropping the dead ones a write: fsck settles
// them). pending is the referrers (their keys) of the entries that are
// pending, once each. Reads one object, and none if the item has no
// .refs.
func (f *Filesystem) ReferrerCounts(ctx context.Context, key string) (counts map[string]int, pending []string, err error) {
	refs, _, err := loadRefs(ctx, f.store, key)
	if err != nil {
		return nil, nil, err
	}
	counts = map[string]int{}
	seen := map[string]bool{}
	for _, e := range refs.Referrers {
		counts[e.TargetVersion]++
		if e.Pending && !seen[e.ReferrerKey] {
			seen[e.ReferrerKey] = true
			pending = append(pending, e.ReferrerKey)
		}
	}
	return counts, pending, nil
}

// Replacing a duplicate (icbfs-cli fs dedupe). ReplaceWithRef replaces a
// file's current version, a copy of another version, by a stub naming
// that one, as if it had always been one: it verifies the target as
// PutRef does; puts a pending entry in the target's .refs; puts the stub
// on the key, conditional on the current version's ETag, dated (a
// declared time, then RecordDates) as the version it replaces is; records
// the stub's Ref, its checksums and the old mtime in .metadata (KeepDate:
// that write takes the date of the .metadata it replaces); erases the
// old version and its line in the .version record; and finalizes the
// entry. The stub names the version it replaces (MetaRefReplaces), so a
// run killed anywhere after the entry is finished from the entry alone
// (FinishReplacements).

// MetaRefReplaces is the user-metadata name, on a stub ReplaceWithRef
// wrote, of the version id it replaced.
const MetaRefReplaces = "icbfs_ref_replaces"

// ErrIsRef is ReplaceWithRef's refusal of a version that is already a
// stub.
var ErrIsRef = errors.New("the version is already a stub")

// ReplaceWithRef replaces key's current version, version, by a stub
// naming targetKey's version targetVersion, whose bytes are the same:
// sha256 is both versions' SHA-256, which each one's .metadata must
// record. It refuses (and writes nothing) a version that isn't key's
// current one, or whose If-Match is lost to a writer: ErrFileChanged; one
// that is a stub, ErrIsRef; one stubs name, ErrReferenced; a target that
// isn't one, ErrRefTarget, or another content, ErrRefChecksum. It returns
// the stub. See MetaRefReplaces for what a kill leaves.
func (f *Filesystem) ReplaceWithRef(ctx context.Context, key, version, targetKey, targetVersion string, sha256 []byte) (*objstore.Object, error) {
	return f.replaceCurrent(ctx, key, version, targetKey, targetVersion, sha256, false)
}

// ErrNotRef is RepointRef's refusal of a version that isn't a stub.
var ErrNotRef = errors.New("the version isn't a stub")

// RepointRef is ReplaceWithRef for a current version that is a stub
// already (else ErrNotRef): it is replaced by a stub naming targetKey's
// version targetVersion instead, whose bytes are the same (sha256, which
// key's .metadata must record for the stub, and the target's for it).
// The old stub's entry in its target's .refs is dropped (dropRef, which
// erases a retired holder left with nothing live) once the old stub is
// erased. A stub naming that target already is left as it is, and
// returned.
func (f *Filesystem) RepointRef(ctx context.Context, key, version, targetKey, targetVersion string, sha256 []byte) (*objstore.Object, error) {
	return f.replaceCurrent(ctx, key, version, targetKey, targetVersion, sha256, true)
}

// replaceCurrent is ReplaceWithRef, or with repoint RepointRef.
func (f *Filesystem) replaceCurrent(ctx context.Context, key, version, targetKey, targetVersion string, sha256 []byte, repoint bool) (*objstore.Object, error) {
	if err := f.checkWritable(); err != nil {
		return nil, err
	}
	ctx = undeclared(ctx)
	cur, err := f.store.Head(ctx, key)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if cur.VersionID != version {
		return nil, fmt.Errorf("%s: version %s isn't current, %s is: %w", key, version, cur.VersionID, ErrFileChanged)
	}
	oldRef, isRef, err := RefOf(cur)
	switch {
	case err != nil:
		return nil, err
	case isRef && !repoint:
		return nil, fmt.Errorf("%s version %s: %w", key, version, ErrIsRef)
	case !isRef && repoint:
		return nil, fmt.Errorf("%s version %s: %w", key, version, ErrNotRef)
	case isRef && oldRef.Key == targetKey && oldRef.VersionID == targetVersion:
		return cur, nil
	}
	attr, err := f.FileMetadata(ctx, key)
	if err != nil {
		return nil, err
	}
	if _, ok := attr.Refs[cur.ETag]; ok && !repoint {
		return nil, fmt.Errorf("%s version %s: %w", key, version, ErrIsRef)
	}
	if err := f.unreferenced(ctx, key, version); err != nil {
		return nil, err
	}
	if len(sha256) == 0 || !bytes.Equal(attr.Checksums[cur.ETag].SHA256, sha256) {
		return nil, fmt.Errorf("%s version %s: SHA-256 %x recorded, given %x: %w", key, version, attr.Checksums[cur.ETag].SHA256, sha256, ErrRefChecksum)
	}
	t, err := f.checkRefTarget(ctx, targetKey, targetVersion, sha256)
	if err != nil {
		return nil, err
	}
	if !t.recorded || !bytes.Equal(t.sums.SHA256, sha256) {
		return nil, fmt.Errorf("%s version %s: no SHA-256 recorded: %w", targetKey, targetVersion, ErrRefChecksum)
	}
	if size, _ := ContentSize(cur); t.obj.Size != size {
		return nil, fmt.Errorf("%s version %s is %d bytes, %s version %s %d: %w", targetKey, targetVersion, t.obj.Size, key, version, size, ErrRefChecksum)
	}
	at, err := f.versionDate(ctx, key, version)
	if err != nil {
		return nil, err
	}
	body, meta, err := t.stub()
	if err != nil {
		return nil, err
	}
	meta[MetaRefReplaces] = version
	if err := f.raiseFormat(ctx); err != nil {
		return nil, err
	}
	if err := f.addReferrer(ctx, t, key); err != nil {
		return nil, err
	}
	rec := &objstore.Recorder{}
	dctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, at), rec)
	stub, err := f.store.Put(dctx, key, bytes.NewReader(body), meta, cur.ETag)
	if objstore.IsPreconditionFailed(err) {
		// No stub: the entry goes now, not left for fsck.
		mine := &pb.Referrer{ReferrerKey: key, Pending: true, TargetVersion: t.version}
		if derr := f.dropRef(ctx, t.key, func(r *pb.Referrer) bool { return sameReferrer(r, mine) }); derr != nil {
			return nil, derr
		}
		return nil, fmt.Errorf("%s: a writer replaced version %s: %w", key, version, ErrFileChanged)
	}
	if err != nil {
		return nil, err // the pending entry stays: FinishReplacements drops it
	}
	if _, err := f.RecordDates(dctx, rec); err != nil {
		return nil, fmt.Errorf("stub %s version %s written; dating it: %w", key, stub.VersionID, err)
	}
	stub.Metadata = meta
	if err := f.finishReplace(ctx, t, key, stub, cur); err != nil {
		return nil, err
	}
	return stub, nil
}

// versionDate is the effective date of key's version: its line in the
// .version record, else when the store wrote it, as a listing has it (to
// the millisecond: a Head's is to the second).
func (f *Filesystem) versionDate(ctx context.Context, key, version string) (time.Time, error) {
	history, err := f.store.History(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	dates, err := objstore.ReadDates(ctx, f.store, key)
	if err != nil {
		return time.Time{}, err
	}
	for i := range history {
		if history[i].VersionID == version {
			return dates.Time(&history[i]), nil
		}
	}
	return time.Time{}, fmt.Errorf("%s version %s: %w", key, version, ErrNotFound)
}

// unreferenced refuses (ErrReferenced) version of key if an entry in
// key's .refs, pending or not, names it.
func (f *Filesystem) unreferenced(ctx context.Context, key, version string) error {
	counts, _, err := f.ReferrerCounts(ctx, key)
	if err != nil {
		return err
	}
	if n := counts[version]; n > 0 {
		return fmt.Errorf("%s version %s: %d stubs name it: %w", key, version, n, ErrReferenced)
	}
	return nil
}

// finishReplace does what is left of the replacement of key's version
// old (nil if it is erased already) by stub, naming t, once the stub is
// on key: each step is skipped if done. The stub is dated as old if the
// .version record has no line for it; .metadata records the stub's Ref,
// its checksums and old's mtime; old's line goes and old is erased, if no
// stub names it now, and, if old is a stub (RepointRef), its entry in its
// target's .refs is dropped; the entry is finalized.
func (f *Filesystem) finishReplace(ctx context.Context, t *stubTarget, key string, stub, old *objstore.Object) error {
	var dates objstore.Dates
	if old != nil {
		var err error
		if dates, err = objstore.ReadDates(ctx, f.store, key); err != nil {
			return err
		}
		if _, ok := dates[stub.VersionID]; !ok {
			at, err := f.versionDate(ctx, key, old.VersionID)
			if err != nil {
				return err
			}
			if _, err := objstore.UpdateDates(ctx, f.store, key, objstore.Dates{stub.VersionID: at}); err != nil {
				return err
			}
		}
	}
	_, _, err := f.writeMetadataRead(objstore.KeepDate(ctx), key, false, func(a *Attr) bool {
		if _, ok := a.Refs[stub.ETag]; ok {
			return false
		}
		t.record(a, stub.ETag)
		if old == nil {
			return true
		}
		if sums := a.Checksums[old.ETag]; !t.recorded && sums.Complete() {
			a.Checksums[stub.ETag] = sums
		}
		// The mtime a Stat gave the old version, kept (combineFileAttr).
		mtime := a.Mtime
		if mtime.IsZero() || a.mtimeOverrideETag != old.ETag {
			mtime = old.LastModified
			if created := createdBound(Btime(key)); created.After(mtime) {
				mtime = created
			}
		}
		a.Mtime, a.mtimeOverrideETag = mtime, stub.ETag
		a.dated = true // the item has a .version record now
		return true
	})
	if err != nil {
		return fmt.Errorf("stub %s version %s written; its .metadata: %w", key, stub.VersionID, err)
	}
	if old != nil {
		if err := f.unreferenced(ctx, key, old.VersionID); err != nil {
			// Named since the check: it stays, the stub on top of it.
			f.finalizeReferrer(ctx, t, key, stub.VersionID)
			return fmt.Errorf("stub %s version %s written; the old version kept: %w", key, stub.VersionID, err)
		}
		// Its line first, as restoreCurrent does: a version the record
		// doesn't list is only one more for eraseUnfinished.
		if _, ok := dates[old.VersionID]; ok {
			if _, err := objstore.UpdateDates(ctx, f.store, key, nil, old.VersionID); err != nil {
				return err
			}
		}
		if err := f.store.Erase(ctx, key, old.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
		if r, ok, _ := RefOf(old); ok {
			if err := f.dropRef(ctx, r.Key, func(e *pb.Referrer) bool {
				return e.ReferrerKey == key && e.ReferrerVersion == old.VersionID && !e.Pending
			}); err != nil {
				return err
			}
		}
	}
	f.finalizeReferrer(ctx, t, key, stub.VersionID)
	return nil
}

// FinishReplacements settles the pending entries referrer has in
// target's .refs, as fsck's repair does, finishing a replacement a
// killed ReplaceWithRef left: an entry whose stub is on referrer is
// finalized, after what is left of the replacement is done, if the stub
// is one ReplaceWithRef wrote (MetaRefReplaces); an entry with no stub is
// dropped (dropRef, which erases a retired holder left with nothing
// live). It returns how many replacements it finished.
func (f *Filesystem) FinishReplacements(ctx context.Context, target, referrer string) (int, error) {
	if err := f.checkWritable(); err != nil {
		return 0, err
	}
	ctx = undeclared(ctx)
	refs, _, err := loadRefs(ctx, f.store, target)
	if err != nil {
		return 0, err
	}
	registered := map[string]bool{}
	for _, e := range refs.Referrers {
		if !e.Pending {
			registered[e.ReferrerKey+"@"+e.ReferrerVersion] = true
		}
	}
	finished := 0
	var drop []*pb.Referrer
	for _, e := range refs.Referrers {
		if !e.Pending || e.ReferrerKey != referrer {
			continue
		}
		v, err := f.findStub(ctx, referrer, target, e.TargetVersion, registered)
		if err != nil {
			return finished, err
		}
		if v == "" {
			drop = append(drop, e)
			continue
		}
		registered[referrer+"@"+v] = true
		stub, err := f.store.HeadVersion(ctx, referrer, v)
		if err != nil {
			return finished, err
		}
		t := &stubTarget{key: target, version: e.TargetVersion}
		replaced := stub.Metadata[MetaRefReplaces]
		if replaced == "" {
			f.finalizeReferrer(ctx, t, referrer, v)
			continue
		}
		if t.obj, err = f.store.HeadVersion(ctx, target, e.TargetVersion); objstore.IsNotFound(err) || err == nil && t.obj.Deleted {
			continue // the stub's target is gone: fsck's to report, not ours to finish
		} else if err != nil {
			return finished, err
		}
		if t.sums, err = f.recordedChecksums(ctx, target, t.obj.ETag); err != nil {
			return finished, err
		}
		t.recorded = t.sums.Complete()
		old, err := f.store.HeadVersion(ctx, referrer, replaced)
		switch {
		case objstore.IsNotFound(err) || err == nil && old.Deleted:
			old = nil
		case err != nil:
			return finished, err
		default:
			if size, _ := ContentSize(old); size != t.obj.Size {
				old = nil // not the copy it replaced: left alone
			}
		}
		if err := f.finishReplace(ctx, t, referrer, stub, old); err != nil {
			return finished, err
		}
		finished++
	}
	if len(drop) > 0 {
		err := f.dropRef(ctx, target, func(r *pb.Referrer) bool {
			for _, d := range drop {
				if sameReferrer(r, d) {
					return true
				}
			}
			return false
		})
		if err != nil {
			return finished, err
		}
	}
	return finished, nil
}

// recordedChecksums are the checksums key's .metadata records for etag:
// none for a key with no .metadata (a retired holder's).
func (f *Filesystem) recordedChecksums(ctx context.Context, key, etag string) (Checksums, error) {
	attr, err := f.FileMetadata(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Checksums{}, nil
	}
	if err != nil {
		return Checksums{}, err
	}
	return attr.Checksums[etag], nil
}

// Referrer is one entry of an item's .refs: the stub version of
// referrer Key, Version, naming the item's version TargetVersion; a
// pending one has no Version yet.
type Referrer struct {
	Key, Version, TargetVersion string
	Pending                     bool
}

// LiveReferrers is the live entries of the item at key's .refs (a
// pending one counts as live), its own stubs' aside: the referrers a
// history rewrite of key refuses for (ReplaceFileHistory). The dead
// entries found are dropped (dropRef).
func (f *Filesystem) LiveReferrers(ctx context.Context, key string) ([]Referrer, error) {
	live, err := f.liveReferrers(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	var out []Referrer
	for _, e := range live {
		if e.ReferrerKey != key {
			out = append(out, Referrer{Key: e.ReferrerKey, Version: e.ReferrerVersion, TargetVersion: e.TargetVersion, Pending: e.Pending})
		}
	}
	return out, nil
}

// RetiredHolder reports whether key is a retired holder: what a purge
// left of an item stubs still name (its .refs retired, no .metadata).
func (f *Filesystem) RetiredHolder(ctx context.Context, key string) (bool, error) {
	refs, obj, err := loadRefs(ctx, f.store, key)
	if err != nil || obj == nil || !refs.Retired {
		return false, err
	}
	if _, err := f.store.Head(ctx, metadataObjectKey(key)); objstore.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// PastChangedFunc is the fn SetPastChanged set, if any: for a caller
// that holds the marks back for a while and puts it back after.
func (f *Filesystem) PastChangedFunc() func(context.Context) { return f.pastChanged }
