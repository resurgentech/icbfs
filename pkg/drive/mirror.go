package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Mirror keeps a drive matching one source of documents (a WebDAV
// share, say): a sync daemon says what each source item is
// now, and the mirror works out what that means on the drive, keyed by
// the item's origin (the source drive and the item's id there).
//
// The trash is the mirror's undo. A source item deleted and then brought
// back with the same id (from a recycle bin, a folder shared again, a
// flaky API that came back short) is restored, history and all, not
// copied again; and a whole pass's removals can be undone at once. So a
// daemon needn't cap how much a pass may remove to limit the damage of a
// bad listing.
type Mirror struct {
	d           Drive
	source      string
	sourceTimes bool
	hashes      *HashMap
	hashStore   HashStore
	kinds       map[string]hashKind
}

// hashKind is how the mirror computes a source's kind of hash: the hash,
// and its value as the source writes it.
type hashKind struct {
	new    func() hash.Hash
	encode func([]byte) string
}

// NewMirror is a mirror of the source drive source (an Origin.Drive) on d.
// d needs the Origin and Trash capabilities. A backend that implements
// OriginFinder finds items by origin in a lookup; any other is searched
// (a listing of the whole drive and its trash per lookup).
func NewMirror(d Drive, source string) *Mirror {
	return &Mirror{d: d, source: source, hashes: NewHashMap(), kinds: map[string]hashKind{
		"md5":    {new: md5.New},
		"sha1":   {new: sha1.New},
		"sha256": {new: sha256.New},
	}}
}

// HashKind teaches the mirror a source's kind of hash (SourceItem.Hash):
// newHash makes one, and encode writes a sum as the source does (nil:
// lowercase hex). md5, sha1 and sha256, in hex, it knows. As it reads a
// source item's bytes, the mirror computes their hash of that kind, and
// records what it is in SHA-256: the next item the source says has it is
// written without opening the source. A kind it can't compute is never
// recorded, so a source's word alone never makes a stub.
func (m *Mirror) HashKind(kind string, newHash func() hash.Hash, encode func([]byte) string) *Mirror {
	m.kinds[kind] = hashKind{new: newHash, encode: encode}
	return m
}

// InSourceTime makes the mirror write the source's history in the
// source's time (WithSourceTime), so the drive's versions, trash and
// views of the past show when things happened there: an item is created
// at its Created time (else its ModTime), a new version written at its
// ModTime, a move made at its Changed time; Remove deletes at ctx's
// source time, which the daemon sets to when the source deleted it. A
// time the source item doesn't give is ctx's source time, if it has one,
// else now. Changes come in any order, though oldest first costs least.
// The drive's filesystem has to start before the source's history does
// (Settings.Created).
func (m *Mirror) InSourceTime() *Mirror {
	m.sourceTimes = true
	return m
}

// at is ctx for a change that happened at t at the source, when the
// mirror writes in source time and t is known.
func (m *Mirror) at(ctx context.Context, t time.Time) context.Context {
	if !m.sourceTimes || t.IsZero() {
		return ctx
	}
	return WithSourceTime(ctx, t)
}

// born is when src came to be at the source, as far as it says.
func born(src SourceItem) time.Time {
	if !src.Created.IsZero() {
		return src.Created
	}
	return src.ModTime
}

// OriginFinder is what a backend implements to find an item by its origin
// quickly: the live item copied from the source item, or, if it was
// deleted, its trash entry. Neither: ErrNotFound.
type OriginFinder interface {
	FindByOrigin(ctx context.Context, originDrive, originID string) (Item, *TrashEntry, error)
}

// SourceItem is what a sync daemon knows about one source item, now.
type SourceItem struct {
	// Origin is the source item: its id, the version to hold, its path
	// there. Origin.Drive is the mirror's source if left empty; Copied
	// is now if zero.
	Origin  Origin
	Path    string    // where it belongs on the drive
	Type    string    // TypeFile or TypeDir
	ModTime time.Time // its time at the source, if known
	Created time.Time // its creation time at the source, if known
	// Changed is when it was last renamed or moved at the source, if
	// known: what a move is dated with in source time (InSourceTime).
	Changed     time.Time
	ContentType string
	// App, Mode and WindowsAttrs are recorded on the item as ItemInfo
	// says, in the same write as its times and origin; nil leaves the
	// item's as they are. An item is Unchanged only when none differ.
	App          map[string][]byte
	Mode         *uint32
	WindowsAttrs *uint32
	// Open gives the content; it's called only when the bytes are needed,
	// never for a move, a restore of the version held, an item already
	// up to date, a new file whose path is taken, or a Hash the mirror
	// knows. A version is read to its end before it's written (a Spool).
	Open func(ctx context.Context) (io.ReadCloser, error)
	// Hash is the source's own hash of the content, if it gives one, and
	// Size the content's size with it. A hash the mirror has seen before,
	// at that size, is written as a stub of the version that had it,
	// without opening the source.
	Hash Hash
	Size int64
	// FoldInto, if set, is the id of an older source item with the same
	// name in the same folder (case and all): if the drive holds it, live,
	// with the same bytes, this item is folded into its item (an alias,
	// Origin.Aliases) instead of being an item of its own.
	FoldInto string
	// FoldPath, if set, is where a folded item belongs: the item holding
	// this one is moved there. Without it, the item stays where it is.
	FoldPath string
}

// Action is one thing Apply did.
type Action string

const (
	Created   Action = "created"
	Restored  Action = "restored" // from the trash, with its history
	Moved     Action = "moved"
	Updated   Action = "updated" // a new version
	Retimed   Action = "retimed" // its times set as the source's, no new version
	Unchanged Action = "unchanged"
	Folded    Action = "folded"   // held by another source item's item (SourceItem.FoldInto)
	Unfolded  Action = "unfolded" // no longer: given an item of its own
)

// Applied is what Apply did, in order, and the item now.
type Applied struct {
	Item    Item
	Actions []Action
}

// ErrNotMirrored is Apply's answer when the path is taken by an item that
// wasn't copied from that source item (one made on the drive, say): a
// mirror doesn't write over what it didn't make. A directory is adopted
// instead.
var ErrNotMirrored = errors.New("the path holds an item not copied from that source item")

// Known is what the drive holds for a source item: the live item, or its
// trash entry, or neither (found false).
func (m *Mirror) Known(ctx context.Context, sourceID string) (item Item, trashed *TrashEntry, found bool, err error) {
	item, trashed, err = m.find(ctx, sourceID)
	if errors.Is(err, ErrNotFound) {
		return Item{}, nil, false, nil
	}
	if err != nil {
		return Item{}, nil, false, err
	}
	m.noteItem(item, "")
	return item, trashed, true, nil
}

// Apply makes the drive match the source item: it creates the item if the
// drive has none for it; restores it if it's in the trash; moves it if
// its path changed; writes a new version if the version held differs from
// src's (the times and the origin in the same write); sets its times if
// only they differ from src's (on a drive that's a TimeSetter: a
// directory's creation time, a file's creation and modified times); or
// does nothing.
//
// A version is written as the cheapest thing that holds its bytes (on a
// drive that keeps references, WriteOptions.SameAs): a stub of the item's
// own current version if the bytes are the same (the source's new
// version is kept, at its time); else a stub of a version the hash map
// has with the same SHA-256 and size (a twin); else the bytes. A source
// hash the map knows (SourceItem.Hash) decides without opening the
// source; any other version is read to its end first (a Spool), its
// SHA-256 taken on the way.
//
// A source item with FoldInto set, that the drive holds nothing for, is
// folded into the item of the one it names, if that's live with the same
// SHA-256 and size: an alias on its origin (Folded), nothing else
// written, and the item moved to FoldPath if set. An alias stays one
// while its FoldInto and its bytes agree with the item's; else it's
// dropped (Unfolded) and given an item of its own.
func (m *Mirror) Apply(ctx context.Context, src SourceItem) (Applied, error) {
	o := src.Origin
	if o.Drive == "" {
		o.Drive = m.source
	}
	if o.Drive != m.source {
		return Applied{}, fmt.Errorf("source item from %q on a mirror of %q: %w", o.Drive, m.source, ErrInvalid)
	}
	if o.ID == "" || src.Path == "" {
		return Applied{}, fmt.Errorf("a source item needs an id and a path: %w", ErrInvalid)
	}
	if o.Copied.IsZero() {
		o.Copied = time.Now().UTC()
	}
	o.Aliases = nil // the mirror's to keep, not the source's to say
	if src.Type == "" {
		src.Type = TypeFile
	}

	// What's learnt of the content (its SHA-256, or a spool of it) is
	// used by every write that follows: it's read once.
	c := &content{}
	defer c.close()
	var pre []Action
	item, trashed, err := m.find(ctx, o.ID)
	if err == nil && trashed == nil && item.Alias != "" {
		a, held, aerr := m.applyAlias(ctx, &src, o, item, c)
		if aerr != nil || held {
			return a, aerr
		}
		pre = []Action{Unfolded}
		item, err = Item{}, fmt.Errorf("source item %s: %w", o.ID, ErrNotFound)
	}
	if errors.Is(err, ErrNotFound) && src.FoldInto != "" && src.Type != TypeDir {
		a, ok, err := m.fold(ctx, &src, o, c)
		if err != nil {
			return Applied{}, err
		}
		if ok {
			a.Actions = append(pre, a.Actions...)
			return a, nil
		}
	}
	a, err := m.apply(ctx, src, o, item, trashed, err, c)
	if err != nil {
		return Applied{}, err
	}
	a.Actions = append(pre, a.Actions...)
	return a, nil
}

// apply is Apply for a source item that isn't an alias: item and trashed
// are what find gave, or err.
func (m *Mirror) apply(ctx context.Context, src SourceItem, o Origin, item Item, trashed *TrashEntry, err error, c *content) (Applied, error) {
	var done []Action
	// The item's aliases stay with it, and a folded item sits at its
	// FoldPath, if the source gives one.
	var had *Origin
	if trashed != nil {
		had = trashed.Origin
	} else if err == nil {
		had = item.Origin
	}
	if had != nil && had.Drive == o.Drive && had.ID == o.ID {
		o.Aliases = had.Aliases
	}
	if len(o.Aliases) > 0 && src.FoldPath != "" {
		src.Path = src.FoldPath
	}
	if errors.Is(err, ErrNotFound) {
		// A file is written if its path is free, which is the write's own
		// condition: the path is looked at only if it's taken.
		if src.Type != TypeDir {
			item, err := m.write(m.at(ctx, born(src)), src, o, Condition{IfNoneMatch: true}, nil, c)
			var pe *PreconditionError
			if err == nil {
				return Applied{Item: item, Actions: []Action{Created}}, nil
			}
			if !errors.As(err, &pe) && !errors.Is(err, ErrIsDir) {
				return Applied{}, err
			}
		}
		// An item copied before the drive kept an index of origins, still
		// where it was copied to, is adopted: indexed now, then brought up
		// to date like any other.
		at, serr := m.d.Stat(ctx, ByPath(src.Path), At{})
		if serr != nil && !errors.Is(serr, ErrNotFound) {
			return Applied{}, serr
		}
		if serr != nil || at.Origin == nil || at.Origin.Drive != m.source || at.Origin.ID != o.ID {
			return m.create(ctx, src, o, at, serr == nil, c)
		}
		if err := m.d.SetOrigin(ctx, ItemRef{ID: at.ID, Path: at.Path}, *at.Origin); err != nil {
			return Applied{}, err
		}
		item, err = at, nil
	}
	switch {
	case err != nil:
		return Applied{}, err
	case trashed != nil:
		item, err = m.d.Restore(m.at(ctx, src.Changed), trashed.ID, RestoreOptions{To: src.Path, Parents: true})
		if err != nil {
			return Applied{}, fmt.Errorf("restore %s: %w", src.Path, err)
		}
		done = append(done, Restored)
	case item.Path != src.Path:
		mctx := m.at(ctx, src.Changed)
		if err := m.parentOf(mctx, src.Path); err != nil {
			return Applied{}, err
		}
		moved, err := m.d.Move(mctx, ItemRef{ID: item.ID, Path: item.Path}, src.Path, false)
		if err != nil {
			return Applied{}, fmt.Errorf("move %s to %s: %w", item.Path, src.Path, err)
		}
		item = moved
		done = append(done, Moved)
	}
	m.noteItem(item, "")

	if src.Type == TypeDir || item.Origin != nil && item.Origin.Version == o.Version {
		// Nothing to write but what the item's record lacks: its times,
		// app data, mode, Windows attributes and origin (the source item's
		// path there, say), in one write and no new version. A
		// directory's modified time is its entries', here as anywhere, so
		// only its creation time follows the source after it's made.
		modified := src.ModTime
		if src.Type == TypeDir {
			modified = time.Time{}
		}
		item, changed, err := m.reinfo(ctx, item, modified, src, o)
		if err != nil {
			return Applied{}, err
		}
		if changed {
			done = append(done, Retimed)
		}
		if len(done) == 0 {
			done = append(done, Unchanged)
		}
		return Applied{Item: item, Actions: done}, nil
	}
	cond := Condition{}
	if item.Version != nil {
		cond.IfMatch = item.Version.ID
	}
	item, err = m.write(m.at(ctx, src.ModTime), src, o, cond, &item, c)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Item: item, Actions: append(done, Updated)}, nil
}

// content is what one Apply has learnt of a source item's bytes: their
// SHA-256 and size, from the source's hash, an alias's record or a spool
// of them (sp), which later writes read instead of the source.
type content struct {
	sha  string
	size int64
	sp   *Spool
}

func (c *content) close() { c.sp.Close() }

// open reads the spool: a SourceItem's Open, once it has been read.
func (c *content) open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(c.sp.Reader()), nil
}

// measure learns src's SHA-256 and size into c, if it hasn't: from a
// source hash the map knows, else by reading the source into a spool
// (its own kind of hash recorded on the way).
func (m *Mirror) measure(ctx context.Context, src SourceItem, c *content) error {
	if c.sha != "" {
		return nil
	}
	if sha, e, ok := m.sourceHit(src); ok {
		c.sha, c.size = sha, e.Size
		return nil
	}
	if src.Open == nil {
		return fmt.Errorf("%s: the content is needed and there's no Open: %w", src.Path, ErrInvalid)
	}
	sp, done, closeSrc, err := m.spoolSource(ctx, src, 0)
	closeSrc()
	if err != nil {
		return err
	}
	c.sha, c.size, c.sp = done(), sp.Size(), sp
	return nil
}

// spoolSource reads src's content into a Spool, with SpoolOptions'
// InFile inFile: to its end, unless it's streamed. done, called once the
// spool has had the whole body (at once, or at a streamed one's end),
// is its SHA-256 (hex), and records what the source's own kind of hash
// of it is in it. closeSrc closes the source, which a streamed spool
// still reads from.
func (m *Mirror) spoolSource(ctx context.Context, src SourceItem, inFile int64) (sp *Spool, done func() string, closeSrc func() error, err error) {
	var kind *hashKind
	var kh hash.Hash
	opts := SpoolOptions{InFile: inFile}
	if k, ok := m.kinds[src.Hash.Kind]; ok && src.Hash.Kind != "" {
		kind, kh = &k, k.new()
		opts.Hashes = []hash.Hash{kh}
	}
	body := &openOnRead{ctx: ctx, src: src}
	if sp, err = NewSpool(body, opts); err != nil {
		return nil, nil, body.Close, err
	}
	done = func() string {
		sha := hex.EncodeToString(sp.SHA256)
		if kind != nil {
			value := hex.EncodeToString(kh.Sum(nil))
			if kind.encode != nil {
				value = kind.encode(kh.Sum(nil))
			}
			m.hashes.AddSource(Hash{Kind: src.Hash.Kind, Value: value}, sha)
		}
		return sha
	}
	return sp, done, body.Close, nil
}

// aliasAt is the index of id in o's aliases, or -1.
func aliasAt(o *Origin, id string) int {
	if o == nil {
		return -1
	}
	return slices.IndexFunc(o.Aliases, func(a Alias) bool { return a.ID == id })
}

// holds reports whether item's current content is sha (hex) at size.
func holds(item Item, sha string, size int64) bool {
	return item.Checksums != nil && item.Checksums.SHA256 != "" && sha != "" &&
		strings.EqualFold(item.Checksums.SHA256, sha) && item.Size == size
}

// fold folds src into the item of the source item it names (FoldInto),
// if the drive holds that item live, as its origin, with src's bytes:
// src's id becomes one of the item's aliases (its origin record, on a
// drive that keeps them), and nothing else is written; then the item is
// moved to src.FoldPath, if that's elsewhere. ok is false, and nothing
// done, if it isn't folded.
func (m *Mirror) fold(ctx context.Context, src *SourceItem, o Origin, c *content) (Applied, bool, error) {
	w, trashed, err := m.find(ctx, src.FoldInto)
	if errors.Is(err, ErrNotFound) {
		return Applied{}, false, nil
	}
	if err != nil {
		return Applied{}, false, err
	}
	if trashed != nil || w.Alias != "" || w.Type != TypeFile || w.Origin == nil ||
		w.Origin.ID != src.FoldInto || w.Checksums == nil || w.Checksums.SHA256 == "" {
		return Applied{}, false, nil
	}
	if err := m.measure(ctx, *src, c); err != nil {
		return Applied{}, false, err
	}
	if !holds(w, c.sha, c.size) {
		return Applied{}, false, nil
	}
	origin := *w.Origin
	origin.Aliases = slices.DeleteFunc(slices.Clone(origin.Aliases), func(a Alias) bool { return a.ID == o.ID })
	origin.Aliases = append(origin.Aliases, Alias{
		ID: o.ID, Version: o.Version, Path: o.Path, Created: born(*src), Copied: o.Copied,
		SHA256: strings.ToLower(c.sha), Size: c.size,
	})
	if err := m.d.SetOrigin(ctx, ItemRef{ID: w.ID, Path: w.Path}, origin); err != nil {
		return Applied{}, false, fmt.Errorf("fold %s into %s: %w", src.Path, w.Path, err)
	}
	w.Origin, w.Alias = &origin, o.ID
	a, err := m.foldPath(ctx, *src, w, []Action{Folded})
	return a, true, err
}

// foldPath moves the folded item to src.FoldPath, if it's set and
// elsewhere: one move, its history and origin as they are.
func (m *Mirror) foldPath(ctx context.Context, src SourceItem, item Item, done []Action) (Applied, error) {
	if src.FoldPath == "" || src.FoldPath == item.Path {
		return Applied{Item: item, Actions: done}, nil
	}
	mctx := m.at(ctx, src.Changed)
	if err := m.parentOf(mctx, src.FoldPath); err != nil {
		return Applied{}, err
	}
	moved, err := m.d.Move(mctx, ItemRef{ID: item.ID, Path: item.Path}, src.FoldPath, false)
	if err != nil {
		return Applied{}, fmt.Errorf("move %s to %s: %w", item.Path, src.FoldPath, err)
	}
	moved.Alias = item.Alias
	if moved.Origin == nil {
		moved.Origin = item.Origin
	}
	return Applied{Item: moved, Actions: append(done, Moved)}, nil
}

// applyAlias is Apply for a source item folded into item (an alias of its
// origin). The fold holds while src still names item's origin (FoldInto)
// and the bytes agree, the item's and src's version's: then the alias's
// version is recorded if it's new (Folded), and the item moved to
// src.FoldPath. Otherwise the alias is dropped from item (unfolded), held
// false, for Apply to give it an item of its own; c then has the bytes'
// SHA-256 if they're known.
func (m *Mirror) applyAlias(ctx context.Context, src *SourceItem, o Origin, item Item, c *content) (Applied, bool, error) {
	i := aliasAt(item.Origin, o.ID)
	if i < 0 {
		return Applied{}, false, fmt.Errorf("%s isn't an alias of %s: %w", o.ID, item.Path, ErrInvalid)
	}
	al := item.Origin.Aliases[i]
	held := src.Type != TypeDir && src.FoldInto == item.Origin.ID && holds(item, al.SHA256, al.Size)
	if held && o.Version != al.Version {
		// A new version at the source: still the item's bytes?
		if err := m.measure(ctx, *src, c); err != nil {
			return Applied{}, false, err
		}
		held = holds(item, c.sha, c.size)
	} else if o.Version == al.Version && c.sha == "" {
		c.sha, c.size = al.SHA256, al.Size // the alias's bytes: no download
	}
	if !held {
		return Applied{}, false, m.unalias(ctx, item, o.ID)
	}
	done := []Action{}
	if o.Version != al.Version || o.Path != al.Path {
		origin := *item.Origin
		origin.Aliases = slices.Clone(origin.Aliases)
		al.Version, al.Path, al.Copied = o.Version, o.Path, o.Copied
		origin.Aliases[i] = al
		if err := m.d.SetOrigin(ctx, ItemRef{ID: item.ID, Path: item.Path}, origin); err != nil {
			return Applied{}, false, fmt.Errorf("record %s on %s: %w", o.ID, item.Path, err)
		}
		item.Origin = &origin
		done = append(done, Folded)
	}
	a, err := m.foldPath(ctx, *src, item, done)
	if err == nil && len(a.Actions) == 0 {
		a.Actions = []Action{Unchanged}
	}
	return a, true, err
}

// unalias drops the alias id from item's origin (and its origin record,
// on a drive that keeps them).
func (m *Mirror) unalias(ctx context.Context, item Item, id string) error {
	origin := *item.Origin
	origin.Aliases = slices.DeleteFunc(slices.Clone(origin.Aliases), func(a Alias) bool { return a.ID == id })
	if err := m.d.SetOrigin(ctx, ItemRef{ID: item.ID, Path: item.Path}, origin); err != nil {
		return fmt.Errorf("unfold %s from %s: %w", id, item.Path, err)
	}
	return nil
}

// promote hands item's origin to its oldest alias, as its origin is
// removed at the source: the alias created first there (Alias.Created;
// one with no time after those with one), else folded first. The others
// stay aliases. An alias no longer folded into item (given an item of its
// own since) is dropped. It reports false if no alias was left to take
// over.
func (m *Mirror) promote(ctx context.Context, item Item) (bool, error) {
	var live []Alias
	for _, a := range item.Origin.Aliases {
		it, trashed, err := m.find(ctx, a.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if trashed == nil && it.ID == item.ID && it.Alias == a.ID {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return false, nil
	}
	slices.SortStableFunc(live, func(x, y Alias) int {
		switch {
		case x.Created.IsZero() != y.Created.IsZero():
			if x.Created.IsZero() {
				return 1
			}
			return -1
		default:
			return x.Created.Compare(y.Created)
		}
	})
	heir := live[0]
	origin := Origin{
		Drive: item.Origin.Drive, ID: heir.ID, Version: heir.Version, Path: heir.Path, Copied: heir.Copied,
		Aliases: live[1:],
	}
	if len(origin.Aliases) == 0 {
		origin.Aliases = nil
	}
	if err := m.d.SetOrigin(ctx, ItemRef{ID: item.ID, Path: item.Path}, origin); err != nil {
		return false, fmt.Errorf("hand %s to %s: %w", item.Path, heir.ID, err)
	}
	return true, nil
}

// parentOf makes p's parent directory if it doesn't exist yet, as a
// write with Parents would: Move doesn't.
func (m *Mirror) parentOf(ctx context.Context, p string) error {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return nil
	}
	if _, err := m.d.Stat(ctx, ByPath(dir), At{}); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := m.d.Mkdir(ctx, dir, true)
	return err
}

// sameOrigin reports whether a says what o does, when it was copied aside.
func sameOrigin(a *Origin, o Origin) bool {
	return a != nil && a.Drive == o.Drive && a.ID == o.ID && a.Version == o.Version && a.Path == o.Path &&
		slices.EqualFunc(a.Aliases, o.Aliases, func(x, y Alias) bool {
			return x.ID == y.ID && x.Version == y.Version && x.Path == y.Path && x.Created.Equal(y.Created) &&
				x.Copied.Equal(y.Copied) && x.SHA256 == y.SHA256 && x.Size == y.Size
		})
}

// create makes a source item the drive has never held; existing is
// what's at its path, if taken.
func (m *Mirror) create(ctx context.Context, src SourceItem, o Origin, existing Item, taken bool, c *content) (Applied, error) {
	ctx = m.at(ctx, born(src))
	var err error
	if src.Type == TypeDir {
		item := existing
		modified := time.Time{} // an adopted directory's is its entries'
		if !taken {
			if item, err = m.d.Mkdir(ctx, src.Path, true); err != nil {
				return Applied{}, err
			}
			modified = src.ModTime
		} else if existing.Type != TypeDir {
			return Applied{}, fmt.Errorf("%s: %w", src.Path, ErrNotMirrored)
		}
		if item, _, err = m.reinfo(ctx, item, modified, src, o); err != nil {
			return Applied{}, err
		}
		return Applied{Item: item, Actions: []Action{Created}}, nil
	}
	if taken {
		return Applied{}, fmt.Errorf("%s: %w", src.Path, ErrNotMirrored)
	}
	item, err := m.write(ctx, src, o, Condition{IfNoneMatch: true}, nil, c)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Item: item, Actions: []Action{Created}}, nil
}

// write stores src's content at its path, its time and origin with it,
// over cur (the item as it is, nil for a new one): a stub where the hash
// map or cur already has the bytes (Apply), else the bytes. A stub the
// drive can't make (ErrRefUnavailable, ErrForbidden) drops its map entry
// and the bytes are written.
func (m *Mirror) write(ctx context.Context, src SourceItem, o Origin, cond Condition, cur *Item, c *content) (Item, error) {
	if src.Open == nil {
		return Item{}, fmt.Errorf("%s: the content is needed and there's no Open: %w", src.Path, ErrInvalid)
	}
	opts := WriteOptions{
		Parents: true, ContentType: src.ContentType, ModTime: src.ModTime, Created: src.Created, Origin: &o,
		App: src.App, Mode: src.Mode, WindowsAttrs: src.WindowsAttrs,
	}
	if c.sp != nil {
		src.Open = c.open // read once already, for a fold
	}
	// A source hash the map knows, or a SHA-256 known for the content
	// (an alias's, unfolded): no download. The source is opened only if
	// the drive reads the body (one that keeps no references).
	sha, e, ok := m.sourceHit(src)
	if !ok && c.sha != "" && c.sp == nil {
		sha = c.sha
		e, ok = m.hashes.Lookup(sha)
		ok = ok && e.Size == c.size && c.size > 0
	}
	if ok {
		if src.Size <= 0 {
			src.Size = e.Size
		}
		if own, ok := current(cur, sha, src.Size); ok {
			e = own
		}
		body := &openOnRead{ctx: ctx, src: src}
		it, err := m.writeRef(ctx, src.Path, body, src.Size, cond, opts, sha, e)
		body.Close()
		if err == nil || !refused(err) {
			return it, err
		}
		m.hashes.Drop(sha, e)
	}

	// Else the bytes. The drive is given a body that reads the source
	// at its first Read, once the drive has checked the write's condition
	// (a refused write opens nothing), by the spool's tiers. Small, it's
	// read to its end and served; unless it's a stub after all, when the
	// probe stops that write, and the stub is written under the same
	// condition. Larger, in a file, the write is stopped too, and the
	// bytes go from the file. Larger still, past SpoolInFile, it's served
	// as it arrives, for the drive to stream (a multipart upload, on
	// icbfs), and its hash, known at its end, decides there: a stub stops
	// the write before it completes, so that nothing of it is written,
	// and the stub is written instead.
	pb := &probe{m: m, ctx: ctx, src: src, cur: cur}
	defer pb.Close()
	it, err := m.d.Write(ctx, src.Path, pb, cond, opts)
	if !pb.stopped || err == nil {
		if err != nil {
			return Item{}, err
		}
		m.noteItem(it, pb.sha)
		return it, nil
	}
	sp := pb.sp
	if sp.Streamed() {
		// Its bytes went by; for a drive that keeps no references, or one
		// that refuses this one, they're read from the source again.
		body := &openOnRead{ctx: ctx, src: src}
		defer body.Close()
		it, err := m.writeRef(ctx, src.Path, body, sp.Size(), cond, opts, pb.sha, *pb.stub)
		if err == nil || !refused(err) {
			return it, err
		}
		m.hashes.Drop(pb.sha, *pb.stub)
		if it, err = m.d.Write(ctx, src.Path, &openOnRead{ctx: ctx, src: src}, cond, opts); err != nil {
			return Item{}, err
		}
		m.noteItem(it, pb.sha)
		return it, nil
	}
	if pb.stub != nil {
		it, err := m.writeRef(ctx, src.Path, sp.Reader(), sp.Size(), cond, opts, pb.sha, *pb.stub)
		if err == nil || !refused(err) {
			return it, err
		}
		m.hashes.Drop(pb.sha, *pb.stub)
	}
	if fw, ok := m.d.(FileWriter); ok && sp.File() != nil {
		it, err = fw.WriteFrom(ctx, src.Path, sp, sp.Size(), cond, opts)
	} else {
		if sp.File() != nil {
			opts.Size = sp.Size() // a large one goes as it's read
		}
		it, err = m.d.Write(ctx, src.Path, sp.Reader(), cond, opts)
	}
	if err != nil {
		return Item{}, err
	}
	m.noteItem(it, pb.sha)
	return it, nil
}

// errDecided stops a write whose body turned out to be better written
// another way (probe).
var errDecided = errors.New("the mirror writes this content another way")

// probe is a source item's content as a mirror's write body: opened at
// the drive's first Read and read into a Spool, its SHA-256 (and the
// source's kind of hash) taken on the way. The same bytes as cur's
// current version, or as a version the hash map has, are a stub (stub),
// and the write is stopped (errDecided); so is one in a file, for the
// drive to read from it at offsets (FileWriter). Else it serves the
// bytes. A body past SpoolInFile is served as it arrives (the spool
// streams), and decided at its end: a stub fails the read that would
// have been the end (errDecided) instead of ending, so that the drive
// writes none of it.
type probe struct {
	m        *Mirror
	ctx      context.Context
	src      SourceItem
	cur      *Item
	sp       *Spool
	closeSrc func() error
	sha      string
	stub     *HashEntry
	r        io.Reader
	err      error
	read     bool
	stopped  bool
}

func (p *probe) Read(b []byte) (int, error) {
	if !p.read {
		p.read = true
		p.err = p.spool()
	}
	if p.err != nil {
		return 0, p.err
	}
	return p.r.Read(b)
}

// spool reads the source into the spool and decides what it is; or, for
// one that streams, decides at its end.
func (p *probe) spool() error {
	sp, done, closeSrc, err := p.m.spoolSource(p.ctx, p.src, SpoolInFile)
	p.closeSrc = closeSrc
	if err != nil {
		return err
	}
	p.sp = sp
	if sp.Streamed() {
		p.r = sp.Stream(func() error {
			p.sha = done()
			if p.decide() {
				p.stopped = true
				return errDecided
			}
			return nil
		})
		return nil
	}
	p.sha = done()
	if p.decide() || sp.File() != nil {
		p.stopped = true
		return errDecided
	}
	p.r = sp.Reader()
	return nil
}

// decide notes the version the spooled bytes are a stub of, if there's
// one, and reports whether there is.
func (p *probe) decide() bool {
	size := p.sp.Size()
	e, ok := current(p.cur, p.sha, size)
	if !ok {
		e, ok = p.m.hashes.Lookup(p.sha)
		ok = ok && e.Size == size
	}
	if ok && size > 0 { // nothing is cheaper than a stub of nothing
		p.stub = &e
	}
	return p.stub != nil
}

func (p *probe) Close() error {
	if p.closeSrc != nil {
		p.closeSrc()
	}
	return p.sp.Close()
}

// writeRef writes path as a stub of e, whose SHA-256 is sha, with body
// for a drive that keeps no references; and records it in the map.
func (m *Mirror) writeRef(ctx context.Context, p string, body io.Reader, size int64, cond Condition, opts WriteOptions, sha string, e HashEntry) (Item, error) {
	opts.SameAs = &Ref{ID: e.ID, Version: e.Version, SHA256: sha}
	opts.Size = size
	it, err := m.d.Write(ctx, p, body, cond, opts)
	if err != nil {
		return Item{}, err
	}
	m.noteItem(it, sha)
	return it, nil
}

// refused is whether a stub's write was refused for its target: one the
// drive can't reference, or may not read.
func refused(err error) bool {
	return errors.Is(err, ErrRefUnavailable) || errors.Is(err, ErrForbidden)
}

// current is cur's current version, if its content has SHA-256 sha and
// size size: what an identical consecutive version is a stub of.
func current(cur *Item, sha string, size int64) (HashEntry, bool) {
	if cur == nil || cur.Version == nil || cur.Version.ID == "" || cur.Checksums == nil ||
		!strings.EqualFold(cur.Checksums.SHA256, sha) || cur.Size != size {
		return HashEntry{}, false
	}
	return HashEntry{ID: cur.ID, Version: cur.Version.ID, Size: cur.Size}, true
}

// sourceHit is the SHA-256 and version the map has for src's own hash,
// at src's size.
func (m *Mirror) sourceHit(src SourceItem) (string, HashEntry, bool) {
	if src.Hash.Kind == "" || src.Hash.Value == "" || src.Size <= 0 {
		return "", HashEntry{}, false
	}
	sha, e, ok := m.hashes.LookupSource(src.Hash)
	if !ok || e.Size != src.Size {
		return "", HashEntry{}, false
	}
	return sha, e, true
}

// noteItem records a file the mirror wrote or read back in the map: its
// current version, by its recorded SHA-256 (else sha, the one the
// mirror computed for what it wrote).
func (m *Mirror) noteItem(it Item, sha string) {
	if it.Type != "" && it.Type != TypeFile || it.Version == nil {
		return
	}
	if it.Checksums != nil && it.Checksums.SHA256 != "" {
		sha = it.Checksums.SHA256
	}
	m.hashes.Add(sha, HashEntry{ID: it.ID, Version: it.Version.ID, Size: it.Size})
}

// openOnRead is a source item's content, opened at the first Read.
type openOnRead struct {
	ctx  context.Context
	src  SourceItem
	body io.ReadCloser
	err  error
}

func (r *openOnRead) Read(p []byte) (int, error) {
	if r.body == nil && r.err == nil {
		if r.body, r.err = r.src.Open(r.ctx); r.err != nil {
			r.err = fmt.Errorf("open %s at the source: %w", r.src.Path, r.err)
		}
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.body.Read(p)
}

func (r *openOnRead) Close() error {
	if r.body == nil {
		return nil
	}
	return r.body.Close()
}

// reinfo records on item what src says and item's record doesn't yet
// have: modified (zero to leave it) and src.Created where they differ,
// app keys whose values differ, a differing mode or Windows attributes,
// and origin o if it isn't item's; in one write on a drive that's an
// InfoSetter, else times (TimeSetter) and origin separately, and app
// keys, mode and attributes not at all (ErrNotSupported if given). It
// reports whether anything but the origin was written.
func (m *Mirror) reinfo(ctx context.Context, item Item, modified time.Time, src SourceItem, o Origin) (Item, bool, error) {
	info := ItemInfo{}
	if !modified.IsZero() && !modified.Equal(item.Modified) {
		info.Modified = modified
	}
	if !src.Created.IsZero() && !src.Created.Equal(item.Created) {
		info.Created = src.Created
	}
	for k, v := range src.App {
		cur, has := item.App[k]
		if v == nil && has || v != nil && (!has || !bytes.Equal(cur, v)) {
			if info.App == nil {
				info.App = map[string][]byte{}
			}
			info.App[k] = v
		}
	}
	if src.Mode != nil && *src.Mode != item.Mode {
		info.Mode = src.Mode
	}
	if src.WindowsAttrs != nil && (item.WindowsAttrs == nil || *item.WindowsAttrs != *src.WindowsAttrs) {
		info.WindowsAttrs = src.WindowsAttrs
	}
	changed := !info.Modified.IsZero() || !info.Created.IsZero() || len(info.App) > 0 || info.Mode != nil || info.WindowsAttrs != nil
	if !sameOrigin(item.Origin, o) {
		info.Origin = &o
	} else if !changed {
		return item, false, nil
	}
	ref := ItemRef{ID: item.ID, Path: item.Path}
	if is, ok := m.d.(InfoSetter); ok {
		set, err := is.SetInfo(ctx, ref, info)
		if err != nil {
			return item, false, fmt.Errorf("record %s: %w", item.Path, err)
		}
		if set.Origin == nil {
			set.Origin = &o
		}
		return set, changed, nil
	}
	if len(info.App) > 0 || info.Mode != nil || info.WindowsAttrs != nil {
		return item, false, fmt.Errorf("app data, mode or attributes on %s: %w", item.Path, ErrNotSupported)
	}
	if info.Origin != nil {
		if err := m.d.SetOrigin(ctx, ref, o); err != nil {
			return item, false, err
		}
		item.Origin = &o
	}
	if !changed {
		return item, false, nil
	}
	ts, ok := m.d.(TimeSetter)
	if !ok {
		return item, false, nil
	}
	retimed, err := ts.SetTimes(ctx, ref, info.Modified, info.Created)
	if err != nil {
		return item, false, fmt.Errorf("set the times of %s: %w", item.Path, err)
	}
	if retimed.Origin == nil {
		retimed.Origin = item.Origin
	}
	return retimed, true, nil
}

// Remove sends the item copied from the source item to the trash (a
// directory with everything in it), recorded as ctx's pass if it has one
// (WithPass). It reports whether there was a live item to remove; one
// already in the trash, or never held, is nothing to do. In source time
// (InSourceTime), the deletion is dated with ctx's source time: when the
// source deleted it. An alias (SourceItem.FoldInto) is dropped from the
// item it's folded into, and only it. An item's origin that has aliases
// isn't trashed: the oldest alias becomes its origin (promote).
func (m *Mirror) Remove(ctx context.Context, sourceID string) (bool, error) {
	item, trashed, err := m.find(ctx, sourceID)
	if errors.Is(err, ErrNotFound) || err == nil && trashed != nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if item.Alias != "" {
		// Folded into another's item: only the alias goes.
		return true, m.unalias(ctx, item, sourceID)
	}
	if item.Origin != nil && item.Origin.ID == sourceID && len(item.Origin.Aliases) > 0 {
		// The item's origin, with aliases: the oldest takes over, and the
		// item stays.
		if ok, err := m.promote(ctx, item); err != nil || ok {
			return ok, err
		}
	}
	if err := m.d.Delete(ctx, ItemRef{ID: item.ID, Path: item.Path}, item.Type == TypeDir, Condition{}); err != nil {
		return false, err
	}
	return true, nil
}

// Undo restores everything the pass removed, each where it was: what a
// daemon does when it finds a pass ran on a bad listing. It returns how
// many items it brought back (a directory counts once, with its contents).
func (m *Mirror) Undo(ctx context.Context, pass string) (int, error) {
	if pass == "" {
		return 0, fmt.Errorf("undo needs a pass: %w", ErrInvalid)
	}
	var entries []TrashEntry
	for cursor := ""; ; {
		page, err := m.d.Trash(ctx, TrashOptions{Pass: pass, Cursor: cursor})
		if err != nil {
			return 0, err
		}
		entries = append(entries, page.Entries...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	n := 0
	// Oldest first, so a directory removed before something put back in it
	// is back first; inside a recursive delete only the directory that
	// started it, which brings back the rest.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Batch != "" && e.Batch != e.ID {
			continue
		}
		if _, err := m.d.Restore(ctx, e.ID, RestoreOptions{Parents: true}); err != nil {
			return n, fmt.Errorf("restore %s: %w", e.Path, err)
		}
		n++
	}
	return n, nil
}

// find is the drive's item, or trash entry, for the source item.
func (m *Mirror) find(ctx context.Context, sourceID string) (Item, *TrashEntry, error) {
	if f, ok := m.d.(OriginFinder); ok {
		return f.FindByOrigin(ctx, m.source, sourceID)
	}
	return m.search(ctx, sourceID)
}

// search finds by origin on a backend that can't: a listing of the whole
// drive, then of its trash.
func (m *Mirror) search(ctx context.Context, sourceID string) (Item, *TrashEntry, error) {
	var found, alias *Item
	for cursor := ""; ; {
		page, err := m.d.List(ctx, ByPath("/"), ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return Item{}, nil, err
		}
		for i := range page.Items {
			it := page.Items[i]
			if it.Origin != nil && it.Origin.Drive == m.source && it.Origin.ID == sourceID {
				if found == nil || it.Origin.Copied.After(found.Origin.Copied) {
					found = &it // the newest claim wins
				}
			} else if alias == nil && it.Origin != nil && it.Origin.Drive == m.source && aliasAt(it.Origin, sourceID) >= 0 {
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
	trash, err := m.d.Trash(ctx, TrashOptions{OriginDrive: m.source, OriginID: sourceID, Limit: 1})
	if err != nil {
		return Item{}, nil, err
	}
	if len(trash.Entries) > 0 {
		return Item{}, &trash.Entries[0], nil
	}
	return Item{}, nil, fmt.Errorf("source item %s: %w", sourceID, ErrNotFound)
}

// Hash is a source's own hash of an item's content: its kind (as the
// source names it, "quickXorHash" say) and its value, as the source
// gives it.
type Hash struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// HashEntry is a version a mirror wrote or read with some content: the
// item, its version, and the content's size.
type HashEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Size    int64  `json:"size"`
}

// less orders entries by item id, then version: of two versions with the
// same content, a map keeps the lesser, so that one rebuilt from a
// listing is the one the writes built.
func (e HashEntry) less(o HashEntry) bool {
	if e.ID != o.ID {
		return e.ID < o.ID
	}
	return e.Version < o.Version
}

// A HashMap is what a mirror knows of the content on its drive: a
// version holding each SHA-256 it has seen, and what the source's own
// hashes are in SHA-256. The mirror writes a stub of that version for a
// source item with the same content, not its bytes again. It's safe for
// concurrent use.
type HashMap struct {
	mu     sync.Mutex
	sha    map[string]HashEntry // by SHA-256, lowercase hex
	source map[Hash]string      // a source hash's SHA-256
}

// NewHashMap is an empty map.
func NewHashMap() *HashMap {
	return &HashMap{sha: map[string]HashEntry{}, source: map[Hash]string{}}
}

// Lookup is the version recorded for content with SHA-256 sha (hex).
func (h *HashMap) Lookup(sha string) (HashEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.sha[strings.ToLower(sha)]
	return e, ok
}

// LookupSource is the SHA-256 a source hash was seen to be, and its
// version.
func (h *HashMap) LookupSource(s Hash) (string, HashEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sha, ok := h.source[s]
	if !ok {
		return "", HashEntry{}, false
	}
	e, ok := h.sha[sha]
	return sha, e, ok
}

// Add records that e holds content with SHA-256 sha. Of two versions with
// the same content, the map keeps one (the lesser by item id, then
// version), whichever came first.
func (h *HashMap) Add(sha string, e HashEntry) {
	if sha == "" || e.ID == "" || e.Version == "" {
		return
	}
	sha = strings.ToLower(sha)
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.sha[sha]; !ok || e.less(cur) {
		h.sha[sha] = e
	}
}

// AddSource records that the source hash s is content with SHA-256 sha.
func (h *HashMap) AddSource(s Hash, sha string) {
	if s.Kind == "" || s.Value == "" || sha == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.source[s] = strings.ToLower(sha)
}

// Drop forgets sha's version if it's still e: one the drive can't
// reference any more (erased, say).
func (h *HashMap) Drop(sha string, e HashEntry) {
	sha = strings.ToLower(sha)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sha[sha] == e {
		delete(h.sha, sha)
	}
}

// set replaces the map's contents.
func (h *HashMap) set(sha map[string]HashEntry, source map[Hash]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sha, h.source = sha, source
}

// Entries is a copy of the map by SHA-256.
func (h *HashMap) Entries() map[string]HashEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.sha)
}

// SourceEntries is a copy of what the source's hashes are in SHA-256.
func (h *HashMap) SourceEntries() map[Hash]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.source)
}

// hashMapJSON is a HashMap as it's kept.
type hashMapJSON struct {
	Format  int                  `json:"format"`
	SHA256  map[string]HashEntry `json:"sha256"`
	Sources []sourceHashJSON     `json:"sources,omitempty"`
}

type sourceHashJSON struct {
	Hash
	SHA256 string `json:"sha256"`
}

// MarshalJSON is the map as a HashStore keeps it.
func (h *HashMap) MarshalJSON() ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := hashMapJSON{Format: 1, SHA256: h.sha}
	for s, sha := range h.source {
		out.Sources = append(out.Sources, sourceHashJSON{Hash: s, SHA256: sha})
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		a, b := out.Sources[i], out.Sources[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Value < b.Value
	})
	return json.Marshal(out)
}

// UnmarshalJSON replaces the map with a kept one: ErrInvalid if it isn't.
func (h *HashMap) UnmarshalJSON(data []byte) error {
	var in hashMapJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("a hash map: %v: %w", err, ErrInvalid)
	}
	if in.Format != 1 {
		return fmt.Errorf("a hash map of format %d: %w", in.Format, ErrInvalid)
	}
	sha := map[string]HashEntry{}
	for k, e := range in.SHA256 {
		sha[strings.ToLower(k)] = e
	}
	source := map[Hash]string{}
	for _, s := range in.Sources {
		source[s.Hash] = strings.ToLower(s.SHA256)
	}
	h.set(sha, source)
	return nil
}

// A HashStore keeps a mirror's hash map between runs (a file beside a
// connector's state, say): LoadHashes gives what SaveHashes was last
// given, or nil if nothing is kept.
type HashStore interface {
	LoadHashes(ctx context.Context) ([]byte, error)
	SaveHashes(ctx context.Context, data []byte) error
}

// Hashes is the mirror's hash map.
func (m *Mirror) Hashes() *HashMap { return m.hashes }

// WithHashStore makes the mirror load and save its hash map in s
// (LoadHashes, SaveHashes).
func (m *Mirror) WithHashStore(s HashStore) *Mirror {
	m.hashStore = s
	return m
}

// LoadHashes replaces the hash map with the one the HashStore keeps; none
// kept leaves it empty. One that isn't a hash map is ErrInvalid, and the
// map is left empty: a connector sets it aside and rebuilds
// (RebuildHashes).
func (m *Mirror) LoadHashes(ctx context.Context) error {
	if m.hashStore == nil {
		return fmt.Errorf("no hash store: %w", ErrInvalid)
	}
	data, err := m.hashStore.LoadHashes(ctx)
	if err != nil {
		return err
	}
	h := NewHashMap()
	if len(data) > 0 {
		err = h.UnmarshalJSON(data)
	}
	if err != nil {
		h = NewHashMap()
	}
	m.hashes.set(h.sha, h.source)
	return err
}

// SaveHashes gives the HashStore the hash map as it is now.
func (m *Mirror) SaveHashes(ctx context.Context) error {
	if m.hashStore == nil {
		return fmt.Errorf("no hash store: %w", ErrInvalid)
	}
	data, err := m.hashes.MarshalJSON()
	if err != nil {
		return err
	}
	return m.hashStore.SaveHashes(ctx, data)
}

// RebuildHashes rebuilds the hash map from the drive: every version of
// every live file copied from the source, by the SHA-256 recorded for it.
// A version with none recorded is skipped, not computed. What the
// source's hashes are is kept for the SHA-256s still there: the drive
// doesn't know them.
func (m *Mirror) RebuildHashes(ctx context.Context) error {
	h := NewHashMap()
	for cursor := ""; ; {
		page, err := m.d.List(ctx, ByPath("/"), ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			if it.Type != TypeFile || it.Origin == nil || it.Origin.Drive != m.source {
				continue
			}
			vs, err := m.d.Versions(ctx, ItemRef{ID: it.ID, Path: it.Path})
			if errors.Is(err, ErrNotFound) {
				continue // gone since the listing
			}
			if err != nil {
				return fmt.Errorf("versions of %s: %w", it.Path, err)
			}
			for _, v := range vs {
				if v.Checksums != nil && v.Checksums.SHA256 != "" {
					h.Add(v.Checksums.SHA256, HashEntry{ID: it.ID, Version: v.ID, Size: v.Size})
				}
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	for s, sha := range m.hashes.SourceEntries() {
		if _, ok := h.sha[sha]; ok {
			h.source[s] = sha
		}
	}
	m.hashes.set(h.sha, h.source)
	return nil
}
