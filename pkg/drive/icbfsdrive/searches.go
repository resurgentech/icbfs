package icbfsdrive

import (
	"sync"
	"time"
)

// --- searches by id that stopped (T82-15) ---

// A search by id that stops (the search budget, a cut context) keeps the
// folders it found and has not read yet, in the walk's order, so the next
// call for the same id in the same view goes on from there instead of
// reading again every folder it read: a retry reads the next budget's
// worth, and an item at the walk's k-th folder is found by the
// ceil(k/budget)-th call. What is kept is bounded for the whole drive:
// at most maxStopped searches and maxStoppedDirs folders in all (about
// 180 bytes a folder: its key and path; so about 3 MiB at most), each for
// stoppedFor after it stopped; the oldest go first.
const (
	maxStopped     = 16
	maxStoppedDirs = 16384
	stoppedFor     = 2 * time.Minute
)

// stoppedKey is a search: the view's memory it belongs to (the present's
// or a view of the past's) and the id sought.
type stoppedKey struct {
	mem *idPathCache
	id  string
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

// dropPast forgets every stopped search but the present's (present): the
// past changed, so what a view's search had still to read may not be what
// that view reads now.
func (s *stoppedSearches) dropPast(present *idPathCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if k.mem != present {
			s.removeLocked(k, v)
		}
	}
}
