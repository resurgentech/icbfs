// Package memside is an in-memory syncer.Side, for tests: a folder or a
// drive, with ids, versions (every content kept), a trash, stored hashes,
// read counts, and injectable failures.
package memside

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
)

// Side is the in-memory side.
type Side struct {
	mu   sync.Mutex
	caps syncer.Caps
	// Drive makes it behave like icbfs: versions, a trash, stored hashes.
	drive bool
	fold  bool
	items map[string]*Item
	trash []*trashed
	next  int

	// Reads counts content reads, by path.
	Reads map[string]int
	// Writes counts content writes, by path.
	Writes map[string]int
	// Fail makes an operation on a path fail: "write:a/b", "read:x",
	// "list", "move:a", "remove:a".
	Fail map[string]error
}

// Item is a file or directory.
type Item struct {
	Path    string
	Dir     bool
	Data    []byte
	ModTime time.Time
	ID      string
	Version int
	// History is every content the item has had, oldest first, on a
	// drive side.
	History [][]byte
	ACL     string
	Hash    string
}

type trashed struct {
	id      string
	item    *Item
	deleted time.Time
}

// NewFolder is a folder: no versions, no trash. ids reports whether it
// has stable ids (NTFS) or not (a WebDAV drive); res is its time
// resolution.
func NewFolder(ids bool, res time.Duration) *Side {
	return &Side{caps: syncer.Caps{IDs: ids, TimeResolution: res}, fold: true, items: map[string]*Item{},
		Reads: map[string]int{}, Writes: map[string]int{}, Fail: map[string]error{}}
}

// NewDrive is a drive: ids, versions, a trash, stored hashes.
func NewDrive() *Side {
	s := NewFolder(true, time.Nanosecond)
	s.drive = true
	return s
}

// WithACLs makes the side keep real ACLs.
func (s *Side) WithACLs() *Side { s.caps.ACLs = true; return s }

func (s *Side) key(p string) string { return syncer.Key(p, s.fold) }

func (s *Side) fail(op, p string) error {
	if err, ok := s.Fail[op+":"+p]; ok {
		return err
	}
	return nil
}

func (s *Side) id() string { s.next++; return fmt.Sprintf("i%d", s.next) }

// --- for tests ---

// Put makes path a file with data and modTime, making its parents.
func (s *Side) Put(p string, data string, mod time.Time) *Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.put(p, []byte(data), mod)
}

func (s *Side) put(p string, data []byte, mod time.Time) *Item {
	s.mkdirs(path.Dir(p))
	it, ok := s.items[s.key(p)]
	if !ok {
		it = &Item{Path: p, ID: s.id()}
		s.items[s.key(p)] = it
	}
	it.Data = append([]byte(nil), data...)
	it.ModTime = mod
	it.Version++
	it.Hash = ""
	if s.drive {
		it.History = append(it.History, it.Data)
	}
	return it
}

func (s *Side) mkdirs(p string) {
	if p == "." || p == "" {
		return
	}
	s.mkdirs(path.Dir(p))
	if _, ok := s.items[s.key(p)]; !ok {
		s.items[s.key(p)] = &Item{Path: p, Dir: true, ID: s.id()}
	}
}

// MkdirAll makes a directory and its parents.
func (s *Side) MkdirAll(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirs(p)
}

// Rename renames as a user would: the item (and what's under it) keeps
// its id, or, on a side without stable ids, gets new ones.
func (s *Side) Rename(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.move(from, to)
	if !s.caps.IDs {
		for k, it := range s.items {
			if k == s.key(to) || strings.HasPrefix(k, s.key(to)+"/") {
				it.ID = s.id()
			}
		}
	}
}

// Delete removes a file or a directory with everything under it, as a
// user would.
func (s *Side) Delete(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(p)
	for ik, it := range s.items {
		if ik == k || strings.HasPrefix(ik, k+"/") {
			if s.drive && !it.Dir {
				s.trash = append(s.trash, &trashed{id: s.id(), item: it, deleted: time.Now()})
			}
			delete(s.items, ik)
		}
	}
}

// Get is an item, if it exists.
func (s *Side) Get(p string) (*Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[s.key(p)]
	return it, ok
}

// Tree renders the side: "path=content" for files, "path/" for
// directories, sorted.
func (s *Side) Tree() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []string
	for _, it := range s.items {
		if it.Dir {
			rows = append(rows, it.Path+"/")
		} else {
			rows = append(rows, it.Path+"="+string(it.Data))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, " ")
}

// Contents is every content the side holds or ever held (a drive's
// history and trash included).
func (s *Side) Contents() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	add := func(it *Item) {
		out[string(it.Data)] = true
		for _, h := range it.History {
			out[string(h)] = true
		}
	}
	for _, it := range s.items {
		if !it.Dir {
			add(it)
		}
	}
	for _, t := range s.trash {
		add(t.item)
	}
	return out
}

// TrashCount is how many files are in the trash.
func (s *Side) TrashCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.trash)
}

// --- syncer.Side ---

func (s *Side) Caps() syncer.Caps { return s.caps }

func (s *Side) entry(it *Item) syncer.Entry {
	e := syncer.Entry{Path: it.Path, Dir: it.Dir, Size: int64(len(it.Data)), ModTime: it.ModTime, ACL: it.ACL}
	if s.caps.IDs {
		e.ID = it.ID
	}
	if s.drive && !it.Dir {
		e.Version = fmt.Sprintf("%s.v%d", it.ID, it.Version)
	}
	if s.caps.TimeResolution >= time.Second {
		e.ModTime = it.ModTime.Truncate(s.caps.TimeResolution)
	}
	return e
}

func (s *Side) List(ctx context.Context) ([]syncer.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.Fail["list"]; ok {
		return nil, err
	}
	var out []syncer.Entry
	for _, it := range s.items {
		out = append(out, s.entry(it))
	}
	return out, nil
}

func (s *Side) Read(ctx context.Context, p string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("read", p); err != nil {
		return nil, err
	}
	it, ok := s.items[s.key(p)]
	if !ok || it.Dir {
		return nil, fmt.Errorf("read %s: %w", p, os.ErrNotExist)
	}
	s.Reads[p]++
	return io.NopCloser(bytes.NewReader(it.Data)), nil
}

func (s *Side) Write(ctx context.Context, p string, r io.Reader, size int64, mod time.Time) (syncer.Entry, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return syncer.Entry{}, err
	}
	if int64(len(data)) != size {
		return syncer.Entry{}, fmt.Errorf("write %s: %d bytes, want %d", p, len(data), size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("write", p); err != nil {
		return syncer.Entry{}, err
	}
	if it, ok := s.items[s.key(p)]; ok && it.Dir {
		return syncer.Entry{}, fmt.Errorf("write %s: is a directory", p)
	}
	s.Writes[p]++
	it := s.put(p, data, mod)
	it.Path = p
	return s.entry(it), nil
}

func (s *Side) Mkdir(ctx context.Context, p string) (syncer.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("mkdir", p); err != nil {
		return syncer.Entry{}, err
	}
	s.mkdirs(p)
	it := s.items[s.key(p)]
	it.Path = p
	return s.entry(it), nil
}

func (s *Side) move(from, to string) {
	fk, tk := s.key(from), s.key(to)
	s.mkdirs(path.Dir(to))
	moved := map[string]*Item{}
	for k, it := range s.items {
		switch {
		case k == fk:
			delete(s.items, k)
			it.Path = to
			moved[tk] = it
		case strings.HasPrefix(k, fk+"/"):
			delete(s.items, k)
			it.Path = to + it.Path[len(from):]
			moved[tk+k[len(fk):]] = it
		}
	}
	for k, it := range moved {
		s.items[k] = it
	}
}

func (s *Side) Move(ctx context.Context, from, to string) (syncer.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("move", from); err != nil {
		return syncer.Entry{}, err
	}
	if _, ok := s.items[s.key(from)]; !ok {
		return syncer.Entry{}, fmt.Errorf("move %s: %w", from, os.ErrNotExist)
	}
	if s.key(from) != s.key(to) {
		if _, taken := s.items[s.key(to)]; taken {
			return syncer.Entry{}, fmt.Errorf("move to %s: %w", to, os.ErrExist)
		}
	}
	s.move(from, to)
	return s.entry(s.items[s.key(to)]), nil
}

// ErrNotEmpty is Remove on a directory with something in it.
var ErrNotEmpty = errors.New("directory not empty")

func (s *Side) Remove(ctx context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("remove", p); err != nil {
		return err
	}
	k := s.key(p)
	it, ok := s.items[k]
	if !ok {
		return fmt.Errorf("remove %s: %w", p, os.ErrNotExist)
	}
	if it.Dir {
		for ik := range s.items {
			if strings.HasPrefix(ik, k+"/") {
				return fmt.Errorf("remove %s: %w", p, ErrNotEmpty)
			}
		}
	} else if s.drive {
		s.trash = append(s.trash, &trashed{id: s.id(), item: it, deleted: time.Now()})
	}
	delete(s.items, k)
	return nil
}

func (s *Side) SetACL(ctx context.Context, p, sddl string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[s.key(p)]
	if !ok {
		return fmt.Errorf("set acl %s: %w", p, os.ErrNotExist)
	}
	it.ACL = sddl
	return nil
}

// --- syncer.Hashes and syncer.Trash, on a drive ---

// Hashing is the drive's optional parts, which a folder doesn't have.
type Hashing struct{ *Side }

// Drive is the drive's view with hashes and the trash.
func (s *Side) Drive() Hashing { return Hashing{s} }

func (h Hashing) StoredHash(ctx context.Context, p string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	it, ok := h.items[h.key(p)]
	if !ok || it.Hash == "" {
		return "", false
	}
	return it.Hash, true
}

func (h Hashing) StoreHash(ctx context.Context, p, hash string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	it, ok := h.items[h.key(p)]
	if !ok {
		return os.ErrNotExist
	}
	it.Hash = hash
	return nil
}

func (h Hashing) TrashedFiles(ctx context.Context, size int64) ([]syncer.Trashed, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []syncer.Trashed
	for _, t := range h.trash {
		if int64(len(t.item.Data)) == size {
			out = append(out, syncer.Trashed{ID: t.id, Path: t.item.Path, Size: size, DeletedAt: t.deleted})
		}
	}
	return out, nil
}

func (h Hashing) TrashedHash(ctx context.Context, t syncer.Trashed) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range h.trash {
		if x.id == t.ID {
			h.Reads["trash:"+x.item.Path]++
			sum := sha256.Sum256(x.item.Data)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", os.ErrNotExist
}

func (h Hashing) Restore(ctx context.Context, t syncer.Trashed, to string) (syncer.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, x := range h.trash {
		if x.id == t.ID {
			if _, taken := h.items[h.key(to)]; taken {
				return syncer.Entry{}, os.ErrExist
			}
			h.trash = append(h.trash[:i], h.trash[i+1:]...)
			h.mkdirs(path.Dir(to))
			x.item.Path = to
			h.items[h.key(to)] = x.item
			return h.entry(x.item), nil
		}
	}
	return syncer.Entry{}, os.ErrNotExist
}

var (
	_ syncer.Side   = (*Side)(nil)
	_ syncer.Hashes = Hashing{}
	_ syncer.Trash  = Hashing{}
)
