// Package drive is the document API's library: a Drive interface (the
// superset of what drives can do, with per-backend capabilities), the
// icbfs backend (package icbfsdrive), copying between drives, diffs,
// drafts in a side store, and the HTTP handlers (package driveapi) that
// a host application mounts. See docs/api.md.
//
// Who is acting, and as which profile on the drive's storage, comes in
// the context (WithActor, WithProfile). A Drive checks the item's own
// permissions as that profile; the role check (may this actor do this at
// all) is the caller's, before it calls.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Drive is one place in some backend's storage: a whole icbfs filesystem
// or a directory in one, and later a WebDAV share, a file share, a
// Git repository. Paths are relative to the drive's root, '/'-separated,
// with or without a leading '/'. An operation the backend can't do
// returns ErrNotSupported.
type Drive interface {
	Capabilities() Capabilities

	List(ctx context.Context, dir ItemRef, opts ListOptions) (Page, error)
	Stat(ctx context.Context, item ItemRef, at At) (Item, error)
	Read(ctx context.Context, item ItemRef, at At, rng *Range) (io.ReadCloser, Item, error)
	Write(ctx context.Context, path string, body io.Reader, cond Condition, opts WriteOptions) (Item, error)
	Mkdir(ctx context.Context, path string, parents bool) (Item, error)
	Move(ctx context.Context, from ItemRef, to string, replace bool) (Item, error)
	Delete(ctx context.Context, item ItemRef, recursive bool, cond Condition) error

	Versions(ctx context.Context, item ItemRef) ([]Version, error)
	Names(ctx context.Context) ([]Name, error)
	CreateName(ctx context.Context, name string, at time.Time, filesystemWide bool) (Name, error)
	DeleteName(ctx context.Context, name string) error
	Trash(ctx context.Context, opts TrashOptions) (TrashPage, error)
	Restore(ctx context.Context, entry string, opts RestoreOptions) (Item, error)
	Purge(ctx context.Context, item ItemRef, allNames bool) error
	Changes(ctx context.Context) (<-chan Change, error)
	Identities(ctx context.Context, progress func(scanned int)) ([]Identity, error)
	Permissions(ctx context.Context, item ItemRef) (Permissions, error)

	// SetOrigin records where an item was copied from, where the backend
	// can keep it (Capabilities.Origin).
	SetOrigin(ctx context.Context, item ItemRef, o Origin) error
}

// A TimeSetter is a drive that can set an item's times as they were at
// its source, without a new version of its content: what a sync daemon
// needs for a directory (Mkdir takes no times) and for an item copied
// before it kept them. A zero time is left as it is. A file's modified
// time is tied to its content now, as WriteOptions.ModTime's is.
type TimeSetter interface {
	SetTimes(ctx context.Context, item ItemRef, modified, created time.Time) (Item, error)
}

// FolderHistorian is a drive whose folders' history can be read and
// written over: for a caller (a mirror) correcting a folder's past, say
// for a date its source got wrong. A change that merely arrives late is
// put in at its date by the drive itself (WithSourceTime).
type FolderHistorian interface {
	// FolderHistory is dir's whole history: each state it has been in,
	// oldest first, as the drive's views of the past see it.
	FolderHistory(ctx context.Context, dir ItemRef) (FolderHistory, error)

	// ReplaceFolderHistory erases dir's history and writes states in its
	// place, oldest first; the last is dir now, so it must hold exactly
	// what dir holds now. Items are untouched: an entry names an existing
	// item by id, whatever folder it's in now. cond.IfMatch is the Version
	// FolderHistory returned: a PreconditionError if dir has changed
	// since. The caller makes no other change to dir while it runs.
	ReplaceFolderHistory(ctx context.Context, dir ItemRef, states []FolderState, cond Condition) error
}

// FileHistorian is a drive whose files' history can be read and written
// over, as a FolderHistorian's folders' can: for a caller correcting a
// file's past.
type FileHistorian interface {
	// FileHistory is item's content versions, newest first by date, and
	// the current version's id (for ReplaceFileHistory's condition).
	FileHistory(ctx context.Context, item ItemRef) (FileHistory, error)

	// ReplaceFileHistory erases item's history and writes states in its
	// place, oldest first; the last is the file now. A state keeps an
	// existing version (copied, nothing downloaded) or gives new content.
	// cond.IfMatch is the Version FileHistory returned: a
	// PreconditionError if the file has changed since. The caller makes no
	// other change to the file while it runs.
	ReplaceFileHistory(ctx context.Context, item ItemRef, states []FileState, cond Condition) error
}

// FileHistory is a file's history: its versions, and its current
// version's id.
type FileHistory struct {
	Versions []Version `json:"versions"`
	Version  string    `json:"version"`
}

// FileState is a file's state from At: content Version (an existing
// version's id) or Content (new bytes), and its record then (Info: a
// zero Modified is At; the rest, if unset, is the file's record now).
type FileState struct {
	At      time.Time
	Version string
	Content io.Reader
	Info    ItemInfo
}

// FolderHistory is a folder's history. Version names its current state,
// for a ReplaceFolderHistory conditional on nothing having changed since.
type FolderHistory struct {
	States  []FolderState `json:"states"`
	Version string        `json:"version"`
}

// FolderState is a folder's entries from At until the next state.
type FolderState struct {
	At      time.Time     `json:"at"`
	Entries []FolderEntry `json:"entries"`
}

// FolderEntry is one name in a folder: the item it names, by id, and the
// item's type.
type FolderEntry struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Type string `json:"type"`
}

// Capabilities says what a drive's backend can do.
type Capabilities struct {
	IDs              bool   `json:"ids"`      // ids stable across renames
	Versions         bool   `json:"versions"` // a file's older versions
	AsOf             bool   `json:"asOf"`     // the tree at a past time
	Names            bool   `json:"names"`    // named versions
	Trash            bool   `json:"trash"`
	Purge            bool   `json:"purge"`
	ConditionalWrite bool   `json:"conditionalWrite"`
	Events           bool   `json:"events"`
	Origin           bool   `json:"origin"`      // keeps a copy's origin
	Permissions      string `json:"permissions"` // "windows", "posix", or "" for none
	Diff             string `json:"diff"`        // "computed" or "native"
	AppObjects       bool   `json:"appObjects"`  // keeps a tool's objects beside the drive (AppObjects)
	// ConditionalAppObjects: app object writes can be conditioned on the
	// version read (ConditionalAppObjects).
	ConditionalAppObjects bool `json:"conditionalAppObjects"`
	// Locks: items can be locked (Locker), and Write, Move and Delete
	// refuse an item another holder has locked (ErrLocked).
	Locks bool `json:"locks"`
}

// AppObjects is a drive that keeps objects for a tool beside its
// storage, not in the tree: a mirror's table of the ids its app keys use,
// say. Named like app keys, "<tool>.<name>" (CheckAppKey); small (a few
// MB); versioned, so a view of the past reads the one current then. Not
// in the HTTP API. Reading one needs the right to read the drive's root,
// writing one the right to write it.
type AppObjects interface {
	AppObject(ctx context.Context, name string) ([]byte, error)
	SetAppObject(ctx context.Context, name string, data []byte) error
	DeleteAppObject(ctx context.Context, name string) error
	// ListAppObjects lists the names starting with prefix ("" for all).
	ListAppObjects(ctx context.Context, prefix string) ([]string, error)
}

// ConditionalAppObjects is an AppObjects whose writes can be conditioned
// on the version read, as Write and Delete are on an item's: two tools
// rewriting one object then can't lose each other's update. A lost race
// is ErrPrecondition, a *PreconditionError with the current version.
// Versions are the store's, opaque; compare them, don't order them.
type ConditionalAppObjects interface {
	AppObjects
	// AppObjectVersion is AppObject with the version read.
	AppObjectVersion(ctx context.Context, name string) (data []byte, version string, err error)
	// SetAppObjectIf writes name when cond holds (IfMatch: it is still that
	// version; IfNoneMatch: it doesn't exist yet; neither: always), and
	// returns the version written.
	SetAppObjectIf(ctx context.Context, name string, data []byte, cond Condition) (version string, err error)
	// DeleteAppObjectIf deletes name when cond holds (IfMatch; IfNoneMatch
	// is meaningless here and ErrInvalid).
	DeleteAppObjectIf(ctx context.Context, name string, cond Condition) error
	// AppObjectVersions lists name's versions, newest first, with their
	// times; a deleted object's still list. A name never written is
	// ErrNotFound.
	AppObjectVersions(ctx context.Context, name string) ([]Version, error)
	// AppObjectAt reads one of those versions; an unknown one is
	// ErrNotFound.
	AppObjectAt(ctx context.Context, name, version string) ([]byte, error)
}

// Locker is a drive whose files can be locked against others' writes: a
// whole-file, exclusive, leased lock, the same lock a mount's flock or
// fcntl takes, so a file locked either way is locked to both. A tool
// locks the files it is about to write and read back, so the set is all
// its own at one moment. Locks are best-effort, built from the store's
// conditional writes (docs/architecture.md, "Locking"): a lock can land
// between a writer's check and its write.
//
// holder names who holds the lock: any string, the same for every call
// of one holder, and what a mount's fcntl(F_GETLK) reports. Writes pass
// a lock only under a context WithLockHolder of the same holder.
//
// A mount honours these locks only with its locking on (the default on
// Linux; -locking=false turns it off), and a Windows mount never does:
// WinFsp keeps its byte-range locks in its driver, and they never reach
// the store.
type Locker interface {
	// Lock takes item for holder, for ttl, without waiting: ErrLocked if
	// another holder has it; ErrIsDir for a directory. A holder that
	// locks a file it holds already extends the lease.
	Lock(ctx context.Context, item ItemRef, holder string, ttl time.Duration) (Lease, error)
	// Renew extends l for ttl more from now. ErrNotLockHolder once the
	// lease lapsed and the lock is gone or another's.
	Renew(ctx context.Context, l Lease, ttl time.Duration) error
	// Unlock releases l. ErrNotLockHolder if it had lapsed and another
	// holder took the lock; releasing a lock that simply expired is fine.
	Unlock(ctx context.Context, l Lease) error
}

// Lease is a lock held: the item, by id, whose, and until when, by this
// program's clock at the moment of the Lock or Renew plus the ttl. The
// store's clock decides the real expiry, so renew well before it.
type Lease struct {
	Item    ItemRef
	Holder  string
	Expires time.Time
}

// ItemRef names an item by its id or by its path; ID wins if both are set.
type ItemRef struct {
	Path string
	ID   string
}

// ByPath and ByID make an ItemRef.
func ByPath(p string) ItemRef { return ItemRef{Path: p} }
func ByID(id string) ItemRef  { return ItemRef{ID: id} }

// At is when to look: now (the zero At), a time, a named version, or —
// for one file's content — one version by its id. Anything but now is a
// read-only view.
type At struct {
	Time    time.Time
	Name    string
	Version string
}

// Now reports whether a is the present.
func (a At) Now() bool { return a.Time.IsZero() && a.Name == "" && a.Version == "" }

// Range is a byte range of content: Length < 0 means to the end.
type Range struct {
	Offset, Length int64
}

// Condition makes a write or delete conditional: IfMatch, only if the
// item is still that version (by id); IfNoneMatch, only if it doesn't
// exist.
type Condition struct {
	IfMatch     string
	IfNoneMatch bool
}

// WriteOptions for Write.
type WriteOptions struct {
	Parents     bool   // make missing parent directories
	ContentType string // recorded with the content, where the backend keeps it
	// Size is the body's length when the caller knows it: a download's
	// reported length, a listing's size, a local file's, an HTTP
	// upload's Content-Length. Known, the write goes to the store as the
	// body arrives, one part in memory at a time and nothing on disk,
	// and a body that turns out shorter or longer fails the write.
	// Unknown (0, the zero value; a known-empty body may say 0 too, it
	// reads the same) the body is read first, into memory when small and
	// else into a temporary file.
	Size int64
	// ModTime, if set, is the file's time at its source (a document's
	// modification date), shown as its modified time instead of the moment
	// of the write; a later write without one puts it back to the write's.
	ModTime time.Time
	// Created, if set, is the file's creation time at its source, shown
	// as its creation time instead of the moment it was first written
	// here; it stays through later writes.
	Created time.Time
	// Origin, if set, records where the content was copied from, in the
	// same write (as SetOrigin would after it).
	Origin *Origin
	// Checksums, if set, are what the caller knows the content's to be
	// (from its source): the write fails with ErrChecksum if what arrived
	// doesn't match, before anything is written when the body is read
	// first, and erasing the version written when it was sent as it
	// arrived (Size). Either may be empty.
	Checksums *Checksums
	// App, Mode and WindowsAttrs are recorded in the same metadata write
	// as the times and the origin (see ItemInfo).
	App          map[string][]byte
	Mode         *uint32
	WindowsAttrs *uint32
}

// ItemInfo is what SetInfo records about an item in one metadata write,
// with no new version of its content. Every field is optional.
type ItemInfo struct {
	// Modified and Created are the item's times at its source; a zero
	// time is left as it is. A file's modified time is tied to its content
	// now, as WriteOptions.ModTime's is.
	Modified, Created time.Time
	// App is app data the caller keeps on the item, keyed "<tool>.<name>"
	// (CheckAppKey); "drive." keys are the drive's own. A nil value removes
	// the key. App data is bookkeeping, not a change to the item: it
	// doesn't move the item's changed time.
	App map[string][]byte
	// Mode is the item's permission bits; WindowsAttrs its ReadOnly,
	// Hidden, System and Archive bits. A source item locked at its source
	// is shown read-only on every mount with Mode 0444 (0555 for a
	// directory) and WindowsAttrs ReadOnly.
	Mode         *uint32
	WindowsAttrs *uint32
	// Origin, if set, records where the item was copied from.
	Origin *Origin
}

// An InfoSetter is a drive that records ItemInfo in one write: SetTimes
// with the rest of what a mirror knows.
type InfoSetter interface {
	SetInfo(ctx context.Context, item ItemRef, info ItemInfo) (Item, error)
}

// A FileWriter is a drive that writes from content the caller holds,
// read at offsets: a local file in a sync, a connector's own spool. The
// caller's file is the scratch file, parts are read from it as they go
// up and read again if one is retried, and nothing is copied. Otherwise
// Write: the same conditions, options, versions and checksums.
type FileWriter interface {
	WriteFrom(ctx context.Context, path string, content io.ReaderAt, size int64, cond Condition, opts WriteOptions) (Item, error)
}

// CheckAppKey is nil if key is a valid app key for a caller to set:
// "<tool>.<name>", with a tool of lowercase letters and digits, and not
// the drive's own ("drive.").
func CheckAppKey(key string) error {
	tool, name, ok := strings.Cut(key, ".")
	if !ok || tool == "" || name == "" {
		return fmt.Errorf("app key %q: want <tool>.<name>: %w", key, ErrInvalid)
	}
	for _, r := range tool {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return fmt.Errorf("app key %q: the tool is lowercase letters and digits: %w", key, ErrInvalid)
		}
	}
	if tool == "drive" {
		return fmt.Errorf("app key %q: drive.* keys are the drive's own: %w", key, ErrInvalid)
	}
	return nil
}

// Checksums are a file version's MD5 and SHA-256, in hex; one not
// computed is empty. A drive computes both as a write's bytes go by, and
// any version's on demand (Checksummer).
type Checksums struct {
	MD5    string `json:"md5,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Checksummer is a drive that gives any version's checksums, computing
// them (once: it keeps them) if they're missing.
type Checksummer interface {
	// Checksums are item's content version version's ("" for the
	// current one).
	Checksums(ctx context.Context, item ItemRef, version string) (Checksums, error)
}

// Clock is a drive that tells its store's time: the clock the store
// stamps versions with, rather than the caller's. Its resolution is
// whole seconds (a response's Date header), coarser than a store's
// stamps (milliseconds on MinIO and Azure, seconds on AWS S3).
type Clock interface {
	// StoreTime is the store's clock now, truncated to the second.
	StoreTime(ctx context.Context) (time.Time, error)
}

// ListOptions for List.
type ListOptions struct {
	At     At
	Depth  int    // 1 (the default) lists the directory itself; more lists below
	Limit  int    // at most this many entries (0: all)
	Cursor string // a previous Page's Next
	Sort   string // "name" (default), "modified", "size"
}

// Page is one page of a listing.
type Page struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
}

// Item types.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
)

// Item is a file, directory or symlink.
type Item struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType,omitempty"`
	Version     *Version  `json:"version,omitempty"` // a file's current version
	Modified    time.Time `json:"modified"`
	Changed     time.Time `json:"changed"`
	Created     time.Time `json:"created,omitempty"`
	Target      string    `json:"target,omitempty"` // a symlink's
	Origin      *Origin   `json:"origin,omitempty"`
	// Checksums are a file's current content's, when computed.
	Checksums *Checksums `json:"checksums,omitempty"`
	// App, Mode and WindowsAttrs are the item's app data, permission bits
	// and Windows attribute bits (nil if it has none), as ItemInfo sets
	// them. For callers of the library; not in the HTTP API.
	App          map[string][]byte `json:"-"`
	Mode         uint32            `json:"-"`
	WindowsAttrs *uint32           `json:"-"`
	HasDraft     bool              `json:"hasDraft,omitempty"` // filled in by the HTTP layer, from the drafts
	DraftsBy     []string          `json:"draftsBy,omitempty"`

	// Stat only.
	ParentID string   `json:"parentId,omitempty"`
	Paths    []string `json:"paths,omitempty"` // every path to it (hard links)
	Owner    string   `json:"owner,omitempty"` // a SID, or uid:N
	Rights   *Rights  `json:"rights,omitempty"`
}

// Rights are what the acting profile may do to an item.
type Rights struct {
	Read              bool `json:"read"`
	Write             bool `json:"write"`
	Delete            bool `json:"delete"`
	ChangePermissions bool `json:"changePermissions"`
}

// Version is one state of a file's content. Time is when it came to be:
// at its source, when it was written under WithSourceTime, else when it
// was stored, which Stored always is.
type Version struct {
	ID      string    `json:"version"`
	Time    time.Time `json:"time"`
	Stored  time.Time `json:"stored"`
	Size    int64     `json:"size"`
	Current bool      `json:"current,omitempty"`
	Names   []string  `json:"names,omitempty"` // named versions it was current at
	// Checksums, when computed.
	Checksums *Checksums `json:"checksums,omitempty"`
}

// Name is a named version: a name for a moment.
type Name struct {
	Name           string    `json:"name"`
	Time           time.Time `json:"time"`
	FilesystemWide bool      `json:"filesystemWide,omitempty"`
	Scope          string    `json:"scope,omitempty"` // the drive-relative path it was made for, if not the whole filesystem
	CreatedBy      string    `json:"createdBy,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// TrashOptions for Trash.
type TrashOptions struct {
	Path   string // only what was deleted from under this directory
	Limit  int
	Cursor string
	// OriginDrive, if set, keeps only entries whose item had been copied
	// from that drive, and OriginID, if set too, from that item there: how
	// a mirror finds what it trashed when the source brings an item back.
	OriginDrive string
	OriginID    string
	// Pass, if set, keeps only what that sync pass removed (WithPass).
	Pass string
}

// TrashEntry is one deleted item.
type TrashEntry struct {
	ID        string    `json:"id"`
	ItemID    string    `json:"itemId"`
	Name      string    `json:"name"`
	Path      string    `json:"path,omitempty"` // where it was, drive-relative, when known
	Type      string    `json:"type"`
	DeletedAt time.Time `json:"deletedAt"`
	DeletedBy string    `json:"deletedBy,omitempty"`
	Size      int64     `json:"size"`
	Version   string    `json:"version,omitempty"`
	Batch     string    `json:"batch,omitempty"`
	// Origin is where the item had been copied from, as it was when the
	// item was deleted (SetOrigin, Copy); nil for an item with none.
	Origin *Origin `json:"origin,omitempty"`
	// Pass is the sync pass that removed it (WithPass), if one did.
	Pass string `json:"pass,omitempty"`
}

// TrashPage is one page of the trash.
type TrashPage struct {
	Entries []TrashEntry `json:"entries"`
	Next    string       `json:"next,omitempty"`
}

// RestoreOptions for Restore: To is the path to restore to (default:
// where it was).
type RestoreOptions struct {
	To      string
	Parents bool
	Replace bool
}

// Change is something that changed on the drive.
type Change struct {
	Path string `json:"path,omitempty"` // the directory that changed, when known
	ID   string `json:"id,omitempty"`
}

// Identity is one principal found on a drive's items.
type Identity struct {
	ID    string `json:"id"` // a SID, uid:N or gid:N
	Kind  string `json:"kind"`
	Items int    `json:"items"`
}

// Permissions are an item's own permissions.
type Permissions struct {
	Kind      string `json:"kind"`                // "windows" or "posix"
	SDDL      string `json:"sddl,omitempty"`      // windows
	Inherited string `json:"inherited,omitempty"` // windows: the ancestor path it came from, when the item has none of its own
	Owner     string `json:"owner,omitempty"`
	Group     string `json:"group,omitempty"`
	Mode      uint32 `json:"mode,omitempty"` // posix
}

// Origin records where a copy came from.
type Origin struct {
	Drive   string    `json:"drive"`
	ID      string    `json:"id"`
	Version string    `json:"version,omitempty"`
	Path    string    `json:"path,omitempty"`
	Copied  time.Time `json:"copied"`
}

// Errors drives return; the HTTP layer maps each to a status and code.
var (
	ErrNotFound     = errors.New("not found")
	ErrForbidden    = errors.New("forbidden")
	ErrNoAccess     = errors.New("no profile on this drive")
	ErrNotSupported = errors.New("not supported by this drive")
	ErrExists       = errors.New("already exists")
	ErrNotEmpty     = errors.New("directory not empty")
	ErrNotDir       = errors.New("not a directory")
	ErrIsDir        = errors.New("is a directory")
	ErrHasLinks     = errors.New("the file has other names")
	ErrPrecondition = errors.New("precondition failed")
	ErrInvalidName  = errors.New("invalid name or path")
	ErrTooLarge     = errors.New("too large")
	ErrReadOnly     = errors.New("read-only")
	ErrBusy         = errors.New("busy")
	ErrInvalid      = errors.New("invalid request")
	ErrChecksum     = errors.New("the content doesn't match the checksum given")
	// ErrLocked: the item is locked by another holder (Locker).
	ErrLocked = errors.New("locked by another holder")
	// ErrNotLockHolder: the lease lapsed and the lock is gone or another's (Locker).
	ErrNotLockHolder = errors.New("not the lock's holder")
)

// PreconditionError is ErrPrecondition with the current version.
type PreconditionError struct {
	Current string
}

func (e *PreconditionError) Error() string { return "precondition failed: now version " + e.Current }
func (e *PreconditionError) Unwrap() error { return ErrPrecondition }
