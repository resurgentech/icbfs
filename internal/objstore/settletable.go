package objstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
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
//	settle_read:    settle_entries as the states were read, before any change was played:
//	                what the settle's proof (SettleDir) checks each generated state against
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
	`CREATE TABLE IF NOT EXISTS settle_read (
		run     TEXT NOT NULL,
		name    TEXT NOT NULL,
		item    TEXT NOT NULL,
		type    INTEGER NOT NULL,
		from_at INTEGER NOT NULL,
		to_at   INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS settle_read_from ON settle_read (run, from_at)`,
}

// settleTables is the tables holding a run's rows.
var settleTables = []string{"settle_entries", "settle_read", "settle_states", "settle_blocks", "settle_runs"}

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
	// The states are all read before any change is played: settle_entries
	// and settle_read are the same until then, and settle_read stays so.
	for _, e := range entries {
		if open[e] {
			delete(open, e) // still there
			continue
		}
		for _, table := range []string{"settle_entries", "settle_read"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+` VALUES (?, ?, ?, ?, ?, NULL)`, t.run, e.Name, e.Item, e.Type, nanos(at)); err != nil {
				return false, err
			}
		}
	}
	for e := range open { // gone
		for _, table := range []string{"settle_entries", "settle_read"} {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET to_at = ? WHERE run = ? AND name = ? AND item = ? AND type = ? AND to_at IS NULL`,
				nanos(at), t.run, e.Name, e.Item, e.Type); err != nil {
				return false, err
			}
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

// Add plays a late add into the states in [at, until): there, e's name
// names e's item wherever no lifetime of the name covers (an occupant
// then keeps it). The states from until on are not touched: the late
// write put the change into them itself (it was written into the
// directory's present, dated until). Played again, it changes nothing.
func (t *SettleTable) Add(ctx context.Context, at, until time.Time, e SettleEntry) error {
	if !until.After(at) {
		return fmt.Errorf("an add at %s until %s: until must be after at", at.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	}
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
		if to <= from {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO settle_entries VALUES (?, ?, ?, ?, ?, ?)`, t.run, e.Name, e.Item, e.Type, from, to)
		return err
	}
	end := nanos(until)
	cursor := nanos(at)
	for _, l := range ls {
		if cursor >= end {
			break
		}
		if l.from > cursor {
			if err := insert(cursor, min(l.from, end)); err != nil {
				return err
			}
		}
		if l.to < 0 {
			return tx.Commit() // covered from here on
		}
		cursor = max(cursor, l.to)
	}
	if err := insert(cursor, end); err != nil {
		return err
	}
	return tx.Commit()
}

// Remove plays a late removal into the states in [at, until): there,
// name's lifetimes naming item end (a lifetime that goes on past until
// keeps [until, its end) as its own). A lifetime of the name naming
// another item (another occupant) is not this removal's.
func (t *SettleTable) Remove(ctx context.Context, at, until time.Time, name, item string) error {
	if !until.After(at) {
		return fmt.Errorf("a removal at %s until %s: until must be after at", at.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ls, err := t.lifetimes(ctx, tx, name, at)
	if err != nil {
		return err
	}
	a, u := nanos(at), nanos(until)
	for _, l := range ls {
		if l.item != item || l.from >= u {
			continue
		}
		if l.to < 0 || l.to > u {
			// It goes on past until: [until, its end) stays.
			var end any
			if l.to >= 0 {
				end = l.to
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO settle_entries SELECT run, name, item, type, ?, ? FROM settle_entries WHERE rowid = ?`, u, end, l.id); err != nil {
				return err
			}
		}
		if l.from >= a {
			_, err = tx.ExecContext(ctx, `DELETE FROM settle_entries WHERE rowid = ?`, l.id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE settle_entries SET to_at = ? WHERE rowid = ?`, a, l.id)
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

// ReadEntriesAt is the state at at as the states were read, before any
// change was played: the entries of the latest state read at or before
// at.
func (t *SettleTable) ReadEntriesAt(ctx context.Context, at time.Time) ([]SettleEntry, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT name, item, type FROM settle_read
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

// UpTo is a cache sharing c's database whose history of each key upTo
// names leaves out the versions stored after the version it names: a
// rerun of a cut settle reads the directory's states as they were when
// the cut settle began (SettleDir's record, V0), without the versions
// the cut settle wrote after it. Nothing else is left out: no version is
// judged by its date against the others (a view takes, of the versions
// dated alike, the newest stored, as any view does). It is closed with c,
// never itself.
func (c *HistoryCache) UpTo(upTo map[string]string) *HistoryCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &HistoryCache{store: c.store, db: c.db, ttl: c.ttl, offset: c.offset, offsetAt: c.offsetAt,
		upTo: upTo}
}

// dropAfter is list without the versions stored after the upTo version
// of key, if the cache names one.
func (c *HistoryCache) dropAfter(key string, list []Object) []Object {
	if id, ok := c.upTo[key]; ok {
		if i := VersionIndex(list, id); i > 0 {
			return list[i:]
		}
	}
	return list
}

// VersionIndex is the place of version id in history (newest stored
// first), -1 if it isn't there: the versions before it were stored after
// it.
func VersionIndex(history []Object, id string) int {
	for i := range history {
		if history[i].VersionID == id {
			return i
		}
	}
	return -1
}

// NotStates is what in history (newest stored first) is not a state of
// the key, by dates: shadowed, the versions with a version stored after
// them dated alike that is itself not late (a view takes the newest
// stored of those); and late, of the rest, the versions stored after one
// dated later, written behind their key's newest state. A late version
// shadows nothing: the settled state at a late write's own date stays a
// state. Deletions are neither.
func NotStates(history []Object, dates Dates) (shadowed, late map[string]bool) {
	late = map[string]bool{}
	for {
		shadowed = map[string]bool{}
		newer := map[int64]bool{}
		for i := range history {
			o := &history[i]
			if o.Deleted {
				continue
			}
			t := dates.Time(o).UnixNano()
			if newer[t] {
				shadowed[o.VersionID] = true
			}
			if !late[o.VersionID] {
				newer[t] = true
			}
		}
		next := map[string]bool{}
		var latest time.Time
		for i := len(history) - 1; i >= 0; i-- {
			o := &history[i]
			if o.Deleted || shadowed[o.VersionID] {
				continue
			}
			t := dates.Time(o)
			if t.Before(latest) {
				next[o.VersionID] = true
				continue
			}
			latest = t
		}
		if len(next) == len(late) {
			return shadowed, next
		}
		late = next
	}
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
