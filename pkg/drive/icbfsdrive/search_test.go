package icbfsdrive

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// oldSearch is drive.Mirror's search as it was until 2026-11-17 (icbfs
// 8aff656, pkg/drive/mirror.go:1166-1206), word for word but for its
// receiver: how a mirror found a source item's copy on a drive that isn't
// an OriginFinder. A listing of the whole drive per lookup, so it is gone
// from the product (a mirror refuses such a drive); it is kept here only
// to prove the lookups that replace it give the same answers: the newest
// live copy, else the first item it is an alias on, else its trash entry.
func oldSearch(ctx context.Context, d drive.Drive, source, sourceID string) (drive.Item, *drive.TrashEntry, error) {
	var found, alias *drive.Item
	for cursor := ""; ; {
		page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return drive.Item{}, nil, err
		}
		for i := range page.Items {
			it := page.Items[i]
			if it.Origin != nil && it.Origin.Drive == source && it.Origin.ID == sourceID {
				if found == nil || it.Origin.Copied.After(found.Origin.Copied) {
					found = &it // the newest claim wins
				}
			} else if alias == nil && it.Origin != nil && it.Origin.Drive == source && hasAlias(it.Origin, sourceID) {
				it.Alias = sourceID
				alias = &it
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if found != nil {
		return *found, nil, nil
	}
	if alias != nil {
		return *alias, nil, nil // folded into another's item
	}
	trash, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: source, OriginID: sourceID, Limit: 1})
	if err != nil {
		return drive.Item{}, nil, err
	}
	if len(trash.Entries) > 0 {
		return drive.Item{}, &trash.Entries[0], nil
	}
	return drive.Item{}, nil, fmt.Errorf("source item %s: %w", sourceID, drive.ErrNotFound)
}

// found is what a lookup's answer is compared by: the item (id, path, the
// alias asked by, its origin) or the trash entry, or not found, or another
// error.
type found struct {
	item, path, alias, origin, version string
	trash, trashItem                   string
	err                                string
}

// kind is which of the answers it is.
func (f found) kind() string {
	switch {
	case f.err != "":
		return f.err
	case f.trash != "":
		return "trash"
	case f.alias != "":
		return "alias"
	}
	return "live"
}

func foundOf(it drive.Item, te *drive.TrashEntry, err error) found {
	switch {
	case errors.Is(err, drive.ErrNotFound):
		return found{err: "not found"}
	case err != nil:
		return found{err: err.Error()}
	case te != nil:
		return found{trash: te.ID, trashItem: te.ItemID}
	}
	f := found{item: it.ID, path: it.Path, alias: it.Alias}
	if it.Origin != nil {
		f.origin, f.version = it.Origin.ID, it.Origin.Version
	}
	return f
}

// TestFindByOriginMatchesOldSearch (2026-11-17): a mirror finds a source
// item's copy by a lookup (FindByOrigin) and nothing else, its
// whole-drive search gone; the lookups that replace the search give its
// answers. Over random sequences of what happens to a mirror's drive
// (applies that create, update, move and fold items and folders, unfold
// and promote aliases; removes; undone passes; second claims, a copy made
// twice; and, as through a mount, moves, deletes and restores from the
// trash), made on a plainDrive, every lookup in plainDrive's index gives
// what the old search gives, and so does Known: the newest live copy,
// else the item it's an alias on, else its trash entry, else not found.
//
// icbfs's own FindByOrigin, which every daemon's mirror uses and which
// this change doesn't touch, is looked up too, and where it answers
// otherwise than the search that is logged, not failed: it always has
// in a few cases (the report of 2026-11-17 lists them).
//
// And told where the item is (FindByOriginHint, T82-13): before each
// plain lookup, the same lookup with a hint that is right (where the
// search found it), stale (where an earlier lookup found it), wrong
// (nowhere), or another item's path, some of them after the drive
// forgot every path it remembered (a start). Every hinted answer is the
// plain lookup's, exactly.
// ICBFS_SEARCH_SEEDS and ICBFS_SEARCH_STEPS run it longer.
func TestFindByOriginMatchesOldSearch(t *testing.T) {
	seeds, steps := envInt("ICBFS_SEARCH_SEEDS", 6), envInt("ICBFS_SEARCH_STEPS", 60)
	store := miniotest.Store(t)
	var mu sync.Mutex
	answers := map[string]int{}      // of each kind, over every seed
	icbfsDiffers := map[string]int{} // by case
	t.Cleanup(func() {
		// The answers the sequences reached: each kind, or the test
		// proved less than it says.
		t.Logf("answers: %v; icbfs's FindByOrigin answered otherwise: %v", answers, icbfsDiffers)
		for _, kind := range []string{"live", "alias", "trash", "not found"} {
			if answers[kind] == 0 && !t.Failed() {
				t.Errorf("no lookup answered %q: the sequences don't reach it", kind)
			}
		}
	})
	for seed := range seeds {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			t.Parallel()
			idrv := newDrive(t, store, icbfs.Settings{Name: fmt.Sprint("search-", seed), Size: 1 << 30, RootMode: 0o777}, "")
			d := plain(t, idrv)
			r := &searchRun{t: t, ctx: system(), d: d, idrv: idrv, m: drive.NewMirror(d, srcDrive), rng: rand.New(rand.NewPCG(uint64(seed), 2026_11_17)),
				hintRng: rand.New(rand.NewPCG(uint64(seed), 82_13)), lastPath: map[string]string{}, hints: map[string]int{},
				src: &source{}, versions: map[string]int{}, bodies: map[string]string{}, kinds: map[string]int{}, answers: map[string]int{}, icbfsDiffers: map[string]int{}}
			if err := r.m.Err(); err != nil {
				t.Fatal(err)
			}
			for step := range steps {
				what := r.change()
				r.log = append(r.log, fmt.Sprintf("%d: %s", step, what))
				if !r.compare() {
					t.Fatalf("after step %d; the changes:\n%s", step, strings.Join(r.log, "\n"))
				}
			}
			t.Logf("%d changes, %d refused (%v); answers %v; hinted lookups %v", steps, r.refused, r.kinds, r.answers, r.hints)
			for _, kind := range []string{"right", "stale", "wrong", "another"} {
				if r.hints[kind] == 0 {
					t.Errorf("no lookup was given a %s hint", kind)
				}
			}
			mu.Lock()
			for k, n := range r.answers {
				answers[k] += n
			}
			for k, n := range r.icbfsDiffers {
				icbfsDiffers[k] += n
			}
			mu.Unlock()
		})
	}
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// The source items: files and folders; where they go; their bytes.
var (
	searchFiles   = []string{"f0", "f1", "f2", "f3", "f4", "f5", "f6", "f7"}
	searchFolders = []string{"d0", "d1", "d2"}
	searchDirs    = []string{"/A", "/B", "/C", "/D"}
	searchNames   = []string{"x.txt", "y.txt", "z.txt"}
	searchBodies  = []string{mirrorData("one"), mirrorData("two"), mirrorData("three")}
)

// searchRun is one random sequence of changes to a mirror's drive.
type searchRun struct {
	t        *testing.T
	ctx      context.Context
	d        plainDrive // the mirror's drive, and what the changes go through
	idrv     *Drive     // the icbfs drive behind it
	m        *drive.Mirror
	rng      *rand.Rand
	src      *source
	versions map[string]int    // a source file's version now
	bodies   map[string]string // and its bytes
	passes   int
	claims   int
	log      []string
	refused  int
	kinds    map[string]int // the changes made, by kind
	answers  map[string]int // the old search's answers, by kind
	// icbfsDiffers counts icbfs FindByOrigin's answers that aren't the
	// search's, by case.
	icbfsDiffers map[string]int
	// The hinted lookups: their own random numbers (the changes are the
	// same sequences as without them), where each id was last found, and
	// how many lookups had each kind of hint.
	hintRng  *rand.Rand
	lastPath map[string]string
	hints    map[string]int
}

func (r *searchRun) pick(s []string) string { return s[r.rng.IntN(len(s))] }

// change makes one random change and says what it was.
func (r *searchRun) change() string {
	ctx := r.ctx
	var what string
	var err error
	switch k := r.rng.IntN(100); {
	case k < 36: // a source file applied: new, a new version, moved, or folded
		id := r.pick(searchFiles)
		if r.versions[id] == 0 || r.rng.IntN(3) == 0 {
			r.versions[id]++
			r.bodies[id] = r.pick(searchBodies)
		}
		p := r.pick(searchDirs) + "/" + r.pick(searchNames)
		if r.rng.IntN(4) == 0 {
			p = "/" + r.pick(searchNames)
		}
		it := r.src.item(id, strconv.Itoa(r.versions[id]), p, r.bodies[id], time.Time{})
		if r.rng.IntN(3) == 0 {
			winner := r.pick(searchFiles)
			foldPath := ""
			if r.rng.IntN(3) == 0 {
				foldPath = p
			}
			it = into(it, winner, foldPath)
			what = fmt.Sprintf("apply %s v%d at %s, fold into %s (path %q)", id, r.versions[id], p, winner, foldPath)
		} else {
			what = fmt.Sprintf("apply %s v%d at %s", id, r.versions[id], p)
		}
		var a drive.Applied
		a, err = r.m.Apply(ctx, it)
		what += fmt.Sprintf(": %v", a.Actions)
		r.kinds["apply"]++
	case k < 46: // a source folder applied: made, or moved with all in it
		id, p := r.pick(searchFolders), r.pick(searchDirs)
		var a drive.Applied
		a, err = r.m.Apply(ctx, drive.SourceItem{Origin: drive.Origin{ID: id, Version: "1"}, Path: p, Type: drive.TypeDir})
		what = fmt.Sprintf("apply folder %s at %s: %v", id, p, a.Actions)
		r.kinds["folder"]++
	case k < 50: // a second claim on a source file, newer: a copy made twice (drive.Copy)
		id := r.pick(searchFiles)
		r.claims++
		p := fmt.Sprintf("/copies/c%d", r.claims)
		o := &drive.Origin{Drive: srcDrive, ID: id, Version: "copy", Path: "/src/copy", Copied: time.Now()}
		_, err = r.d.Write(ctx, p, strings.NewReader(r.pick(searchBodies)), drive.Condition{}, drive.WriteOptions{Parents: true, Origin: o})
		what = fmt.Sprintf("a copy of %s at %s", id, p)
		r.kinds["claim"]++
	case k < 65: // removed at the source, in a pass of its own
		id := r.pick(append(append([]string{}, searchFiles...), searchFolders...))
		r.passes++
		var ok bool
		ok, err = r.m.Remove(drive.WithPass(ctx, fmt.Sprint("pass", r.passes)), id)
		what = fmt.Sprintf("remove %s in pass%d: %v", id, r.passes, ok)
		r.kinds["remove"]++
	case k < 72: // a pass undone
		if r.passes == 0 {
			return "no pass to undo"
		}
		pass := fmt.Sprint("pass", 1+r.rng.IntN(r.passes))
		var n int
		n, err = r.m.Undo(ctx, pass)
		what = fmt.Sprintf("undo %s: %d", pass, n)
		r.kinds["undo"]++
	case k < 82: // moved on the drive, as through a mount
		it, ok := r.someItem()
		if !ok {
			return "nothing to move"
		}
		to := r.pick(searchDirs) + "/m" + strconv.Itoa(r.rng.IntN(5))
		if it.Type == drive.TypeFile && r.rng.IntN(2) == 0 {
			to = "/" + r.pick(searchNames)
		}
		_, err = r.d.Move(ctx, drive.ItemRef{ID: it.ID, Path: it.Path}, to, false)
		what = fmt.Sprintf("move %s to %s on the drive", it.Path, to)
		r.kinds["move"]++
	case k < 90: // deleted on the drive
		it, ok := r.someItem()
		if !ok {
			return "nothing to delete"
		}
		err = r.d.Delete(ctx, drive.ItemRef{ID: it.ID, Path: it.Path}, true, drive.Condition{})
		what = fmt.Sprintf("delete %s on the drive", it.Path)
		r.kinds["delete"]++
	default: // restored from the trash on the drive
		page, terr := r.d.Trash(ctx, drive.TrashOptions{})
		if terr != nil {
			r.t.Fatal(terr)
		}
		if len(page.Entries) == 0 {
			return "nothing to restore"
		}
		// By path, then id (made in time order), not the trash's order,
		// which goes by deletion times to the second: the same seed
		// picks the same entry however fast the changes ran.
		slices.SortFunc(page.Entries, func(a, b drive.TrashEntry) int {
			return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.ID, b.ID))
		})
		e := page.Entries[r.rng.IntN(len(page.Entries))]
		_, err = r.d.Restore(ctx, e.ID, drive.RestoreOptions{Parents: true})
		what = fmt.Sprintf("restore %s (%s) on the drive", e.Path, e.ID)
		r.kinds["restore"]++
	}
	if err != nil {
		r.refused++
		what += fmt.Sprintf(" (refused: %v)", err)
	}
	return what
}

// someItem is a live item on the drive, picked at random.
func (r *searchRun) someItem() (drive.Item, bool) {
	page, err := r.d.List(r.ctx, drive.ByPath("/"), drive.ListOptions{Depth: 1 << 30})
	if err != nil {
		r.t.Fatal(err)
	}
	if len(page.Items) == 0 {
		return drive.Item{}, false
	}
	return page.Items[r.rng.IntN(len(page.Items))], true
}

// compare looks every source item up, and one never seen: in
// plainDrive's index, by Known, and by the old search. All the same:
// true. It also notes where icbfs's FindByOrigin answers otherwise.
func (r *searchRun) compare() bool {
	ok := true
	ids := append(append([]string{"never"}, searchFiles...), searchFolders...)
	for _, id := range ids {
		sIt, sTe, sErr := oldSearch(r.ctx, r.idrv, srcDrive, id)
		want := foundOf(sIt, sTe, sErr)
		r.answers[want.kind()]++
		if got := foundOf(r.d.lookup(r.ctx, srcDrive, id)); got != want {
			r.t.Errorf("the index's FindByOrigin(%s) = %+v; the old search: %+v", id, got, want)
			ok = false
		}
		it, trashed, isKnown, err := r.m.Known(r.ctx, id)
		if !isKnown && err == nil {
			err = drive.ErrNotFound
		}
		if known := foundOf(it, trashed, err); known != want {
			r.t.Errorf("Known(%s) = %+v; the old search: %+v", id, known, want)
			ok = false
		}
		hinted := r.hinted(id, sIt.Path, ids)
		it, te, err := r.idrv.FindByOrigin(r.ctx, srcDrive, id)
		plainAnswer := foundOf(it, te, err)
		for _, h := range hinted {
			if h.got != plainAnswer {
				r.t.Errorf("icbfs's FindByOriginHint(%s, %s hint %q) = %+v; FindByOrigin: %+v", id, h.kind, h.hint, h.got, plainAnswer)
				ok = false
			}
		}
		if err == nil && te == nil {
			r.lastPath[id] = it.Path
		}
		if got := plainAnswer; got != want {
			c := "other"
			if newestClaimTrashed(id, sIt, sTe, sErr, te, err) {
				c = "newest claim trashed"
			}
			if r.icbfsDiffers[c]++; r.icbfsDiffers[c] <= 2 {
				r.t.Logf("icbfs's FindByOrigin(%s) = %+v; the old search: %+v (%s), after:\n%s", id, got, want, c, strings.Join(r.log, "\n"))
			}
		}
	}
	return ok
}

// hintedAnswer is one hinted lookup's answer.
type hintedAnswer struct {
	kind, hint string
	got        found
}

// hinted looks id up in icbfs with hints of each kind, in a random order,
// sometimes after the drive forgot the paths it remembered; right is
// where the search found it ("" if not live), ids every id looked up.
func (r *searchRun) hinted(id, right string, ids []string) []hintedAnswer {
	if r.hintRng.IntN(3) == 0 {
		r.idrv.idPaths.forget()
	}
	other := r.lastPath[ids[r.hintRng.IntN(len(ids))]]
	cands := []struct{ kind, hint string }{
		{"right", right},
		{"stale", r.lastPath[id]},
		{"wrong", "/nowhere/" + id},
		{"another", other},
	}
	r.hintRng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	var out []hintedAnswer
	for _, c := range cands {
		if c.hint == "" || c.kind == "stale" && c.hint == right || c.kind == "another" && c.hint == right {
			continue
		}
		r.hints[c.kind]++
		out = append(out, hintedAnswer{c.kind, c.hint, foundOf(r.idrv.FindByOriginHint(r.ctx, srcDrive, id, c.hint))})
	}
	return out
}

// newestClaimTrashed is one case where icbfs's lookup has always
// answered otherwise than the search (icbfs 8aff656 and before; not
// changed here): two items claim one source item (a copy made twice),
// and the newest claim, the one its origin record names, is in the
// trash. The lookup answers that claim's trash entry. The search
// answered an older claim: a live one if there was one (a live claim
// before any trash entry), else the trash entry the trash lists first.
func newestClaimTrashed(id string, sIt drive.Item, sTe *drive.TrashEntry, sErr error, te *drive.TrashEntry, err error) bool {
	if err != nil || te == nil || te.Origin == nil || te.Origin.ID != id || sErr != nil {
		return false
	}
	older := sIt.Origin
	if sTe != nil {
		older = sTe.Origin
	} else if sIt.Alias != "" {
		return false
	}
	return older != nil && older.ID == id && te.Origin.Copied.After(older.Copied)
}
