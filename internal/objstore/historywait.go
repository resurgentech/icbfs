package objstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HistoryWait is the longest one operation of a HistoryCache waits for
// the database: its turn to write in this process, then across processes,
// then SQLite's own lock. Past it, a read answers as a miss (the caller
// reads the store), a keep is skipped, and a forget is held in memory
// (see pendingForget); each is counted (ContendedMisses). The cache never
// returns "database is locked" to its caller.
//
// Five seconds: turns are given in order (historyFile.take), so a writer
// waits for the writers ahead of it, each a few milliseconds and under a
// loaded disk's fsync some hundreds; a dozen of those fit with room. And
// a miss costs one store request (tens to a few hundred milliseconds),
// so waiting longer than this for the cache is worse than reading the
// store.
const HistoryWait = 5 * time.Second

// The connection's busy wait (SQLite's own, for a lock held by a writer
// that takes no turn: another process's settle table, an older binary) is
// HistoryWait, set once at open. An operation that has already spent part
// of its bound waiting (for its turn, or the connection) sets it to what
// is left, and back: one that has not, and that is every operation with
// no contention, runs its statements as they are, on the caller's
// context. So no operation waits longer than HistoryWait and
// historyBusyMargin, and an uncontended one costs what it did before the
// turn.
const historyBusyMargin = 50 * time.Millisecond

// settleTurnWait is how long a write of the settle's working table waits
// for the write turn before it goes ahead on SQLite's own busy wait
// (inTurn).
const settleTurnWait = 10 * time.Second

// historyFile is one history cache database, open once in this process
// and shared by every HistoryCache opened on its path: several drives of
// one store in one process wait their turns in this process's queue, not
// in SQLite's busy wait.
type historyFile struct {
	key  string // historyFiles' key: the path, made absolute
	db   *sql.DB
	refs int // under historyFiles.mu

	ready   chan struct{} // closed when the first open's init is done
	initErr error

	// turn is the write turn in this process: one writer at a time, the
	// others queued in the order they came (a channel's senders are).
	turn chan struct{}
	// lock is the write turn across processes; nil where the file system
	// has no locks (SQLite's busy wait alone then).
	lock *historyLock

	mu      sync.Mutex
	pending []pendingForget
	seq     uint64 // the last pendingForget.seq given

	// inflight is the store fetches under way in this process, by name
	// (fetching): closed when each is done.
	inflight map[string]chan struct{}
}

// fetching waits until no fetch of name is under way in this process (or
// ctx is done), then marks one as under way until done is called.
func (f *historyFile) fetching(ctx context.Context, name string) (done func(), err error) {
	for {
		f.mu.Lock()
		ch, busy := f.inflight[name]
		if !busy {
			if f.inflight == nil {
				f.inflight = map[string]chan struct{}{}
			}
			ch = make(chan struct{})
			f.inflight[name] = ch
			f.mu.Unlock()
			return func() {
				f.mu.Lock()
				delete(f.inflight, name)
				f.mu.Unlock()
				close(ch)
			}, nil
		}
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// pendingForget is a forget that did not get the database within
// HistoryWait. Until a later write of this process applies it (every
// write applies the pending forgets first, in its own transaction), every
// handle in this process reads what it names as a miss.
type pendingForget struct {
	key   string
	under bool   // every key starting with key; else the one key
	seq   uint64 // its place in the list: a write drops those it applied
}

// maxPendingForgets bounds the forgets held in memory. Past it they are
// folded into one forget of everything (every read in this process a
// miss until a write applies it): a file kept from this process for a
// long time costs reads of the store, never a list without end.
const maxPendingForgets = 256

var historyFiles struct {
	mu sync.Mutex
	m  map[string]*historyFile
}

// openHistoryFile is path's historyFile, opened if this process has not
// it open, with a reference taken; first is true for the caller that
// opened it, which must init it and then call initDone.
func openHistoryFile(path string) (f *historyFile, first bool, err error) {
	key := path
	if abs, err := filepath.Abs(path); err == nil {
		key = abs
	}
	historyFiles.mu.Lock()
	defer historyFiles.mu.Unlock()
	if f := historyFiles.m[key]; f != nil {
		f.refs++
		return f, false, nil
	}
	// Transactions take the write lock as they begin (_txlock=immediate):
	// one that read first and then wrote would fail outright, not wait,
	// if another process wrote in between.
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_txlock=immediate", path, HistoryWait.Milliseconds()))
	if err != nil {
		return nil, false, err
	}
	// One connection: this process's statements on the file take turns.
	db.SetMaxOpenConns(1)
	f = &historyFile{key: key, db: db, refs: 1, ready: make(chan struct{}), turn: make(chan struct{}, 1)}
	if l, err := openHistoryLock(path + ".lock"); err == nil {
		f.lock = l
	}
	if historyFiles.m == nil {
		historyFiles.m = map[string]*historyFile{}
	}
	historyFiles.m[key] = f
	return f, true, nil
}

// initDone records the first open's init, for the openers waiting on it;
// a file whose init failed is no longer handed out (each opener lets go
// of its own reference).
func (f *historyFile) initDone(err error) {
	f.initErr = err
	if err != nil {
		// Openers after this one open the file afresh.
		historyFiles.mu.Lock()
		if historyFiles.m[f.key] == f {
			delete(historyFiles.m, f.key)
		}
		historyFiles.mu.Unlock()
	}
	close(f.ready)
}

// release lets go of a reference, and closes the file with the last.
func (f *historyFile) release() error {
	historyFiles.mu.Lock()
	f.refs--
	last := f.refs == 0
	if last && historyFiles.m[f.key] == f {
		delete(historyFiles.m, f.key)
	}
	historyFiles.mu.Unlock()
	if !last {
		return nil
	}
	err := f.db.Close()
	if f.lock != nil {
		// A writer that gave up waiting for the lock left a goroutine
		// blocked in it, holding the turn: the lock's file is closed when
		// that goroutine has let go of both.
		select {
		case f.turn <- struct{}{}:
			f.lock.close()
		default:
			go func() {
				f.turn <- struct{}{}
				f.lock.close()
			}()
		}
	}
	return err
}

// take waits, until ctx is done, for the write turn: this process's,
// then the file's across processes. release gives both back.
func (f *historyFile) take(ctx context.Context) (release func(), err error) {
	select {
	case f.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.lock == nil {
		return func() { <-f.turn }, nil
	}
	got := make(chan error, 1)
	go func() { got <- f.lock.lock() }()
	select {
	case err := <-got:
		if err != nil {
			// No lock to be had here (a file system without them):
			// SQLite's own lock still keeps the writes apart.
			return func() { <-f.turn }, nil
		}
		return func() {
			_ = f.lock.unlock()
			<-f.turn
		}, nil
	case <-ctx.Done():
		// The goroutine still waiting for the lock keeps this process's
		// turn until it has the lock, and then gives both back: the turn
		// is never given while a lock call on this file is pending.
		go func() {
			if <-got == nil {
				_ = f.lock.unlock()
			}
			<-f.turn
		}()
		return nil, ctx.Err()
	}
}

// inTurn runs fn, a write of the settle's working table (SettleTable) on
// this file, in the same write turn as the cache's writes, so that
// neither kind passes the other over. A settle's write is not a cache's
// and cannot be skipped: it waits for its turn up to settleTurnWait and
// then goes ahead without it, on SQLite's own busy wait (HistoryWait), as
// it did before the turn existed; the turn never holds it longer.
func (f *historyFile) inTurn(ctx context.Context, fn func() error) error {
	if f == nil {
		return fn()
	}
	turnCtx, cancel := context.WithTimeout(ctx, settleTurnWait)
	release, err := f.take(turnCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fn()
	}
	defer release()
	return fn()
}

// pendingNow is the forgets pending now.
func (f *historyFile) pendingNow() []pendingForget {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pendingForget(nil), f.pending...)
}

// addPending holds a forget that did not get the database; past
// maxPendingForgets, the list becomes one forget of everything.
func (f *historyFile) addPending(p pendingForget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	p.seq = f.seq
	if len(f.pending) >= maxPendingForgets {
		// A new seq: a write that took the list before this is not
		// taken to have applied it.
		f.pending = []pendingForget{{key: "", under: true, seq: f.seq}}
		return
	}
	f.pending = append(f.pending, p)
}

// applied drops the forgets a committed write applied: those up to seq,
// the last of the list it took.
func (f *historyFile) applied(seq uint64) {
	if seq == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.pending[:0:0]
	for _, p := range f.pending {
		if p.seq > seq {
			kept = append(kept, p)
		}
	}
	f.pending = kept
}

// pendingOn reports whether a pending forget covers key (under false) or
// any key under the prefix key (under true): what it covers is read as a
// miss until the forget is applied.
func (f *historyFile) pendingOn(key string, under bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pending {
		switch {
		case !under && !p.under && p.key == key,
			!under && p.under && strings.HasPrefix(key, p.key),
			under && strings.HasPrefix(p.key, key),
			under && p.under && strings.HasPrefix(key, p.key):
			return true
		}
	}
	return false
}

// withConn runs fn on the file's connection, got before bounded (which
// has a deadline) is done. SQLite's busy wait stays HistoryWait unless
// less than that, by more than historyBusyMargin, is left of bounded's
// time: then it is set to what is left for fn, and back after.
func (f *historyFile) withConn(bounded context.Context, fn func(*sql.Conn) error) error {
	conn, err := f.db.Conn(bounded)
	if err != nil {
		return err
	}
	defer conn.Close()
	d, _ := bounded.Deadline()
	if left := time.Until(d); left < HistoryWait-historyBusyMargin {
		// Statements that set the wait never wait themselves: no context
		// to watch.
		ms := max(left.Milliseconds(), 1)
		if _, err := conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout = %d`, ms)); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout = %d`, HistoryWait.Milliseconds()))
	}
	return fn(conn)
}

// ContendedMisses is how many of this handle's operations did not get
// the database within HistoryWait: reads answered as misses, keeps
// skipped, forgets held pending.
func (c *HistoryCache) ContendedMisses() int64 { return c.misses.Load() }

// settle is an operation's outcome: done, or, past the bound (or SQLite
// busy), a miss counted and no error; ctx's own end, or any other fault,
// is the caller's error.
func (c *HistoryCache) settle(ctx, bounded context.Context, err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case bounded.Err() != nil || isBusy(err):
		c.misses.Add(1)
		return false, nil
	}
	return false, err
}

// query runs fn, a read, within the bound: the wait for the connection
// is bounded by the deadline, and fn's statements, run on the caller's
// ctx (a statement under a deadline costs the driver a watch of its
// own), by SQLite's busy wait (withConn). ok false with no error is a
// miss.
func (c *HistoryCache) query(ctx context.Context, fn func(context.Context, *sql.Conn) error) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	err := c.f.withConn(bounded, func(conn *sql.Conn) error { return fn(ctx, conn) })
	return c.settle(ctx, bounded, err)
}

// write runs fn in a write transaction when its turn comes, within the
// bound, after the forgets still pending: the waits for the turn and the
// connection are bounded by the deadline, the transaction's statements
// (on the caller's ctx) by SQLite's busy wait, as query's. ok false with
// no error: it did not get the database, and nothing was written.
func (c *HistoryCache) write(ctx context.Context, fn func(context.Context, *sql.Tx) error) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	var applied uint64
	release, err := c.f.take(bounded)
	if err == nil {
		err = c.f.withConn(bounded, func(conn *sql.Conn) error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			p := c.f.pendingNow()
			for _, f := range p {
				if err := forgetTx(ctx, tx, f); err != nil {
					return err
				}
			}
			if err := fn(ctx, tx); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			if len(p) > 0 {
				applied = p[len(p)-1].seq
			}
			return nil
		})
		release()
	}
	c.f.applied(applied)
	return c.settle(ctx, bounded, err)
}
