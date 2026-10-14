// Package syncer keeps a folder and an icbfs drive in step: one-way
// (the folder is the truth; the drive mirrors it) or two-way (changes on
// either side carry to the other; when both changed, the folder wins).
// Nothing is ever lost on the icbfs side: an overwrite is a new version,
// a delete goes to its trash.
//
// It works from snapshots, not events: each pass lists the folder (and,
// when due, the drive), compares both with the state recorded at the
// last sync (the base), plans, and carries out the plan. That makes it
// work on folders that can't report changes — a WebDAV drive, say —
// and lets it catch up after any time away.
//
// The parts: Side (a folder or a drive, listed and changed), Plan (pure:
// snapshots and base in, actions out), Engine (runs passes: lists,
// plans, checks the plan is safe, carries it out, records the new base).
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"time"
)

// Entry is a file or directory as a side lists it.
type Entry struct {
	Path    string // '/'-separated, relative to the side's root, as spelled there
	Dir     bool
	Size    int64
	ModTime time.Time
	// ID is the side's stable id for the item, surviving renames, if it
	// has one (Caps.IDs): an icbfs item id, an NTFS file id.
	ID string
	// Version is the content version, if the side has versions: on
	// icbfs, the version id of the current content.
	Version string
	// ACL is the item's security descriptor as SDDL, if the side keeps
	// real ones (Caps.ACLs).
	ACL string
}

// Caps is what a side can tell and do.
type Caps struct {
	IDs  bool // Entry.ID is stable across renames
	ACLs bool // Entry.ACL is real, and SetACL works
	// TimeResolution is how finely ModTime is kept: a WebDAV drive keeps
	// whole seconds, NTFS 100ns.
	TimeResolution time.Duration
}

// Side is one end of a pair.
type Side interface {
	Caps() Caps
	// List is every file and directory under the root, but the root.
	List(ctx context.Context) ([]Entry, error)
	Read(ctx context.Context, path string) (io.ReadCloser, error)
	// Write makes path a file with r's content (size bytes) and modTime,
	// creating or replacing it, and answers what it now is.
	Write(ctx context.Context, path string, r io.Reader, size int64, modTime time.Time) (Entry, error)
	Mkdir(ctx context.Context, path string) (Entry, error)
	// Move renames or moves a file or directory, making any missing
	// parent directories; the item keeps its id.
	Move(ctx context.Context, from, to string) (Entry, error)
	// Remove deletes a file or an empty directory. On icbfs it goes to
	// the trash.
	Remove(ctx context.Context, path string) error
	SetACL(ctx context.Context, path, sddl string) error
}

// Hashes is a side that can keep a content hash with a file, so a later
// rename can be recognized without reading the file again.
type Hashes interface {
	StoredHash(ctx context.Context, path string) (string, bool)
	StoreHash(ctx context.Context, path, hash string) error
}

// Trashed is an item in a side's trash.
type Trashed struct {
	ID        string // the trash entry's id
	Path      string // where it was
	Size      int64
	DeletedAt time.Time
}

// Trash is a side with a trash a deleted file can be brought back from,
// history and all: so a file that was renamed, but seen first as gone
// and only later as new under another name, can still be moved, not
// re-uploaded.
type Trash interface {
	TrashedFiles(ctx context.Context, size int64) ([]Trashed, error)
	TrashedHash(ctx context.Context, t Trashed) (string, error)
	Restore(ctx context.Context, t Trashed, to string) (Entry, error)
}

// Mode says which way changes go.
type Mode int

const (
	// OneWay: the folder is the truth. The drive is made to match it;
	// changes made on the drive are overwritten (kept in its history),
	// and what is only on the drive goes to its trash.
	OneWay Mode = iota
	// TwoWay: changes on either side carry to the other; when both
	// changed, the folder wins (the drive's version stays in its
	// history), and an edit beats a delete.
	TwoWay
)

func (m Mode) String() string {
	if m == TwoWay {
		return "two-way"
	}
	return "one-way"
}

// Base is what both sides agreed an item was at the last sync.
type Base struct {
	Path     string // as spelled, relative
	Dir      bool
	Size     int64
	ModTime  time.Time
	LocalID  string
	RemoteID string
	// RemoteVersion is the drive's content version at the sync.
	RemoteVersion string
	// Hash is the content's SHA-256, when it has been taken.
	Hash string
	// ACLHash is a hash of the folder side's ACL, when it has real ones.
	ACLHash string
}

// Key is how an item is matched between the sides and the base: its
// path, case-folded when the pair is case-insensitive (Windows names).
func Key(path string, caseInsensitive bool) string {
	if caseInsensitive {
		return strings.ToLower(path)
	}
	return path
}

// HashReader is the SHA-256 of r's content, as a sync side keeps it.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
