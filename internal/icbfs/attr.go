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

	"github.com/resurgentech/icbfs/internal/block"
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

// Attr is the POSIX-ish attribute set for one inode, assembled from the
// blob's own object metadata (mode/uid/gid/nlink) plus properties the
// object store gives us for free (size, mtime). See ARCHITECTURE.md's
// metadata model for why mode/uid/gid/nlink live in object metadata rather
// than the directory row.
type Attr struct {
	Mode  uint32 // permission bits only (e.g. 0644) — type bits are not stored here
	Uid   uint32
	Gid   uint32
	Nlink uint32
	Size  int64
	Mtime time.Time // derived from the current version's LastModified
	Btime time.Time // derived from the UUIDv7 key; zero if the key isn't a UUIDv7
}

const (
	metaMode  = "mode"
	metaUid   = "uid"
	metaGid   = "gid"
	metaNlink = "nlink"
)

// metadataFromAttr builds the object-metadata map Attr's mutable fields are
// stored as.
func metadataFromAttr(a Attr) map[string]string {
	return map[string]string{
		metaMode:  strconv.FormatUint(uint64(a.Mode), 10),
		metaUid:   strconv.FormatUint(uint64(a.Uid), 10),
		metaGid:   strconv.FormatUint(uint64(a.Gid), 10),
		metaNlink: strconv.FormatUint(uint64(a.Nlink), 10),
	}
}

func parseMetaUint(m map[string]string, key string) uint32 {
	v, _ := strconv.ParseUint(m[key], 10, 32)
	return uint32(v)
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

// Btime extracts the embedded creation timestamp from a UUIDv7 string.
// Returns the zero Time if key is not a valid UUIDv7 (e.g. a root block's
// naming-convention key).
func Btime(key string) time.Time {
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
