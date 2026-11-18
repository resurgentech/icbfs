package icbfs

import (
	"context"
	"encoding"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// Uploads in parts (T82-10): a file's content sent as parts of
// UploadPartSize, each stored as an object of its own as it arrives, and
// written as the file's new version by a copy of the parts on the store's
// side (PutParts with every part a copy), so that whoever sends it holds
// one part, never the file, and nothing between parts.
//
// An upload's objects are under <id>.upload-<upload>/, outside the
// filesystem's own prefix (<id>-): no listing of the filesystem (DiskUsage,
// fsck, HistoryUnder, the MinIO notification filter on Prefix), no view of
// its tree or its past, and no mount ever sees them, since none reads a key
// it was not led to from the root or under <id>-. Prune erases them with
// the filesystem.
//
//   - record: the caller's own small record of the upload (StartUpload),
//     whose presence says the upload exists;
//   - p00001 ... p10000: the parts, each but the last exactly
//     UploadPartSize. Each part's metadata holds the MD5 and SHA-256 state
//     of the content up to its end, so the file's checksums are made here,
//     a part at a time, and the last part's state is the file's. That
//     chains the parts: part n is staged only after part n-1, and a part
//     staged again (a retry) is taken only while it is the last.
//
// On S3 a copied part must be at least 5 MiB unless it is the last
// (UploadPartCopy), and there are at most 10,000: UploadPartSize and
// MaxUploadParts are those limits, so every staged part becomes one part
// of the file, whatever the filesystem's own part size (partSize). On
// Azure each becomes one block, staged from the part's blob by
// StageBlockFromURL, under the same limits (at most 50,000 blocks).
const (
	UploadPartSize int64 = 5 << 20
	MaxUploadParts       = 10000
	// MaxUploadRecord is the largest record an upload keeps.
	MaxUploadRecord = 16 << 10
)

// ErrUploadOrder is a part staged out of order (not the next, nor the
// last again); ErrUploadParts is an upload whose parts are not the ones
// its completion names (one missing, more than named, a size wrong).
var (
	ErrUploadOrder = errors.New("upload part out of order")
	ErrUploadParts = errors.New("the upload's parts are not as named")
)

// metadata names of a part's checksum state.
const (
	mdMD5State    = "icbfs_md5_state"
	mdSHA256State = "icbfs_sha256_state"
)

func (f *Filesystem) uploadPrefix() string { return f.id + ".upload-" }

// UploadPrefix is the key prefix of every upload's objects of this
// filesystem: outside Prefix.
func (f *Filesystem) UploadPrefix() string { return f.uploadPrefix() }

func (f *Filesystem) uploadDir(id string) string { return f.uploadPrefix() + id + "/" }

func uploadRecordKey(dir string) string { return dir + "record" }

func uploadPartKey(dir string, n int) string { return fmt.Sprintf("%sp%05d", dir, n) }

// CheckUploadID is nil if id can name an upload: 16 to 64 lowercase
// letters, digits and '-'.
func CheckUploadID(id string) error {
	if len(id) < 16 || len(id) > 64 {
		return fmt.Errorf("upload id %q: 16 to 64 characters: %w", id, ErrInvalidArgument)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("upload id %q: lowercase letters, digits and '-': %w", id, ErrInvalidArgument)
		}
	}
	return nil
}

// ErrUploadsUnsupported is a store whose part limits can't take an
// upload's parts as the file's parts.
var ErrUploadsUnsupported = errors.New("this store's part limits can't take uploads in parts")

// uploadsFit is nil if a store with limits lim takes every staged part as
// one part of the file: UploadPartSize within Min and Max, and
// MaxUploadParts within Count. S3 (5 MiB, 5 GiB, 10,000) and Azure (1 B,
// 4000 MiB, 50,000) both do.
func uploadsFit(lim objstore.PartLimits) error {
	if UploadPartSize < lim.Min || UploadPartSize > lim.Max || MaxUploadParts > lim.Count {
		return fmt.Errorf("%w: parts of %d, at most %d, against %+v", ErrUploadsUnsupported, UploadPartSize, MaxUploadParts, lim)
	}
	return nil
}

// StartUpload writes upload id's record, which makes it exist.
func (f *Filesystem) StartUpload(ctx context.Context, id string, record []byte) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if err := CheckUploadID(id); err != nil {
		return err
	}
	if len(record) > MaxUploadRecord {
		return fmt.Errorf("upload record of %d bytes, over %d: %w", len(record), MaxUploadRecord, ErrInvalidArgument)
	}
	// The filesystem's mark first (if absent): a sweep that lists the marks
	// finds this upload's filesystem once its record is there.
	if _, err := f.store.PutIfAbsent(ctx, f.uploadMark(), strings.NewReader(""), nil); err != nil && !objstore.IsPreconditionFailed(err) {
		return err
	}
	_, err := f.store.PutIfAbsent(ctx, uploadRecordKey(f.uploadDir(id)), strings.NewReader(string(record)), nil)
	if objstore.IsPreconditionFailed(err) {
		return fmt.Errorf("upload %s: %w", id, ErrExists)
	}
	return err
}

// UploadRecord is upload id's record and when it was written.
func (f *Filesystem) UploadRecord(ctx context.Context, id string) ([]byte, time.Time, error) {
	if err := CheckUploadID(id); err != nil {
		return nil, time.Time{}, err
	}
	body, obj, err := f.store.Get(ctx, uploadRecordKey(f.uploadDir(id)))
	if err != nil {
		return nil, time.Time{}, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, MaxUploadRecord+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	return data, obj.LastModified, nil
}

// StageUploadPart stores part n (from 1) of upload id: size bytes of
// content, 1 to UploadPartSize. Part n is taken when the upload exists,
// part n-1 is there and whole (UploadPartSize), and no part n+1 is: the
// next part, or the last one again. It holds nothing of the content
// beyond what content holds; the store reads it from there.
func (f *Filesystem) StageUploadPart(ctx context.Context, id string, n int, content io.ReaderAt, size int64) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if err := CheckUploadID(id); err != nil {
		return err
	}
	if n < 1 || n > MaxUploadParts {
		return fmt.Errorf("part %d: parts are 1 to %d: %w", n, MaxUploadParts, ErrInvalidArgument)
	}
	if size < 1 || size > UploadPartSize {
		return fmt.Errorf("part %d of %d bytes: a part is 1 to %d: %w", n, size, UploadPartSize, ErrInvalidArgument)
	}
	if int64(n-1)*UploadPartSize+size > f.maxFileSize {
		return ErrFileTooLarge
	}
	if err := uploadsFit(f.store.PartLimits()); err != nil {
		return err
	}
	dir := f.uploadDir(id)
	if _, err := f.store.Head(ctx, uploadRecordKey(dir)); err != nil {
		return fmt.Errorf("upload %s: %w", id, mapNotFound(err))
	}
	sum := NewChecksummer()
	if n > 1 {
		prev, err := f.store.Head(ctx, uploadPartKey(dir, n-1))
		if objstore.IsNotFound(err) {
			return fmt.Errorf("part %d before part %d: %w", n, n-1, ErrUploadOrder)
		}
		if err != nil {
			return err
		}
		if prev.Size != UploadPartSize {
			return fmt.Errorf("part %d after part %d of %d bytes, which was the last: %w", n, n-1, prev.Size, ErrUploadOrder)
		}
		if err := sum.restore(prev.Metadata); err != nil {
			return fmt.Errorf("part %d: %w", n-1, err)
		}
	}
	if _, err := f.store.Head(ctx, uploadPartKey(dir, n+1)); err == nil {
		return fmt.Errorf("part %d again after part %d: %w", n, n+1, ErrUploadOrder)
	} else if !objstore.IsNotFound(err) {
		return err
	}
	if _, err := io.Copy(sum, io.NewSectionReader(content, 0, size)); err != nil {
		return err
	}
	md, err := sum.state()
	if err != nil {
		return err
	}
	_, err = f.store.Put(ctx, uploadPartKey(dir, n), io.NewSectionReader(content, 0, size), md, "")
	return err
}

// UploadState is what an upload has received.
type UploadState struct {
	Parts    int       // parts 1 to Parts are there
	Size     int64     // their bytes
	LastPart time.Time // when part Parts was stored; zero with none
}

// UploadState lists upload id's parts, a page at a time.
func (f *Filesystem) UploadState(ctx context.Context, id string) (UploadState, error) {
	if err := CheckUploadID(id); err != nil {
		return UploadState{}, err
	}
	dir := f.uploadDir(id)
	if _, err := f.store.Head(ctx, uploadRecordKey(dir)); err != nil {
		return UploadState{}, fmt.Errorf("upload %s: %w", id, mapNotFound(err))
	}
	var st UploadState
	last := 0
	err := objstore.ListEach(ctx, f.store, dir+"p", func(page []objstore.Object) error {
		for _, o := range page {
			n, err := strconv.Atoi(strings.TrimPrefix(o.Key, dir+"p"))
			if err != nil {
				continue
			}
			st.Parts++
			st.Size += o.Size
			last = max(last, n)
		}
		return nil
	})
	if err != nil {
		return UploadState{}, err
	}
	if last != st.Parts {
		return UploadState{}, fmt.Errorf("upload %s: %d parts, the last numbered %d: %w", id, st.Parts, last, ErrUploadParts)
	}
	if last > 0 {
		obj, err := f.store.Head(ctx, uploadPartKey(dir, last))
		if err != nil {
			return UploadState{}, err
		}
		st.LastPart = obj.LastModified
	}
	return st, nil
}

// UploadSource is an upload's parts as the copies that make the file,
// with the file's size and checksums.
type UploadSource struct {
	ID string // the upload's: recorded on the version it writes (MetaUpload)
	// Completed is when the caller accepted the upload's complete, if it
	// said (recorded as MetaUploadAt).
	Completed time.Time
	Parts     []objstore.Part
	Size      int64
	Sums      Checksums
}

// MetaUpload is the content version's metadata naming the upload it was
// written from (WriteUpload): how a write run again finds it already
// landed (UploadVersion), whatever was written to the file since.
const MetaUpload = "icbfs_upload"

// MetaUploadAt is the content version's metadata saying when the caller
// accepted the complete of the upload it was written from (RFC 3339 with
// nanoseconds, UTC): how an upload's write run late finds that one
// completed after it already wrote the file (UploadVersions' newer,
// T82-10 round 6, AS-31).
const MetaUploadAt = "icbfs_upload_at"

// uploadHeads is how many parts' Heads UploadSources has in flight.
const uploadHeads = 8

// UploadSources are upload id's parts 1 to count, which must be all its
// parts, each but the last whole, as the parts of a PutParts that copies
// them: one Head each, uploadHeads at a time. What it holds is the result:
// each part's key, size and version (about 170 bytes a part), and the
// last part's checksum state; a Head's answer is dropped as soon as those
// are taken from it (T82-10 review R4).
func (f *Filesystem) UploadSources(ctx context.Context, id string, count int) (UploadSource, error) {
	if err := CheckUploadID(id); err != nil {
		return UploadSource{}, err
	}
	if count < 1 || count > MaxUploadParts {
		return UploadSource{}, fmt.Errorf("%d parts: 1 to %d: %w", count, MaxUploadParts, ErrUploadParts)
	}
	dir := f.uploadDir(id)
	if _, err := f.store.Head(ctx, uploadRecordKey(dir)); err != nil {
		return UploadSource{}, fmt.Errorf("upload %s: %w", id, mapNotFound(err))
	}
	src := UploadSource{ID: id, Parts: make([]objstore.Part, count)}
	var lastMeta map[string]string
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var first error
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	sem := make(chan struct{}, uploadHeads)
	var wg sync.WaitGroup
	for n := 1; n <= count && hctx.Err() == nil; n++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			defer func() { <-sem }()
			key := uploadPartKey(dir, n)
			o, err := f.store.Head(hctx, key)
			switch {
			case objstore.IsNotFound(err):
				fail(fmt.Errorf("upload %s has no part %d: %w", id, n, ErrUploadParts))
				return
			case err != nil:
				fail(err)
				return
			case n < count && o.Size != UploadPartSize:
				fail(fmt.Errorf("upload %s: part %d is %d bytes, not %d: %w", id, n, o.Size, UploadPartSize, ErrUploadParts))
				return
			}
			// Only what the copy needs is kept: the part's own slot.
			src.Parts[n-1] = objstore.Part{Size: o.Size, CopyKey: key, CopyVersion: o.VersionID}
			if n == count {
				lastMeta = o.Metadata
			}
		}(n)
	}
	wg.Wait()
	if first != nil {
		return UploadSource{}, first
	}
	if err := ctx.Err(); err != nil {
		return UploadSource{}, err
	}
	if _, err := f.store.Head(ctx, uploadPartKey(dir, count+1)); err == nil {
		return UploadSource{}, fmt.Errorf("upload %s has more than %d parts: %w", id, count, ErrUploadParts)
	} else if !objstore.IsNotFound(err) {
		return UploadSource{}, err
	}
	for _, p := range src.Parts {
		src.Size += p.Size
	}
	if src.Size > f.maxFileSize {
		return UploadSource{}, ErrFileTooLarge
	}
	sum := NewChecksummer()
	if err := sum.restore(lastMeta); err != nil {
		return UploadSource{}, fmt.Errorf("upload %s part %d: %w", id, count, err)
	}
	src.Sums = sum.Sum()
	return src, nil
}

// WriteUpload writes key's content from src's parts, copied on the store's
// side, conditional on ifMatch as WriteFile is.
func (f *Filesystem) WriteUpload(ctx context.Context, key string, src UploadSource, ifMatch string) (*objstore.Object, error) {
	if err := f.checkWritable(); err != nil {
		return nil, err
	}
	if len(src.Parts) == 0 {
		return nil, ErrInvalidArgument
	}
	var md map[string]string
	if src.ID != "" {
		md = map[string]string{MetaUpload: src.ID}
		if !src.Completed.IsZero() {
			md[MetaUploadAt] = src.Completed.UTC().Format(time.RFC3339Nano)
		}
	}
	return f.store.PutParts(ctx, key, src.Parts, md, ifMatch)
}

// UploadVersions looks at key's versions dated since (less
// uploadClockSlack), newest first, one Head each, for two things: the
// version upload id wrote (own; nil if none), and whether a version was
// written by an upload completed after completed (newer: its
// MetaUploadAt later; T82-10 round 6, AS-31). A key with no versions is
// neither.
func (f *Filesystem) UploadVersions(ctx context.Context, key, id string, since, completed time.Time) (own *objstore.Object, newer bool, err error) {
	if err := CheckUploadID(id); err != nil {
		return nil, false, err
	}
	hist, err := objstore.VersionsSince(ctx, f.store, key, since.Add(-uploadClockSlack))
	if objstore.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, v := range hist {
		obj, err := f.store.HeadVersion(ctx, key, v.VersionID)
		if objstore.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		up := obj.Metadata[MetaUpload]
		if up == id {
			if own == nil {
				own = obj
			}
			continue
		}
		if up == "" || completed.IsZero() {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, obj.Metadata[MetaUploadAt]); err == nil && at.After(completed) {
			newer = true
		}
	}
	return own, newer, nil
}

// EraseKey erases every version of every object under key (a new file's
// content key, its .metadata and .version): EraseUnnamed's work, the
// caller having made sure no name reaches key.
func (f *Filesystem) EraseKey(ctx context.Context, key string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if !strings.HasPrefix(key, f.id+"-") || len(key) < len(f.id)+2 {
		return fmt.Errorf("key %q is not one of filesystem %s's: %w", key, f.id, ErrInvalidArgument)
	}
	objstore.ForgetNew(ctx, key)
	return f.store.EraseAll(ctx, key)
}

// The filesystems of a bucket that have uploads in parts are marked, one
// small object each under one prefix (T82-10 round 6, bound 2.5: the
// sweep lists one prefix, then only those filesystems' uploads, not every
// filesystem's). StartUpload writes the mark before the record, if absent
// (no version a start); the sweep erases it once the filesystem has no
// upload left (UnmarkUploads); DropUploads and Prune erase it.
const uploadMarks = "_uploads/"

func (f *Filesystem) uploadMark() string { return uploadMarks + f.id }

// UnmarkUploads erases the filesystem's mark: its caller listed its
// uploads and found none, and no start of its own is under way.
func (f *Filesystem) UnmarkUploads(ctx context.Context) error {
	return f.store.EraseAll(ctx, f.uploadMark())
}

// A store copy (S3's multipart upload) that could not be ended where it
// was found (an Upstream's removal, whose prune then erases the notes
// that name it) is kept named under _uploads/copies/<key>/<id> (round 6,
// the final review's S2), outside every filesystem, and ended by
// EndKeptCopies at the next start's first sweep.
const keptCopies = uploadMarks + "copies/"

// KeepCopy names copy id into key for EndKeptCopies.
func KeepCopy(ctx context.Context, store objstore.Store, key, id string) error {
	if strings.Contains(key, "/") || strings.Contains(id, "/") || key == "" || id == "" {
		return fmt.Errorf("a kept copy's key %q and id %q: %w", key, id, ErrInvalidArgument)
	}
	_, err := store.Put(ctx, keptCopies+key+"/"+id, strings.NewReader(""), nil, "")
	return err
}

// EndKeptCopies ends every copy KeepCopy named (objstore.AbortCopy), and
// forgets each it ended; one it cannot end stays named, for the next
// call. It returns how many are left.
func EndKeptCopies(ctx context.Context, store objstore.Store) (int, error) {
	var names []string
	if err := objstore.ListEach(ctx, store, keptCopies, func(page []objstore.Object) error {
		for _, o := range page {
			names = append(names, o.Key)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	left := 0
	var errs []error
	for _, n := range names {
		key, id, ok := strings.Cut(strings.TrimPrefix(n, keptCopies), "/")
		if !ok {
			continue
		}
		if err := objstore.AbortCopy(ctx, store, key, id); err != nil {
			left++
			errs = append(errs, err)
			continue
		}
		if err := store.EraseAll(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return left, errors.Join(errs...)
}

// UploadingFilesystems is the names of the store's filesystems that have
// a mark (uploads in parts, or had them until their last sweep): one
// listing of the marks, and the master block for the names.
func UploadingFilesystems(ctx context.Context, store objstore.Store) (map[string]bool, error) {
	ids := map[string]bool{}
	err := objstore.ListEach(ctx, store, uploadMarks, func(page []objstore.Object) error {
		for _, o := range page {
			ids[strings.TrimPrefix(o.Key, uploadMarks)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	mb, _, err := readMaster(ctx, store)
	if err != nil {
		return nil, mapNotFound(err)
	}
	for i, e := range mb.Filesystems {
		if e.Name != "" && ids[fsID(i)] {
			out[e.Name] = true
		}
	}
	return out, nil
}

// uploadClockSlack is how far before since UploadVersion looks: the
// caller's clock against the store's.
const uploadClockSlack = 10 * time.Minute

// UploadVersion is the version of key that upload id wrote (WriteUpload
// records it in the version's metadata), looking at the versions dated
// since (less uploadClockSlack), newest first, one Head each: a write run
// again after it landed finds it here. ErrNotFound if none.
func (f *Filesystem) UploadVersion(ctx context.Context, key, id string, since time.Time) (*objstore.Object, error) {
	if err := CheckUploadID(id); err != nil {
		return nil, err
	}
	// Only back to since (T82-10 round 4): the listing stops there, and
	// holds only the versions since, not the file's whole history.
	hist, err := objstore.VersionsSince(ctx, f.store, key, since.Add(-uploadClockSlack))
	if err != nil {
		return nil, mapNotFound(err)
	}
	for _, v := range hist {
		obj, err := f.store.HeadVersion(ctx, key, v.VersionID)
		if objstore.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if obj.Metadata[MetaUpload] == id {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("upload %s: no version of %s: %w", id, key, ErrNotFound)
}

// An upload's notes are small objects beside its record that its caller
// keeps there, by name (lowercase letters): what a caller that assembles
// the file after the request answers records ("completing", "done",
// "failed"), so a restart or a client asking later finds it.
func uploadNoteKey(dir, note string) string { return dir + "note-" + note }

func checkNote(note string) error {
	if note == "" || len(note) > 32 {
		return fmt.Errorf("upload note %q: 1 to 32 lowercase letters: %w", note, ErrInvalidArgument)
	}
	for _, r := range note {
		if r < 'a' || r > 'z' {
			return fmt.Errorf("upload note %q: lowercase letters: %w", note, ErrInvalidArgument)
		}
	}
	return nil
}

// SetUploadNote writes upload id's note (at most MaxUploadRecord bytes).
func (f *Filesystem) SetUploadNote(ctx context.Context, id, note string, data []byte) error {
	if err := CheckUploadID(id); err != nil {
		return err
	}
	if err := checkNote(note); err != nil {
		return err
	}
	if len(data) > MaxUploadRecord {
		return fmt.Errorf("upload note of %d bytes, over %d: %w", len(data), MaxUploadRecord, ErrInvalidArgument)
	}
	_, err := f.store.Put(ctx, uploadNoteKey(f.uploadDir(id), note), strings.NewReader(string(data)), nil, "")
	return err
}

// SetUploadNoteIf is SetUploadNote conditional on the note's current
// version: ifMatch an etag UploadNoteTag gave (the note is still that), or
// "" (the note does not exist yet). A lost race is an error
// objstore.IsPreconditionFailed recognizes (ErrUploadTaken). It returns
// the etag written.
func (f *Filesystem) SetUploadNoteIf(ctx context.Context, id, note string, data []byte, ifMatch string) (string, error) {
	if err := CheckUploadID(id); err != nil {
		return "", err
	}
	if err := checkNote(note); err != nil {
		return "", err
	}
	if len(data) > MaxUploadRecord {
		return "", fmt.Errorf("upload note of %d bytes, over %d: %w", len(data), MaxUploadRecord, ErrInvalidArgument)
	}
	key := uploadNoteKey(f.uploadDir(id), note)
	var obj *objstore.Object
	var err error
	if ifMatch == "" {
		obj, err = f.store.PutIfAbsent(ctx, key, strings.NewReader(string(data)), nil)
	} else {
		obj, err = f.store.Put(ctx, key, strings.NewReader(string(data)), nil, ifMatch)
	}
	if objstore.IsPreconditionFailed(err) {
		return "", fmt.Errorf("upload %s note %s: %w (%w)", id, note, ErrUploadTaken, err)
	}
	if err != nil {
		return "", err
	}
	return obj.ETag, nil
}

// ErrUploadTaken is a conditional note write that lost its race.
var ErrUploadTaken = errors.New("the upload's note was changed by another writer")

// UploadNoteTag is UploadNote with the note's etag, for SetUploadNoteIf.
func (f *Filesystem) UploadNoteTag(ctx context.Context, id, note string) ([]byte, string, time.Time, error) {
	if err := CheckUploadID(id); err != nil {
		return nil, "", time.Time{}, err
	}
	if err := checkNote(note); err != nil {
		return nil, "", time.Time{}, err
	}
	body, obj, err := f.store.Get(ctx, uploadNoteKey(f.uploadDir(id), note))
	if err != nil {
		return nil, "", time.Time{}, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, MaxUploadRecord+1))
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return data, obj.ETag, obj.LastModified, nil
}

// UploadNote is upload id's note and when it was written; ErrNotFound if
// it has none.
func (f *Filesystem) UploadNote(ctx context.Context, id, note string) ([]byte, time.Time, error) {
	if err := CheckUploadID(id); err != nil {
		return nil, time.Time{}, err
	}
	if err := checkNote(note); err != nil {
		return nil, time.Time{}, err
	}
	body, obj, err := f.store.Get(ctx, uploadNoteKey(f.uploadDir(id), note))
	if err != nil {
		return nil, time.Time{}, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, MaxUploadRecord+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	return data, obj.LastModified, nil
}

// CheckUploadParts checks, from a listing (a page of 1,000 keys a
// request, not a request a part), that upload id has exactly parts 1 to
// count, each but the last UploadPartSize, and returns their size and the
// file's checksums (the last part's state: one Head).
func (f *Filesystem) CheckUploadParts(ctx context.Context, id string, count int) (int64, Checksums, error) {
	if err := CheckUploadID(id); err != nil {
		return 0, Checksums{}, err
	}
	if count < 1 || count > MaxUploadParts {
		return 0, Checksums{}, fmt.Errorf("%d parts: 1 to %d: %w", count, MaxUploadParts, ErrUploadParts)
	}
	dir := f.uploadDir(id)
	var size int64
	seen, last := 0, 0
	var bad error
	err := objstore.ListEach(ctx, f.store, dir+"p", func(page []objstore.Object) error {
		for _, o := range page {
			n, err := strconv.Atoi(strings.TrimPrefix(o.Key, dir+"p"))
			if err != nil {
				continue
			}
			seen++
			last = max(last, n)
			size += o.Size
			if bad == nil && n < count && o.Size != UploadPartSize {
				bad = fmt.Errorf("upload %s: part %d is %d bytes, not %d: %w", id, n, o.Size, UploadPartSize, ErrUploadParts)
			}
		}
		return nil
	})
	if err != nil {
		return 0, Checksums{}, err
	}
	switch {
	case bad != nil:
		return 0, Checksums{}, bad
	case seen != count || last != count:
		return 0, Checksums{}, fmt.Errorf("upload %s has %d parts, the last numbered %d; complete names %d: %w", id, seen, last, count, ErrUploadParts)
	case size > f.maxFileSize:
		return 0, Checksums{}, ErrFileTooLarge
	}
	o, err := f.store.Head(ctx, uploadPartKey(dir, count))
	if err != nil {
		return 0, Checksums{}, mapNotFound(err)
	}
	sum := NewChecksummer()
	if err := sum.restore(o.Metadata); err != nil {
		return 0, Checksums{}, fmt.Errorf("upload %s part %d: %w", id, count, err)
	}
	return size, sum.Sum(), nil
}

// DropUploadParts erases upload id's parts, every version, and keeps its
// record and notes: what a finished upload no longer needs.
func (f *Filesystem) DropUploadParts(ctx context.Context, id string) error {
	if err := CheckUploadID(id); err != nil {
		return err
	}
	return f.store.EraseAll(ctx, f.uploadDir(id)+"p")
}

// DropUploads erases every upload of the filesystem, every version: what
// the filesystem's end (a fork deleted) leaves of them, as Prune does.
func (f *Filesystem) DropUploads(ctx context.Context) error {
	if err := f.store.EraseAll(ctx, f.uploadPrefix()); err != nil {
		return err
	}
	return f.UnmarkUploads(ctx)
}

// DropUpload erases upload id: its record and parts, every version (the
// bucket keeps versions: a delete would leave them). The parts first, the
// notes next, the record last (T82-10 round 6, UP-16): EraseAll promises
// no order, and an erase that fails partway must leave the record, so
// that the upload is still there to be erased again (an abort sent again,
// or its expiry), and a part sent meanwhile finds its part before it gone
// (refused, ErrUploadOrder).
func (f *Filesystem) DropUpload(ctx context.Context, id string) error {
	if err := CheckUploadID(id); err != nil {
		return err
	}
	dir := f.uploadDir(id)
	for _, p := range []string{dir + "p", dir + "note-", dir} {
		if err := f.store.EraseAll(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// UploadEntry is one upload ListUploads found: whether it has its record,
// and how many parts.
type UploadEntry struct {
	ID     string
	Record bool
	Notes  []string // the names of its notes, in order
	// NoteTimes are the notes' times as the listing gives them (the
	// store's LastModified; zero where the store's listing has none).
	NoteTimes map[string]time.Time
	Parts     int
}

// ListUploads calls fn with each upload of the filesystem, in id order,
// listing a page at a time: it holds one page and one entry, never all.
func (f *Filesystem) ListUploads(ctx context.Context, fn func(UploadEntry) error) error {
	prefix := f.uploadPrefix()
	var cur *UploadEntry
	err := objstore.ListEach(ctx, f.store, prefix, func(page []objstore.Object) error {
		for _, o := range page {
			id, rest, ok := strings.Cut(strings.TrimPrefix(o.Key, prefix), "/")
			if !ok {
				continue
			}
			if cur != nil && cur.ID != id {
				if err := fn(*cur); err != nil {
					return err
				}
				cur = nil
			}
			if cur == nil {
				cur = &UploadEntry{ID: id}
			}
			if rest == "record" {
				cur.Record = true
			} else if note, ok := strings.CutPrefix(rest, "note-"); ok {
				cur.Notes = append(cur.Notes, note)
				if cur.NoteTimes == nil {
					cur.NoteTimes = map[string]time.Time{}
				}
				cur.NoteTimes[note] = o.LastModified
			} else if strings.HasPrefix(rest, "p") {
				cur.Parts++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if cur != nil {
		return fn(*cur)
	}
	return nil
}

// state is h's hashes' state as a part's metadata.
func (h *Checksummer) state() (map[string]string, error) {
	md := map[string]string{}
	for name, x := range map[string]any{mdMD5State: h.md5, mdSHA256State: h.sha256} {
		b, err := x.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return nil, err
		}
		md[name] = base64.StdEncoding.EncodeToString(b)
	}
	return md, nil
}

// restore sets h's hashes to the state a part's metadata holds.
func (h *Checksummer) restore(md map[string]string) error {
	for name, x := range map[string]any{mdMD5State: h.md5, mdSHA256State: h.sha256} {
		b, err := base64.StdEncoding.DecodeString(md[name])
		if err != nil || len(b) == 0 {
			return fmt.Errorf("no checksum state %s: %w", name, ErrUploadParts)
		}
		if err := x.(encoding.BinaryUnmarshaler).UnmarshalBinary(b); err != nil {
			return fmt.Errorf("checksum state %s: %v: %w", name, err, ErrUploadParts)
		}
	}
	return nil
}
