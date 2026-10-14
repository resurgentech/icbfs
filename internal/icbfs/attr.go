// Package icbfs is the filesystem core: the object store and directory
// blocks made into filesystem operations (lookup, mkdir, create, read,
// write, link, rename, ...), shared by every access layer.
package icbfs

import (
	"errors"
	"strconv"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	ErrNameTooLong     = errors.New("name too long")
	ErrFileTooLarge    = errors.New("file too large for this filesystem")
	ErrInvalidRename   = errors.New("new path is inside the directory being renamed")
	ErrInvalidArgument = errors.New("invalid argument")
)

// The Windows FILE_ATTRIBUTE_* bits stored for an item: Win32's values,
// written out so this package needs no Windows dependency, and the same
// numbers, so winfspserver uses them as is.
const (
	WindowsAttrReadOnly uint32 = 0x01
	WindowsAttrHidden   uint32 = 0x02
	WindowsAttrSystem   uint32 = 0x04
	WindowsAttrArchive  uint32 = 0x20

	windowsAttrMask = WindowsAttrReadOnly | WindowsAttrHidden | WindowsAttrSystem | WindowsAttrArchive
)

// WindowsAttributesXattrKey is the xattr key of an item's stored Windows
// attribute bits.
const WindowsAttributesXattrKey = "windows.attributes"

// WindowsAttributesFromXattrs returns an item's stored Windows attribute
// bits; ok is false if none were ever stored, and the caller derives them
// instead (winfspserver takes ReadOnly from the mode).
func WindowsAttributesFromXattrs(xattrs map[string][]byte) (bits uint32, ok bool) {
	v, present := xattrs[WindowsAttributesXattrKey]
	if !present || len(v) == 0 {
		return 0, false
	}
	return uint32(v[0]) & windowsAttrMask, true
}

// WindowsACLXattrKey is the xattr key of an item's Windows security
// descriptor, stored as SDDL ("O:...G:...D:..."), which
// golang.org/x/sys/windows parses and prints, rather than as the binary
// self-relative layout.
const WindowsACLXattrKey = "windows.acl"

// WindowsACLFromXattrs returns an item's stored SDDL; ok is false if none
// was stored, and the caller (winfspserver) falls back to a default.
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

// GenericReparseXattrKey is the xattr key of a file's non-symlink reparse
// point data (winfsp-tests' reparse_guid_test and reparse_nfs_test set such
// tags with FSCTL_SET_REPARSE_POINT). Unlike a symlink, which is an icbfs
// entry type, its buffer is stored and returned exactly as given.
const GenericReparseXattrKey = "windows.reparse"

// GenericReparseFromXattrs returns a file's stored non-symlink reparse
// buffer; ok is false if it has none.
func GenericReparseFromXattrs(xattrs map[string][]byte) (buf []byte, ok bool) {
	v, present := xattrs[GenericReparseXattrKey]
	if !present || len(v) == 0 {
		return nil, false
	}
	return v, true
}

// Attr is one item's attributes (docs/architecture.md's Metadata section):
//
//   - For a file or symlink: mode, owner, link count and xattrs come from
//     its <uuid>.metadata object; size, mtime and btime from the content
//     object; ctime is the later of the content's last write and the
//     ctime stored in .metadata (see combineFileAttr).
//   - For a directory (root included): everything is in its .metadata,
//     mtime included (its block needn't change when its entries do), and
//     ctime moves whenever an entry is added or removed; size comes from
//     its block. A directory from before directories had .metadata keeps
//     them in its block's native metadata (dirAttrFromObject) until its
//     first attribute write migrates them.
type Attr struct {
	Mode   uint32 // permission bits only (e.g. 0644) — type bits are not stored here
	Uid    uint32
	Gid    uint32
	Nlink  uint32
	Size   int64
	Atime  time.Time
	Mtime  time.Time
	Ctime  time.Time
	Btime  time.Time // derived from the UUIDv7 key; zero if the key isn't a UUIDv7
	Xattrs map[string][]byte

	// Streams maps a named data stream's name to its id (see StreamKey)
	// — Windows alternate data streams; nil when there are none.
	Streams map[string]uint64
	Rdev    uint32 // TypeCharDevice/TypeBlockDevice only; 0 for every other type

	// mtimeOverrideETag is the content ETag an explicit mtime override
	// (Mtime, as decoded from .metadata) was set against — see
	// combineFileAttr. Internal plumbing only, never meaningful to callers.
	mtimeOverrideETag string

	// nextStreamID is the last stream id handed out (see Streams).
	nextStreamID uint64
}

// --- Legacy directory attributes: native object metadata on the block
// object, read only for directories created before directories had
// .metadata (see Attr's doc comment). Never written any more. ---

const (
	dirMetaMode    = "mode"
	dirMetaUid     = "uid"
	dirMetaGid     = "gid"
	dirMetaNlink   = "nlink"
	dirMetaMtime   = "mtime"
	dirMetaAtime   = "atime"
	dirMetaWinAttr = "winattr"
)

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

// metadataObjectKey is the key of an item's .metadata object.
//
// An S3 ETag is a hash of the body, so native object metadata changes
// without the ETag changing, and a conditional write can't protect it.
// .metadata's body is the attributes, so every change moves its ETag, and
// concurrent chmod, chown and link serialize through compare-and-swap.
func metadataObjectKey(uuid string) string { return uuid + ".metadata" }

// encodeFileMetadata serializes mode/uid/gid/nlink/xattrs to the wire
// format stored at metadataObjectKey.
func encodeFileMetadata(a Attr) ([]byte, error) {
	// a.Mtime/a.Btime here are only ever overrides (see combineFileAttr):
	// a real content write's mtime comes from the content object and is
	// never passed through this struct. Zero times stay unset.
	return proto.Marshal(&pb.Metadata{
		Mode:              a.Mode,
		Uid:               a.Uid,
		Gid:               a.Gid,
		Nlink:             a.Nlink,
		Xattrs:            a.Xattrs,
		Rdev:              a.Rdev,
		Atime:             timestampOrNil(a.Atime),
		MtimeOverride:     timestampOrNil(a.Mtime),
		MtimeOverrideEtag: a.mtimeOverrideETag,
		BtimeOverride:     timestampOrNil(a.Btime),
		Ctime:             timestampOrNil(a.Ctime),
		Streams:           a.Streams,
		NextStreamId:      a.nextStreamID,
	})
}

func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func timeOrZero(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// decodeFileMetadata parses a .metadata body. An empty body decodes to all
// zeros.
func decodeFileMetadata(data []byte) (Attr, error) {
	if len(data) == 0 {
		return Attr{}, nil
	}
	var m pb.Metadata
	if err := proto.Unmarshal(data, &m); err != nil {
		return Attr{}, err
	}
	atime := timeOrZero(m.Atime)
	if atime.IsZero() && m.AtimeUnixNano != 0 {
		atime = time.Unix(0, m.AtimeUnixNano).UTC() // legacy field
	}
	mtimeOverride := timeOrZero(m.MtimeOverride)
	btimeOverride := timeOrZero(m.BtimeOverride)
	return Attr{
		Mode:   m.Mode,
		Uid:    m.Uid,
		Gid:    m.Gid,
		Nlink:  m.Nlink,
		Xattrs: m.Xattrs,
		Atime:  atime,
		// Only the explicit override, if any; combineFileAttr falls back to
		// the content's time.
		Mtime:             mtimeOverride,
		mtimeOverrideETag: m.MtimeOverrideEtag,
		Streams:           m.Streams,
		nextStreamID:      m.NextStreamId,
		Btime:             btimeOverride,       // same: override only; see combineFileAttr
		Ctime:             timeOrZero(m.Ctime), // the stored status-change time only; see combineFileAttr
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

// Btime is the creation time embedded in an item's key: the UUIDv7 after
// the "<id>-" prefix. It's zero for a key with none, such as a root key
// ("<id>-root-<name>").
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

// Ino is the inode number for a key: a 64-bit xxhash, stateless and stable
// across remounts. The kernel pairs it with the mount's device, so it only
// has to be unique within one mount.
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
	TypeFifo        = block.TypeFifo        // POSIX/FUSE-only, no Windows/NTFS equivalent
	TypeCharDevice  = block.TypeCharDevice  // same
	TypeBlockDevice = block.TypeBlockDevice // same
	TypeSocket      = block.TypeSocket      // same
)
