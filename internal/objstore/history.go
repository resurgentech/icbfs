package objstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

// DefaultHistoryTTL is how long a HistoryCache keeps what it fetched.
const DefaultHistoryTTL = 30 * 24 * time.Hour

// HistoryCache keeps keys' histories, as the store lists them, in a local
// SQLite database that AsOf views of any time answer from: moving from
// one time to another needs nothing more from the store. The past
// doesn't change, except that Erase (a purge) can remove entries; a view
// that finds a cached version gone forgets the key and asks again, and
// everything is fetched again once it's older than the cache's TTL, so
// a purged name lingers in it no longer than that.
//
// Several processes can share one database (WAL, with a busy timeout):
// a mount as of some time and the history app, say.
type HistoryCache struct {
	store Store
	db    *sql.DB
	ttl   time.Duration
	temp  string // the directory Close removes, for OpenTempHistoryCache's

	mu       sync.Mutex
	offset   time.Duration // the store's clock minus ours
	offsetAt time.Time     // when offset was measured; zero for never
}

// historyLayout is the database's layout (PRAGMA user_version). A cache
// of another layout, older or newer, is dropped and started again.
const historyLayout = 1

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
	// Transactions take the write lock as they begin (_txlock=immediate):
	// one that read first and then wrote would fail outright, not wait,
	// if another process wrote in between.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// One connection serializes this process's access; other processes
	// wait on the busy timeout.
	db.SetMaxOpenConns(1)
	c := &HistoryCache{store: store, db: db, ttl: ttl}
	if err := c.init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("history cache %s: %w", path, err)
	}
	return c, nil
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

// Close closes the database, and removes a temporary cache.
func (c *HistoryCache) Close() error {
	err := c.db.Close()
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
// prefix (whole = 1), complete for every time up to upto. fetched times,
// by this machine's clock, are what the TTL is counted from.
func (c *HistoryCache) init(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var layout int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&layout); err != nil {
		return err
	}
	if layout != historyLayout {
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS versions`,
			`DROP TABLE IF EXISTS covered`,
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
			fmt.Sprintf(`PRAGMA user_version = %d`, historyLayout),
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	cutoff := time.Now().Add(-c.ttl).UnixNano()
	for _, table := range []string{"versions", "covered"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE fetched < ?`, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// storeNow is the store's clock now, from an offset measured every
// historyClockCheck rather than a request each time.
func (c *HistoryCache) storeNow(ctx context.Context) (time.Time, error) {
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
	now, err := c.storeNow(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if safe := now.Add(-2 * time.Second); safe.After(at) {
		return safe, nil
	}
	return at, nil
}

// history is key's history, newest first, complete at least up to at.
func (c *HistoryCache) history(ctx context.Context, key string, at time.Time) ([]Object, error) {
	ok, err := c.covers(ctx, `(whole = 1 AND prefix = ?1) OR (whole = 0 AND substr(?1, 1, length(prefix)) = prefix)`, key, at)
	if err != nil {
		return nil, err
	}
	if ok {
		return c.read(ctx, `SELECT key, version, etag, size, modified, deleted FROM versions WHERE key = ? ORDER BY pos`, key)
	}
	upto, err := c.upto(ctx, at)
	if err != nil {
		return nil, err
	}
	list, err := c.store.History(ctx, key)
	if err != nil {
		return nil, err
	}
	return list, c.save(ctx, key, true, list, upto)
}

// historyUnder is the history of every key starting with prefix, keys in
// order and each newest first, complete at least up to at.
func (c *HistoryCache) historyUnder(ctx context.Context, prefix string, at time.Time) ([]Object, error) {
	ok, err := c.covers(ctx, `whole = 0 AND substr(?1, 1, length(prefix)) = prefix`, prefix, at)
	if err != nil {
		return nil, err
	}
	if ok {
		where, args := underPrefix(prefix)
		return c.read(ctx, `SELECT key, version, etag, size, modified, deleted FROM versions WHERE `+where+` ORDER BY key, pos`, args...)
	}
	upto, err := c.upto(ctx, at)
	if err != nil {
		return nil, err
	}
	list, err := c.store.HistoryUnder(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return list, c.save(ctx, prefix, false, list, upto)
}

// covers reports whether a covered row matching match (with ?1 the key
// or prefix asked for) is complete up to at and within the TTL.
func (c *HistoryCache) covers(ctx context.Context, match, keyOrPrefix string, at time.Time) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM covered WHERE (`+match+`) AND upto >= ?2 AND fetched >= ?3`,
		keyOrPrefix, at.UnixNano(), time.Now().Add(-c.ttl).UnixNano()).Scan(&n)
	return n > 0, err
}

func (c *HistoryCache) read(ctx context.Context, query string, args ...any) ([]Object, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
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
// complete up to upto.
func (c *HistoryCache) save(ctx context.Context, keyOrPrefix string, whole bool, list []Object, upto time.Time) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO covered VALUES (?, ?, ?, ?)`, keyOrPrefix, whole, upto.UnixNano(), fetched); err != nil {
		return err
	}
	return tx.Commit()
}

// forget drops key's cached history, and anything that claims to cover
// it, so the next look at it asks the store: for a key with a cached
// version the store no longer has (erased by a purge).
func (c *HistoryCache) forget(ctx context.Context, key string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM versions WHERE key = ?`, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM covered WHERE (whole = 1 AND prefix = ?1) OR (whole = 0 AND substr(?1, 1, length(prefix)) = prefix)`, key); err != nil {
		return err
	}
	return tx.Commit()
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
