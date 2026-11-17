package drive

import "context"

// KeyVersioner is a drive that can read a file's versions by its key
// alone, for a caller that has the file's Item in hand from a Walk or a
// List of this drive (a Mirror's RebuildHashes): it doesn't find the
// file by its path again. What it gives is what Versions gives for
// ItemRef{ID: it.ID, Path: it.Path}, newest first, except that a
// version's Names are left out (they cost a read of the drive's names
// a call). ErrNotFound if the file is gone, ErrIsDir if it isn't a
// file. Permissions are Versions': only a profile that may read
// anything skips finding the file; any other is checked as Versions
// checks it.
type KeyVersioner interface {
	VersionsOf(ctx context.Context, it Item) ([]Version, error)
}
