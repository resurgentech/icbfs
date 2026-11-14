package icbfsdrive

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// The tree TestTreeMemory walks: 300 folders down to 8 levels (items 9
// deep), 30,000 items, one folder of 2,000, paths of 130 bytes on
// average.
const (
	treeChains    = 37 // chains of folders, each treeChainLen deep
	treeChainLen  = 8
	treeExtraDirs = 300 - treeChains*treeChainLen // more folders at the top
	treeItems     = 30000
	treeFiles     = treeItems - 300
	treeBig       = 2000
	treeDirName   = 15 // bytes in a folder's name
	treeFileName  = 58 // bytes in a file's name
)

// tree is what buildTree made.
type tree struct {
	avgPath  float64
	versions int             // the files' versions, all told
	shas     []string        // the distinct SHA-256s of their contents, hex
	two      map[string]bool // the paths of the files with a second version
}

// treeSecond: file number g has a second version, 3 in 100 of them.
func treeSecond(g int) bool { r := g % 100; return r == 12 || r == 45 || r == 78 }

// treeContent is the content of the tree's version j: file number j's
// first version for j < treeFiles, and the second versions after, in the
// order of their files. 2 in 5 are a copy of another's: of the first
// version of a file numbered 2 mod 5, spread over the tree.
func treeContent(j int) []byte {
	if j%5 < 2 {
		t := (j * 7919) % treeFiles
		if j = t - t%5 + 2; j >= treeFiles {
			j -= 5
		}
	}
	return fmt.Appendf(nil, "content %d\n", j)
}

// treeOrigin is file number g's origin at version v, as a Mirror of
// srcDrive records it.
func treeOrigin(g, v int, p string) drive.Origin {
	return drive.Origin{Drive: srcDrive, ID: fmt.Sprintf("src-%06d", g), Version: fmt.Sprint(v), Path: "/src" + p,
		Copied: time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)}
}

// buildTree makes TestTreeMemory's tree: the folders and the files'
// first versions straight on the filesystem, 32 creates at once (a
// folder's one after another: creates in one folder contend for its
// block; the big folder's in 8 runs of names of their own), each file
// with its content, its checksums and its origin in the create, as the
// drive's own write records them; then the second versions through the
// drive's Write, 32 at once.
func buildTree(t *testing.T, d *Drive) tree {
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
	// Its files are numbered from num on.
	type job struct {
		dr       *dir
		prefix   byte
		from, to int
		num      int
	}
	var jobs []job
	num := 0
	const bigJobs = 8
	for j := 0; j < bigJobs; j++ {
		jb := job{big, byte('a' + j), j * big.files / bigJobs, (j + 1) * big.files / bigJobs, num}
		jobs = append(jobs, jb)
		num += jb.to - jb.from
	}
	for _, dr := range dirs {
		if dr != big {
			jobs = append(jobs, job{dr, 'f', 0, dr.files, num})
			num += dr.files
		}
	}
	if num != treeFiles {
		t.Fatalf("numbered %d files, want %d", num, treeFiles)
	}
	type second struct {
		g    int
		path string
	}
	var seconds []second
	for _, jb := range jobs {
		for f := jb.from; f < jb.to; f++ {
			if g := jb.num + f - jb.from; treeSecond(g) {
				seconds = append(seconds, second{g, jb.dr.path + "/" + pad(fmt.Sprintf("%c%05d-", jb.prefix, f), treeFileName)})
			}
		}
	}
	began := time.Now()
	work := make(chan job)
	var mu sync.Mutex
	var failed error
	fail := func(err error) {
		mu.Lock()
		failed = cmp.Or(failed, err)
		mu.Unlock()
	}
	for w := 0; w < 32; w++ {
		wg.Go(func() {
			for jb := range work {
				for f := jb.from; f < jb.to; f++ {
					g := jb.num + f - jb.from
					name := pad(fmt.Sprintf("%c%05d-", jb.prefix, f), treeFileName)
					data := treeContent(g)
					sums, err := icbfs.ChecksumsOf(bytes.NewReader(data))
					if err != nil {
						fail(err)
						break
					}
					origin, err := json.Marshal(treeOrigin(g, 1, jb.dr.path+"/"+name))
					if err != nil {
						fail(err)
						break
					}
					info := icbfs.ItemInfo{App: map[string][]byte{icbfs.AppDriveOrigin: origin}, Checksums: &sums}
					if _, _, err := fs.CreateWith(ctx, jb.dr.key, name, 0o644, 0, 0, data, nil, info); err != nil {
						fail(fmt.Errorf("%s/%s: %w", jb.dr.path, name, err))
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
	firsts := time.Since(began).Round(time.Millisecond)
	began = time.Now()
	todo := make(chan int)
	for w := 0; w < 32; w++ {
		wg.Go(func() {
			for k := range todo {
				s := seconds[k]
				o := treeOrigin(s.g, 2, s.path)
				if _, err := d.Write(system(), s.path, bytes.NewReader(treeContent(treeFiles+k)), drive.Condition{}, drive.WriteOptions{Origin: &o}); err != nil {
					fail(fmt.Errorf("%s's second version: %w", s.path, err))
				}
			}
		})
	}
	for k := range seconds {
		todo <- k
	}
	close(todo)
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}
	t.Logf("every file's first version in %v, %d second versions in %v", firsts, len(seconds), time.Since(began).Round(time.Millisecond))

	out := tree{avgPath: float64(total) / float64(count), versions: treeFiles + len(seconds), two: make(map[string]bool, len(seconds))}
	for _, s := range seconds {
		out.two[s.path] = true
	}
	distinct := map[string]bool{}
	for j := 0; j < out.versions; j++ {
		sum := sha256.Sum256(treeContent(j))
		distinct[hex.EncodeToString(sum[:])] = true
	}
	for sha := range distinct {
		out.shas = append(out.shas, sha)
	}
	return out
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
// MiB) on a tree of plain files. This tree's files each carry an origin
// and checksums, as a mirror's do, and its walk measured 7.0 MiB over the
// heap before (3.5 KiB an item of the largest folder, the base included).
// So 3 KiB an item and 4 MiB leave a walk room, and a walk that held
// even a third of the tree no room.
const (
	walkPerItem = 3 << 10
	walkBase    = 4 << 20
)

// rebuildInFlight is what a rebuild may hold besides a walk's share: its
// eight Versions calls at once, with their answers and the store
// client's connections for them. Measured at about 2 MiB.
const rebuildInFlight = 4 << 20

func mib[T int64 | uint64](n T) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// TestTreeMemory builds a tree of 30,000 items once, on one drive, for
// the tests of what a pass over all of it holds: a walk (walkMemory) and
// a mirror's rebuild of its hash map (rebuildMemory).
func TestTreeMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a tree of 30,000 items")
	}
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "treemem", Size: 1 << 40, RootMode: 0o777}, "")
	start := time.Now()
	tr := buildTree(t, d)
	t.Logf("built %d items in 300 folders, %d versions of %d contents, paths %.1f bytes on average, in %v",
		treeItems, tr.versions, len(tr.shas), tr.avgPath, time.Since(start).Round(time.Second))
	if tr.avgPath < 120 || tr.avgPath > 140 {
		t.Fatalf("paths %.1f bytes on average, want about 130", tr.avgPath)
	}
	t.Run("walk", func(t *testing.T) { walkMemory(t, d) })
	t.Run("rebuild hashes", func(t *testing.T) { rebuildMemory(t, d, tr) })
}

// walkMemory: a walk of 30,000 items holds one folder at a time. The
// heap in use, sampled in fn, stays under a bound set by the largest
// folder (2,000 items), not the tree, and is no higher in the last third
// of the walk than in the first.
func walkMemory(t *testing.T, d *Drive) {
	type sample struct {
		seen int // items walked before
		heap uint64
	}
	var samples []sample
	seen, largest, folders := 0, 0, 0
	base := heapInUse()
	start := time.Now()
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
	t.Logf("walked in %v; heap in use before %s, peak %s (bound %s: %s + %s + %d KiB x %d items), first third %s, last third %s",
		time.Since(start).Round(time.Second), mib(base), mib(peak), mib(bound), mib(base), mib(uint64(walkBase)), walkPerItem>>10, largest, mib(first), mib(last))
	if peak > bound {
		t.Errorf("heap in use reached %s in the walk, over the bound %s", mib(peak), mib(bound))
	}
	// 1 MiB for what a collection leaves at random (partly used spans).
	if last > first+1<<20 {
		t.Errorf("heap in use in the last third %s, over the first third's %s", mib(last), mib(first))
	}
}
