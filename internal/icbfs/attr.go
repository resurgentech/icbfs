// Package icbfs is the core filesystem logic: it ties the object store and
// the directory block format together into filesystem operations
// (lookup, mkdir, create, read, write, link, ...), independent of any
// particular access layer (FUSE, WinFsp, ...).
package icbfs

import (
	"errors"
	"strconv"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/pb"
)

// Sentinel errors filesystem operations return, so access layers can map
// them to their own error conventions (e.g. syscall.Errno for FUSE).
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrNotEmpty = errors.New("directory not empty")
	ErrNotDir   = errors.New("not a directory")
	ErrIsDir    = errors.New("is a directory")
)

// Attr is the POSIX-ish attribute set for one inode. Where it comes from
// differs by type — see ARCHITECTURE.md's Metadata model and Multiple
// filesystems per bucket sections:
//
//   - For a file or symlink: mode/uid/gid/nlink/xattrs come from the
//     dedicated <uuid>.metadata object's body (Protobuf-encoded, below);
//     size/mtime/btime come from the content object itself; ctime is
//     max(contentObject.LastModified, metadataObject.LastModified).
//   - For a directory (root included): mode/uid/gid live in native
//     object metadata on the directory block object itself (no
//     .metadata object — directories aren't hard-linkable, so the
//     real-CAS-protection reason files/symlinks need one doesn't apply);
//     mtime is tracked explicitly there too (see dirMetadataFromAttr,
//     below), since a directory's own body (its entries) and its
//     attributes share one object, the same conflation problem a
//     .metadata split fixes for files would otherwise reappear.
type Attr struct {
	Mode   uint32 // permission bits only (e.g. 0644) — type bits are not stored here
	Uid    uint32
	Gid    uint32
	Nlink  uint32
	Size   int64
	Mtime  time.Time
	Ctime  time.Time
	Btime  time.Time         // derived from the UUIDv7 key; zero if the key isn't a UUIDv7
	Xattrs map[string][]byte // file/symlink only; nil for directories
}

// --- Directory attributes: native object metadata on the block object
// itself (unchanged by the .metadata migration — see Attr's doc comment
// for why directories stay on this older, simpler mechanism). ---

const (
	dirMetaMode  = "mode"
	dirMetaUid   = "uid"
	dirMetaGid   = "gid"
	dirMetaNlink = "nlink"
	dirMetaMtime = "mtime"
)

// dirMetadataFromAttr builds the object-metadata map a directory block
// object's mode/uid/gid/nlink/mtime are stored as.
func dirMetadataFromAttr(a Attr) map[string]string {
	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = time.Now()
	}
	return map[string]string{
		dirMetaMode:  strconv.FormatUint(uint64(a.Mode), 10),
		dirMetaUid:   strconv.FormatUint(uint64(a.Uid), 10),
		dirMetaGid:   strconv.FormatUint(uint64(a.Gid), 10),
		dirMetaNlink: strconv.FormatUint(uint64(a.Nlink), 10),
		dirMetaMtime: strconv.FormatInt(mtime.UnixNano(), 10),
	}
}

func parseMetaUint(m map[string]string, key string) uint32 {
	v, _ := strconv.ParseUint(m[key], 10, 32)
	return uint32(v)
}

func parseMetaTime(m map[string]string, key string) time.Time {
	v, err := strconv.ParseInt(m[key], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

// --- File/symlink attributes: the dedicated <uuid>.metadata object,
// Protobuf-encoded (proto/icbfs/v1/metadata.proto). ---

// metadataObjectKey is the dedicated side object holding a file or
// symlink's mode/uid/gid/nlink/xattrs.
//
// This exists because of a hard limitation discovered empirically
// against real MinIO, not assumed: ETag on an S3-compatible store is a
// hash of the object's *body*, so a metadata-only update to native
// object metadata never changes the ETag when the body is unchanged —
// which means ETag-based CAS gives zero protection to fields stored that
// way. metadataObjectKey's own body *is* the attribute data, so any real
// change to it is a real content write, and ETag CAS becomes meaningful
// again — for every field here, not just nlink, which is what makes
// consolidating mode/uid/gid/nlink into one object (rather than keeping
// them on native metadata and only nlink in its own side object, the
// first iteration of this fix) also quietly close the chmod/chown race
// gap, as a side effect of the consolidation rather than a separate fix.
func metadataObjectKey(uuid string) string { return uuid + ".metadata" }

// encodeFileMetadata serializes mode/uid/gid/nlink/xattrs to the wire
// format stored at metadataObjectKey.
func encodeFileMetadata(a Attr) ([]byte, error) {
	return proto.Marshal(&pb.Metadata{
		Mode:   a.Mode,
		Uid:    a.Uid,
		Gid:    a.Gid,
		Nlink:  a.Nlink,
		Xattrs: a.Xattrs,
	})
}

// decodeFileMetadata parses metadataObjectKey's body. Empty input
// decodes to all-zero fields (mode/uid/gid/nlink all 0, no xattrs) —
// callers should not encounter this in practice, since the object is
// always written with real content at creation, but it's a safe,
// unsurprising zero value rather than an error if they ever do.
func decodeFileMetadata(data []byte) (Attr, error) {
	if len(data) == 0 {
		return Attr{}, nil
	}
	var m pb.Metadata
	if err := proto.Unmarshal(data, &m); err != nil {
		return Attr{}, err
	}
	return Attr{
		Mode:   m.Mode,
		Uid:    m.Uid,
		Gid:    m.Gid,
		Nlink:  m.Nlink,
		Xattrs: m.Xattrs,
	}, nil
}

// NewUUIDv7 generates a new blob identifier. Using UUIDv7 means every new
// object's creation time (Btime) is embedded in its key for free.
func NewUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Btime extracts the embedded creation timestamp from a key. Every
// object a filesystem owns is prefixed with its 4-hex-digit ID (see
// ARCHITECTURE.md's Multiple filesystems per bucket section), so this
// strips a recognized "<4-hex>-" prefix before trying to parse what
// remains as a UUIDv7. Returns the zero Time if nothing UUIDv7-shaped is
// found — e.g. a root key, which is "<id>-root-<name>", not a UUID at
// all, correctly yields zero here after the prefix strip leaves
// "root-<name>" behind.
func Btime(key string) time.Time {
	if len(key) > 5 && key[4] == '-' {
		if _, err := strconv.ParseUint(key[:4], 16, 16); err == nil {
			key = key[5:]
		}
	}
	id, err := uuid.Parse(key)
	if err != nil || id.Version() != 7 {
		return time.Time{}
	}
	ms := int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 |
		int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
	return time.UnixMilli(ms).UTC()
}

// Ino derives a FUSE/stat inode number from a blob's UUID via a
// high-quality 64-bit hash. This is stateless by design — see
// ARCHITECTURE.md's FUSE inode mapping decision: inode numbers only need
// to be unique and stable within one mount's lifetime, so no shared or
// persisted allocation table is needed, and a deterministic hash also
// gets us stability across remounts for free.
func Ino(key string) uint64 {
	return xxhash.Sum64String(key)
}

// typeOf is a convenience re-export so callers outside this package don't
// need to import internal/block directly just to name an entry type.
type EntryType = block.EntryType

const (
	TypeFile    = block.TypeFile
	TypeDir     = block.TypeDir
	TypeSymlink = block.TypeSymlink
)
