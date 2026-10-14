package objstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

// ErrReadOnly is what every write to an AsOf view returns.
var ErrReadOnly = errors.New("read-only: a view of the store as of a past time")

// AsOf is a read-only view of a Store as it was at one moment: Get and
// Head return the newest version of each key last modified at or before
// that time (not found if that was a delete, or if the key didn't exist
// yet), ListByPrefix lists what existed then, and every write fails with
// ErrReadOnly. icbfs relies on the bucket's versioning for this: every
// change to every object is a new version with a store-assigned time.
//
// Resolving a key means listing its versions, so each answer is kept in
// a local SQLite database. The past doesn't change, so an answer never
// goes stale; the database lives in a fresh directory that Close
// removes (and that the next NewAsOf removes, if its process was killed
// before it could).
type AsOf struct {
	store Store
	at    time.Time
	dir   string
	db    *sql.DB
}

var _ Store = (*AsOf)(nil)

// asOfClockSlack is how far ahead of the store's clock an as-of time
// may be and still be waited for.
const asOfClockSlack = 2 * time.Second

// NewAsOf returns store as it was at at, refusing a time after the
// store's own current time (whose view would still be changing), unless
// it's within asOfClockSlack, which it waits out. Its cache goes in a
// new directory under cacheParent ("" for the system temp directory).
func NewAsOf(ctx context.Context, store Store, at time.Time, cacheParent string) (*AsOf, error) {
	// The store's clock reads in whole seconds (an HTTP Date), so a time
	// just made — a version name made now — can look up to a second
	// ahead: wait that out rather than refuse it.
	deadline := time.Now().Add(asOfClockSlack)
	for {
		now, err := store.ServerTime(ctx)
		if err != nil {
			return nil, fmt.Errorf("as-of: read the store's clock: %w", err)
		}
		if !at.After(now) {
			break
		}
		if at.Sub(now) > asOfClockSlack || time.Now().After(deadline) {
			return nil, fmt.Errorf("as-of %s is after the object store's current time %s", at.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	removeStaleAsOfCaches(cacheParent)
	dir, err := os.MkdirTemp(cacheParent, asOfCachePrefix)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "versions.db"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// One connection serializes access, which is all a lookup cache
	// needs and keeps SQLite from ever reporting itself busy.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE resolved (
		key      TEXT PRIMARY KEY,
		present  INTEGER NOT NULL,
		version  TEXT NOT NULL,
		etag     TEXT NOT NULL,
		size     INTEGER NOT NULL,
		modified INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &AsOf{store: store, at: at, dir: dir, db: db}, nil
}

const asOfCachePrefix = "icbfs-asof-"

// removeStaleAsOfCaches removes version caches left behind under parent
// by as-of mounts that ended without Close (killed outright): each
// cache directory records its owner's pid, and one whose owner is gone
// is deleted.
func removeStaleAsOfCaches(parent string) {
	if parent == "" {
		parent = os.TempDir()
	}
	dirs, _ := filepath.Glob(filepath.Join(parent, asOfCachePrefix+"*"))
	for _, d := range dirs {
		data, err := os.ReadFile(filepath.Join(d, "pid"))
		if err != nil {
			continue // not ours to judge, or still being created
		}
		if pid, err := strconv.Atoi(string(data)); err == nil && pid != os.Getpid() && !processAlive(pid) {
			_ = os.RemoveAll(d)
		}
	}
}

// At is the moment this view shows.
func (a *AsOf) At() time.Time { return a.at }

// Close removes the version cache.
func (a *AsOf) Close() error {
	err := a.db.Close()
	if rmErr := os.RemoveAll(a.dir); err == nil {
		err = rmErr
	}
	return err
}

// asOfVersion picks, from one key's history (newest first), the version
// current at at: ok is false if the key didn't exist then.
func asOfVersion(history []Object, at time.Time) (Object, bool) {
	for _, v := range history {
		if !v.LastModified.After(at) {
			return v, !v.DeleteMarker
		}
	}
	return Object{}, false
}

func (a *AsOf) remember(ctx context.Context, key string, v Object, present bool) error {
	_, err := a.db.ExecContext(ctx, `INSERT OR REPLACE INTO resolved VALUES (?, ?, ?, ?, ?, ?)`,
		key, present, v.VersionID, v.ETag, v.Size, v.LastModified.UnixNano())
	return err
}

// resolve returns the version of key current at a.at.
func (a *AsOf) resolve(ctx context.Context, key string) (Object, bool, error) {
	var present bool
	var v Object
	var modified int64
	err := a.db.QueryRowContext(ctx, `SELECT present, version, etag, size, modified FROM resolved WHERE key = ?`, key).
		Scan(&present, &v.VersionID, &v.ETag, &v.Size, &modified)
	if err == nil {
		v.Key, v.LastModified = key, time.Unix(0, modified).UTC()
		return v, present, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Object{}, false, err
	}
	history, err := a.store.ListVersions(ctx, key)
	if err != nil {
		return Object{}, false, err
	}
	v, present = asOfVersion(history, a.at)
	if err := a.remember(ctx, key, v, present); err != nil {
		return Object{}, false, err
	}
	return v, present, nil
}

// notAt is the not-found error (satisfying IsNotFound) for a key that
// didn't exist at a.at.
func (a *AsOf) notAt(key string) error {
	return fmt.Errorf("%s as of %s: %w", key, a.at.Format(time.RFC3339), &types.NoSuchKey{Message: aws.String("did not exist at that time")})
}

func (a *AsOf) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	v, ok, err := a.resolve(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, a.notAt(key)
	}
	return a.store.GetVersion(ctx, key, v.VersionID)
}

func (a *AsOf) Head(ctx context.Context, key string) (*Object, error) {
	v, ok, err := a.resolve(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, a.notAt(key)
	}
	return a.store.HeadVersion(ctx, key, v.VersionID)
}

func (a *AsOf) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error) {
	return a.store.GetVersion(ctx, key, versionID)
}

func (a *AsOf) HeadVersion(ctx context.Context, key, versionID string) (*Object, error) {
	return a.store.HeadVersion(ctx, key, versionID)
}

// ListVersions lists key's versions up to a.at.
func (a *AsOf) ListVersions(ctx context.Context, key string) ([]Object, error) {
	all, err := a.store.ListVersions(ctx, key)
	if err != nil {
		return nil, err
	}
	return upTo(all, a.at), nil
}

// ListAllVersions lists the versions under prefix up to a.at.
func (a *AsOf) ListAllVersions(ctx context.Context, prefix string) ([]Object, error) {
	all, err := a.store.ListAllVersions(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return upTo(all, a.at), nil
}

func upTo(versions []Object, at time.Time) []Object {
	var out []Object
	for _, v := range versions {
		if !v.LastModified.After(at) {
			out = append(out, v)
		}
	}
	return out
}

// ListByPrefix lists every key under prefix that existed at a.at, with
// its size then — every version under the prefix has to be listed for
// that, so it's as slow as the prefix's whole history is long. The
// answers go into the version cache too.
func (a *AsOf) ListByPrefix(ctx context.Context, prefix string) ([]Object, error) {
	all, err := a.store.ListAllVersions(ctx, prefix)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO resolved VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insert.Close()

	var out []Object
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].Key == all[i].Key {
			j++
		}
		v, ok := asOfVersion(all[i:j], a.at)
		if ok {
			out = append(out, Object{Key: v.Key, Size: v.Size})
		}
		if _, err := insert.ExecContext(ctx, all[i].Key, ok, v.VersionID, v.ETag, v.Size, v.LastModified.UnixNano()); err != nil {
			return nil, err
		}
		i = j
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *AsOf) ServerTime(ctx context.Context) (time.Time, error) {
	return a.store.ServerTime(ctx)
}

func (a *AsOf) Put(context.Context, string, io.Reader, map[string]string, string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) PutIfAbsent(context.Context, string, io.Reader, map[string]string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) PutParts(context.Context, string, []Part, map[string]string, string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) UpdateMetadata(context.Context, string, map[string]string, string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) Delete(context.Context, string, string) error {
	return ErrReadOnly
}

func (a *AsOf) DeleteVersion(context.Context, string, string) error {
	return ErrReadOnly
}
