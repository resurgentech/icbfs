package icbfs

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"hash"
	"io"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// Checksums are a content version's MD5 and SHA-256, kept in the item's
// .metadata by the version's ETag: computed while a write's bytes go by
// (a whole file written at once), or later, on demand (VersionChecksums).
// Writing them isn't a change to the item: its ctime and its history
// don't move. Either is nil if not computed.
type Checksums struct {
	MD5, SHA256 []byte
}

// Complete reports whether both are there.
func (c Checksums) Complete() bool { return len(c.MD5) > 0 && len(c.SHA256) > 0 }

// ContentChecksums are a file's current content's, if they've been
// computed.
func (a Attr) ContentChecksums() (Checksums, bool) {
	c, ok := a.Checksums[a.contentETag]
	return c, ok && a.contentETag != ""
}

// ContentETag is a file's current content ETag, as Stat read it.
func (a Attr) ContentETag() string { return a.contentETag }

// ContentVersion is a file's current content version as Stat read it
// (its version id, ETag, size and when the store wrote it), so a caller
// that wants the version needn't ask the store again. ok is false for
// attributes Stat didn't read.
func (a Attr) ContentVersion() (objstore.Object, bool) {
	return a.content, a.content.VersionID != ""
}

// Dated reports whether the item has a .version record, which dates some
// of its versions otherwise than the store does (objstore.Dates): set by
// any write of its .metadata made under a declared time, and kept.
func (a Attr) Dated() bool { return a.dated }

// Checksummer computes Checksums of what's written to it.
type Checksummer struct {
	md5, sha256 hash.Hash
}

// NewChecksummer is a Checksummer of nothing yet.
func NewChecksummer() *Checksummer {
	return &Checksummer{md5: md5.New(), sha256: sha256.New()}
}

func (h *Checksummer) Write(p []byte) (int, error) {
	h.md5.Write(p)
	h.sha256.Write(p)
	return len(p), nil
}

// Sum is the checksums of everything written.
func (h *Checksummer) Sum() Checksums {
	return Checksums{MD5: h.md5.Sum(nil), SHA256: h.sha256.Sum(nil)}
}

// ChecksumsOf are r's checksums.
func ChecksumsOf(r io.Reader) (Checksums, error) {
	h := NewChecksummer()
	if _, err := io.Copy(h, r); err != nil {
		return Checksums{}, err
	}
	return h.Sum(), nil
}

// SetChecksums records c as the checksums of key's content version etag,
// adding to what's there (a part c lacks stays as it was). It isn't a
// status change: ctime stays.
func (f *Filesystem) SetChecksums(ctx context.Context, key, etag string, c Checksums) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	return f.writeMetadataIf(ctx, key, false, func(a *Attr) bool {
		old := a.Checksums[etag]
		merged := old
		if len(c.MD5) > 0 {
			merged.MD5 = c.MD5
		}
		if len(c.SHA256) > 0 {
			merged.SHA256 = c.SHA256
		}
		if bytes.Equal(merged.MD5, old.MD5) && bytes.Equal(merged.SHA256, old.SHA256) {
			return false
		}
		if a.Checksums == nil {
			a.Checksums = map[string]Checksums{}
		}
		a.Checksums[etag] = merged
		return true
	})
}

// VersionChecksums are the checksums of key's content version versionID
// ("" for the current one). Missing, they're computed, reading the version
// once, and recorded (on a writable filesystem), so the next ask reads
// only .metadata.
func (f *Filesystem) VersionChecksums(ctx context.Context, key, versionID string) (Checksums, error) {
	var obj *objstore.Object
	var err error
	if versionID == "" {
		obj, err = f.store.Head(ctx, key)
	} else {
		obj, err = f.store.HeadVersion(ctx, key, versionID)
	}
	if err != nil {
		return Checksums{}, mapNotFound(err)
	}
	body, _, err := f.store.Get(objstore.CacheContent(ctx), metadataObjectKey(key))
	if err != nil {
		return Checksums{}, mapNotFound(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return Checksums{}, err
	}
	attr, err := decodeFileMetadata(data)
	if err != nil {
		return Checksums{}, err
	}
	if c := attr.Checksums[obj.ETag]; c.Complete() {
		return c, nil
	}
	content, _, err := f.store.GetVersion(ctx, key, obj.VersionID)
	if err != nil {
		return Checksums{}, mapNotFound(err)
	}
	c, err := ChecksumsOf(content)
	content.Close()
	if err != nil {
		return Checksums{}, err
	}
	if !f.readOnly && !f.archived {
		// Best effort: unrecorded, they're only computed again next time.
		_ = f.SetChecksums(ctx, key, obj.ETag, c)
	}
	return c, nil
}

func checksumsToWire(m map[string]Checksums) map[string]*pb.Checksums {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*pb.Checksums, len(m))
	for etag, c := range m {
		out[etag] = &pb.Checksums{Md5: c.MD5, Sha256: c.SHA256}
	}
	return out
}

func checksumsFromWire(m map[string]*pb.Checksums) map[string]Checksums {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]Checksums, len(m))
	for etag, c := range m {
		out[etag] = Checksums{MD5: c.GetMd5(), SHA256: c.GetSha256()}
	}
	return out
}
