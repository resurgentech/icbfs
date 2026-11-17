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
	// CompleteUpload writes path's content from upload id's parts 1 to
	// parts, exactly as Write would from the same bytes (condition,
	// permissions, locks, checksums, events). The upload stays: the
	// caller records the result (FinishUpload) and erases the parts
	// (DropUploadParts) when it chooses, out of the request. Parts not as
	// named (one missing, more, a size wrong) is ErrUploadParts, and
	// nothing is written.
	CompleteUpload(ctx context.Context, id, path string, parts int, cond Condition, opts WriteOptions) (Item, error)
	// FinishUpload records the caller's result of upload id (at most 16
	// KiB), and UploadResult reads it back (ErrNotFound: not finished).
	FinishUpload(ctx context.Context, id string, result []byte) error
	UploadResult(ctx context.Context, id string) (result []byte, finished time.Time, err error)
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
	Done   bool // finished: its result recorded (FinishUpload)
	Parts  int
}

// ErrUploadOrder is a part out of order; ErrUploadParts an upload whose
// parts are not what its completion names.
var (
	ErrUploadOrder = errors.New("upload part out of order")
	ErrUploadParts = errors.New("the upload's parts are not as named")
)
