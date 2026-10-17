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
	"io"
	"time"
)

// Drive is one place in some backend's storage: a whole icbfs filesystem
// or a directory in one, and later a SharePoint library, a file share, a
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
	HasDraft    bool      `json:"hasDraft,omitempty"` // filled in by the HTTP layer, from the drafts
	DraftsBy    []string  `json:"draftsBy,omitempty"`

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

// Version is one state of a file's content.
type Version struct {
	ID      string    `json:"version"`
	Time    time.Time `json:"time"`
	Size    int64     `json:"size"`
	Current bool      `json:"current,omitempty"`
	Names   []string  `json:"names,omitempty"` // named versions it was current at
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
)

// PreconditionError is ErrPrecondition with the current version.
type PreconditionError struct {
	Current string
}

func (e *PreconditionError) Error() string { return "precondition failed: now version " + e.Current }
func (e *PreconditionError) Unwrap() error { return ErrPrecondition }
