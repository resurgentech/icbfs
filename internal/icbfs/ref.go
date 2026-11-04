package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// A stub is a content version that holds no bytes of its own: its body
// is a RefRecord (spec/proto/icbfs/v1/ref.proto) naming another version,
// its target, whose bytes are the file's. Its user metadata says it is
// one and repeats the target and the target's size, so a Head learns the
// file's real size without reading the body. A target is never itself a
// stub. Every reader of a file's bytes follows a stub with one more
// GetVersion (FollowRef); every reader of its size reports MetaRefSize
// (ContentSize). See docs/architecture.md's Object layout section.

// The user-metadata names on a stub.
const (
	MetaRef        = "icbfs_ref"         // "1" on a stub
	MetaRefKey     = "icbfs_ref_key"     // the target's key
	MetaRefVersion = "icbfs_ref_version" // the target's version id
	MetaRefSize    = "icbfs_ref_size"    // the target's size, in decimal: the file's real size
)

// refLayout is the RefRecord layout this package reads.
const refLayout = 1

// formatVersionRefs is the filesystem format (FormatVersion) at which a
// filesystem may hold stubs. This build opens filesystems up to it and
// still creates them at FormatVersion; a filesystem is raised to it at its
// first stub, so an older binary, which can't follow one, won't open it.
const formatVersionRefs = 2

// maxFormatVersion is the newest filesystem format this build opens.
const maxFormatVersion = formatVersionRefs

// openFormatVersion is the newest format applyEntry opens: this build's,
// maxFormatVersion. A variable for the tests, which pin a build to an
// older format to see it refuse a raised filesystem.
var openFormatVersion uint32 = maxFormatVersion

// maxRefRecordSize bounds how much of a stub's body is read: a RefRecord
// is a key, a version id, a size and a hash, a few hundred bytes.
const maxRefRecordSize = 64 << 10

var (
	// ErrRefToRef is the refusal of a stub whose target is itself a stub.
	ErrRefToRef = errors.New("a stub's target is itself a stub")
	// ErrBadRef is the refusal of a stub that doesn't describe its target
	// consistently: no target or size in its metadata, a record that
	// disagrees with its metadata, or a target of another size.
	ErrBadRef = errors.New("a stub that doesn't match its target")
)

// Ref is a stub's target: the key and version whose bytes the stub's
// are, and their size (an Attr's Refs, by the stub's ETag).
type Ref struct {
	Key       string
	VersionID string
	Size      int64
}

// IsRef reports whether obj, as a Head or Get described it, is a stub.
// An object described without its metadata (a history listing) never is.
func IsRef(obj *objstore.Object) bool {
	return obj != nil && obj.Metadata[MetaRef] == "1"
}

// RefOf is the target obj's metadata names, if obj is a stub (ok).
func RefOf(obj *objstore.Object) (r Ref, ok bool, err error) {
	if !IsRef(obj) {
		return Ref{}, false, nil
	}
	size, err := strconv.ParseInt(obj.Metadata[MetaRefSize], 10, 64)
	if err != nil || size < 0 {
		return Ref{}, true, fmt.Errorf("stub %s version %s: size %q: %w", obj.Key, obj.VersionID, obj.Metadata[MetaRefSize], ErrBadRef)
	}
	r = Ref{Key: obj.Metadata[MetaRefKey], VersionID: obj.Metadata[MetaRefVersion], Size: size}
	if r.Key == "" || r.VersionID == "" {
		return Ref{}, true, fmt.Errorf("stub %s version %s names no target: %w", obj.Key, obj.VersionID, ErrBadRef)
	}
	return r, true, nil
}

// ContentSize is the size of the file content obj is: a stub's target's
// (MetaRefSize), not the stub's own few hundred bytes; any other object's
// own size.
func ContentSize(obj *objstore.Object) (int64, error) {
	r, ok, err := RefOf(obj)
	if err != nil {
		return 0, err
	}
	if ok {
		return r.Size, nil
	}
	return obj.Size, nil
}

// decodeRefRecord parses a stub's body, refusing a layout this build
// doesn't read (ErrNewerFormat for a newer one).
func decodeRefRecord(data []byte) (*pb.RefRecord, error) {
	var rec pb.RefRecord
	if err := proto.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode stub: %w", err)
	}
	if err := block.CheckLayout("stub", rec.Layout, refLayout); err != nil {
		return nil, err
	}
	return &rec, nil
}

// FollowRef is the follow step: given a content version (obj) and its
// open body, as a Get or GetVersion returned them, it returns the bytes of
// the file content that version is. For anything but a stub that's body
// itself. For a stub, it reads and closes body (the RefRecord) and returns
// its target's, with one GetVersion. A stub whose record is in a layout
// this build doesn't read, whose target is a stub, or that disagrees with
// its target is refused: never served as bytes. body is closed on any
// error.
func FollowRef(ctx context.Context, store objstore.Store, obj *objstore.Object, body io.ReadCloser) (io.ReadCloser, error) {
	meta, ok, err := RefOf(obj)
	if !ok {
		return body, nil
	}
	if err != nil {
		body.Close()
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxRefRecordSize+1))
	body.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > maxRefRecordSize {
		return nil, fmt.Errorf("stub %s version %s: a record of over %d bytes: %w", obj.Key, obj.VersionID, maxRefRecordSize, ErrBadRef)
	}
	rec, err := decodeRefRecord(data)
	if err != nil {
		return nil, fmt.Errorf("stub %s version %s: %w", obj.Key, obj.VersionID, err)
	}
	if rec.Key != meta.Key || rec.VersionId != meta.VersionID || rec.Size != meta.Size {
		return nil, fmt.Errorf("stub %s version %s: its record names %s version %s (%d bytes), its metadata %s version %s (%d bytes): %w",
			obj.Key, obj.VersionID, rec.Key, rec.VersionId, rec.Size, meta.Key, meta.VersionID, meta.Size, ErrBadRef)
	}
	target, tobj, err := store.GetVersion(ctx, rec.Key, rec.VersionId)
	if err != nil {
		return nil, fmt.Errorf("stub %s version %s: its target %s version %s: %w", obj.Key, obj.VersionID, rec.Key, rec.VersionId, mapNotFound(err))
	}
	if IsRef(tobj) {
		target.Close()
		return nil, fmt.Errorf("stub %s version %s: its target %s version %s: %w", obj.Key, obj.VersionID, rec.Key, rec.VersionId, ErrRefToRef)
	}
	if tobj.Size != rec.Size {
		target.Close()
		return nil, fmt.Errorf("stub %s version %s: its target %s version %s is %d bytes, the stub says %d: %w",
			obj.Key, obj.VersionID, rec.Key, rec.VersionId, tobj.Size, rec.Size, ErrBadRef)
	}
	return target, nil
}

// GetContent is store.Get of key, followed through a stub (FollowRef):
// the file content's bytes, and the version of key they're the content
// of (the stub, if it's one: its ETag is what a conditional write to key
// is conditional on).
func GetContent(ctx context.Context, store objstore.Store, key string) (io.ReadCloser, *objstore.Object, error) {
	body, obj, err := store.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	body, err = FollowRef(ctx, store, obj, body)
	if err != nil {
		return nil, nil, err
	}
	return body, obj, nil
}

// GetContentVersion is GetContent for one version of key.
func GetContentVersion(ctx context.Context, store objstore.Store, key, versionID string) (io.ReadCloser, *objstore.Object, error) {
	body, obj, err := store.GetVersion(ctx, key, versionID)
	if err != nil {
		return nil, nil, err
	}
	body, err = FollowRef(ctx, store, obj, body)
	if err != nil {
		return nil, nil, err
	}
	return body, obj, nil
}

// refsToWire and refsFromWire convert an Attr's Refs to and from
// .metadata's.
func refsToWire(m map[string]Ref) map[string]*pb.Ref {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*pb.Ref, len(m))
	for etag, r := range m {
		out[etag] = &pb.Ref{Key: r.Key, VersionId: r.VersionID, Size: r.Size}
	}
	return out
}

func refsFromWire(m map[string]*pb.Ref) map[string]Ref {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]Ref, len(m))
	for etag, r := range m {
		out[etag] = Ref{Key: r.GetKey(), VersionID: r.GetVersionId(), Size: r.GetSize()}
	}
	return out
}

// Writing a stub (PutRef, CreateWithRef) checks its target, then writes,
// in this order: the target's .refs gains the stub's entry, pending; the
// stub is put on its key; the key's .metadata records the target's
// checksums and the stub's Ref under the stub's ETag; and the .refs entry
// is finalized with the stub's version id, best effort. The entry comes
// first on purpose: a crash before the stub leaves an entry naming no
// stub, which only holds a purge of the target back until fsck drops it,
// where the other order could leave a stub whose target is purged.

// refsSuffix names a target's .refs object: <id>-<uuid>.refs beside its
// content key, listing its referrers (pb.Refs).
const refsSuffix = ".refs"

// refsLayout is the .refs layout this package reads and writes.
const refsLayout = 1

// refsObjectKey is the key of the .refs object of the item at key.
func refsObjectKey(key string) string { return key + refsSuffix }

var (
	// ErrRefTarget is the refusal of a stub's target: it doesn't exist,
	// is a stub, isn't in this filesystem, is a version not dated in a
	// filesystem that dates every version (one that may be a write that
	// never finished, and swept), or is a purged item's (its .refs is
	// retired). It wraps the reason.
	ErrRefTarget = errors.New("not a stub's target")
	// ErrRefChecksum is the refusal of a stub whose caller's SHA-256 isn't
	// the one recorded for its target.
	ErrRefChecksum = errors.New("the target's recorded SHA-256 differs")

	errRefElsewhere = errors.New("not an item of this filesystem")
	errRefUndated   = errors.New("not dated: a write that may not have finished")
	errRefRetired   = errors.New("its item is purged: what is left of it is only for the stubs that already name it")
)

// stubTarget is a stub's target as checked before anything is written.
type stubTarget struct {
	key, version string
	obj          *objstore.Object
	sums         Checksums // the target's, as its .metadata records them
	recorded     bool      // sums are recorded
	sha256       []byte    // what the record says: the recorded hash, else the caller's
}

// ref is the stub's Ref: what its metadata and .metadata's Refs say.
func (t *stubTarget) ref() Ref {
	return Ref{Key: t.key, VersionID: t.version, Size: t.obj.Size}
}

// stub is the stub's body (its RefRecord) and user metadata.
func (t *stubTarget) stub() ([]byte, map[string]string, error) {
	body, err := proto.Marshal(&pb.RefRecord{Layout: refLayout, Key: t.key, VersionId: t.version, Size: t.obj.Size, Sha256: t.sha256})
	if err != nil {
		return nil, nil, err
	}
	return body, map[string]string{
		MetaRef: "1", MetaRefKey: t.key, MetaRefVersion: t.version, MetaRefSize: strconv.FormatInt(t.obj.Size, 10),
	}, nil
}

// record notes, in the .metadata of the stub's file, the stub's Ref and
// (if recorded) the target's checksums, under the stub's ETag.
func (t *stubTarget) record(a *Attr, etag string) {
	if t.recorded {
		if a.Checksums == nil {
			a.Checksums = map[string]Checksums{}
		}
		a.Checksums[etag] = t.sums
	}
	if a.Refs == nil {
		a.Refs = map[string]Ref{}
	}
	a.Refs[etag] = t.ref()
}

// checkRefTarget checks a stub's target before anything is written: its
// version exists, isn't a stub, is an item of this filesystem, and is
// dated if the filesystem dates every version (Filesystem.dated); and the
// caller's SHA-256, if any, is the one its .metadata records, if any.
func (f *Filesystem) checkRefTarget(ctx context.Context, targetKey, targetVersion string, sha []byte) (*stubTarget, error) {
	refuse := func(reason error) error {
		return fmt.Errorf("%s version %s: %w: %w", targetKey, targetVersion, ErrRefTarget, reason)
	}
	if !strings.HasPrefix(targetKey, f.prefix()) || objstore.BaseKey(targetKey) != targetKey || targetVersion == "" {
		return nil, refuse(errRefElsewhere)
	}
	obj, err := f.store.HeadVersion(ctx, targetKey, targetVersion)
	if objstore.IsNotFound(err) {
		return nil, refuse(ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if obj.Deleted {
		return nil, refuse(ErrNotFound)
	}
	if IsRef(obj) {
		return nil, refuse(ErrRefToRef)
	}
	if f.dated(targetKey) {
		// An item with no table yet has every version at the store's
		// time: its first record will list them all.
		dates, err := objstore.ReadDates(undeclared(ctx), f.store, targetKey)
		if err != nil {
			return nil, err
		}
		if _, ok := dates[targetVersion]; len(dates) > 0 && !ok {
			return nil, refuse(errRefUndated)
		}
	}
	t := &stubTarget{key: targetKey, version: targetVersion, obj: obj, sha256: sha}
	body, _, err := f.store.Get(objstore.CacheContent(ctx), metadataObjectKey(targetKey))
	switch {
	case objstore.IsNotFound(err):
		// A trashed target: nothing recorded to compare.
	case err != nil:
		return nil, err
	default:
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return nil, err
		}
		attr, err := decodeFileMetadata(data)
		if err != nil {
			return nil, err
		}
		t.sums, t.recorded = attr.Checksums[obj.ETag]
	}
	if t.recorded && len(t.sums.SHA256) > 0 {
		if len(sha) > 0 && !bytes.Equal(sha, t.sums.SHA256) {
			return nil, fmt.Errorf("%s version %s: SHA-256 %x, given %x: %w", targetKey, targetVersion, t.sums.SHA256, sha, ErrRefChecksum)
		}
		t.sha256 = t.sums.SHA256
	}
	return t, nil
}

// raiseFormat raises this filesystem to formatVersionRefs, by CAS on the
// master block, unless it's there already: before its first stub, so a
// build that can't follow one won't open it. A filesystem at it needs
// no master write.
func (f *Filesystem) raiseFormat(ctx context.Context) error {
	if f.format.Load() >= formatVersionRefs {
		return nil
	}
	err := updateFilesystemEntryIf(undeclared(ctx), f.store, f.fsName, func(e *pb.FilesystemEntry) bool {
		if e.FormatVersion >= formatVersionRefs {
			return false
		}
		e.FormatVersion = formatVersionRefs
		return true
	})
	if err != nil {
		return fmt.Errorf("raise filesystem %q to format %d: %w", f.fsName, formatVersionRefs, err)
	}
	f.format.Store(formatVersionRefs)
	return nil
}

// loadRefs is the item at key's .refs as it is now, and the store's
// description of it: empty, with a nil object, if it has none.
func loadRefs(ctx context.Context, store objstore.Store, key string) (*pb.Refs, *objstore.Object, error) {
	body, obj, err := store.Get(ctx, refsObjectKey(key))
	if objstore.IsNotFound(err) {
		return &pb.Refs{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, nil, err
	}
	var refs pb.Refs
	if err := proto.Unmarshal(data, &refs); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", refsObjectKey(key), err)
	}
	if err := block.CheckLayout("referrers", refs.Layout, refsLayout); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", refsObjectKey(key), err)
	}
	return &refs, obj, nil
}

// updateRefs applies mutate to the .refs of the item at key: a Get, then
// a Put conditional on it (or on there being none), retried on a lost
// race. mutate may decline (false: nothing is written) or refuse (its
// error, returned as is). The writes are bookkeeping, not the item's
// history: never dated, whatever ctx declares.
func (f *Filesystem) updateRefs(ctx context.Context, key string, mutate func(*pb.Refs) (bool, error)) error {
	ctx = undeclared(ctx)
	rk := refsObjectKey(key)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		refs, obj, err := loadRefs(ctx, f.store, key)
		if err != nil {
			return err
		}
		ok, err := mutate(refs)
		if err != nil || !ok {
			return err
		}
		refs.Layout = refsLayout
		data, err := proto.Marshal(refs)
		if err != nil {
			return err
		}
		if obj == nil {
			_, err = f.store.PutIfAbsent(ctx, rk, bytes.NewReader(data), nil)
		} else {
			_, err = f.store.Put(ctx, rk, bytes.NewReader(data), nil, obj.ETag)
		}
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("update %s: exceeded %d retries", rk, maxTreeRetries)
}

// addReferrer adds key's entry, pending, to t's target's .refs: before
// the stub on key is written. A retired .refs (a purge of the target has
// begun) refuses it, ErrRefTarget. A .refs this creates may be one a
// purge erased just before, with the target's versions (a purge erases
// the content key before its .refs), so then the target's version is
// looked for again: gone, the entry is taken back, and that too is
// ErrRefTarget.
func (f *Filesystem) addReferrer(ctx context.Context, t *stubTarget, key string) error {
	fresh := false
	err := f.updateRefs(ctx, t.key, func(refs *pb.Refs) (bool, error) {
		if refs.Retired {
			return false, fmt.Errorf("%s version %s: %w: %w", t.key, t.version, ErrRefTarget, errRefRetired)
		}
		fresh = len(refs.Referrers) == 0
		refs.Referrers = append(refs.Referrers, &pb.Referrer{ReferrerKey: key, Pending: true, TargetVersion: t.version})
		return true, nil
	})
	if err != nil || !fresh {
		return err
	}
	obj, err := f.store.HeadVersion(undeclared(ctx), t.key, t.version)
	if err == nil && !obj.Deleted {
		return nil
	}
	if err != nil && !objstore.IsNotFound(err) {
		return err
	}
	mine := &pb.Referrer{ReferrerKey: key, Pending: true, TargetVersion: t.version}
	if derr := f.dropRef(ctx, t.key, func(r *pb.Referrer) bool { return sameReferrer(r, mine) }); derr != nil {
		return derr
	}
	// Nothing can name an item whose version is gone: a .refs left with
	// no entries is this write's own, and goes.
	if refs, _, lerr := loadRefs(undeclared(ctx), f.store, t.key); lerr == nil && len(refs.Referrers) == 0 {
		_ = f.store.EraseAll(undeclared(ctx), refsObjectKey(t.key))
	}
	return fmt.Errorf("%s version %s: %w: %w", t.key, t.version, ErrRefTarget, ErrNotFound)
}

// finalizeReferrer marks key's pending entry in t's target's .refs as
// the stub version versionID, best effort: a pending entry already holds
// a purge back, and fsck settles it. An entry gone meanwhile is added
// back, finalized.
func (f *Filesystem) finalizeReferrer(ctx context.Context, t *stubTarget, key, versionID string) {
	_ = f.updateRefs(ctx, t.key, func(refs *pb.Refs) (bool, error) {
		for _, r := range refs.Referrers {
			if r.ReferrerKey == key && r.Pending && r.ReferrerVersion == "" && r.TargetVersion == t.version {
				r.ReferrerVersion, r.Pending = versionID, false
				return true, nil
			}
		}
		refs.Referrers = append(refs.Referrers, &pb.Referrer{ReferrerKey: key, ReferrerVersion: versionID, TargetVersion: t.version})
		return true, nil
	})
}

// PutRef writes a stub as key's new content version: a version holding
// no bytes of its own, whose bytes are targetKey's version targetVersion
// (see FollowRef). sha256, if given, must be the target's SHA-256 as its
// .metadata records it, if it does (ErrRefChecksum); a target that isn't
// one is ErrRefTarget. ifMatch makes the write conditional, as
// WriteFile's does. The filesystem's first stub raises its format (see
// formatVersionRefs). Under a declared time, the stub and its .metadata
// write are recorded and dated as any write is (RecordDates). It returns
// the stub's version.
func (f *Filesystem) PutRef(ctx context.Context, key, targetKey, targetVersion string, sha256 []byte, ifMatch string) (*objstore.Object, error) {
	if err := f.checkWritable(); err != nil {
		return nil, err
	}
	t, err := f.checkRefTarget(ctx, targetKey, targetVersion, sha256)
	if err != nil {
		return nil, err
	}
	if testHookRefChecked != nil {
		testHookRefChecked()
	}
	body, meta, err := t.stub()
	if err != nil {
		return nil, err
	}
	if err := f.raiseFormat(ctx); err != nil {
		return nil, err
	}
	if err := f.addReferrer(ctx, t, key); err != nil {
		return nil, err
	}
	stub, err := f.store.Put(ctx, key, bytes.NewReader(body), meta, ifMatch)
	if err != nil {
		return nil, err // the pending entry stays, for fsck
	}
	if _, _, err := f.writeMetadataRead(ctx, key, false, func(a *Attr) bool { t.record(a, stub.ETag); return true }); err != nil {
		return nil, fmt.Errorf("stub %s version %s written; its .metadata: %w", key, stub.VersionID, err)
	}
	f.finalizeReferrer(ctx, t, key, stub.VersionID)
	return stub, nil
}

// CreateWithRef is CreateWith whose content is a stub naming targetKey's
// version targetVersion, as PutRef writes one: the new file's first
// content version is the stub. The attributes returned are what a Stat
// would say: the target's size.
func (f *Filesystem) CreateWithRef(ctx context.Context, dirKey, name string, mode, uid, gid uint32, sddl []byte, info ItemInfo, targetKey, targetVersion string, sha256 []byte) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	if err := f.checkName(name); err != nil {
		return "", Attr{}, err
	}
	t, err := f.checkRefTarget(ctx, targetKey, targetVersion, sha256)
	if err != nil {
		return "", Attr{}, err
	}
	body, meta, err := t.stub()
	if err != nil {
		return "", Attr{}, err
	}
	if err := f.raiseFormat(ctx); err != nil {
		return "", Attr{}, err
	}
	key, attr, err := f.createWith(ctx, dirKey, name, mode, uid, gid, sddl, info,
		func(ctx context.Context, key string) (*objstore.Object, error) {
			if err := f.addReferrer(ctx, t, key); err != nil {
				return nil, err
			}
			return f.store.Put(ctx, key, bytes.NewReader(body), meta, "")
		},
		func(a *Attr, content *objstore.Object) { t.record(a, content.ETag) })
	if err != nil {
		return "", Attr{}, err
	}
	if cv, ok := attr.ContentVersion(); ok {
		f.finalizeReferrer(ctx, t, key, cv.VersionID)
	}
	return key, attr, nil
}

// testHookRefChecked, if set, runs in PutRef between the check of the
// target and its .refs entry: for the tests, to race a purge there.
var testHookRefChecked func()

// Referrers, live and dead. A purge of a target retires it (see Purge):
// its .refs is marked retired before anything is erased, and what it
// keeps, as a retired holder, is only the versions its live referrers
// name, behind a delete marker on its key, with the .refs beside them.
// The holder goes with its last referrer (dropRef).

// sameReferrer reports whether a and b are the same .refs entry.
func sameReferrer(a, b *pb.Referrer) bool {
	return a.ReferrerKey == b.ReferrerKey && a.ReferrerVersion == b.ReferrerVersion &&
		a.Pending == b.Pending && a.TargetVersion == b.TargetVersion
}

// referrerLive reports whether finalized entry e of target's .refs names
// a live stub: a HeadVersion of its version finds a stub, and it names
// target's version e.TargetVersion. A pending entry isn't judged here.
func (f *Filesystem) referrerLive(ctx context.Context, target string, e *pb.Referrer) (bool, error) {
	if e.ReferrerVersion == "" {
		return false, nil
	}
	obj, err := f.store.HeadVersion(ctx, e.ReferrerKey, e.ReferrerVersion)
	if objstore.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if obj.Deleted {
		return false, nil
	}
	r, ok, err := RefOf(obj)
	if !ok || err != nil {
		return false, nil
	}
	return r.Key == target && r.VersionID == e.TargetVersion, nil
}

// liveReferrers is key's .refs's live entries, dropping (dropRef) the
// dead ones found on the way. A pending entry is live: it may be a stub
// being written now, which a purge can't tell from one that never will
// be (fsck settles those). An entry whose referrer is in gone (a purge's
// own items) is dead.
func (f *Filesystem) liveReferrers(ctx context.Context, key string, gone map[string]bool) ([]*pb.Referrer, error) {
	ctx = undeclared(ctx)
	refs, _, err := loadRefs(ctx, f.store, key)
	if err != nil {
		return nil, err
	}
	var live, dead []*pb.Referrer
	for _, e := range refs.Referrers {
		ok := false
		switch {
		case gone[e.ReferrerKey]:
		case e.Pending:
			ok = true
		default:
			if ok, err = f.referrerLive(ctx, key, e); err != nil {
				return nil, err
			}
		}
		if ok {
			live = append(live, e)
		} else {
			dead = append(dead, e)
		}
	}
	if len(dead) > 0 {
		err := f.dropRef(ctx, key, func(r *pb.Referrer) bool {
			for _, d := range dead {
				if sameReferrer(r, d) {
					return true
				}
			}
			return false
		})
		if err != nil {
			return nil, err
		}
	}
	return live, nil
}

// markRetired marks key's .refs retired, creating it if need be: from
// then on no new stub names key (addReferrer).
func (f *Filesystem) markRetired(ctx context.Context, key string) error {
	return f.updateRefs(ctx, key, func(refs *pb.Refs) (bool, error) {
		if refs.Retired {
			return false, nil
		}
		refs.Retired = true
		return true, nil
	})
}

// dropRef removes from target's .refs every entry drop picks: the one way
// an entry leaves (a referrer's purge, a history rewrite dropping or
// replacing a stub, a stub re-pointed, fsck's repair). A target already
// gone is fine. When target is a retired holder (its .refs retired, no
// .metadata) and no entry left in it is live, the holder goes: one
// EraseAll of its prefix, the key's versions and its .refs.
func (f *Filesystem) dropRef(ctx context.Context, target string, drop func(*pb.Referrer) bool) error {
	ctx = undeclared(ctx)
	retired := false
	err := f.updateRefs(ctx, target, func(refs *pb.Refs) (bool, error) {
		retired = refs.Retired
		kept := refs.Referrers[:0:0]
		for _, r := range refs.Referrers {
			if !drop(r) {
				kept = append(kept, r)
			}
		}
		if len(kept) == len(refs.Referrers) {
			return false, nil
		}
		refs.Referrers = kept
		return true, nil
	})
	if err != nil || !retired {
		return err
	}
	return f.settleHolder(ctx, target)
}

// settleHolder erases target if it is a retired holder none of whose
// entries is live (a pending one counts as live: it may be a copy of a
// stub being written). Dead entries go with it.
func (f *Filesystem) settleHolder(ctx context.Context, target string) error {
	ctx = undeclared(ctx)
	refs, _, err := loadRefs(ctx, f.store, target)
	if err != nil || !refs.Retired {
		return err
	}
	if _, err := f.store.Head(ctx, metadataObjectKey(target)); err == nil {
		return nil // not a holder yet: a purge of it is under way, and decides
	} else if !objstore.IsNotFound(err) {
		return err
	}
	for _, e := range refs.Referrers {
		if e.Pending {
			return nil
		}
		live, err := f.referrerLive(ctx, target, e)
		if err != nil || live {
			return err
		}
	}
	// A retired .refs takes no new entry, so none can come between the
	// look above and the erase. The prefix's listing puts the key's own
	// versions before its .refs.
	return f.store.EraseAll(ctx, target)
}

// copyReferrer prepares a copy of stub version oldVersion of key, naming
// r, as key's new version: r's .refs gains a pending entry for it. A
// retired holder takes one only from a referrer that already holds it
// (an entry for oldVersion).
func (f *Filesystem) copyReferrer(ctx context.Context, r Ref, key, oldVersion string) error {
	return f.updateRefs(ctx, r.Key, func(refs *pb.Refs) (bool, error) {
		if refs.Retired {
			held := false
			for _, e := range refs.Referrers {
				held = held || e.ReferrerKey == key && e.ReferrerVersion == oldVersion && e.TargetVersion == r.VersionID
			}
			if !held {
				return false, fmt.Errorf("%s version %s: %w: %w", r.Key, r.VersionID, ErrRefTarget, errRefRetired)
			}
		}
		refs.Referrers = append(refs.Referrers, &pb.Referrer{ReferrerKey: key, Pending: true, TargetVersion: r.VersionID})
		return true, nil
	})
}

// copiedReferrer, once the copy is written as newVersion, finalizes its
// entry and drops the one for oldVersion (dropRef).
func (f *Filesystem) copiedReferrer(ctx context.Context, r Ref, key, oldVersion, newVersion string) error {
	f.finalizeReferrer(ctx, &stubTarget{key: r.Key, version: r.VersionID}, key, newVersion)
	return f.dropRef(ctx, r.Key, func(e *pb.Referrer) bool {
		return e.ReferrerKey == key && e.ReferrerVersion == oldVersion && !e.Pending
	})
}
