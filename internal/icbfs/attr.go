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
	ErrNotFound        = errors.New("not found")
	ErrExists          = errors.New("already exists")
	ErrNotEmpty        = errors.New("directory not empty")
	ErrNotDir          = errors.New("not a directory")
	ErrIsDir           = errors.New("is a directory")
	ErrInvalidName     = errors.New("name reserved on a primary-Windows filesystem")
	ErrInvalidRename   = errors.New("new path is inside the directory being renamed")
	ErrInvalidArgument = errors.New("invalid argument")
)

// Windows FILE_ATTRIBUTE_* bits this project stores with native
// fidelity for a file/symlink's Hidden/System/ReadOnly/Archive state
// (ROADMAP.md's Part F, task F7; ARCHITECTURE.md's "File attribute
// bits" rule). These duplicate golang.org/x/sys/windows's own
// documented Win32 values (not imported from there) so this package
// stays portable with no Windows dependency — same reasoning
// winnames.go's own reserved-character constants give. Numerically
// identical to FILE_ATTRIBUTE_READONLY/HIDDEN/SYSTEM/ARCHIVE, so
// winfspserver can widen one of these straight into a
// FSP_FSCTL_FILE_INFO.FileAttributes value with no translation table.
const (
	WindowsAttrReadOnly uint32 = 0x01
	WindowsAttrHidden   uint32 = 0x02
	WindowsAttrSystem   uint32 = 0x04
	WindowsAttrArchive  uint32 = 0x20

	windowsAttrMask = WindowsAttrReadOnly | WindowsAttrHidden | WindowsAttrSystem | WindowsAttrArchive
)

// WindowsAttributesXattrKey is the well-known xattrs map key a file/
// symlink's native Windows attribute byte is stored under — "another
// entry in .metadata's xattrs map," the same place task F8 plans to
// land Windows ACLs (under "windows.acl") and POSIX already keeps its
// own ACL entry (under "system.posix_acl_access"): no special-casing
// at the storage layer, just another named entry in the union map.
const WindowsAttributesXattrKey = "windows.attributes"

// WindowsAttributesFromXattrs reads a file/symlink's natively-stored
// Windows attribute bits, if any were ever written via
// SetWindowsAttributes. ok is false if nothing has been stored yet —
// callers decide what to derive instead (e.g. winfspserver falls back
// to approximating ReadOnly from the POSIX mode bit, the same
// approximation used before this field existed at all).
func WindowsAttributesFromXattrs(xattrs map[string][]byte) (bits uint32, ok bool) {
	v, present := xattrs[WindowsAttributesXattrKey]
	if !present || len(v) == 0 {
		return 0, false
	}
	return uint32(v[0]) & windowsAttrMask, true
}

// WindowsACLXattrKey is the well-known xattrs map key a file/symlink's
// native Windows security descriptor is stored under (ROADMAP.md's
// Part F, task F8) — metadata.proto's own doc comment already reserved
// this name. The value is a security descriptor's SDDL string
// representation (ASCII, e.g. "O:...G:...D:..."), not the raw
// self-relative binary layout: SDDL round-trips through
// golang.org/x/sys/windows's own SecurityDescriptorFromString/
// (*SECURITY_DESCRIPTOR).String() with no manual struct-layout work on
// either side, and stores as ordinary bytes in this already-binary-safe
// map with no further encoding needed, same as every other xattr here.
const WindowsACLXattrKey = "windows.acl"

// WindowsACLFromXattrs reads a file/symlink's natively-stored security
// descriptor's SDDL string, if SetWindowsACL has ever been called for
// it. ok is false if nothing has been stored yet — callers (winfspserver)
// decide what to fall back to (the mounting process's own identity, per
// ROADMAP.md's F8 entry).
func WindowsACLFromXattrs(xattrs map[string][]byte) (sddl []byte, ok bool) {
	v, present := xattrs[WindowsACLXattrKey]
	if !present || len(v) == 0 {
		return nil, false
	}
	return v, true
}

// WindowsDirSymlinkXattrKey marks a symlink created as a Windows
// *directory* symlink (CreateSymbolicLinkW's SYMBOLIC_LINK_FLAG_DIRECTORY):
// Windows reports and opens those as directories (RemoveDirectory, not
// DeleteFile, removes one), whereas POSIX has no such distinction.
const WindowsDirSymlinkXattrKey = "windows.symlinkd"

// GenericReparseXattrKey is the well-known xattrs map key a file's
// *non-symlink* reparse point data is stored under (ROADMAP.md's Part
// G, task G4) — `winfsp-tests`' own `reparse_guid_test`/
// `reparse_nfs_test` set an arbitrary custom/NFS-style reparse tag on
// an ordinary file via `FSCTL_SET_REPARSE_POINT`, carrying an opaque
// data buffer this driver has no reason to parse — unlike the
// symlink case (a first-class icbfs entry type, translated to/from
// Windows' own `REPARSE_DATA_BUFFER_SYMBOLIC_LINK` layout), a generic
// reparse point's raw buffer is stored and returned byte-exact, same
// "just another named entry, no special-casing at the storage layer"
// pattern `windows.acl`/`windows.attributes` already established.
const GenericReparseXattrKey = "windows.reparse"

// GenericReparseFromXattrs reads a file's raw, natively-stored
// non-symlink reparse buffer, if SetGenericReparseData has ever been
// called for it. ok is false if nothing has been stored (including a
// plain file that's never had any reparse point set at all).
func GenericReparseFromXattrs(xattrs map[string][]byte) (buf []byte, ok bool) {
	v, present := xattrs[GenericReparseXattrKey]
	if !present || len(v) == 0 {
		return nil, false
	}
	return v, true
}

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
	Atime  time.Time
	Mtime  time.Time
	Ctime  time.Time
	Btime  time.Time         // derived from the UUIDv7 key; zero if the key isn't a UUIDv7
	Xattrs map[string][]byte // directories: only WindowsAttributesXattrKey, if ever set
	Rdev   uint32            // TypeCharDevice/TypeBlockDevice only (ROADMAP.md's Part G, task G2); 0 for every other type

	// mtimeOverrideETag is the content ETag an explicit mtime override
	// (Mtime, as decoded from .metadata) was set against — see
	// combineFileAttr. Internal plumbing only, never meaningful to callers.
	mtimeOverrideETag string
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
	dirMetaAtime = "atime"
	// dirMetaWinAttr holds a directory's Windows attribute bits (e.g.
	// ReadOnly, Hidden), surfaced through Attr.Xattrs under
	// WindowsAttributesXattrKey — the same place a file's live — so
	// access layers read them the same way for both.
	dirMetaWinAttr = "winattr"
)

// dirMetadataFromAttr builds the object-metadata map a directory block
// object's mode/uid/gid/nlink/mtime/atime are stored as.
func dirMetadataFromAttr(a Attr) map[string]string {
	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = time.Now()
	}
	m := map[string]string{
		dirMetaMode:  strconv.FormatUint(uint64(a.Mode), 10),
		dirMetaUid:   strconv.FormatUint(uint64(a.Uid), 10),
		dirMetaGid:   strconv.FormatUint(uint64(a.Gid), 10),
		dirMetaNlink: strconv.FormatUint(uint64(a.Nlink), 10),
		dirMetaMtime: strconv.FormatInt(mtime.UnixNano(), 10),
	}
	// atime is only written when the caller actually has one to set
	// (Stat's read-bumps-atime path, or an explicit utimensat) — unlike
	// mtime, a brand-new directory has no meaningful "last accessed"
	// yet, and omitting the key here lets parseMetaTime's zero-value
	// default (never accessed) stay honest rather than defaulting to
	// "now" the way mtime does above.
	if !a.Atime.IsZero() {
		m[dirMetaAtime] = strconv.FormatInt(a.Atime.UnixNano(), 10)
	}
	if bits, ok := WindowsAttributesFromXattrs(a.Xattrs); ok {
		m[dirMetaWinAttr] = strconv.FormatUint(uint64(bits), 10)
	}
	return m
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
// Protobuf-encoded (spec/proto/icbfs/v1/metadata.proto). ---

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
	var atimeNano int64
	if !a.Atime.IsZero() {
		// Go's zero time.Time isn't the Unix epoch — its own
		// UnixNano() is a large negative number, not 0, so a never-
		// yet-accessed file must stay at proto3's real 0 default
		// rather than encoding that nonsense value.
		atimeNano = a.Atime.UnixNano()
	}
	var mtimeOverrideNano int64
	if !a.Mtime.IsZero() {
		// Same zero-value care as atime above. a.Mtime here is only
		// ever the *override* (see combineFileAttr/decodeFileMetadata):
		// a real content write's mtime always comes for free from the
		// content object's own LastModified and is never passed through
		// this struct at all, so there's nothing to accidentally encode
		// here on an ordinary write.
		mtimeOverrideNano = a.Mtime.UnixNano()
	}
	var btimeOverrideNano int64
	if !a.Btime.IsZero() {
		// Same as mtime: a.Btime here is only ever the override.
		btimeOverrideNano = a.Btime.UnixNano()
	}
	return proto.Marshal(&pb.Metadata{
		Mode:                  a.Mode,
		Uid:                   a.Uid,
		Gid:                   a.Gid,
		Nlink:                 a.Nlink,
		Xattrs:                a.Xattrs,
		AtimeUnixNano:         atimeNano,
		Rdev:                  a.Rdev,
		MtimeOverrideUnixNano: mtimeOverrideNano,
		MtimeOverrideEtag:     a.mtimeOverrideETag,
		BtimeOverrideUnixNano: btimeOverrideNano,
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
	var atime time.Time
	if m.AtimeUnixNano != 0 {
		atime = time.Unix(0, m.AtimeUnixNano).UTC()
	}
	var mtimeOverride, btimeOverride time.Time
	if m.MtimeOverrideUnixNano != 0 {
		mtimeOverride = time.Unix(0, m.MtimeOverrideUnixNano).UTC()
	}
	if m.BtimeOverrideUnixNano != 0 {
		btimeOverride = time.Unix(0, m.BtimeOverrideUnixNano).UTC()
	}
	return Attr{
		Mode:   m.Mode,
		Uid:    m.Uid,
		Gid:    m.Gid,
		Nlink:  m.Nlink,
		Xattrs: m.Xattrs,
		Atime:  atime,
		// Mtime here carries only the explicit override, if any — see
		// combineFileAttr, which is what this decode's caller (statFile)
		// always goes through for the real, content-derived Mtime it
		// falls back to when this is zero.
		Mtime:             mtimeOverride,
		mtimeOverrideETag: m.MtimeOverrideEtag,
		Btime:             btimeOverride, // same: override only; see combineFileAttr
		Rdev:              m.Rdev,
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
	TypeFile        = block.TypeFile
	TypeDir         = block.TypeDir
	TypeSymlink     = block.TypeSymlink
	TypeFifo        = block.TypeFifo        // ROADMAP.md's Part G, task G2 — POSIX/FUSE-only, no Windows/NTFS equivalent
	TypeCharDevice  = block.TypeCharDevice  // same
	TypeBlockDevice = block.TypeBlockDevice // same
	TypeSocket      = block.TypeSocket      // same
)
