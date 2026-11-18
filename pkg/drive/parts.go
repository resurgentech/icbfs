package drive

import (
	"context"
	"errors"
	"io"
	"time"
)

// PartWriter is a drive that takes a file's content as parts staged in
// its store, each as it arrives, and writes the file from them by a copy
// on the store's side (T82-10): whoever sends a large file holds one part
// at a time, and nothing of it between parts. An upload is named by an id
// its caller makes (16 to 64 lowercase letters, digits and '-'); it exists
// from StartUpload until CompleteUpload or DropUpload, and its objects are
// in no listing, view or walk of the drive. Who may do what is the
// caller's to check, as for Write; CompleteUpload checks the item's own
// permissions as Write does.
type PartWriter interface {
	// UploadPartSize is the size of every part but the last;
	// MaxUploadParts the most parts an upload has.
	UploadPartSize() int64
	MaxUploadParts() int

	// StartUpload makes upload id with record, the caller's own (at most
	// 16 KiB). An id in use is ErrExists.
	StartUpload(ctx context.Context, id string, record []byte) error
	// UploadRecord is upload id's record and when the upload started;
	// none is ErrNotFound.
	UploadRecord(ctx context.Context, id string) (record []byte, started time.Time, err error)
	// StagePart stores part n (from 1) of upload id: size bytes of content,
	// 1 to UploadPartSize, every part but the last exactly UploadPartSize.
	// Parts come in order: part n is the next, or the last again (a retry,
	// which replaces it); any other is ErrUploadOrder.
	StagePart(ctx context.Context, id string, n int, content io.ReaderAt, size int64) error
	// UploadState is what upload id has received.
	UploadState(ctx context.Context, id string) (UploadState, error)
	// CheckUploadParts checks, from a listing, that upload id has exactly
	// parts 1 to count, every one but the last whole, and returns the
	// file's size and checksums (ErrUploadParts if not): what a caller
	// answers a complete from before it assembles the file later.
	CheckUploadParts(ctx context.Context, id string, count int) (size int64, sums Checksums, err error)
	// CompleteUpload writes path's content from upload id's parts 1 to
	// parts, exactly as Write would from the same bytes (condition,
	// permissions, locks, checksums, events). The upload stays: its caller
	// records the outcome (SetUploadNote) and erases the parts
	// (DropUploadParts). Parts not as named (one missing, more, a size
	// wrong) is ErrUploadParts, and nothing is written.
	CompleteUpload(ctx context.Context, id, path string, parts int, cond Condition, opts WriteOptions) (Item, error)
	// CompleteUploadWith is CompleteUpload with up: the time the caller
	// accepted the complete, recorded on the version written (FindUpload's
	// Newer), and Plan, told the file's key before a byte is written and
	// the store's copy id once the store has made one (T82-10 round 6).
	CompleteUploadWith(ctx context.Context, id, path string, parts int, cond Condition, opts WriteOptions, up UploadWrite) (Item, error)
	// FindUpload is what an earlier write of upload id left under key
	// (the key a Plan named; "" for the file at path now), looking at the
	// versions dated since: the version it wrote, if any; whether a person
	// has the file (at path, or since renamed, moved or deleted: in the
	// trash); and whether an upload completed after completed wrote the
	// file meanwhile. An error is "could not be searched", never "not
	// there".
	FindUpload(ctx context.Context, id, key, path string, since, completed time.Time) (UploadFind, error)
	// EraseUnnamed erases every object under key, a new file's key that a
	// Plan named and no name reaches (its write cut before its link). A
	// key some name reaches is refused (ErrExists) and nothing is erased.
	EraseUnnamed(ctx context.Context, key string) error
	// AbortCopy ends the store's copy copyID into key (a Plan's Copy: on
	// S3 a multipart upload), so its pieces are not kept: nothing to do on
	// a store without one, and an id the store no longer has is not an
	// error.
	AbortCopy(ctx context.Context, key, copyID string) error
	// UnmarkUploads erases the drive's filesystem's mark in its store's
	// index of filesystems with uploads (UploadIndex): its caller listed
	// its uploads, found none, and starts none meanwhile.
	UnmarkUploads(ctx context.Context) error
	// SetUploadNote keeps a note of the caller's beside upload id's record
	// (a name of lowercase letters; at most 16 KiB), and UploadNote reads
	// it back (ErrNotFound: none).
	SetUploadNote(ctx context.Context, id, note string, data []byte) error
	UploadNote(ctx context.Context, id, note string) (data []byte, written time.Time, err error)
	// SetUploadNoteIf is SetUploadNote conditional on the note being still
	// at etag (UploadNoteTag's), or absent ("" ): a lost race is
	// ErrPrecondition. It returns the etag written.
	SetUploadNoteIf(ctx context.Context, id, note string, data []byte, etag string) (string, error)
	UploadNoteTag(ctx context.Context, id, note string) (data []byte, etag string, written time.Time, err error)
	// UploadWritten finds the version of path that upload id wrote, among
	// those dated since (CompleteUpload records the upload on it): a
	// write run again after it landed. The item is path's file with that
	// version; ok false if none.
	UploadWritten(ctx context.Context, id, path string, since time.Time) (Item, bool, error)
	// DropUploadParts erases upload id's parts, every version, keeping its
	// record and result.
	DropUploadParts(ctx context.Context, id string) error
	// DropUpload erases upload id, every version of every object of it.
	DropUpload(ctx context.Context, id string) error
	// DropUploads erases every upload of the drive's filesystem.
	DropUploads(ctx context.Context) error
	// Uploads calls fn with each upload of the drive's filesystem, a page
	// of the store's listing at a time.
	Uploads(ctx context.Context, fn func(UploadEntry) error) error
}

// UploadWrite is what CompleteUploadWith is given beyond CompleteUpload's.
type UploadWrite struct {
	// Completed is when the caller accepted the upload's complete: it is
	// recorded on the version written, so a later look at the file
	// (FindUpload) knows an upload completed after another wrote it.
	Completed time.Time
	// Plan, if not nil, is called with the file's key before the first
	// byte is written (an existing file's key, or a new file's freshly
	// made one: New), and again with Copy once the store has made a copy
	// whose pieces outlive a process killed in it (S3's multipart upload;
	// Azure has none). An error ends the write before anything more is
	// written.
	Plan func(ctx context.Context, p UploadPlan) error
}

// UploadPlan is what an upload's write names before it writes.
type UploadPlan struct {
	Key  string `json:"key,omitempty"`
	New  bool   `json:"new,omitempty"`
	Copy string `json:"copy,omitempty"`
}

// UploadFind is what FindUpload found.
type UploadFind struct {
	// Version is the version upload id wrote under the key, if any.
	Version *Version
	// Key is the key looked at (the one given, or the file at path's).
	Key string
	// AtPath: the key is the file at path now. Named: some name reaches
	// it (at path, or elsewhere since a rename or move). Trashed: it was
	// deleted, and is in the trash.
	AtPath, Named, Trashed bool
	// Newer: a version of the key dated since was written by an upload
	// completed after the one asked about.
	Newer bool
}

// CopyKeeper is a drive that can name a store copy it could not end now
// (an Upstream's removal), outside every filesystem, and end the copies
// so named later (T82-10 round 6).
type CopyKeeper interface {
	KeepCopy(ctx context.Context, key, copyID string) error
	EndKeptCopies(ctx context.Context) (left int, err error)
}

// UploadIndex is a drive that can say which filesystems of its store
// have uploads in parts, in one listing (T82-10 round 6): a sweep reads
// only those.
type UploadIndex interface {
	UploadingFilesystems(ctx context.Context) (map[string]bool, error)
}

// UploadState is what an upload has received: parts 1 to Parts, Size
// bytes, the last stored at LastPart (zero with none).
type UploadState struct {
	Parts    int
	Size     int64
	LastPart time.Time
}

// UploadEntry is one upload a listing found: whether its record is there
// (a crash between steps can leave parts without one), and its parts.
type UploadEntry struct {
	ID     string
	Record bool
	Notes  []string // its notes' names (SetUploadNote), in order
	// NoteTimes are the notes' times as the store's listing gives them.
	NoteTimes map[string]time.Time
	Parts     int
}

// HasNote: e has the note.
func (e UploadEntry) HasNote(note string) bool {
	for _, n := range e.Notes {
		if n == note {
			return true
		}
	}
	return false
}

// ErrUploadOrder is a part out of order; ErrUploadParts an upload whose
// parts are not what its completion names.
var (
	ErrUploadOrder = errors.New("upload part out of order")
	ErrUploadParts = errors.New("the upload's parts are not as named")
)
