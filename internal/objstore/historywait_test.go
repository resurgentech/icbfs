package objstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingStore is fixedStore, counting the histories it is asked for.
type countingStore struct {
	fixedStore
	n *atomic.Int64
}

func (s countingStore) History(ctx context.Context, key string) ([]Object, error) {
	s.n.Add(1)
	return s.fixedStore.History(ctx, key)
}

// heldOpen is a cache on a new file with a short bound, a store that
// counts what it is asked, and a second connection to the file, as
// another process (or older code, which takes no turn) has.
func heldOpen(t *testing.T) (c *HistoryCache, store countingStore, other *sql.DB, path string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "history.db")
	store = countingStore{n: new(atomic.Int64)}
	c, err := OpenHistoryCache(ctx, store, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.wait = 200 * time.Millisecond
	other, err = sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	other.SetMaxOpenConns(1)
	t.Cleanup(func() { other.Close() })
	return c, store, other, path
}

// holdWriter begins a write transaction on db and keeps it open (SQLite's
// write lock held) until the returned func.
func holdWriter(t *testing.T, db *sql.DB) func() {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO notes VALUES ('held', 'by the test')`); err != nil {
		t.Fatal(err)
	}
	return func() { tx.Rollback() }
}

// heldAt is the time mustHistory views "k" at: one time, so a history
// kept covers the next look.
var heldAt = time.Now().Add(-10 * time.Second)

// mustHistory is c.history of "k", which must be fixedStore's, with no
// error, back within the bound and a little.
func mustHistory(t *testing.T, c *HistoryCache) {
	t.Helper()
	start := time.Now()
	h, err := c.history(context.Background(), "k", heldAt)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 || h[0].VersionID != "v1" {
		t.Fatalf("history %v", h)
	}
	if took := time.Since(start); took > c.wait+2*time.Second {
		t.Fatalf("history took %v, bound %v", took, c.wait)
	}
}

// A writer the test holds open: the cache's keep waits out its bound and
// is skipped, a forget is held pending and read as a miss until a later
// write applies it, and no caller gets an error.
func TestHistoryCacheHeldWriterIsAMissNotAnError(t *testing.T) {
	ctx := context.Background()
	c, store, other, _ := heldOpen(t)

	mustHistory(t, c) // from the store, kept
	mustHistory(t, c) // from the cache
	if n := store.n.Load(); n != 1 {
		t.Fatalf("store asked %d times, want 1", n)
	}

	release := holdWriter(t, other)
	if err := c.forget(ctx, "k"); err != nil {
		t.Fatalf("forget under a held writer: %v", err)
	}
	if n := c.ContendedMisses(); n != 1 {
		t.Fatalf("contended misses %d after the forget, want 1", n)
	}
	// The forget is pending: the cached history is not read (a miss: the
	// store answers), and the keep of the store's answer is skipped.
	mustHistory(t, c)
	if n := store.n.Load(); n != 2 {
		t.Fatalf("store asked %d times, want 2: a pending forget must read as a miss", n)
	}
	if n := c.ContendedMisses(); n != 2 {
		t.Fatalf("contended misses %d after the skipped keep, want 2", n)
	}
	if err := c.SetNote(ctx, "n", "v"); err != nil {
		t.Fatalf("set note under a held writer: %v", err)
	}
	release()

	// The next write applies the pending forget before its own.
	mustHistory(t, c)
	if n := store.n.Load(); n != 3 {
		t.Fatalf("store asked %d times, want 3", n)
	}
	if p := c.f.pendingNow(); len(p) != 0 {
		t.Fatalf("still pending after a write: %v", p)
	}
	mustHistory(t, c)
	if n := store.n.Load(); n != 3 {
		t.Fatalf("store asked %d times, want 3: kept again once the writer let go", n)
	}
	if v, err := c.Note(ctx, "n"); err != nil || v != "" {
		t.Fatalf("note %q, %v: the skipped SetNote kept nothing", v, err)
	}
}

// The process's connection to the file held (by the test, as a long
// statement of another handle would): reads wait their bound and are
// misses, keeps are skipped, and nothing is an error. (A writer in
// another process cannot keep a WAL reader out: readers wait only here.)
func TestHistoryCacheHeldConnectionReadsAreMisses(t *testing.T) {
	ctx := context.Background()
	c, store, _, _ := heldOpen(t)
	mustHistory(t, c)
	if err := c.SetNote(ctx, "n", "v"); err != nil {
		t.Fatal(err)
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	before := c.ContendedMisses()
	mustHistory(t, c)
	if n := store.n.Load(); n != 2 {
		t.Fatalf("store asked %d times, want 2: the cached history could not be read", n)
	}
	if v, err := c.Note(ctx, "n"); err != nil || v != "" {
		t.Fatalf("note %q, %v with the connection held: want a miss", v, err)
	}
	if _, _, ok, err := c.blob(ctx, "k", "v1", false); ok || err != nil {
		t.Fatalf("blob %v, %v with the connection held: want a miss", ok, err)
	}
	// The history's read and its keep, the note, the blob.
	if got := c.ContendedMisses() - before; got != 4 {
		t.Fatalf("contended misses %d, want 4", got)
	}
}

// The turn across processes held by another (the lock file, as another
// process's cache holds it): a write waits its bound and is skipped; the
// turn left with the goroutine still waiting for the lock comes back when
// the lock is let go, and the next write is done.
func TestHistoryCacheHeldTurnAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	c, _, _, path := heldOpen(t)
	if c.f.lock == nil {
		t.Skip("no file locks here")
	}
	l, err := openHistoryLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	if err := l.lock(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := c.SetNote(ctx, "n", "held"); err != nil {
		t.Fatalf("set note while another process has the turn: %v", err)
	}
	if took := time.Since(start); took < c.wait || took > c.wait+2*time.Second {
		t.Fatalf("set note took %v, bound %v", took, c.wait)
	}
	if n := c.ContendedMisses(); n != 1 {
		t.Fatalf("contended misses %d, want 1", n)
	}
	if err := l.unlock(); err != nil {
		t.Fatal(err)
	}
	c.wait = 5 * time.Second
	if err := c.SetNote(ctx, "n", "mine"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Note(ctx, "n"); err != nil || v != "mine" {
		t.Fatalf("note %q, %v: want the write after the lock came back", v, err)
	}
	if n := c.ContendedMisses(); n != 1 {
		t.Fatalf("contended misses %d, want still 1", n)
	}
}

// Handles on one path in one process share one database, closed with the
// last of them.
func TestHistoryCacheHandlesShareOneFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	a, err := OpenHistoryCache(ctx, fixedStore{}, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenHistoryCache(ctx, fixedStore{}, filepath.Join(filepath.Dir(path), ".", "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a.f != b.f {
		t.Fatal("two handles on one path have two files")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.SetNote(ctx, "n", "v"); err != nil {
		t.Fatalf("the other handle after one closed: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenHistoryCache(ctx, fixedStore{}, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.f == a.f {
		t.Fatal("a closed file was handed out again")
	}
	if v, err := c.Note(ctx, "n"); err != nil || v != "v" {
		t.Fatalf("note %q, %v", v, err)
	}
}

// Forgets held pending past maxPendingForgets fold into one forget of
// everything: memory stays bounded, every read in the process is a miss
// until a write applies it, and then nothing is pending.
func TestHistoryCachePendingForgetsAreBounded(t *testing.T) {
	ctx := context.Background()
	c, store, other, _ := heldOpen(t)
	mustHistory(t, c)
	c.wait = 5 * time.Millisecond
	release := holdWriter(t, other)
	for i := 0; i <= maxPendingForgets; i++ {
		if err := c.forget(ctx, fmt.Sprintf("key-%d", i)); err != nil {
			t.Fatalf("forget %d: %v", i, err)
		}
	}
	p := c.f.pendingNow()
	if len(p) != 1 || p[0].key != "" || !p[0].under {
		t.Fatalf("pending %d forgets (first %+v), want one forget of everything", len(p), p[0])
	}
	if !c.f.pendingOn("k", false) || !c.f.pendingOn("any/prefix", true) {
		t.Fatal("a forget of everything must make every read a miss")
	}
	release()
	c.wait = 5 * time.Second
	mustHistory(t, c) // a miss (the store answers), and its keep applies the forget
	if n := store.n.Load(); n != 2 {
		t.Fatalf("store asked %d times, want 2", n)
	}
	if p := c.f.pendingNow(); len(p) != 0 {
		t.Fatalf("still pending after a write: %d", len(p))
	}
	mustHistory(t, c)
	if n := store.n.Load(); n != 2 {
		t.Fatalf("store asked %d times, want 2: kept again", n)
	}
}

// A settle's working table writes in the cache's write turn: while
// another process has the turn, a settle's write waits in it (it is not
// passed over and does not fail), and goes on once the turn is let go.
func TestHistoryCacheSettleTableWritesTakeTheTurn(t *testing.T) {
	ctx := context.Background()
	c, _, _, path := heldOpen(t)
	if c.f.lock == nil {
		t.Skip("no file locks here")
	}
	tab, err := c.SettleTable(ctx, "dir")
	if err != nil {
		t.Fatal(err)
	}
	l, err := openHistoryLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	if err := l.lock(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tab.EnsureState(ctx, time.Unix(100, 0)) }()
	select {
	case err := <-done:
		t.Fatalf("the settle's write went on while another process had the turn: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := l.unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the settle's write after the turn came: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the settle's write did not go on once the turn was let go")
	}
	states, err := tab.States(ctx, time.Time{})
	if err != nil || len(states) != 1 {
		t.Fatalf("states %v, %v: want the one written", states, err)
	}
}

// A writer that takes no turn (another process's settle table, an older
// binary) holds the file while an operation that waited for nothing runs
// its statements on the caller's context: SQLite's own busy wait, set once
// at open to HistoryWait, bounds it, and the keep is a miss, counted, no
// error, back within HistoryWait and its margin.
func TestHistoryCacheOutsideWriterBoundsTheUncontendedPath(t *testing.T) {
	ctx := context.Background()
	c, _, other, _ := heldOpen(t)
	c.wait = HistoryWait
	release := holdWriter(t, other)
	defer release()
	start := time.Now()
	if err := c.SetNote(ctx, "n", "v"); err != nil {
		t.Fatalf("set note against a writer outside the turn: %v", err)
	}
	took := time.Since(start)
	if took < HistoryWait-time.Second || took > HistoryWait+historyBusyMargin+time.Second {
		t.Fatalf("set note took %v, want about HistoryWait (%v)", took, HistoryWait)
	}
	if n := c.ContendedMisses(); n != 1 {
		t.Fatalf("contended misses %d, want 1", n)
	}
}

// slowStore is countingStore whose History waits for its gate.
type slowStore struct {
	countingStore
	gate chan struct{}
}

func (s slowStore) History(ctx context.Context, key string) ([]Object, error) {
	<-s.gate
	return s.countingStore.History(ctx, key)
}

// Two lookups of one key at once in one process ask the store once: the
// second waits for the first's fetch and reads what it kept.
func TestHistoryCacheOneFetchOfAKeyAtATime(t *testing.T) {
	ctx := context.Background()
	store := slowStore{countingStore{n: new(atomic.Int64)}, make(chan struct{})}
	c, err := OpenHistoryCache(ctx, store, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			h, err := c.history(ctx, "k", heldAt)
			if err == nil && (len(h) != 1 || h[0].VersionID != "v1") {
				err = fmt.Errorf("history %v", h)
			}
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond) // both asked: one in the store, one waiting for it
	close(store.gate)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := store.n.Load(); n != 1 {
		t.Fatalf("store asked %d times for one key looked up twice at once, want 1", n)
	}
}
