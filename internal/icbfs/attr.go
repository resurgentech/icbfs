// Package icbfs is the filesystem core: the object store and directory
// blocks made into filesystem operations (lookup, mkdir, create, read,
// write, link, rename, ...), shared by every access layer.
package icbfs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	ErrNoSpace         = errors.New("no space left on this filesystem")
	ErrInvalidRename   = errors.New("new path is inside the directory being renamed")
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNewerFormat is an object, or the filesystem as a whole, in a
	// format newer than this build knows (see FormatVersion).
	ErrNewerFormat = block.ErrNewerFormat
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

// WindowsACL returns the item's Windows security descriptor, as SDDL
// ("O:...G:...D:..."), which golang.org/x/sys/windows parses and prints;
// ok is false if it has none, and the caller (winfspserver) falls back to
// a default.
func (a Attr) WindowsACL() (sddl []byte, ok bool) {
	return a.WindowsSDDL, len(a.WindowsSDDL) > 0
}

// WindowsAttributes returns the item's stored Windows attribute bits; ok
// is false if none were ever stored, and the caller derives them instead
// (winfspserver takes ReadOnly from the mode).
func (a Attr) WindowsAttributes() (bits uint32, ok bool) {
	return a.WindowsAttrs & windowsAttrMask, a.HasWindowsAttrs
}

// GenericReparse returns a file's non-symlink reparse point buffer, as
// given (winfsp-tests' reparse_guid_test and reparse_nfs_test set such
// tags with FSCTL_SET_REPARSE_POINT); ok is false if it has none. A
// symlink is an icbfs entry type, not this.
func (a Attr) GenericReparse() (buf []byte, ok bool) {
	return a.Reparse, len(a.Reparse) > 0
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
	Mode  uint32 // permission bits only (e.g. 0644) — type bits are not stored here
	Uid   uint32
	Gid   uint32
	Nlink uint32
	Size  int64
	Atime time.Time
	Mtime time.Time
	Ctime time.Time
	Btime time.Time // derived from the UUIDv7 key; zero if the key isn't a UUIDv7

	// Xattrs are the item's extended attributes: only what users set,
	// which is all a mount's getxattr and setxattr see. icbfs's own state
	// is in the fields below.
	Xattrs map[string][]byte

	// The item's Windows state: its security descriptor (SDDL), its
	// ReadOnly, Hidden, System and Archive bits (when HasWindowsAttrs), a
	// non-symlink reparse point's buffer, and whether a symlink was made
	// as a Windows directory symlink, which Windows treats as a directory.
	WindowsSDDL       []byte
	WindowsAttrs      uint32
	HasWindowsAttrs   bool
	Reparse           []byte
	WindowsDirSymlink bool

	// PosixACLAccess and PosixACLDefault are the item's POSIX ACLs, in
	// Linux's xattr encoding; nil for none.
	PosixACLAccess  []byte
	PosixACLDefault []byte

	// App is data icbfs's own tools keep about the item, keyed
	// "<tool>.<name>" (AppSyncHash and the like). No mount shows it.
	App map[string][]byte

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
	var win *pb.Windows
	if len(a.WindowsSDDL) > 0 || a.HasWindowsAttrs || len(a.Reparse) > 0 || a.WindowsDirSymlink {
		win = &pb.Windows{Sddl: string(a.WindowsSDDL), Reparse: a.Reparse, DirSymlink: a.WindowsDirSymlink}
		if a.HasWindowsAttrs {
			bits := a.WindowsAttrs & windowsAttrMask
			win.Attributes = &bits
		}
	}
	return proto.Marshal(&pb.Metadata{
		Layout:            metadataLayout,
		Mode:              a.Mode,
		Uid:               a.Uid,
		Gid:               a.Gid,
		Nlink:             a.Nlink,
		Xattrs:            a.Xattrs,
		Windows:           win,
		PosixAclAccess:    a.PosixACLAccess,
		PosixAclDefault:   a.PosixACLDefault,
		App:               a.App,
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
	if m.Layout > metadataLayout {
		return Attr{}, fmt.Errorf("metadata layout %d: %w", m.Layout, ErrNewerFormat)
	}
	atime := timeOrZero(m.Atime)
	if atime.IsZero() && m.AtimeUnixNano != 0 {
		atime = time.Unix(0, m.AtimeUnixNano).UTC() // legacy field
	}
	mtimeOverride := timeOrZero(m.MtimeOverride)
	btimeOverride := timeOrZero(m.BtimeOverride)
	a := Attr{
		Mode:            m.Mode,
		Uid:             m.Uid,
		Gid:             m.Gid,
		Nlink:           m.Nlink,
		Xattrs:          m.Xattrs,
		PosixACLAccess:  m.PosixAclAccess,
		PosixACLDefault: m.PosixAclDefault,
		App:             m.App,
		Atime:           atime,
		// Only the explicit override, if any; combineFileAttr falls back to
		// the content's time.
		Mtime:             mtimeOverride,
		mtimeOverrideETag: m.MtimeOverrideEtag,
		Streams:           m.Streams,
		nextStreamID:      m.NextStreamId,
		Btime:             btimeOverride,       // same: override only; see combineFileAttr
		Ctime:             timeOrZero(m.Ctime), // the stored status-change time only; see combineFileAttr
		Rdev:              m.Rdev,
	}
	if w := m.Windows; w != nil {
		a.WindowsSDDL = []byte(w.Sddl)
		a.Reparse = w.Reparse
		a.WindowsDirSymlink = w.DirSymlink
		if w.Attributes != nil {
			a.WindowsAttrs, a.HasWindowsAttrs = *w.Attributes&windowsAttrMask, true
		}
	}
	if m.Layout == 0 {
		fromLegacyXattrs(&a)
	}
	return a, nil
}

// metadataLayout is the .metadata layout this package writes: users'
// xattrs apart from icbfs's own state.
const metadataLayout = 1

// legacyAppXattrs are the xattr names icbfs's tools used in layout-0
// records, and the App keys they are now.
var legacyAppXattrs = map[string]string{
	"user.sync.sha256":       AppSyncHash,
	"user.drive.contentType": AppDriveContentType,
	"user.drive.origin":      AppDriveOrigin,
}

// fromLegacyXattrs moves a layout-0 record's internal entries out of
// Xattrs into their own fields, leaving only users' xattrs.
func fromLegacyXattrs(a *Attr) {
	take := func(name string) ([]byte, bool) {
		v, ok := a.Xattrs[name]
		if ok {
			delete(a.Xattrs, name)
		}
		return v, ok
	}
	if v, ok := take("windows.acl"); ok && len(v) > 0 {
		a.WindowsSDDL = v
	}
	if v, ok := take("windows.attributes"); ok && len(v) > 0 {
		a.WindowsAttrs, a.HasWindowsAttrs = uint32(v[0])&windowsAttrMask, true
	}
	if v, ok := take("windows.reparse"); ok && len(v) > 0 {
		a.Reparse = v
	}
	if _, ok := take("windows.symlinkd"); ok {
		a.WindowsDirSymlink = true
	}
	if v, ok := take("system.posix_acl_access"); ok {
		a.PosixACLAccess = v
	}
	if v, ok := take("system.posix_acl_default"); ok {
		a.PosixACLDefault = v
	}
	for name := range a.Xattrs {
		key, ok := legacyAppXattrs[name]
		if !ok && strings.HasPrefix(name, "user.side.") {
			key, ok = AppSidePrefix+strings.TrimPrefix(name, "user.side."), true
		}
		if ok {
			if a.App == nil {
				a.App = map[string][]byte{}
			}
			a.App[key] = a.Xattrs[name]
			delete(a.Xattrs, name)
		}
	}
	if len(a.Xattrs) == 0 {
		a.Xattrs = nil
	}
}

// App keys of icbfs's own tools.
const (
	AppSyncHash         = "sync.sha256"       // icbfs-sync: the content's SHA-256, hex
	AppDriveContentType = "drive.contentType" // the drive API: the content type a write gave
	AppDriveOrigin      = "drive.origin"      // the drive API: where a copy came from
	AppSidePrefix       = "side."             // the drive API's side store: an object's metadata
)

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
