package objstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The settle's working table: a directory's history from the state just
// before its earliest late change on, as entry lifetimes, kept in the
// history cache's database (a file on the cache's volume, not SQLite's
// TEMP store, which goes to the temp directory) so a directory's history
// is never in memory whole. The tables are ordinary ones made with
// CREATE TABLE IF NOT EXISTS, outside the cache's layout: an older build
// sharing the file ignores them.
//
// Rows are a settle's own (its run), not a directory's: one cache is
// shared by every drive a library opens and every program of one store
// and credential on a machine (storeflags.History), so two settles may
// use the database at once, of one directory too, and neither clears
// the other's rows. A run's rows go when it ends (Clear); a run a
// process didn't end (killed) is cleared by the next SettleTable of any
// directory once it is a day old.
//
//	settle_runs:    each run, its directory and when it started
//	settle_entries: one row per entry per lifetime, [from_at, to_at), to_at NULL while open
//	settle_states:  each state's time, the sum of its entries as read (read = 1) or "" for a
//	                state a change made, the new keys minted for it, its encoded root block
//	                and .metadata once made
//	settle_blocks:  the blocks below the root the read states reached, the latest time
//	                of a state that reached each, and whether the present does
var settleSchema = []string{
	`CREATE TABLE IF NOT EXISTS settle_runs (
		run     TEXT PRIMARY KEY,
		dir     TEXT NOT NULL,
		started INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS settle_entries (
		run     TEXT NOT NULL,
		name    TEXT NOT NULL,
		item    TEXT NOT NULL,
		type    INTEGER NOT NULL,
		from_at INTEGER NOT NULL,
		to_at   INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS settle_entries_from ON settle_entries (run, from_at)`,
	`CREATE INDEX IF NOT EXISTS settle_entries_name ON settle_entries (run, name)`,
	`CREATE TABLE IF NOT EXISTS settle_states (
		run        TEXT NOT NULL,
		at         INTEGER NOT NULL,
		sum        TEXT NOT NULL,
		read       INTEGER NOT NULL,
		lower_keys TEXT NOT NULL DEFAULT '',
		root       BLOB,
		meta       BLOB,
		PRIMARY KEY (run, at)
	)`,
	`CREATE TABLE IF NOT EXISTS settle_blocks (
		run     TEXT NOT NULL,
		key     TEXT NOT NULL,
		last_at INTEGER,
		present INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (run, key)
	)`,
}

// settleTables is the tables holding a run's rows.
var settleTables = []string{"settle_entries", "settle_states", "settle_blocks", "settle_runs"}

// settleRunsKeptFor is how long a run's rows are kept if its settle
// never ends them (its process killed): far longer than a settle takes.
const settleRunsKeptFor = 24 * time.Hour

// SettleEntry is one directory entry in the working table.
type SettleEntry struct {
	Name, Item string
	Type       int
}

// SettleState is one state in the working table: its time, the sum of
// its entries as read ("" for one only a change made), whether it was
// read from the store, and the keys minted for its blocks below the root.
type SettleState struct {
	At      time.Time
	Sum     string
	Read    bool
	NewKeys []string
}

// SettleSum is a sum of entries, whatever their order: how a generated
// state is known to be the one the store has without reading it back.
func SettleSum(entries []SettleEntry) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmt.Sprintf("%s\x00%s\x00%d\n", e.Name, e.Item, e.Type)
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// SettleTable is one settle's rows of the working table: a run of its
// own, for directory dir.
type SettleTable struct {
	db  *sql.DB
	dir string
	run string
}

// SettleTable starts a settle's working table for directory dir: empty,
// whatever another settle, of this directory or another, has or left.
// A history cache opened by an older build has the tables made here on
// first use.
func (c *HistoryCache) SettleTable(ctx context.Context, dir string) (*SettleTable, error) {
	for _, stmt := range settleSchema {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	// What killed runs left, a day on.
	rows, err := c.db.QueryContext(ctx, `SELECT run FROM settle_runs WHERE started < ?`, nanos(time.Now().Add(-settleRunsKeptFor)))
	if err != nil {
		return nil, err
	}
	var stale []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		stale = append(stale, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range stale {
		if err := (&SettleTable{db: c.db, run: r}).Clear(ctx); err != nil {
			return nil, err
		}
	}
	t := &SettleTable{db: c.db, dir: dir, run: uuid.NewString()}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO settle_runs VALUES (?, ?, ?)`, t.run, dir, nanos(time.Now())); err != nil {
		return nil, err
	}
	return t, nil
}

// Clear removes the run's rows: its settle is done.
func (t *SettleTable) Clear(ctx context.Context) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range settleTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE run = ?`, t.run); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

// ReadState records the state read at at, which is after every state
// recorded so far: its entries' lifetimes, diffed against the open ones
// (the state before), and its sum. A state the same as the one before
// is not recorded (kept false).
func (t *SettleTable) ReadState(ctx context.Context, at time.Time, entries []SettleEntry) (kept bool, err error) {
	sum := SettleSum(entries)
	var last string
	err = t.db.QueryRowContext(ctx, `SELECT sum FROM settle_states WHERE run = ? AND read = 1 ORDER BY at DESC LIMIT 1`, t.run).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && last == sum {
		return false, nil
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	open := map[SettleEntry]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT name, item, type FROM settle_entries WHERE run = ? AND to_at IS NULL`, t.run)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var e SettleEntry
		if err := rows.Scan(&e.Name, &e.Item, &e.Type); err != nil {
			rows.Close()
			return false, err
		}
		open[e] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, e := range entries {
		if open[e] {
			delete(open, e) // still there
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settle_entries VALUES (?, ?, ?, ?, ?, NULL)`, t.run, e.Name, e.Item, e.Type, nanos(at)); err != nil {
			return false, err
		}
	}
	for e := range open { // gone
		if _, err := tx.ExecContext(ctx, `UPDATE settle_entries SET to_at = ? WHERE run = ? AND name = ? AND item = ? AND type = ? AND to_at IS NULL`,
			nanos(at), t.run, e.Name, e.Item, e.Type); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO settle_states (run, at, sum, read) VALUES (?, ?, ?, 1)`, t.run, nanos(at), sum); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SettlePresent is the time AddBlocks is given for the blocks of the
// directory as it is now.
var SettlePresent = time.Unix(0, math.MaxInt64).UTC()

// AddBlocks records keys of blocks below the root that the state at at
// reached (SettlePresent: the directory now).
func (t *SettleTable) AddBlocks(ctx context.Context, at time.Time, keys ...string) error {
	stmt := `INSERT INTO settle_blocks (run, key, last_at) VALUES (?, ?, ?)
		ON CONFLICT (run, key) DO UPDATE SET last_at = max(coalesce(last_at, excluded.last_at), excluded.last_at)`
	arg := any(nanos(at))
	if at.Equal(SettlePresent) {
		stmt = `INSERT INTO settle_blocks (run, key, present) VALUES (?, ?, ?)
			ON CONFLICT (run, key) DO UPDATE SET present = 1`
		arg = 1
	}
	for _, k := range keys {
		if _, err := t.db.ExecContext(ctx, stmt, t.run, k, arg); err != nil {
			return err
		}
	}
	return nil
}

// SettleBlock is a block AddBlocks recorded: the latest time of a state
// that reached it (zero for none), and whether the present does.
type SettleBlock struct {
	Key     string
	Last    time.Time
	Present bool
}

// Blocks is the blocks AddBlocks recorded, by key.
func (t *SettleTable) Blocks(ctx context.Context) ([]SettleBlock, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT key, last_at, present FROM settle_blocks WHERE run = ? ORDER BY key`, t.run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettleBlock
	for rows.Next() {
		var b SettleBlock
		var last sql.NullInt64
		if err := rows.Scan(&b.Key, &last, &b.Present); err != nil {
			return nil, err
		}
		if last.Valid {
			b.Last = fromNanos(last.Int64)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// EnsureState makes sure there's a state at at: one a change made, if
// none was read there.
func (t *SettleTable) EnsureState(ctx context.Context, at time.Time) error {
	_, err := t.db.ExecContext(ctx, `INSERT OR IGNORE INTO settle_states (run, at, sum, read) VALUES (?, ?, '', 0)`, t.run, nanos(at))
	return err
}

// lifetime is one row of settle_entries.
type lifetime struct {
	id       int64
	item     string
	from, to int64 // to < 0: open
}

func (t *SettleTable) lifetimes(ctx context.Context, tx *sql.Tx, name string, at time.Time) ([]lifetime, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, item, from_at, coalesce(to_at, -1) FROM settle_entries
		WHERE run = ? AND name = ? AND (to_at IS NULL OR to_at > ?) ORDER BY from_at`, t.run, name, nanos(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lifetime
	for rows.Next() {
		var l lifetime
		if err := rows.Scan(&l.id, &l.item, &l.from, &l.to); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Add plays a late add: from at on, e's name names e's item wherever no
// lifetime of the name covers (an occupant then keeps it). Played again,
// it changes nothing.
func (t *SettleTable) Add(ctx context.Context, at time.Time, e SettleEntry) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ls, err := t.lifetimes(ctx, tx, e.Name, at)
	if err != nil {
		return err
	}
	insert := func(from, to int64) error {
		var end any
		if to >= 0 {
			end = to
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO settle_entries VALUES (?, ?, ?, ?, ?, ?)`, t.run, e.Name, e.Item, e.Type, from, end)
		return err
	}
	cursor := nanos(at)
	for _, l := range ls {
		if l.from > cursor {
			if err := insert(cursor, l.from); err != nil {
				return err
			}
		}
		if l.to < 0 {
			return tx.Commit() // covered from here on
		}
		cursor = max(cursor, l.to)
	}
	if err := insert(cursor, -1); err != nil {
		return err
	}
	return tx.Commit()
}

// Remove plays a late removal: name's lifetimes naming item from at on
// end at at (and go, if they start at or after it). A lifetime of the
// name naming another item (a later occupant) is not this removal's.
func (t *SettleTable) Remove(ctx context.Context, at time.Time, name, item string) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ls, err := t.lifetimes(ctx, tx, name, at)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if l.item != item {
			continue
		}
		if l.from >= nanos(at) {
			_, err = tx.ExecContext(ctx, `DELETE FROM settle_entries WHERE rowid = ?`, l.id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE settle_entries SET to_at = ? WHERE rowid = ?`, nanos(at), l.id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// States is the states at or after from, oldest first.
func (t *SettleTable) States(ctx context.Context, from time.Time) ([]SettleState, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT at, sum, read, lower_keys FROM settle_states WHERE run = ? AND at >= ? ORDER BY at`, t.run, nanos(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettleState
	for rows.Next() {
		var at int64
		var s SettleState
		var keys string
		if err := rows.Scan(&at, &s.Sum, &s.Read, &keys); err != nil {
			return nil, err
		}
		s.At = fromNanos(at)
		if keys != "" {
			s.NewKeys = strings.Split(keys, " ")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EntriesAt is the state at at: the entries whose lifetime covers it.
func (t *SettleTable) EntriesAt(ctx context.Context, at time.Time) ([]SettleEntry, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT name, item, type FROM settle_entries
		WHERE run = ? AND from_at <= ? AND (to_at IS NULL OR to_at > ?)`, t.run, nanos(at), nanos(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettleEntry
	for rows.Next() {
		var e SettleEntry
		if err := rows.Scan(&e.Name, &e.Item, &e.Type); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetNewKeys records the keys minted for the blocks below the root of the
// state at at.
func (t *SettleTable) SetNewKeys(ctx context.Context, at time.Time, keys []string) error {
	_, err := t.db.ExecContext(ctx, `UPDATE settle_states SET lower_keys = ? WHERE run = ? AND at = ?`, strings.Join(keys, " "), t.run, nanos(at))
	return err
}

// SetEncoded keeps the state at at's encoded root block and .metadata,
// for the write-out to put with nothing to make.
func (t *SettleTable) SetEncoded(ctx context.Context, at time.Time, root, meta []byte) error {
	_, err := t.db.ExecContext(ctx, `UPDATE settle_states SET root = ?, meta = ? WHERE run = ? AND at = ?`, root, meta, t.run, nanos(at))
	return err
}

// Encoded is what SetEncoded kept for the state at at.
func (t *SettleTable) Encoded(ctx context.Context, at time.Time) (root, meta []byte, err error) {
	err = t.db.QueryRowContext(ctx, `SELECT root, meta FROM settle_states WHERE run = ? AND at = ?`, t.run, nanos(at)).Scan(&root, &meta)
	return root, meta, err
}

// --- The history cache, for a settle and for the writer it serves ---

// WithoutLate is a cache sharing c's database whose histories leave out
// late versions: those stored after a version of the same key dated later
// (by the item's .version table). A settle reads a directory's states
// through it, so that a block a late write left (the directory as it is
// now, dated at the change) isn't taken for the state at its date. It is
// closed with c, never itself. It takes the store to stand still while
// it's used: what it finds late in a key is found once.
func (c *HistoryCache) WithoutLate() *HistoryCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &HistoryCache{store: c.store, db: c.db, ttl: c.ttl, offset: c.offset, offsetAt: c.offsetAt,
		withoutLate: &lateFilter{late: map[string]map[string]bool{}}}
}

// lateFilter is a WithoutLate cache's: each key's late versions, by key.
type lateFilter struct {
	mu   sync.Mutex
	late map[string]map[string]bool
}

// dropLate is list without its late versions, nor (in a key whose table
// lists any) its unlisted ones: neither is a state.
func (c *HistoryCache) dropLate(ctx context.Context, key string, list []Object, at time.Time) ([]Object, error) {
	if len(list) == 0 {
		return list, nil
	}
	f := c.withoutLate
	f.mu.Lock()
	late, ok := f.late[key]
	f.mu.Unlock()
	if !ok {
		dates, err := c.currentDates(ctx, BaseKey(key), at)
		if err != nil {
			return nil, err
		}
		late = LateVersions(Listed(list, dates), dates)
		for id := range Unlisted(list, dates) {
			late[id] = true
		}
		f.mu.Lock()
		f.late[key] = late
		f.mu.Unlock()
	}
	if len(late) == 0 {
		return list, nil
	}
	out := make([]Object, 0, len(list))
	for _, o := range list {
		if !late[o.VersionID] {
			out = append(out, o)
		}
	}
	return out, nil
}

// Unlisted is the versions in history (deletions aside) that dates, if
// it lists any, doesn't: in an item whose .version lists every version
// written (DateAll), writes that never finished, or versions superseded
// and not yet erased. With dates empty, none.
func Unlisted(history []Object, dates Dates) map[string]bool {
	out := map[string]bool{}
	if len(dates) == 0 {
		return out
	}
	for _, o := range history {
		if _, ok := dates[o.VersionID]; !ok && !o.Deleted {
			out[o.VersionID] = true
		}
	}
	return out
}

// Listed is history without its Unlisted versions.
func Listed(history []Object, dates Dates) []Object {
	un := Unlisted(history, dates)
	if len(un) == 0 {
		return history
	}
	out := make([]Object, 0, len(history)-len(un))
	for _, o := range history {
		if !un[o.VersionID] {
			out = append(out, o)
		}
	}
	return out
}

// LateVersions is the versions in history (newest stored first) stored
// after a version dated later than they are, by dates: written behind
// their key's newest state. Deletions are neither.
func LateVersions(history []Object, dates Dates) map[string]bool {
	late := map[string]bool{}
	var latest time.Time
	for i := len(history) - 1; i >= 0; i-- {
		o := &history[i]
		if o.Deleted {
			continue
		}
		t := dates.Time(o)
		if t.Before(latest) {
			late[o.VersionID] = true
			continue
		}
		latest = t
	}
	return late
}

// currentDates is base's .version table as it is now, through the cache,
// as an AsOf view reads it.
func (c *HistoryCache) currentDates(ctx context.Context, base string, at time.Time) (Dates, error) {
	vkey := base + VersionSuffix
	history, err := c.fullHistory(ctx, vkey, at)
	if err != nil || len(history) == 0 || history[0].Deleted {
		return Dates{}, err
	}
	body, _, err := c.getVersion(CacheContent(ctx), vkey, history[0].VersionID)
	if IsNotFound(err) {
		return Dates{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return decodeDates(data)
}

// FreshHistory is key's history from the store, newest first, kept in
// the cache as complete to now: for a caller that needs it as it is (a
// settle) and views that will read it next.
func (c *HistoryCache) FreshHistory(ctx context.Context, key string) ([]Object, error) {
	upto, err := c.upto(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	list, err := c.store.History(ctx, key)
	if err != nil {
		return nil, err
	}
	return list, c.save(ctx, key, true, list, upto)
}

// ForgetKeys drops what the cache kept of each of keys (their histories
// and versions), so the next look at them asks the store: for the keys a
// write wrote, which it knows, so the rest of the cache stays.
func (c *HistoryCache) ForgetKeys(ctx context.Context, keys ...string) error {
	for _, k := range keys {
		if err := c.forget(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// Keep keeps a version just written, and its content (a block, a
// .metadata: small), as a read of it would: the next view of it is warm.
func (c *HistoryCache) Keep(ctx context.Context, o *Object, content []byte) error {
	switch {
	case len(content) > maxCachedContent:
		content = nil // the object only
	case content == nil:
		content = []byte{} // kept as empty, not as absent
	}
	return c.keepBlob(ctx, o, content)
}
