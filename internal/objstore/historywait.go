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

// historyBusyTimeout is SQLite's busy wait for statements on the file
// that are not the cache's own (the settle table's, SettleTable): as it
// was before HistoryWait. The cache's operations set their own, to what
// is left of their bound, and put this back.
const historyBusyTimeout = 10 * time.Second

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
}

// pendingForget is a forget that did not get the database within
// HistoryWait. Until a later write of this process applies it (every
// write applies the pending forgets first, in its own transaction), every
// handle in this process reads what it names as a miss.
type pendingForget struct {
	key   string
	under bool // every key starting with key; else the one key
}

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
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_txlock=immediate", path, historyBusyTimeout.Milliseconds()))
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

// pendingNow is the forgets pending now.
func (f *historyFile) pendingNow() []pendingForget {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pendingForget(nil), f.pending...)
}

// addPending holds a forget that did not get the database.
func (f *historyFile) addPending(p pendingForget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, p)
}

// applied drops the forgets a committed write applied (the first n of
// them: the list only grows at its end until then).
func (f *historyFile) applied(n int) {
	if n == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append([]pendingForget(nil), f.pending[n:]...)
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

// withConn runs fn on the file's connection, got before ctx (which has a
// deadline) is done, with SQLite's busy wait set to what is left of
// ctx's time.
func (f *historyFile) withConn(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := f.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	d, _ := ctx.Deadline()
	ms := max(time.Until(d).Milliseconds(), 1)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, ms)); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout = %d`, historyBusyTimeout.Milliseconds()))
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

// query runs fn, a read, within the bound. ok false with no error is a
// miss.
func (c *HistoryCache) query(ctx context.Context, fn func(context.Context, *sql.Conn) error) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	err := c.f.withConn(bounded, func(conn *sql.Conn) error { return fn(bounded, conn) })
	return c.settle(ctx, bounded, err)
}

// write runs fn in a write transaction when its turn comes, within the
// bound, after the forgets still pending. ok false with no error: it did
// not get the database, and nothing was written.
func (c *HistoryCache) write(ctx context.Context, fn func(context.Context, *sql.Tx) error) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	pending := 0
	release, err := c.f.take(bounded)
	if err == nil {
		err = c.f.withConn(bounded, func(conn *sql.Conn) error {
			tx, err := conn.BeginTx(bounded, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			p := c.f.pendingNow()
			for _, f := range p {
				if err := forgetTx(bounded, tx, f); err != nil {
					return err
				}
			}
			if err := fn(bounded, tx); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			pending = len(p)
			return nil
		})
		release()
	}
	c.f.applied(pending)
	return c.settle(ctx, bounded, err)
}
