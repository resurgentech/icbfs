package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// Settings are a new filesystem's. All are fixed at creation except Size,
// which Resize changes.
type Settings struct {
	Name           string
	Size           uint64 // declared capacity: what df shows as the total
	PrimaryWindows bool
	MaxFileSize    uint64 // the largest file it holds; 0 means DefaultMaxFileSize
	// DirBlockEntries is how many entries a directory block holds before
	// it splits; 0 means DefaultDirBlockEntries.
	DirBlockEntries uint32

	// The root folder's owner and permissions: mode, uid and gid on any
	// filesystem, and on a primary-Windows one RootSDDL, the root's
	// security descriptor (empty leaves the root without one, and a
	// Windows mount then shows the mounting account's default). The root
	// is a folder like any other, so these can be changed later.
	RootMode, RootUid, RootGid uint32
	RootSDDL                   string

	// Created is when the filesystem's history starts, if not now: a
	// mirror of a source whose history is older gives a time before
	// anything it will copy, which it then writes in order under the
	// times it happened at (objstore.WithDeclaredTime).
	Created time.Time
}

// Info describes one filesystem in a bucket.
type Info struct {
	Name string
	ID   string // the slot's position: reused after a prune
	// UID is the filesystem's own id, written once when it is made and
	// shared with no other filesystem (FilesystemEntry.uid); "" for one
	// made before icbfs wrote it, until FilesystemUID gives it one.
	UID             string
	Size            uint64
	PrimaryWindows  bool
	MaxFileSize     int64
	DirBlockEntries int
	Archived        bool
	Created         time.Time // when it was created
	FormatVersion   int       // its objects' format as a whole (see FormatVersion)
}

// DefaultSettings is a filesystem called name with the defaults icbfs-cli
// fs create uses: primary-POSIX, DefaultSize declared, the largest file
// DefaultMaxFileSize, the root mode DefaultRootMode, owned by uid and gid
// 0 (icbfs-cli makes those the caller's). A host that wants "a
// filesystem called X" creates this, changing what it needs.
func DefaultSettings(name string) Settings {
	return Settings{Name: name, Size: DefaultSize, RootMode: DefaultRootMode}
}

// DefaultRootMode is DefaultSettings' root folder mode.
const DefaultRootMode = 0o755

// DefaultSize is DefaultSettings' declared capacity: what df shows,
// changeable later (Resize).
const DefaultSize = 100 << 30

// filesystemName is what a filesystem may be called: it's part of the
// root's object key, so it's kept to characters every store takes.
var filesystemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ErrInvalidFilesystemName is returned for a name outside filesystemName.
var ErrInvalidFilesystemName = errors.New("filesystem names are 1-63 letters, digits, '.', '_' or '-', starting with a letter or digit")

// CreateFilesystem creates a new filesystem in store, failing with
// ErrExists if the name is taken. This is the only way a filesystem comes
// to exist: mounts attach to existing ones (Filesystem.Attach). A create
// interrupted before the root was written is completed by calling it
// again with the same name.
func CreateFilesystem(ctx context.Context, store objstore.Store, s Settings) (Info, error) {
	if !filesystemName.MatchString(s.Name) {
		return Info{}, fmt.Errorf("%q: %w", s.Name, ErrInvalidFilesystemName)
	}
	if s.MaxFileSize > DefaultMaxFileSize {
		return Info{}, fmt.Errorf("max file size %d is past the largest supported, %d", s.MaxFileSize, int64(DefaultMaxFileSize))
	}
	if n := s.DirBlockEntries; n != 0 && (n < minDirBlockEntries || n > maxDirBlockEntries) {
		return Info{}, fmt.Errorf("directory block entries %d: %d to %d", n, minDirBlockEntries, maxDirBlockEntries)
	}
	created := time.Now()
	if !s.Created.IsZero() {
		created = s.Created
	}
	e := entryOf(s)
	e.Created = timestamppb.New(created)
	id, entry, existed, err := registerFilesystem(ctx, store, e)
	if err != nil {
		return Info{}, err
	}
	f := New(store, s.Name)
	if err := f.applyEntry(id, entry); err != nil {
		return Info{}, err
	}
	if existed {
		if _, err := store.Head(ctx, f.RootKey()); err == nil {
			return Info{}, fmt.Errorf("filesystem %q: %w", s.Name, ErrExists)
		}
		// registered, but the root never landed: finish creating it;
		// unless it did, and was removed outside icbfs since
		if err := rootRemovedOutside(ctx, store, s.Name, f.RootKey()); err != nil {
			return Info{}, err
		}
	}
	attr := Attr{Mode: s.RootMode, Uid: s.RootUid, Gid: s.RootGid}
	if s.PrimaryWindows && s.RootSDDL != "" {
		attr.WindowsSDDL = []byte(s.RootSDDL)
	}
	rootCtx := ctx
	rec := &objstore.Recorder{}
	if !s.Created.IsZero() {
		rootCtx = objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.Created), rec)
	}
	if err := f.createRoot(rootCtx, attr); err != nil {
		return Info{}, err
	}
	if _, err := f.RecordDates(ctx, rec); err != nil {
		return Info{}, err
	}
	return infoOf(id, entry), nil
}

// entryOf is the master-block entry s asks for (Created only when s gives
// one).
func entryOf(s Settings) *pb.FilesystemEntry {
	e := &pb.FilesystemEntry{
		Name: s.Name, Size: s.Size, PrimaryWindows: s.PrimaryWindows, MaxFileSize: s.MaxFileSize,
		DirBlockEntries: s.DirBlockEntries,
		SourceTime:      !s.Created.IsZero(),
	}
	if !s.Created.IsZero() {
		e.Created = timestamppb.New(s.Created)
	}
	return e
}

// ErrLeftovers is CreateFilesystem's refusal to make a filesystem where
// the store holds history under the names it would take: an earlier
// filesystem's, removed outside icbfs (a plain delete of a versioned
// store's objects ends each one and keeps every version). A filesystem
// made there would share that history: its root, its root's .metadata
// and .version, its past-changed mark, its origin records and app
// objects have fixed names, so its views of the past would show the
// earlier filesystem, and a settle of its root would be refused for good.
// Nothing was written.
var ErrLeftovers = errors.New("the store holds an earlier filesystem's versions under the names this one would take; nothing was written")

// slotClean is nil if nothing in store has ever been under the names a
// new filesystem name in slot id takes: its prefix, and its uploads'
// (what Prune erases). One listing request a prefix, of one entry. Else
// ErrLeftovers, saying what is there and the ways out; noMaster says the
// bucket has no master block (removed with the rest).
func slotClean(ctx context.Context, store objstore.Store, name, id string, noMaster bool) error {
	for _, prefix := range []string{id + "-", id + ".upload-", uploadMarks + id} {
		found, err := objstore.HistoryUnderFirst(ctx, store, prefix, 1)
		if err != nil {
			return fmt.Errorf("filesystem %q: looking for an earlier filesystem's versions under %q: %w", name, prefix, err)
		}
		if len(found) == 0 {
			continue
		}
		// Which filesystems they were: the roots' names, by one more
		// listing (a root is <id>-root-<name>).
		roots := map[string]time.Time{}
		if rs, err := objstore.HistoryUnderFirst(ctx, store, id+"-root-", objstore.HistoryPageSize); err == nil {
			for _, o := range rs {
				k := objstore.BaseKey(o.Key)
				if o.LastModified.After(roots[k]) {
					roots[k] = o.LastModified
				}
			}
		}
		var what []string
		for k, t := range roots {
			what = append(what, fmt.Sprintf("the root %s, its last version stored %s", k, t.UTC().Format(time.RFC3339)))
		}
		sort.Strings(what)
		if len(what) == 0 {
			what = []string{fmt.Sprintf("such as %s", found[0].Key)}
		}
		gone := "a filesystem that was in slot " + id
		if noMaster {
			gone += " of a bucket whose master block went with it"
		}
		return fmt.Errorf("filesystem %q would take slot %s, but the store holds versions under %q (%s): %s, removed outside icbfs "+
			"(a plain delete, such as azcopy remove or aws s3 rm, ends each object and the store keeps its versions); "+
			"a filesystem made here would share their history, and its past would show theirs. "+
			"Make it in an empty container or bucket, or erase every version under %q, %q and %q first "+
			"(icbfs-cli fs prune erases a filesystem only while it is registered; fs archive then fs prune is how to discard one before a rebuild): %w",
			name, id, prefix, strings.Join(what, "; "), gone, id+"-", id+".upload-", uploadMarks+id, ErrLeftovers)
	}
	return nil
}

// rootRemovedOutside is ErrLeftovers for a registered filesystem whose
// root is not there but has history: removed outside icbfs (a create cut
// before its root landed leaves the root no history at all).
func rootRemovedOutside(ctx context.Context, store objstore.Store, name, rootKey string) error {
	h, err := store.History(ctx, rootKey)
	if err != nil {
		return fmt.Errorf("filesystem %q: reading its root's history: %w", name, err)
	}
	if len(h) == 0 {
		return nil
	}
	return fmt.Errorf("filesystem %q is registered, and its root %s is not there but the store holds %d version(s) of it, the last stored %s: "+
		"it was removed outside icbfs (a plain delete keeps every version), and a root made now would share their history. "+
		"Discard it with icbfs's own erase (icbfs-cli fs archive -fs %s, then icbfs-cli fs prune -fs %s -yes: every version goes) and create it again, "+
		"or use another name: %w",
		name, rootKey, len(h), h[0].LastModified.UTC().Format(time.RFC3339), name, name, ErrLeftovers)
}

// leftoversSaid is what RootLeftovers found, said for an operator: what
// it is, what it does, and the way out.
func leftoversSaid(left []objstore.Object) string {
	var last time.Time
	for _, o := range left {
		if o.LastModified.After(last) {
			last = o.LastModified
		}
	}
	return fmt.Sprintf("the root block and its .metadata hold %d version(s) stored before this filesystem was created, the last %s: "+
		"an earlier filesystem of this name, removed outside icbfs (which keeps every version), that this one was made over. "+
		"Its views of the past show that filesystem's entries at those times and nothing before them, "+
		"and a settle of / that writes behind them is refused every time. "+
		"Rebuild it: with every writer stopped, discard this filesystem with icbfs's own erase "+
		"(icbfs-cli fs archive, then fs prune -yes: every version under its prefix goes, the earlier filesystem's too) and make it again; "+
		"or rebuild under another name or in an empty container",
		len(left), last.UTC().Format(time.RFC3339))
}

// ErrMadeOverLeftovers is RootLeftovers' finding as an error: the
// filesystem was made over an earlier one's versions of its root.
var ErrMadeOverLeftovers = errors.New("this filesystem was made over an earlier filesystem's versions of its root")

// LeftoversError is leftoversSaid for filesystem name, as an error
// wrapping ErrMadeOverLeftovers.
func LeftoversError(name string, left []objstore.Object) error {
	return fmt.Errorf("filesystem %q: %s: %w", name, leftoversSaid(left), ErrMadeOverLeftovers)
}

// RootLeftovers is the versions of the root block and of its .metadata
// stored before this filesystem's creation wrote its own: an earlier
// filesystem's of the same slot and name, removed outside icbfs, over
// which a build before ErrLeftovers made this one. They are the root's
// first states as every view and every settle reads them, dated at the
// times the store wrote them: its views of the past show the earlier
// filesystem's entries at those times and none before, and a settle of
// the root that would write behind them is refused, every time.
//
// It is known only for a filesystem created at a declared date
// (Settings.Created, as a mirror's is): its creation is its root's oldest
// stored version dated, in the root's .version, at the filesystem's
// creation date. A filesystem created now has its root dated by the
// store, to the second, never at its creation's instant: nil. One read
// of the master block and of the root's .version, two history listings.
func (f *Filesystem) RootLeftovers(ctx context.Context) ([]objstore.Object, error) {
	info, err := DescribeFilesystem(ctx, f.store, f.fsName)
	if err != nil || info.Created.IsZero() {
		return nil, err
	}
	root := f.RootKey()
	dates, err := objstore.ReadDates(ctx, f.store, root)
	if err != nil {
		return nil, err
	}
	var out []objstore.Object
	for _, key := range []string{root, metadataObjectKey(root)} {
		h, err := f.store.History(ctx, key)
		if err != nil {
			return nil, err
		}
		c := -1 // the creation's version: the oldest stored dated at the creation
		for i := len(h) - 1; i >= 0; i-- {
			if !h[i].Deleted && dates.Time(&h[i]).Equal(info.Created) {
				c = i
				break
			}
		}
		for i := c + 1; c >= 0 && i < len(h); i++ {
			if !h[i].Deleted {
				out = append(out, h[i])
			}
		}
	}
	return out, nil
}

// Attach opens an existing filesystem for use, failing with ErrNotFound
// if there's none of this name in the store (or its creation never
// finished).
func (f *Filesystem) Attach(ctx context.Context) error {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		if errors.Is(mapNotFound(err), ErrNotFound) {
			return fmt.Errorf("no filesystem %q in this bucket: %w", f.fsName, ErrNotFound)
		}
		return err
	}
	for i, e := range mb.Filesystems {
		if e.Name == f.fsName {
			if err := f.applyEntry(fsID(i), e); err != nil {
				return err
			}
			if _, err := f.store.Head(ctx, f.RootKey()); err != nil {
				return fmt.Errorf("filesystem %q has no root (an interrupted create?): %w", f.fsName, mapNotFound(err))
			}
			return nil
		}
	}
	return fmt.Errorf("no filesystem %q in this bucket: %w", f.fsName, ErrNotFound)
}

// Archived reports whether the filesystem is archived (read-only for
// good), as of attaching or the last Refresh.
func (f *Filesystem) Archived() bool { return f.archived.Load() }

// Refresh re-reads the filesystem's master block entry, so a filesystem
// archived since it was attached stops taking writes.
func (f *Filesystem) Refresh(ctx context.Context) error {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		return mapNotFound(err)
	}
	for i, e := range mb.Filesystems {
		if e.Name == f.fsName && fsID(i) == f.id {
			f.archived.Store(e.Archived)
			f.sourceTime.Store(e.SourceTime)
			return nil
		}
	}
	return fmt.Errorf("filesystem %q: %w", f.fsName, ErrNotFound)
}

// ListFilesystems lists the filesystems in store's bucket.
func ListFilesystems(ctx context.Context, store objstore.Store) ([]Info, error) {
	mb, _, err := readMaster(ctx, store)
	if err != nil {
		if errors.Is(mapNotFound(err), ErrNotFound) {
			return nil, nil // a bucket nothing has been created in
		}
		return nil, err
	}
	var out []Info
	for i, e := range mb.Filesystems {
		if e.Name != "" {
			out = append(out, infoOf(fsID(i), e))
		}
	}
	return out, nil
}

// DescribeFilesystem is one filesystem's Info, read from the master block
// alone: nothing under the filesystem is listed.
func DescribeFilesystem(ctx context.Context, store objstore.Store, name string) (Info, error) {
	all, err := ListFilesystems(ctx, store)
	if err != nil {
		return Info{}, err
	}
	for _, fs := range all {
		if fs.Name == name {
			return fs, nil
		}
	}
	return Info{}, fmt.Errorf("no filesystem %q in this bucket: %w", name, ErrNotFound)
}

func infoOf(id string, e *pb.FilesystemEntry) Info {
	info := Info{Name: e.Name, ID: id, Size: e.Size, PrimaryWindows: e.PrimaryWindows, Archived: e.Archived, MaxFileSize: DefaultMaxFileSize,
		DirBlockEntries: dirBlockEntries(e), FormatVersion: int(e.FormatVersion), UID: e.Uid}
	if e.MaxFileSize != 0 && e.MaxFileSize <= DefaultMaxFileSize {
		info.MaxFileSize = int64(e.MaxFileSize)
	}
	if e.Created != nil {
		info.Created = e.Created.AsTime()
	}
	return info
}

// createRoot writes an empty root folder with attr's owner, mode and
// xattrs: .metadata first, so an interrupted create leaves no root block
// and is simply redone.
func (f *Filesystem) createRoot(ctx context.Context, attr Attr) error {
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return err
	}
	attr.Nlink, attr.Mtime, attr.Ctime = 1, changeTime(ctx), time.Now()
	metaData, err := f.encodeMetadata(ctx, attr)
	if err != nil {
		return err
	}
	rootKey := f.RootKey()
	if _, err := f.store.Put(ctx, metadataObjectKey(rootKey), bytes.NewReader(metaData), nil, ""); err != nil {
		return err
	}
	_, err = f.store.Put(objstore.AsBlock(ctx, rootKey), rootKey, bytes.NewReader(empty), nil, "")
	return err
}
