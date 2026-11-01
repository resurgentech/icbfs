package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

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
