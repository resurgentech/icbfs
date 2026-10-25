package icbfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// OriginRecord is what the filesystem holds for one source item it was
// copied from (a document in another store, say): which item here it
// became, and what of it was last copied. One small object per source
// item, <id>-origin-<hash of source drive and id>, so a sync daemon finds
// its item for a source id in one read, with no listing of the tree.
//
// Item ids never change, not on a move, a delete to the trash or a
// restore, so a record only changes when an item's origin is set; where
// the item is now is looked up from ItemID (Path is a hint, checked
// before it's trusted).
type OriginRecord struct {
	Drive   string `json:"drive"`   // the source drive
	ID      string `json:"id"`      // the source item's id there
	ItemID  string `json:"itemId"`  // the item here (its key)
	Version string `json:"version"` // the source version last copied
	Path    string `json:"path"`    // where the item was last known to be, filesystem-relative
}

func (f *Filesystem) originKey(sourceDrive, sourceID string) string {
	sum := sha256.Sum256([]byte(sourceDrive + "\x00" + sourceID))
	return f.id + "-origin-" + hex.EncodeToString(sum[:])
}

// OriginRecord returns the record for the source item, with its ETag for
// a conditional update, or ErrNotFound.
func (f *Filesystem) OriginRecord(ctx context.Context, sourceDrive, sourceID string) (OriginRecord, string, error) {
	body, obj, err := f.store.Get(ctx, f.originKey(sourceDrive, sourceID))
	if err != nil {
		return OriginRecord{}, "", mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return OriginRecord{}, "", err
	}
	r, err := decodeOriginRecord(data)
	if err != nil {
		return OriginRecord{}, "", err
	}
	return r, obj.ETag, nil
}

// PutOriginRecord writes r: only if there is none yet when ifMatch is
// empty, else only over the version with that ETag. Either way a lost
// race is ErrExists, so two writers can't both claim one source item.
func (f *Filesystem) PutOriginRecord(ctx context.Context, r OriginRecord, ifMatch string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	data, err := encodeOriginRecord(r)
	if err != nil {
		return err
	}
	key := f.originKey(r.Drive, r.ID)
	if ifMatch == "" {
		_, err = f.store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil)
	} else {
		_, err = f.store.Put(ctx, key, bytes.NewReader(data), nil, ifMatch)
	}
	if objstore.IsPreconditionFailed(err) {
		return ErrExists
	}
	return err
}

// DeleteOriginRecord forgets a source item, as when the item it named is
// purged.
func (f *Filesystem) DeleteOriginRecord(ctx context.Context, sourceDrive, sourceID string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	err := f.store.Delete(ctx, f.originKey(sourceDrive, sourceID))
	if objstore.IsNotFound(err) {
		return nil
	}
	return err
}
