package objstore

import "context"

// CopyConcurrency bounds how many parts a PutParts copies on the store's
// side at once (T82-10: an upload in parts is completed by copying each of
// its staged 5 MiB parts). A copy moves no bytes through the caller; each
// is one request the store answers when it has copied the range, so their
// time adds up one after another unless they overlap. 8 at once: an
// upload's complete of 2,000 parts at the large-Upstream gate's 100 ms a
// request takes about 25 s of copy requests rather than 200 s; and the
// four uploads one caller may have in flight complete with at most 32
// copies at once, the store client's idle connections per host
// (storeguard's MaxIdleConnsPerHost), so connections are reused rather
// than made and dropped.
const CopyConcurrency = 8

// A copy whose pieces outlive a process killed in it (T82-10 round 6,
// AS-14). On S3 a PutParts is a multipart upload: CreateMultipartUpload
// answers an UploadId, and the pieces copied under it stay, invisible and
// billed, until CompleteMultipartUpload or AbortMultipartUpload names it.
// A caller that must be able to end it after a kill learns the id before
// the first piece (WithCopyStarted) and keeps it; whoever finds it later
// ends it (AbortCopy). Azure has no such id: uncommitted blocks go at the
// blob's next commit, or after 7 days.

type copyStartedKey struct{}

// WithCopyStarted is ctx with fn, which a store's PutParts calls with the
// id of the copy it made (S3's UploadId) before it writes the first part;
// an error from fn ends the PutParts, its copy aborted, with that error.
// A store with no such id never calls it.
func WithCopyStarted(ctx context.Context, fn func(id string) error) context.Context {
	return context.WithValue(ctx, copyStartedKey{}, fn)
}

// CopyStarted calls ctx's WithCopyStarted function with id, if it has one.
func CopyStarted(ctx context.Context, id string) error {
	if fn, ok := ctx.Value(copyStartedKey{}).(func(string) error); ok {
		return fn(id)
	}
	return nil
}

// CopyAborter is a Store whose PutParts makes a copy a kill can leave
// (S3Store). A wrapper of a Store passes it through when it says so.
type CopyAborter interface {
	// AbortCopy ends copy id into key; an id the store no longer knows
	// (completed or aborted already) is not an error.
	AbortCopy(ctx context.Context, key, id string) error
}

// AbortCopy is s's AbortCopy, or nothing for a store without one.
func AbortCopy(ctx context.Context, s Store, key, id string) error {
	if a, ok := s.(CopyAborter); ok && id != "" {
		return a.AbortCopy(ctx, key, id)
	}
	return nil
}
