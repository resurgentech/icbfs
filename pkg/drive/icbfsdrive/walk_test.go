package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// listOnly is a drive with nothing but drive.Drive's methods: drive.Walk
// falls back to List on it.
type listOnly struct{ drive.Drive }

// walkCall is one call of a WalkFunc.
type walkCall struct {
	dir   drive.ItemRef
	items []drive.Item
}

func walkCalls(t *testing.T, ctx context.Context, d drive.Drive, ref drive.ItemRef, opts drive.WalkOptions) ([]walkCall, error) {
	t.Helper()
	var calls []walkCall
	err := drive.Walk(ctx, d, ref, opts, func(dir drive.ItemRef, items []drive.Item) error {
		calls = append(calls, walkCall{dir, items})
		return nil
	})
	return calls, err
}

// checkWalk: each call's items are its folder's, sorted as List sorts,
// a folder comes before the folders in it, and together the items are
// exactly what List says, in List's order once sorted as it sorts.
func checkWalk(t *testing.T, what string, calls []walkCall, list []drive.Item, sortBy string) {
	t.Helper()
	seen := map[string]bool{}
	var all []drive.Item
	for i, c := range calls {
		if i > 0 && !seen[path.Dir(c.dir.Path)] {
			t.Fatalf("%s: %s walked before its folder", what, c.dir.Path)
		}
		seen[c.dir.Path] = true
		sorted := append([]drive.Item(nil), c.items...)
		sortItems(sorted, sortBy)
		if len(c.items) > 0 && !reflect.DeepEqual(sorted, c.items) {
			t.Fatalf("%s: %s's items aren't sorted by %q", what, c.dir.Path, sortBy)
		}
		for _, it := range c.items {
			if path.Dir(it.Path) != c.dir.Path {
				t.Fatalf("%s: %s given with folder %s", what, it.Path, c.dir.Path)
			}
		}
		all = append(all, c.items...)
	}
	sortItems(all, sortBy)
	if len(all) == 0 {
		all = []drive.Item{}
	}
	if !reflect.DeepEqual(all, list) {
		t.Fatalf("%s: the walk's items\n%v\nList's\n%v", what, itemPaths(all), itemPaths(list))
	}
}

func itemPaths(items []drive.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

func listAll(t *testing.T, ctx context.Context, d drive.Drive, ref drive.ItemRef, opts drive.ListOptions) []drive.Item {
	t.Helper()
	page, err := d.List(ctx, ref, opts)
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// TestWalk: a walk of a POSIX drive gives List's items a folder at a
// time, in each sort, to each depth, as of a time, through the drive's
// Walker and through the fallback over List; a folder the profile can't
// list is left out, the top one is ErrForbidden, and an error from fn
// stops the walk.
func TestWalk(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "walk", Size: 1 << 30, RootMode: 0o777}, "")
	alice := as(1000, 1000)
	bob := as(1001, 1001)
	write(t, alice, d, "/b.txt", "bb", drive.Condition{})
	write(t, alice, d, "/A/one.txt", "1", drive.Condition{})
	write(t, alice, d, "/A/sub/deep/x.bin", "xxxxxx", drive.Condition{})
	time.Sleep(1100 * time.Millisecond)
	before, _ := store.ServerTime(context.Background())
	time.Sleep(1100 * time.Millisecond)
	write(t, alice, d, "/A/sub/two.txt", "22", drive.Condition{})
	write(t, alice, d, "/c/z.txt", "zzz", drive.Condition{})
	write(t, alice, d, "/private/secret.txt", "s", drive.Condition{})
	if _, err := d.Mkdir(alice, "/empty", false); err != nil {
		t.Fatal(err)
	}
	fs := d.Filesystem()
	e, _, _ := fs.Lookup(context.Background(), fs.RootKey(), "private")
	mode := uint32(0o700)
	if _, err := fs.SetAttr(context.Background(), e.UUID, icbfs.TypeDir, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	for _, sortBy := range []string{"", "modified", "size"} {
		for _, depth := range []int{0, 1, 2, 3} {
			for _, at := range []drive.At{{}, {Time: before}} {
				listDepth := depth
				if depth == 0 {
					listDepth = 1 << 30
				}
				lopts := drive.ListOptions{At: at, Depth: listDepth, Sort: sortBy}
				wopts := drive.WalkOptions{At: at, Depth: depth, Sort: sortBy}
				list := listAll(t, bob, d, drive.ByPath("/"), lopts)
				what := fmt.Sprintf("sort %q depth %d at %v", sortBy, depth, at.Time)
				calls, err := walkCalls(t, bob, d, drive.ByPath("/"), wopts)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				checkWalk(t, what, calls, list, sortBy)
				for _, c := range calls {
					if c.dir.Path == "/private" {
						t.Fatalf("%s: walked /private, which bob can't list", what)
					}
				}
				if calls[0].dir.Path != "/" || calls[0].dir.ID != fs.RootKey() {
					t.Fatalf("%s: the first call is for %+v", what, calls[0].dir)
				}
				// The fallback: the same calls, with /private as an empty
				// folder (a List can't tell it from one).
				fb, err := walkCalls(t, bob, listOnly{d}, drive.ByPath("/"), wopts)
				if err != nil {
					t.Fatalf("%s, the fallback: %v", what, err)
				}
				checkWalk(t, what+", the fallback", fb, list, sortBy)
				byDir := map[string][]drive.Item{}
				for _, c := range calls {
					byDir[c.dir.Path] = c.items
				}
				for _, c := range fb {
					if c.dir.Path == "/private" && len(c.items) == 0 {
						continue
					}
					if got, ok := byDir[c.dir.Path]; !ok || !reflect.DeepEqual(got, c.items) {
						t.Fatalf("%s: the fallback's %s is %v, the walk's %v", what, c.dir.Path, itemPaths(c.items), itemPaths(got))
					}
					delete(byDir, c.dir.Path)
				}
				if len(byDir) != 0 {
					t.Fatalf("%s: the fallback left out %v", what, byDir)
				}
			}
		}
	}

	// alice sees /private's file; a walk of a subfolder starts there
	calls, err := walkCalls(t, alice, d, drive.ByPath("/A"), drive.WalkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checkWalk(t, "alice's /A", calls, listAll(t, alice, d, drive.ByPath("/A"), drive.ListOptions{Depth: 1 << 30}), "")
	if len(calls) != 3 || calls[0].dir.Path != "/A" || calls[1].dir.Path != "/A/sub" || calls[2].dir.Path != "/A/sub/deep" {
		t.Fatalf("alice's walk of /A: %+v", calls)
	}

	if _, err := walkCalls(t, bob, d, drive.ByPath("/private"), drive.WalkOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob walks /private: %v, want ErrForbidden", err)
	}
	if _, err := walkCalls(t, bob, d, drive.ByPath("/b.txt"), drive.WalkOptions{}); !errors.Is(err, drive.ErrNotDir) {
		t.Fatalf("a walk of a file: %v, want ErrNotDir", err)
	}
	stop := errors.New("stop")
	n := 0
	err = drive.Walk(bob, d, drive.ByPath("/"), drive.WalkOptions{}, func(drive.ItemRef, []drive.Item) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("fn's error: %v after %d calls, want stop after 1", err, n)
	}
}

// TestWalkWindows: on a primary-Windows drive a folder with no security
// descriptor of its own takes its nearest ancestor's, however far up, in
// a walk as in List: what a stranger may list is the same either way.
func TestWalkWindows(t *testing.T) {
	store := miniotest.Store(t)
	rootSDDL := "O:" + sidAlice + "G:" + sidAlice + "D:PAI(A;OICI;FA;;;" + sidAlice + ")"
	d := newDrive(t, store, icbfs.Settings{Name: "walkwin", Size: 1 << 30, PrimaryWindows: true, RootSDDL: rootSDDL}, "")
	alice := asSID(sidAlice)
	stranger := asSID("S-1-5-21-9-9-9-500")
	ctx := context.Background()
	fs := d.Filesystem()

	open, err := d.Mkdir(alice, "/Open", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetWindowsACL(ctx, open.ID, icbfs.TypeDir, []byte("O:"+sidAlice+"G:"+sidAlice+"D:P(A;OICI;FA;;;"+sidAlice+")(A;OICI;0x1200a9;;;WD)")); err != nil {
		t.Fatal(err)
	}
	// two levels with no descriptor: both take /Open's
	bare, err := d.Mkdir(alice, "/Open/bare", false)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := d.Mkdir(alice, "/Open/bare/sub", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{bare.ID, sub.ID} {
		if _, err := fs.SetWindowsACL(ctx, id, icbfs.TypeDir, nil); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := d.Permissions(alice, drive.ByPath("/Open/bare/sub")); p.Inherited != "/Open" {
		t.Fatalf("/Open/bare/sub's permissions: %+v, want inherited from /Open", p)
	}
	for _, p := range []string{"/Open/bare/sub/a.txt", "/Open/bare/b.txt", "/Open/closed/c.txt", "/Open/c.txt"} {
		write(t, alice, d, p, "x", drive.Condition{})
	}
	for _, p := range []string{"/Open/bare/sub/a.txt", "/Open/bare/b.txt"} {
		st, _ := d.Stat(alice, drive.ByPath(p), drive.At{})
		if _, err := fs.SetWindowsACL(ctx, st.ID, icbfs.TypeFile, nil); err != nil {
			t.Fatal(err)
		}
	}
	closed, _ := d.Stat(alice, drive.ByPath("/Open/closed"), drive.At{})
	if _, err := fs.SetWindowsACL(ctx, closed.ID, icbfs.TypeDir, []byte("O:"+sidAlice+"G:"+sidAlice+"D:P(A;;FA;;;"+sidAlice+")")); err != nil {
		t.Fatal(err)
	}

	for _, who := range []struct {
		name string
		ctx  context.Context
	}{{"alice", alice}, {"a stranger", stranger}} {
		list := listAll(t, who.ctx, d, drive.ByPath("/Open"), drive.ListOptions{Depth: 1 << 30})
		calls, err := walkCalls(t, who.ctx, d, drive.ByPath("/Open"), drive.WalkOptions{})
		if err != nil {
			t.Fatalf("%s: %v", who.name, err)
		}
		checkWalk(t, who.name, calls, list, "")
		var dirs []string
		for _, c := range calls {
			dirs = append(dirs, c.dir.Path)
		}
		want := "/Open,/Open/bare,/Open/bare/sub,/Open/closed"
		if who.ctx == stranger {
			want = "/Open,/Open/bare,/Open/bare/sub" // /Open/closed is alice's alone
		}
		if got := strings.Join(dirs, ","); got != want {
			t.Fatalf("%s walks %s, want %s", who.name, got, want)
		}
	}
	if _, err := walkCalls(t, stranger, d, drive.ByPath("/"), drive.WalkOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("a stranger walks the root: %v, want ErrForbidden", err)
	}
}

// The tree TestWalkMemory walks: 300 folders down to 8 levels (items 9
// deep), 30,000 items, one folder of 2,000, paths of 130 bytes on
// average.
const (
	treeChains    = 37 // chains of folders, each treeChainLen deep
	treeChainLen  = 8
	treeExtraDirs = 300 - treeChains*treeChainLen // more folders at the top
	treeItems     = 30000
	treeBig       = 2000
	treeDirName   = 15 // bytes in a folder's name
	treeFileName  = 58 // bytes in a file's name
)

// buildTree makes TestWalkMemory's tree straight on the filesystem: the
// folders, then their files, 32 creates at once: a folder's one after
// another (creates in one folder contend for its block), but the big
// folder's in 8 runs of names of their own.
func buildTree(t *testing.T, d *Drive) (avgPath float64) {
	t.Helper()
	ctx := context.Background()
	fs := d.Filesystem()
	pad := func(s string, n int) string { return s + strings.Repeat("x", n-len(s)) }
	type dir struct {
		key, path string
		files     int
	}
	var dirs []*dir
	var chains [][]*dir
	for c := 0; c < treeChains+treeExtraDirs; c++ {
		name := pad(fmt.Sprintf("c%02d-", c), treeDirName)
		key, _, err := fs.Mkdir(ctx, fs.RootKey(), name, 0o755, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		top := &dir{key: key, path: "/" + name}
		dirs = append(dirs, top)
		chains = append(chains, []*dir{top})
	}
	var wg sync.WaitGroup
	errs := make(chan error, treeChains)
	for c := 0; c < treeChains; c++ {
		wg.Go(func() {
			parent := chains[c][0]
			for l := 2; l <= treeChainLen; l++ {
				name := pad(fmt.Sprintf("l%d-", l), treeDirName)
				key, _, err := fs.Mkdir(ctx, parent.key, name, 0o755, 0, 0)
				if err != nil {
					errs <- err
					return
				}
				parent = &dir{key: key, path: parent.path + "/" + name}
				chains[c] = append(chains[c], parent)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	dirs = dirs[:0]
	for _, ch := range chains {
		dirs = append(dirs, ch...)
	}
	// The big folder: the middle chain's fourth level, in the middle of
	// the walk.
	big := chains[treeChains/2][3]
	big.files = treeBig - 1 // and the next level's folder
	files := treeItems - len(dirs) - big.files
	others := len(dirs) - 1
	i := 0
	for _, dr := range dirs {
		if dr == big {
			continue
		}
		dr.files = files / others
		if i < files%others {
			dr.files++
		}
		i++
	}
	var total, count int
	for _, dr := range dirs {
		total += len(dr.path)
		count++
		total += dr.files * (len(dr.path) + 1 + treeFileName)
		count += dr.files
	}
	if count != treeItems || len(dirs) != 300 {
		t.Fatalf("the tree has %d items in %d folders", count, len(dirs))
	}

	// A job is files [from, to) of one folder, named with a prefix:
	// one job a folder, and the big folder in 8, each with its own
	// prefix, so they land in different blocks of it once it splits.
	type job struct {
		dr       *dir
		prefix   byte
		from, to int
	}
	var jobs []job
	const bigJobs = 8
	for j := 0; j < bigJobs; j++ {
		jobs = append(jobs, job{big, byte('a' + j), j * big.files / bigJobs, (j + 1) * big.files / bigJobs})
	}
	for _, dr := range dirs {
		if dr != big {
			jobs = append(jobs, job{dr, 'f', 0, dr.files})
		}
	}
	began := time.Now()
	work := make(chan job)
	var mu sync.Mutex
	var failed error
	for w := 0; w < 32; w++ {
		wg.Go(func() {
			for jb := range work {
				for f := jb.from; f < jb.to; f++ {
					name := pad(fmt.Sprintf("%c%05d-", jb.prefix, f), treeFileName)
					if _, _, err := fs.CreateWith(ctx, jb.dr.key, name, 0o644, 0, 0, []byte("x"), nil, icbfs.ItemInfo{}); err != nil {
						mu.Lock()
						failed = fmt.Errorf("%s/%s: %w", jb.dr.path, name, err)
						mu.Unlock()
						break
					}
				}
			}
		})
	}
	for _, jb := range jobs {
		work <- jb
	}
	close(work)
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}
	t.Logf("every file in %v", time.Since(began).Round(time.Millisecond))
	return float64(total) / float64(count)
}

// heapInUse is the heap in use after a collection: what's live, not
// what's waiting to be collected (which depends on when the collector
// last ran).
func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// What a walk may hold in fn, on top of the heap before it: walkPerItem
// for each item of the largest folder, and walkBase for the rest (the
// store client's buffers, a collection's partly used spans). A folder's
// finished items measured about 1.6 KiB each (2,000 items: 3.2 MiB over
// the heap before), the whole tree's about 1.1 KiB each (30,000: 33.6
// MiB): so 2 KiB an item and 4 MiB leave a walk room, and a walk that
// held even a third of the tree no room.
const (
	walkPerItem = 2 << 10
	walkBase    = 4 << 20
)

// TestWalkMemory: a walk of 30,000 items holds one folder at a time.
// The heap in use, sampled in fn, stays under a bound set by the largest
// folder (2,000 items), not the tree, and is no higher in the last third
// of the walk than in the first.
func TestWalkMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a tree of 30,000 items")
	}
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "walkmem", Size: 1 << 40, RootMode: 0o777}, "")
	start := time.Now()
	avg := buildTree(t, d)
	t.Logf("built %d items in 300 folders, paths %.1f bytes on average, in %v", treeItems, avg, time.Since(start).Round(time.Second))
	if avg < 120 || avg > 140 {
		t.Fatalf("paths %.1f bytes on average, want about 130", avg)
	}

	type sample struct {
		seen int // items walked before
		heap uint64
	}
	var samples []sample
	seen, largest, folders := 0, 0, 0
	base := heapInUse()
	start = time.Now()
	err := drive.Walk(system(), d, drive.ByPath("/"), drive.WalkOptions{}, func(dir drive.ItemRef, items []drive.Item) error {
		samples = append(samples, sample{seen, heapInUse()})
		seen += len(items)
		largest = max(largest, len(items))
		folders++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != treeItems || folders != 301 || largest != treeBig {
		t.Fatalf("walked %d items in %d folders, the largest %d; want %d in 301, the largest %d", seen, folders, largest, treeItems, treeBig)
	}
	bound := base + walkBase + walkPerItem*uint64(largest)
	var peak, first, last uint64
	for _, s := range samples {
		peak = max(peak, s.heap)
		switch {
		case s.seen < treeItems/3:
			first = max(first, s.heap)
		case s.seen >= 2*treeItems/3:
			last = max(last, s.heap)
		}
	}
	mib := func(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
	t.Logf("walked in %v; heap in use before %s, peak %s (bound %s: %s + %s + %d KiB x %d items), first third %s, last third %s",
		time.Since(start).Round(time.Second), mib(base), mib(peak), mib(bound), mib(base), mib(walkBase), walkPerItem>>10, largest, mib(first), mib(last))
	if peak > bound {
		t.Errorf("heap in use reached %s in the walk, over the bound %s", mib(peak), mib(bound))
	}
	// 1 MiB for what a collection leaves at random (partly used spans).
	if last > first+1<<20 {
		t.Errorf("heap in use in the last third %s, over the first third's %s", mib(last), mib(first))
	}
}
