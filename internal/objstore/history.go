package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

// DefaultHistoryTTL is how long a HistoryCache keeps what it fetched.
const DefaultHistoryTTL = 30 * 24 * time.Hour

// HistoryCache keeps keys' histories, as the store lists them, in a local
// SQLite database that AsOf views of any time answer from: moving from
// one time to another needs nothing more from the store. It keeps what
// the versions themselves say too, which never changes: each one's
// metadata (HeadVersion), and the content of reads the caller marks as
// metadata (CacheContent: directory blocks, never file data).
//
// The past doesn't change, except that Erase (a purge) can remove
// entries. A view that finds a cached version gone forgets the key and
// asks again; the filesystem layer drops what a purge erased (ForgetUnder,
// on a mark the purge leaves); and everything is fetched again once it's
// older than the cache's TTL, so nothing lingers in it longer than that.
//
// Several processes can share one database (WAL), a mount as of some
// time and a program browsing history, say, and several handles in one
// process share its one connection (historyFile). Writers take turns in
// order; no operation waits longer than HistoryWait, and past it the
// cache answers as a miss, never "database is locked" (historywait.go).
type HistoryCache struct {
	store Store
	f     *historyFile
	db    *sql.DB // f's
	ttl   time.Duration
	temp  string        // the directory Close removes, for OpenTempHistoryCache's
	wait  time.Duration // HistoryWait; shorter in tests

	misses *atomic.Int64 // ContendedMisses; shared with UpTo's

	mu       sync.Mutex
	offset   time.Duration // the store's clock minus ours
	offsetAt time.Time     // when offset was measured; zero for never

	// upTo, if set: histories leave out what was stored after a version
	// (UpTo).
	upTo map[string]string
}

// historyLayout is the database's layout (PRAGMA user_version). A cache
// of another layout, older or newer, is dropped and started again.
const historyLayout = 4

// maxCachedContent is the largest content a HistoryCache keeps: a
// directory block or a .metadata object is far smaller.
const maxCachedContent = 1 << 20

// historyClockCheck is how often the store's clock is read again.
const historyClockCheck = 10 * time.Minute

// DefaultHistoryCacheDir is where history caches go by default: icbfs's
// directory in the user's cache directory, or the temp directory if
// there's none.
func DefaultHistoryCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "icbfs", "history")
}

// HistoryCachePath is the database for the store storeID names (a
// storeconfig.Config's ID) in dir.
func HistoryCachePath(dir, storeID string) string {
	sum := sha256.Sum256([]byte(storeID))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".db")
}

// OpenHistoryCache opens, or creates, store's history cache at path,
// keeping what it fetches for ttl, and drops whatever is older. Run as
// root, it refuses a path in another user's directory (with sudo -E, a
// root mount would otherwise make one in that user's ~/.cache).
func OpenHistoryCache(ctx context.Context, store Store, path string, ttl time.Duration) (*HistoryCache, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("history cache: TTL %v isn't positive", ttl)
	}
	if err := notOthersHere(path); err != nil {
		return nil, fmt.Errorf("history cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, first, err := openHistoryFile(path)
	if err != nil {
		return nil, err
	}
	c := &HistoryCache{store: store, f: f, db: f.db, ttl: ttl, wait: HistoryWait, misses: new(atomic.Int64)}
	if !first {
		select {
		case <-f.ready:
		case <-ctx.Done():
			f.release()
			return nil, ctx.Err()
		}
		if f.initErr != nil {
			f.release()
			return nil, f.initErr
		}
		return c, nil
	}
	err = c.openFirst(ctx, path)
	f.initDone(err)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// openFirst inits the database for the first handle of this process.
// Opening it can find it busy for longer than one wait (another process
// switching it to WAL, or creating it): try again for a while.
func (c *HistoryCache) openFirst(ctx context.Context, path string) error {
	deadline := time.Now().Add(2 * HistoryWait)
	for {
		done, err := c.write(ctx, c.init)
		switch {
		case err != nil:
			c.f.release()
			return fmt.Errorf("history cache %s: %w", path, err)
		case done:
			return nil
		case time.Now().After(deadline):
			c.f.release()
			return fmt.Errorf("history cache %s: database is locked: no turn in %v", path, 2*HistoryWait)
		}
		select {
		case <-ctx.Done():
			c.f.release()
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// isBusy reports whether err is SQLite's "database is locked" (SQLITE_BUSY).
func isBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 5
}

// OpenTempHistoryCache opens a history cache that lasts until Close, in a
// new directory under parent ("" for the temp directory) that Close
// removes, as does the next OpenTempHistoryCache if this process was
// killed first.
func OpenTempHistoryCache(ctx context.Context, store Store, parent string) (*HistoryCache, error) {
	removeStaleTempCaches(parent)
	dir, err := os.MkdirTemp(parent, tempCachePrefix)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c, err := OpenHistoryCache(ctx, store, filepath.Join(dir, "history.db"), DefaultHistoryTTL)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c.temp = dir
	return c, nil
}

const tempCachePrefix = "icbfs-asof-"

// removeStaleTempCaches removes temporary caches left behind under
// parent by processes that ended without Close (killed outright): each
// cache directory records its owner's pid, and one whose owner is gone
// is deleted.
func removeStaleTempCaches(parent string) {
	if parent == "" {
		parent = os.TempDir()
	}
	dirs, _ := filepath.Glob(filepath.Join(parent, tempCachePrefix+"*"))
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

// Store is the store this cache is of.
func (c *HistoryCache) Store() Store { return c.store }

// Close closes the database (when no other handle of this process has it
// open), and removes a temporary cache. A forget still pending is tried
// once more first, within HistoryWait.
func (c *HistoryCache) Close() error {
	if len(c.f.pendingNow()) > 0 {
		_, _ = c.write(context.Background(), func(context.Context, *sql.Tx) error { return nil })
	}
	err := c.f.release()
	if c.temp != "" {
		if rmErr := os.RemoveAll(c.temp); err == nil {
			err = rmErr
		}
	}
	return err
}

// The database: versions holds keys' histories as the store listed them
// (pos is the order it listed them in), and covered says what has been
// fetched: every key starting with prefix (whole = 0), or the one key
// prefix (whole = 1), complete for every time up to upto. blobs holds
// versions' metadata, and content where it was asked for (else NULL);
// notes, values the caller keeps with the cache (ForgetUnder's marks).
// fetched times, by this machine's clock, are what the TTL is counted
// from.
func (c *HistoryCache) init(ctx context.Context, tx *sql.Tx) error {
	var layout int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&layout); err != nil {
		return err
	}
	if layout != historyLayout {
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS versions`,
			`DROP TABLE IF EXISTS covered`,
			`DROP TABLE IF EXISTS blobs`,
			`DROP TABLE IF EXISTS notes`,
			`CREATE TABLE versions (
				key      TEXT NOT NULL,
				pos      INTEGER NOT NULL,
				version  TEXT NOT NULL,
				etag     TEXT NOT NULL,
				size     INTEGER NOT NULL,
				modified INTEGER NOT NULL,
				deleted  INTEGER NOT NULL,
				fetched  INTEGER NOT NULL,
				PRIMARY KEY (key, pos)
			)`,
			`CREATE TABLE covered (
				prefix  TEXT NOT NULL,
				whole   INTEGER NOT NULL,
				upto    INTEGER NOT NULL,
				fetched INTEGER NOT NULL,
				PRIMARY KEY (prefix, whole)
			)`,
			`CREATE TABLE blobs (
				key      TEXT NOT NULL,
				version  TEXT NOT NULL,
				object   TEXT NOT NULL,
				content  BLOB,
				fetched  INTEGER NOT NULL,
				PRIMARY KEY (key, version)
			)`,
			`CREATE TABLE notes (
				name  TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,
			fmt.Sprintf(`PRAGMA user_version = %d`, historyLayout),
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	cutoff := time.Now().Add(-c.ttl).UnixNano()
	for _, table := range []string{"versions", "covered", "blobs"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE fetched < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// StoreNow is the store's clock now, from an offset measured every
// historyClockCheck rather than a request each time.
func (c *HistoryCache) StoreNow(ctx context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.offsetAt.IsZero() || time.Since(c.offsetAt) > historyClockCheck {
		server, err := c.store.ServerTime(ctx)
		if err != nil {
			return time.Time{}, err
		}
		// The store's clock reads in whole seconds: it's half a second
		// later than it says, on average.
		c.offset = server.Add(500 * time.Millisecond).Sub(time.Now())
		c.offsetAt = time.Now()
	}
	return time.Now().Add(c.offset), nil
}

// upto is how far a history fetched now is complete for a view at at:
// to a little before the store's clock (which is only known to within a
// second or so), or to at itself, which a view never exceeds the store's
// clock with.
func (c *HistoryCache) upto(ctx context.Context, at time.Time) (time.Time, error) {
	now, err := c.StoreNow(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if safe := now.Add(-2 * time.Second); safe.After(at) {
		return safe, nil
	}
	return at, nil
}

// history is key's history, newest first, complete at least up to at
// (without what was stored after a version, for a cache UpTo).
func (c *HistoryCache) history(ctx context.Context, key string, at time.Time) ([]Object, error) {
	list, err := c.fullHistory(ctx, key, at)
	if err != nil || c.upTo == nil || strings.HasSuffix(key, VersionSuffix) {
		return list, err
	}
	return c.dropAfter(key, list), nil
}

func (c *HistoryCache) fullHistory(ctx context.Context, key string, at time.Time) ([]Object, error) {
	kept := func() ([]Object, bool, error) {
		if c.f.pendingOn(key, false) {
			return nil, false, nil
		}
		return c.cached(ctx, `(whole = 1 AND prefix = ?1) OR (whole = 0 AND substr(?1, 1, length(prefix)) = prefix)`, key, at,
			`SELECT key, version, etag, size, modified, deleted FROM versions WHERE key = ? ORDER BY pos`, key)
	}
	return c.fetchOnce(ctx, "key "+key, at, kept, func(ctx context.Context) ([]Object, error) { return c.store.History(ctx, key) },
		func(list []Object, upto time.Time) error { return c.save(ctx, key, true, list, upto) })
}

// historyUnder is the history of every key starting with prefix, keys in
// order and each newest first, complete at least up to at.
func (c *HistoryCache) historyUnder(ctx context.Context, prefix string, at time.Time) ([]Object, error) {
	kept := func() ([]Object, bool, error) {
		if c.f.pendingOn(prefix, true) {
			return nil, false, nil
		}
		where, args := underPrefix(prefix)
		return c.cached(ctx, `whole = 0 AND substr(?1, 1, length(prefix)) = prefix`, prefix, at,
			`SELECT key, version, etag, size, modified, deleted FROM versions WHERE `+where+` ORDER BY key, pos`, args...)
	}
	return c.fetchOnce(ctx, "under "+prefix, at, kept, func(ctx context.Context) ([]Object, error) { return c.store.HistoryUnder(ctx, prefix) },
		func(list []Object, upto time.Time) error { return c.save(ctx, prefix, false, list, upto) })
}

// fetchOnce is what kept answers, or else the store's answer (fetch),
// kept (keep) as complete up to now. In one process one fetch of a name
// is in flight at a time: a caller that finds one waits for it and then
// asks kept again, so two lookups of one key at once ask the store once.
// The second fetches itself when the first's fetch failed (its error is
// the first caller's alone: a cancelled first caller cancels nothing of
// the second's), when the first's keep was skipped, or when the first
// has not finished within the cache's wait (HistoryWait): then it fetches
// alongside, and waits no longer.
func (c *HistoryCache) fetchOnce(ctx context.Context, name string, at time.Time, kept func() ([]Object, bool, error),
	fetch func(context.Context) ([]Object, error), keep func([]Object, time.Time) error) ([]Object, error) {
	if list, hit, err := kept(); err != nil || hit {
		return list, err
	}
	wait, cancel := context.WithTimeout(ctx, c.wait)
	done, waited, err := c.f.fetching(wait, name)
	cancel()
	switch {
	case err == nil:
		defer done()
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		// The fetch under way took longer than the wait: fetch alongside.
	}
	if waited {
		// What the fetch waited for kept, unless its keep was skipped.
		if list, hit, err := kept(); err != nil || hit {
			return list, err
		}
	}
	upto, err := c.upto(ctx, at)
	if err != nil {
		return nil, err
	}
	list, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	return list, keep(list, upto)
}

// cached is what query reads, if a covered row matching match (with ?1
// the key or prefix asked for) is complete up to at and within the TTL
// (hit); a miss if not, or if the database was not this handle's within
// the bound.
func (c *HistoryCache) cached(ctx context.Context, match, keyOrPrefix string, at time.Time, query string, args ...any) (list []Object, hit bool, err error) {
	ok, err := c.query(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM covered WHERE (`+match+`) AND upto >= ?2 AND fetched >= ?3`,
			keyOrPrefix, at.UnixNano(), time.Now().Add(-c.ttl).UnixNano()).Scan(&n); err != nil || n == 0 {
			return err
		}
		list, err = readObjects(ctx, conn, query, args...)
		hit = err == nil
		return err
	})
	if !ok {
		return nil, false, err
	}
	return list, hit, nil
}

func readObjects(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]Object, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		var modified int64
		if err := rows.Scan(&o.Key, &o.VersionID, &o.ETag, &o.Size, &modified, &o.Deleted); err != nil {
			return nil, err
		}
		o.LastModified = time.Unix(0, modified).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

// save replaces what's cached for key (whole) or under prefix with list,
// complete up to upto; not at all if the database was not this handle's
// within the bound.
func (c *HistoryCache) save(ctx context.Context, keyOrPrefix string, whole bool, list []Object, upto time.Time) error {
	_, err := c.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return saveTx(ctx, tx, keyOrPrefix, whole, list, upto)
	})
	return err
}

func saveTx(ctx context.Context, tx *sql.Tx, keyOrPrefix string, whole bool, list []Object, upto time.Time) error {
	where, args := `key = ?`, []any{keyOrPrefix}
	if !whole {
		where, args = underPrefix(keyOrPrefix)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM versions WHERE `+where, args...); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO versions VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	fetched := time.Now().UnixNano()
	for i, o := range list {
		if _, err := insert.ExecContext(ctx, o.Key, i, o.VersionID, o.ETag, o.Size, o.LastModified.UnixNano(), o.Deleted, fetched); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO covered VALUES (?, ?, ?, ?)`, keyOrPrefix, whole, upto.UnixNano(), fetched)
	return err
}

// forget drops key's cached history, and anything that claims to cover
// it, so the next look at it asks the store: for a key with a cached
// version the store no longer has (erased by a purge). If the database
// is not this handle's within the bound, the forget is held pending
// (pendingForget): this process reads the key as a miss until a later
// write applies it.
func (c *HistoryCache) forget(ctx context.Context, key string) error {
	return c.forgetOrHold(ctx, pendingForget{key: key})
}

// ForgetUnder drops everything cached for keys starting with prefix, and
// anything that claims to cover them: for what a purge may have erased.
// Held pending as forget is, past the bound.
func (c *HistoryCache) ForgetUnder(ctx context.Context, prefix string) error {
	return c.forgetOrHold(ctx, pendingForget{key: prefix, under: true})
}

func (c *HistoryCache) forgetOrHold(ctx context.Context, p pendingForget) error {
	done, err := c.write(ctx, func(ctx context.Context, tx *sql.Tx) error { return forgetTx(ctx, tx, p) })
	if err == nil && !done {
		c.f.addPending(p)
	}
	return err
}

// forgetTx deletes what p names.
func forgetTx(ctx context.Context, tx *sql.Tx, p pendingForget) error {
	where, args := `key = ?`, []any{p.key}
	// A covered row is about a key if it is the key's own, or a prefix of
	// it; about a prefix, if either starts with the other.
	covered := `(whole = 1 AND prefix = ?1) OR (whole = 0 AND substr(?1, 1, length(prefix)) = prefix)`
	if p.under {
		where, args = underPrefix(p.key)
		covered = `substr(prefix, 1, length(?1)) = ?1 OR (whole = 0 AND substr(?1, 1, length(prefix)) = prefix)`
	}
	for _, table := range []string{"versions", "blobs"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+where, args...); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM covered WHERE `+covered, p.key)
	return err
}

// Note is the value kept under name ("" if none), and SetNote keeps one.
// Past the bound, Note answers "" (nothing kept: the caller does what it
// would for a cache that has seen nothing) and SetNote keeps nothing.
func (c *HistoryCache) Note(ctx context.Context, name string) (string, error) {
	var v string
	_, err := c.query(ctx, func(ctx context.Context, conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, `SELECT value FROM notes WHERE name = ?`, name).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return v, err
}

func (c *HistoryCache) SetNote(ctx context.Context, name, value string) error {
	_, err := c.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO notes VALUES (?, ?)`, name, value)
		return err
	})
	return err
}

// cacheContentKey marks a context whose reads may keep their content.
type cacheContentKey struct{}

// CacheContent marks ctx's reads, through an AsOf view, as metadata (a
// directory block, a .metadata object) whose content a HistoryCache may
// keep. File data is never marked: it stays in the store.
func CacheContent(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheContentKey{}, true)
}

func cachingContent(ctx context.Context) bool {
	v, _ := ctx.Value(cacheContentKey{}).(bool)
	return v
}

// blob is a cached version: its Object, and its content if kept.
// A miss, too, past the bound or under a pending forget.
func (c *HistoryCache) blob(ctx context.Context, key, versionID string, wantContent bool) (*Object, []byte, bool, error) {
	if c.f.pendingOn(key, false) {
		return nil, nil, false, nil
	}
	var object string
	var content []byte
	found := false
	ok, err := c.query(ctx, func(ctx context.Context, conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, `SELECT object, content FROM blobs WHERE key = ? AND version = ? AND fetched >= ?`,
			key, versionID, time.Now().Add(-c.ttl).UnixNano()).Scan(&object, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if !ok || !found || (wantContent && content == nil) {
		return nil, nil, false, err
	}
	var o Object
	if err := json.Unmarshal([]byte(object), &o); err != nil {
		return nil, nil, false, nil // unreadable: fetch it again
	}
	return &o, content, true, nil
}

// keepBlob caches a version's Object, and content unless it's nil; a
// content-less row never replaces one with content. Nothing is kept if
// the database is not this handle's within the bound.
func (c *HistoryCache) keepBlob(ctx context.Context, o *Object, content []byte) error {
	object, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = c.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO blobs VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (key, version) DO UPDATE SET object = excluded.object,
				content = coalesce(excluded.content, blobs.content), fetched = excluded.fetched`,
			o.Key, o.VersionID, string(object), content, time.Now().UnixNano())
		return err
	})
	return err
}

// HeadVersion is the store's HeadVersion, kept: a version never changes.
func (c *HistoryCache) HeadVersion(ctx context.Context, key, versionID string) (*Object, error) {
	return c.headVersion(ctx, key, versionID)
}

// headVersion is store.HeadVersion, kept: a version never changes.
func (c *HistoryCache) headVersion(ctx context.Context, key, versionID string) (*Object, error) {
	if o, _, ok, err := c.blob(ctx, key, versionID, false); err != nil || ok {
		return o, err
	}
	o, err := c.store.HeadVersion(ctx, key, versionID)
	if err != nil {
		return nil, err
	}
	return o, c.keepBlob(ctx, o, nil)
}

// getVersion is store.GetVersion, with the content kept if ctx asks for
// that (CacheContent) and it's small.
func (c *HistoryCache) getVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error) {
	if !cachingContent(ctx) {
		return c.store.GetVersion(ctx, key, versionID)
	}
	if o, content, ok, err := c.blob(ctx, key, versionID, true); err != nil {
		return nil, nil, err
	} else if ok {
		return io.NopCloser(bytes.NewReader(content)), o, nil
	}
	body, o, err := c.store.GetVersion(ctx, key, versionID)
	if err != nil || o.Size > maxCachedContent {
		return body, o, err
	}
	defer body.Close()
	content, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	if content == nil {
		content = []byte{} // kept as empty, not as absent
	}
	if err := c.keepBlob(ctx, o, content); err != nil {
		return nil, nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), o, nil
}

// underPrefix is a WHERE clause, and its arguments, for the keys starting
// with prefix, as a range the primary key's index can serve.
func underPrefix(prefix string) (string, []any) {
	if prefix == "" {
		return `1`, nil
	}
	if end, ok := prefixEnd(prefix); ok {
		return `key >= ? AND key < ?`, []any{prefix, end}
	}
	return `key >= ?`, []any{prefix}
}

// prefixEnd is the least string greater than every string starting with
// prefix, if there is one (there isn't for one of only 0xff bytes).
func prefixEnd(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}
