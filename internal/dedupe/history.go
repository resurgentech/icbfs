package dedupe

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// The history run (Replace with Options.History, icbfs-cli fs dedupe
// -history). Every version of a group but one becomes a stub naming that
// one, past versions too: each affected item's history is rewritten
// through ReplaceFileHistory, its duplicates as stub states and every
// other version kept (a server-side copy), at the same dates in the same
// order. A rewrite gives every kept version a new id, so the order of the
// work matters:
//
//   - Items are ranked: those without a duplicate of their own first,
//     then those with the most referrers, then the oldest, then by path.
//     A group's canonical copy, the one that stays, is its copy in the
//     best-ranked item, the oldest there; and items are rewritten in rank
//     order, so every stub written names a version that is final by
//     then: in an item done already (its new id), or an earlier version
//     of the item itself (ReplaceFileHistory's self-stub, which names the
//     kept copy's new id). Identical consecutive versions come out so:
//     the newer a stub at the older, unless the content's canonical copy
//     is elsewhere, which both then name.
//   - An item with live referrers has each re-pointed to its version's
//     canonical copy first (a referrer's current stub by RepointRef,
//     one in its past by a rewrite of its history), each old entry
//     dropped (dropRef). A referrer naming a version that stays (a
//     canonical copy, or one with no copy) can't be, and the item is
//     skipped: rewriting it would erase what the stub names.
//   - Then stubs naming a retired holder are re-pointed to a live copy
//     of their content, where there is one; the last one erases the
//     holder.
//
// A file open here, one a writer changed meanwhile, one whose referrers
// can't all be re-pointed, and one whose rewrite is refused otherwise is
// skipped and reported. A killed run leaves each item rewritten or not
// (ReplaceFileHistory's own kill aside), so a rerun does the rest; a run
// with nothing left to do writes nothing.

// histRun is one history run's state.
type histRun struct {
	ctx  context.Context
	f    *icbfs.Filesystem
	res  *Result
	w    *Writes
	skip func(r *Record, reason, detail string)

	inScope map[string]bool
	byItem  map[string][]int             // each item's records, newest first
	canon   map[int]int                  // a duplicate's record -> its canonical copy's
	single  map[content]int              // a content's canonical copy, or its one real copy
	moved   map[string]map[string]string // item -> a version id -> the id its copy got
}

type content struct {
	sha  string
	size int64
}

func (r *Record) content() content { return content{r.SHA256, r.Size} }

// replaceHistory is the history run on res, a full scan.
func replaceHistory(ctx context.Context, f *icbfs.Filesystem, res *Result, w *Writes) error {
	h := &histRun{ctx: ctx, f: f, res: res, w: w, inScope: map[string]bool{}, byItem: map[string][]int{},
		canon: map[int]int{}, single: map[content]int{}, moved: map[string]map[string]string{}}
	h.skip = func(r *Record, reason, detail string) {
		w.Skipped = append(w.Skipped, Skip{Path: r.Path, ItemID: r.ItemID, VersionID: h.id(r), Reason: reason, Detail: detail})
	}
	order := h.rank()
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.item(key); err != nil {
			return err
		}
	}
	return h.holders()
}

// rank ranks the items in scope, picks each group's canonical copy, and
// returns the items in rank order.
func (h *histRun) rank() []string {
	recs := h.res.Records
	type item struct {
		key, path string
		dirty     bool
		referrers int
		oldest    time.Time
	}
	items := map[string]*item{}
	var keys []string
	for i := range recs {
		r := &recs[i]
		h.inScope[r.ItemID] = true
		h.byItem[r.ItemID] = append(h.byItem[r.ItemID], i)
		it := items[r.ItemID]
		if it == nil {
			it = &item{key: r.ItemID, path: r.Path, oldest: r.Date}
			items[r.ItemID] = it
			keys = append(keys, r.ItemID)
		}
		it.referrers += r.Referrers
		if r.Date.Before(it.oldest) {
			it.oldest = r.Date
		}
	}
	for key, idx := range h.byItem {
		seen := map[content]bool{}
		for _, i := range idx {
			if r := &recs[i]; r.Ref == nil {
				items[key].dirty = items[key].dirty || seen[r.content()]
				seen[r.content()] = true
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := items[keys[i]], items[keys[j]]
		switch {
		case a.dirty != b.dirty:
			return !a.dirty
		case a.referrers != b.referrers:
			return a.referrers > b.referrers
		case !a.oldest.Equal(b.oldest):
			return a.oldest.Before(b.oldest)
		}
		return a.path < b.path
	})
	rank := make(map[string]int, len(keys))
	for i, k := range keys {
		rank[k] = i
	}
	// better reports whether record a is a better canonical copy than b:
	// its item ranks first, else (the same item) it is older.
	better := func(a, b int) bool {
		ra, rb := &recs[a], &recs[b]
		if ra.ItemID != rb.ItemID {
			return rank[ra.ItemID] < rank[rb.ItemID]
		}
		if !ra.Date.Equal(rb.Date) {
			return ra.Date.Before(rb.Date)
		}
		return a > b // newest first: the later listed is the older
	}
	for _, g := range h.res.History {
		members := append([]int{g.Canonical}, g.Duplicates...)
		c := members[0]
		for _, m := range members[1:] {
			if better(m, c) {
				c = m
			}
		}
		for _, m := range members {
			if m != c {
				h.canon[m] = c
			}
		}
		h.single[recs[c].content()] = c
	}
	for i := range recs {
		if r := &recs[i]; r.Ref == nil {
			if _, ok := h.single[r.content()]; !ok {
				h.single[r.content()] = i
			}
		}
	}
	return keys
}

// id is the id record r's version has now: its own, or its copy's if a
// rewrite in this run moved it.
func (h *histRun) id(r *Record) string {
	v := r.VersionID
	for m := h.moved[r.ItemID]; m != nil; {
		nv, ok := m[v]
		if !ok {
			break
		}
		v = nv
	}
	return v
}

// recordAt is the record of item key whose version is now version, or -1.
func (h *histRun) recordAt(key, version string) int {
	for _, i := range h.byItem[key] {
		if h.id(&h.res.Records[i]) == version {
			return i
		}
	}
	return -1
}

// target is a stub's target: a version, by its id now, and its SHA-256.
type target struct {
	key, version string
	sha          []byte
}

func (h *histRun) targetOf(c int) (target, error) {
	r := &h.res.Records[c]
	sha, err := hex.DecodeString(r.SHA256)
	return target{r.ItemID, h.id(r), sha}, err
}

// item rewrites item key if it has duplicates: its referrers re-pointed
// first, then its history, each duplicate a stub naming its canonical
// copy. An error only for what ends the run.
func (h *histRun) item(key string) error {
	recs := h.res.Records
	var first *Record // the item's newest duplicate: what a skip reports
	referred := false
	for _, i := range h.byItem[key] {
		if _, ok := h.canon[i]; ok && first == nil {
			first = &recs[i]
		}
		referred = referred || recs[i].Referrers > 0
	}
	if first == nil {
		return nil
	}
	if referred {
		live, err := h.f.LiveReferrers(h.ctx, key)
		if err != nil {
			return err
		}
		plans := map[string]map[string]target{} // referrer -> its stub version -> the new target
		for _, e := range live {
			if e.Pending {
				h.skip(first, SkipReferrers, fmt.Sprintf("a stub of %s being written names it", e.Key))
				return nil
			}
			i := h.recordAt(key, e.TargetVersion)
			c, ok := h.canon[i]
			if i < 0 || !ok || recs[c].ItemID == key {
				h.skip(first, SkipReferrers, fmt.Sprintf("%s's stub names version %s, which stays", e.Key, e.TargetVersion))
				return nil
			}
			t, err := h.targetOf(c)
			if err != nil {
				return err
			}
			if plans[e.Key] == nil {
				plans[e.Key] = map[string]target{}
			}
			plans[e.Key][e.Version] = t
		}
		referrers := make([]string, 0, len(plans))
		for r := range plans {
			referrers = append(referrers, r)
		}
		sort.Strings(referrers)
		for _, r := range referrers {
			if err := h.repoint(r, plans[r]); err != nil {
				reason := historySkipReason(err)
				if reason == "" {
					return fmt.Errorf("re-pointing %s: %w", r, err)
				}
				h.skip(first, reason, fmt.Sprintf("re-pointing its referrer %s: %v", r, err))
				return nil
			}
		}
	}

	hist, cur, err := h.f.FileHistory(h.ctx, key)
	if err != nil {
		return err
	}
	states := make([]icbfs.FileState, len(hist))
	var replaced Tally
	for j := range hist {
		v := &hist[len(hist)-1-j]
		states[j] = icbfs.FileState{Version: v.VersionID}
		if v.Ref != nil {
			continue
		}
		i := h.recordAt(key, v.VersionID)
		c, ok := h.canon[i]
		if i < 0 || !ok {
			continue
		}
		t, err := h.targetOf(c)
		if err != nil {
			return err
		}
		states[j] = icbfs.FileState{RefKey: t.key, RefVersion: t.version, RefSHA256: t.sha}
		replaced.Versions++
		replaced.Bytes += recs[i].Size
	}
	if replaced.Versions == 0 {
		return nil
	}
	if err := h.rewrite(key, hist, cur, states); err != nil {
		reason := historySkipReason(err)
		if reason == "" {
			return fmt.Errorf("%s: %w", first.Path, err)
		}
		h.skip(first, reason, err.Error())
		return nil
	}
	h.w.Replaced.Versions += replaced.Versions
	h.w.Replaced.Bytes += replaced.Bytes
	return nil
}

// repoint re-points referrer key's stubs in plan (by version) to their new
// targets: its current one alone by RepointRef, else by a rewrite of its
// history.
func (h *histRun) repoint(key string, plan map[string]target) error {
	hist, cur, err := h.f.FileHistory(h.ctx, key)
	if err != nil {
		return err
	}
	if t, ok := plan[cur]; ok && len(plan) == 1 {
		if _, err := h.f.RepointRef(h.ctx, key, cur, t.key, t.version, t.sha); err != nil {
			return err
		}
		h.w.Repointed++
		return nil
	}
	states := make([]icbfs.FileState, len(hist))
	n := 0
	for j := range hist {
		v := &hist[len(hist)-1-j]
		states[j] = icbfs.FileState{Version: v.VersionID}
		if t, ok := plan[v.VersionID]; ok && v.Ref != nil {
			states[j] = icbfs.FileState{RefKey: t.key, RefVersion: t.version, RefSHA256: t.sha}
			n++
		}
	}
	if n == 0 {
		return nil
	}
	if err := h.rewrite(key, hist, cur, states); err != nil {
		return err
	}
	h.w.Repointed += n
	return nil
}

// rewrite replaces key's history, hist (newest first, as FileHistory
// gave it with current version cur), by states (oldest first, one for
// each of hist's versions), at hist's dates, and notes the id each
// version's copy got. ReplaceFileHistory takes strictly increasing dates,
// so of versions at one date (a create and its first write, under a
// source time) the older are moved back a nanosecond each: the newest,
// the one a view as of that date shows, keeps it. A past state's mtime
// is its date; the last one's the file's now.
func (h *histRun) rewrite(key string, hist []icbfs.FileVersion, cur string, states []icbfs.FileState) error {
	attr, err := h.f.Stat(h.ctx, key, icbfs.TypeFile)
	if err != nil {
		return err
	}
	for j := len(states) - 1; j >= 0; j-- {
		at := hist[len(hist)-1-j].At
		if j < len(states)-1 && !at.Before(states[j+1].At) {
			at = states[j+1].At.Add(-time.Nanosecond)
		}
		states[j].At = at
		if j < len(states)-1 {
			a := attr
			a.Mtime = time.Time{}
			states[j].Attr = &a
		}
	}
	if err := h.f.ReplaceFileHistory(h.ctx, key, states, cur); err != nil {
		return err
	}
	after, _, err := h.f.FileHistory(h.ctx, key)
	if err != nil {
		return err
	}
	if len(after) != len(hist) {
		return fmt.Errorf("%s: %d versions after its rewrite, %d before", key, len(after), len(hist))
	}
	m := h.moved[key]
	if m == nil {
		m = map[string]string{}
		h.moved[key] = m
	}
	for j := range hist {
		m[hist[j].VersionID] = after[j].VersionID
	}
	return nil
}

// holders re-points the stubs in scope that name a retired holder to a
// live copy of their content, where there is one.
func (h *histRun) holders() error {
	recs := h.res.Records
	holder := map[string]bool{}
	plans := map[string]map[string]target{}
	first := map[string]*Record{}
	for i := range recs {
		r := &recs[i]
		if r.Ref == nil || h.inScope[r.Ref.Key] {
			continue
		}
		is, seen := holder[r.Ref.Key]
		if !seen {
			var err error
			if is, err = h.f.RetiredHolder(h.ctx, r.Ref.Key); err != nil {
				return err
			}
			holder[r.Ref.Key] = is
		}
		c, ok := h.single[r.content()]
		if !is || !ok {
			continue
		}
		t, err := h.targetOf(c)
		if err != nil {
			return err
		}
		if plans[r.ItemID] == nil {
			plans[r.ItemID] = map[string]target{}
			first[r.ItemID] = r
		}
		plans[r.ItemID][h.id(r)] = t
	}
	keys := make([]string, 0, len(plans))
	for k := range plans {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := h.repoint(k, plans[k]); err != nil {
			reason := historySkipReason(err)
			if reason == "" {
				return fmt.Errorf("%s: %w", first[k].Path, err)
			}
			h.skip(first[k], reason, fmt.Sprintf("re-pointing its stub from a retired holder: %v", err))
		}
	}
	return nil
}

// historySkipReason is skipReason, and an open file's.
func historySkipReason(err error) string {
	switch {
	case errors.Is(err, icbfs.ErrFileOpen):
		return SkipOpen
	case errors.Is(err, icbfs.ErrNotRef):
		return SkipChanged
	}
	return skipReason(err)
}
