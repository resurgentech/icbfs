package icbfsdrive

import (
	"context"
	"sync"
	"time"
)

// --- searches by id that stopped (T82-15) ---

// A search by id that stops (the search budget, a cut context, a store
// error on one folder) keeps the folders it found and has not read yet, in
// the walk's order, so the next call for the same id in the same view goes
// on from there instead of reading again every folder it read: a retry
// reads the next budget's worth, and an item at the walk's k-th folder is
// found by the ceil(k/budget)-th call. What is kept is bounded for the
// whole drive: at most maxStopped searches and maxStoppedDirs folders in
// all (about 180 bytes a folder: its key and path; so about 3 MiB at
// most), each for stoppedFor after it stopped; the oldest go first.
//
// Besides (docs/STATES.md 5.1, T82-15 round 2): one search at a time for
// an id in a view, a second lookup waiting for the first (LK-10); and the
// ids found in no folder, remembered for absentFor (LK-13).
const (
	maxStopped     = 16
	maxStoppedDirs = 16384
	stoppedFor     = 2 * time.Minute
	// maxAbsent bounds the ids remembered as not there, for the whole
	// drive (about 100 bytes each): the oldest go first.
	maxAbsent = 1024
)

// absentFor is how long an id found in no folder (or in the trash) is
// answered "not there" with no search (LK-13). A var for tests.
var absentFor = 10 * time.Minute

// stoppedKey is a search: the moment of the view it is in (zero for the
// present) and the id sought. A view of the past is named by its moment,
// not by the view or its memory, so a lookup whose view was dropped while
// it ran keeps its progress where the next view of that moment finds it
// (ME-5).
type stoppedKey struct {
	at time.Time
	id string
}

type searchDir struct{ key, path string }

// stoppedSearch is a search's progress: the folders still to read, and
// how many it has read in every call so far.
type stoppedSearch struct {
	queue []searchDir
	read  int
	at    time.Time // when it stopped
}

type stoppedSearches struct {
	mu   sync.Mutex
	m    map[stoppedKey]*stoppedSearch
	dirs int // folders queued in all
	// running are the searches under way, each closed when it ends.
	running map[stoppedKey]chan struct{}
	// absent are the ids found nowhere, by when.
	absent map[stoppedKey]time.Time
}

// begin waits until no other lookup searches for key, then marks key's
// search as under way: end ends it. waited says another had been under
// way, so what it found or kept is to be looked at first. It gives up when
// ctx ends, with ctx's error.
func (s *stoppedSearches) begin(ctx context.Context, key stoppedKey) (end func(), waited bool, err error) {
	for {
		s.mu.Lock()
		ch, busy := s.running[key]
		if !busy {
			if s.running == nil {
				s.running = map[stoppedKey]chan struct{}{}
			}
			done := make(chan struct{})
			s.running[key] = done
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				delete(s.running, key)
				s.mu.Unlock()
				close(done)
			}, waited, nil
		}
		s.mu.Unlock()
		waited = true
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, waited, ctx.Err()
		}
	}
}

// take is the search for key that stopped, removed while it runs, or nil
// if there is none (or it stopped longer ago than stoppedFor: the tree may
// have changed under it since).
func (s *stoppedSearches) take(key stoppedKey) *stoppedSearch {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr, ok := s.m[key]
	if !ok {
		return nil
	}
	s.removeLocked(key, sr)
	if time.Since(sr.at) > stoppedFor {
		return nil
	}
	return sr
}

// keep keeps sr, stopped, for key, dropping the oldest to stay within the
// bounds; a search with more folders still to read than maxStoppedDirs is
// not kept (the next call starts again).
func (s *stoppedSearches) keep(key stoppedKey, sr *stoppedSearch) {
	if len(sr.queue) > maxStoppedDirs {
		return
	}
	sr.queue = append([]searchDir(nil), sr.queue...) // not the read part's array
	sr.at = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[stoppedKey]*stoppedSearch{}
	}
	if old, ok := s.m[key]; ok {
		s.removeLocked(key, old)
	}
	for len(s.m) > 0 && (len(s.m) >= maxStopped || s.dirs+len(sr.queue) > maxStoppedDirs) {
		var oldest stoppedKey
		var oldestAt time.Time
		for k, v := range s.m {
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldest, oldestAt = k, v.at
			}
		}
		s.removeLocked(oldest, s.m[oldest])
	}
	s.m[key] = sr
	s.dirs += len(sr.queue)
}

func (s *stoppedSearches) removeLocked(key stoppedKey, sr *stoppedSearch) {
	delete(s.m, key)
	s.dirs -= len(sr.queue)
}

// isAbsent says key's id was found nowhere less than absentFor ago.
func (s *stoppedSearches) isAbsent(key stoppedKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.absent[key]
	if !ok {
		return false
	}
	if time.Since(at) >= absentFor {
		delete(s.absent, key)
		return false
	}
	return true
}

// noteAbsent remembers key's id as found nowhere, dropping the oldest
// past maxAbsent.
func (s *stoppedSearches) noteAbsent(key stoppedKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.absent == nil {
		s.absent = map[stoppedKey]time.Time{}
	}
	for len(s.absent) >= maxAbsent {
		var oldest stoppedKey
		var oldestAt time.Time
		for k, at := range s.absent {
			if oldestAt.IsZero() || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(s.absent, oldest)
	}
	s.absent[key] = time.Now()
}

// present forgets that id was found nowhere in the present: it is back (a
// restore through this drive).
func (s *stoppedSearches) present(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.absent, stoppedKey{id: id})
}

// dropPast forgets every stopped search, and every id found nowhere, but
// the present's: the past changed, so what a view's search had still to
// read may not be what that view reads now, and an item may be there now.
func (s *stoppedSearches) dropPast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if !k.at.IsZero() {
			s.removeLocked(k, v)
		}
	}
	for k := range s.absent {
		if !k.at.IsZero() {
			delete(s.absent, k)
		}
	}
}
