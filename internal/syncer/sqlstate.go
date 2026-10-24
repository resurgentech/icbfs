package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

// SQLState keeps a pair's base in a SQLite database file. Each Save
// writes only what changed since the last Load or Save.
type SQLState struct {
	db *sql.DB

	mu   sync.Mutex
	last map[string]Base
}

// OpenState opens (creating) the database at path.
func OpenState(path string) (*SQLState, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`CREATE TABLE IF NOT EXISTS base (
			key TEXT PRIMARY KEY, path TEXT NOT NULL, dir INTEGER NOT NULL, size INTEGER NOT NULL,
			mtime INTEGER NOT NULL, local_id TEXT NOT NULL, remote_id TEXT NOT NULL,
			remote_version TEXT NOT NULL, hash TEXT NOT NULL, acl_hash TEXT NOT NULL,
			created INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("state %s: %w", path, err)
		}
	}
	// A state from before creation times were kept: none recorded, so
	// the next pass sets every item's (a metadata write each, no copy).
	var has int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('base') WHERE name = 'created'`).Scan(&has); err != nil {
		db.Close()
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if has == 0 {
		if _, err := db.Exec(`ALTER TABLE base ADD COLUMN created INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("state %s: %w", path, err)
		}
	}
	return &SQLState{db: db}, nil
}

// Close closes the database.
func (s *SQLState) Close() error { return s.db.Close() }

func (s *SQLState) Load(ctx context.Context) (map[string]Base, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, path, dir, size, mtime, local_id, remote_id, remote_version, hash, acl_hash, created FROM base`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Base{}
	for rows.Next() {
		var k string
		var b Base
		var dir int
		var mtime, created int64
		if err := rows.Scan(&k, &b.Path, &dir, &b.Size, &mtime, &b.LocalID, &b.RemoteID, &b.RemoteVersion, &b.Hash, &b.ACLHash, &created); err != nil {
			return nil, err
		}
		b.Dir = dir != 0
		if mtime != 0 {
			b.ModTime = time.Unix(0, mtime).UTC()
		}
		if created != 0 {
			b.Created = time.Unix(0, created).UTC()
		}
		out[k] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.last = map[string]Base{}
	for k, b := range out {
		s.last[k] = b
	}
	s.mu.Unlock()
	return out, nil
}

func (s *SQLState) Save(ctx context.Context, base map[string]Base) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	del, err := tx.PrepareContext(ctx, `DELETE FROM base WHERE key = ?`)
	if err != nil {
		return err
	}
	put, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO base
		(key, path, dir, size, mtime, local_id, remote_id, remote_version, hash, acl_hash, created) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	for k := range s.last {
		if _, ok := base[k]; !ok {
			if _, err := del.ExecContext(ctx, k); err != nil {
				return err
			}
		}
	}
	for k, b := range base {
		if old, ok := s.last[k]; ok && sameBase(old, b) {
			continue
		}
		dir := 0
		if b.Dir {
			dir = 1
		}
		var mtime, created int64
		if !b.ModTime.IsZero() {
			mtime = b.ModTime.UnixNano()
		}
		if !b.Created.IsZero() {
			created = b.Created.UnixNano()
		}
		if _, err := put.ExecContext(ctx, k, b.Path, dir, b.Size, mtime, b.LocalID, b.RemoteID, b.RemoteVersion, b.Hash, b.ACLHash, created); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.last = map[string]Base{}
	for k, b := range base {
		s.last[k] = b
	}
	return nil
}

func sameBase(a, b Base) bool {
	return a.Path == b.Path && a.Dir == b.Dir && a.Size == b.Size && a.ModTime.Equal(b.ModTime) &&
		a.LocalID == b.LocalID && a.RemoteID == b.RemoteID && a.RemoteVersion == b.RemoteVersion &&
		a.Hash == b.Hash && a.ACLHash == b.ACLHash && a.Created.Equal(b.Created)
}
