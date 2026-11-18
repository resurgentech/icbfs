package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/resurgentech/icbfs/internal/pb"
)

// A version's date is the store's LastModified, unless the item's
// .version object says otherwise: a table, version id -> time, for the
// versions of an item's objects that came to be at another time (a
// mirror's, written in its source's time: WithDeclaredTime). Views of the
// past (AsOf) and version listings go by the effective date.
//
// An item that has a .version has every version of its content and its
// .metadata in it (delete markers aside): the versions written in a
// source's time at that time, any other at the store's (DateAll). Its
// first record lists the versions it had before, at the store's times.
//
// A .version object is <base>.version, where base is the item's key: the
// one object serving its content (or a directory's root block) and its
// companions (.metadata, .ads.N, .lock). A directory's split blocks, under
// keys of their own, have their own.

// VersionSuffix names an item's .version object.
const VersionSuffix = ".version"

// versionDatesLayout is the .version format this package reads and writes.
const versionDatesLayout = 1

// Dates is the override table of one item: version id -> time.
type Dates map[string]time.Time

// BaseKey is the item key a key belongs to: the key itself, or the one
// before a companion suffix (.metadata, .ads.N, .lock, .refs, .version).
func BaseKey(key string) string {
	for _, suffix := range []string{".metadata", ".lock", ".refs", VersionSuffix} {
		if base, ok := strings.CutSuffix(key, suffix); ok {
			return base
		}
	}
	if i := strings.LastIndex(key, ".ads."); i > 0 {
		return key[:i]
	}
	return key
}

// Time is o's effective date: dates' entry for it, else the store's, to
// the second. A store's listing may give a version's time to the
// millisecond (MinIO) while its answer to the write, which is what dates
// the versions a write records (DateAll), gives whole seconds (every
// store; S3's and Azure's listings too): a version no table lists, such
// as a directory block an undated write left, is dated as the write's
// recorded versions are, so that the versions of one write, and a write
// dated at a directory's newest state after it, compare as written.
func (d Dates) Time(o *Object) time.Time {
	if t, ok := d[o.VersionID]; ok {
		return t
	}
	return StoreSecond(o.LastModified)
}

// StoreSecond is t to the whole second below: a store's time as its
// answers to a write give it.
func StoreSecond(t time.Time) time.Time { return t.Truncate(time.Second) }

// Pick is, from one key's history (newest first), the version current at
// at by effective date: the latest-dated version at or before at, the
// newest stored of several dated alike. ok is false if the key didn't
// exist then (no version, or a deletion).
func (d Dates) Pick(history []Object, at time.Time) (Object, bool) {
	var best *Object
	var bestAt time.Time
	for i := range history {
		o := &history[i]
		t := d.Time(o)
		if t.After(at) {
			continue
		}
		if best == nil || t.After(bestAt) {
			best, bestAt = o, t
		}
	}
	if best == nil {
		return Object{}, false
	}
	return *best, !best.Deleted
}

// UpTo is the entries of history dated at or before at, in the order given.
func (d Dates) UpTo(history []Object, at time.Time) []Object {
	var out []Object
	for _, o := range history {
		if !d.Time(&o).After(at) {
			out = append(out, o)
		}
	}
	return out
}

func encodeDates(d Dates) ([]byte, error) {
	w := &pb.VersionDates{Dates: make(map[string]*timestamppb.Timestamp, len(d)), Layout: versionDatesLayout}
	for id, t := range d {
		w.Dates[id] = timestamppb.New(t)
	}
	return proto.Marshal(w)
}

func decodeDates(data []byte) (Dates, error) {
	if len(data) == 0 {
		return Dates{}, nil
	}
	var w pb.VersionDates
	if err := proto.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("decode .version: %w", err)
	}
	if w.Layout != versionDatesLayout {
		return nil, fmt.Errorf(".version layout %d, this build reads %d", w.Layout, versionDatesLayout)
	}
	d := make(Dates, len(w.Dates))
	for id, ts := range w.Dates {
		d[id] = ts.AsTime()
	}
	return d, nil
}

// ReadDates is base's override table as it is now: empty if it has none.
func ReadDates(ctx context.Context, store Store, base string) (Dates, error) {
	d, _, err := readDates(ctx, store, base)
	return d, err
}

// DatesRead holds the .version tables an operation has read or written,
// so that the operation reads each no more than once: a write that looks
// at its directory's table before it writes (how late it is) and dates
// what it wrote after costs the one read it always cost. A table written
// meanwhile by anyone else fails the operation's conditional write of it,
// which then reads it again. Safe for concurrent use.
type DatesRead struct {
	mu sync.Mutex
	m  map[string]datesSeen
}

type datesSeen struct {
	dates Dates
	obj   *Object // nil: none stored (dates is then what a first record would list, or empty)
	full  bool    // with obj nil: dates is storeDates', complete
}

type datesReadKey struct{}

// WithDatesRead has the .version tables read and written under ctx kept
// in r for the reads after (see DatesRead).
func WithDatesRead(ctx context.Context, r *DatesRead) context.Context {
	return context.WithValue(ctx, datesReadKey{}, r)
}

func datesReadFrom(ctx context.Context) *DatesRead {
	r, _ := ctx.Value(datesReadKey{}).(*DatesRead)
	return r
}

func (r *DatesRead) get(base string) (datesSeen, bool) {
	if r == nil {
		return datesSeen{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.m[base]
	if ok {
		c := make(Dates, len(s.dates))
		for k, v := range s.dates {
			c[k] = v
		}
		s.dates = c
	}
	return s, ok
}

func (r *DatesRead) put(base string, s datesSeen) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]datesSeen{}
	}
	c := make(Dates, len(s.dates))
	for k, v := range s.dates {
		c[k] = v
	}
	s.dates = c
	r.m[base] = s
}

func (r *DatesRead) forget(base string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.m, base)
	r.mu.Unlock()
}

// StoreDatesRead is base's table as it is now, through ctx's DatesRead:
// read once, and, when base has none, what its first record will list
// (every version its dated objects have, at the store's times) with
// complete true.
func StoreDatesRead(ctx context.Context, store Store, base string) (d Dates, none bool, err error) {
	cur, obj, err := readDates(ctx, store, base)
	if err != nil || obj != nil {
		return cur, false, err
	}
	r := datesReadFrom(ctx)
	if s, ok := r.get(base); ok && s.full {
		return s.dates, true, nil
	}
	if cur, err = storeDates(WithoutDeclaredTime(ctx), store, base); err != nil {
		return nil, true, err
	}
	r.put(base, datesSeen{dates: cur, full: true})
	return cur, true, nil
}

func readDates(ctx context.Context, store Store, base string) (Dates, *Object, error) {
	if s, ok := datesReadFrom(ctx).get(base); ok {
		if s.obj == nil && s.full {
			return Dates{}, nil, nil
		}
		return s.dates, s.obj, nil
	}
	d, obj, err := readDatesStore(ctx, store, base)
	if err == nil {
		datesReadFrom(ctx).put(base, datesSeen{dates: d, obj: obj})
	}
	return d, obj, err
}

func readDatesStore(ctx context.Context, store Store, base string) (Dates, *Object, error) {
	body, obj, err := store.Get(CacheContent(WithoutDeclaredTime(ctx)), base+VersionSuffix)
	if IsNotFound(err) {
		return Dates{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	d, err := decodeDates(data)
	return d, obj, err
}

// AddDates merges add into base's override table, under compare-and-swap.
// Nothing is written for an empty add.
func AddDates(ctx context.Context, store Store, base string, add Dates) error {
	_, err := MergeDates(ctx, store, base, add, false)
	return err
}

// MergeDates is AddDates returning base's table as it wrote it. fresh
// says base has no .version yet, being an item the operation made
// (Recorder.IsNew): the record is written without reading it first,
// unless it turns out to be there after all. An item that had versions
// before its first record has them listed in it, at the store's times.
func MergeDates(ctx context.Context, store Store, base string, add Dates, fresh bool) (Dates, error) {
	if len(add) == 0 {
		return ReadDates(ctx, store, base)
	}
	return writeDates(ctx, store, base, fresh, true, func(d Dates) Dates {
		for id, t := range add {
			d[id] = t
		}
		return d
	})
}

// MergeDatesOver is MergeDates that also lists each version of over the
// table doesn't list yet, at its time there: a version a write was made
// on top of that no table lists (a write killed after its put and before
// its dating), dated by its writer so that it never outranks what is
// written after it (RecordDates).
//
// listed is the versions of over the table didn't list: the ones a
// killed write left, or an undated write's blocks.
func MergeDatesOver(ctx context.Context, store Store, base string, add, over Dates, fresh bool) (_ Dates, listed []string, _ error) {
	if len(over) == 0 {
		d, err := MergeDates(ctx, store, base, add, fresh)
		return d, nil, err
	}
	// What the table lists now (read once: DatesRead); with none, a first
	// record would list every version at the store's time, which is what
	// over corrects.
	cur, obj, err := readDates(ctx, store, base)
	if err != nil {
		return nil, nil, err
	}
	absent := Dates{}
	for id, t := range over {
		if _, ok := cur[id]; !ok || obj == nil {
			absent[id] = t
		}
	}
	if len(add) == 0 && len(absent) == 0 {
		return cur, nil, nil
	}
	d, err := writeDates(ctx, store, base, fresh && obj == nil, true, func(d Dates) Dates {
		listed = listed[:0]
		for id, t := range absent {
			if prev, ok := d[id]; !ok || obj == nil && !prev.Equal(t) {
				d[id] = t
				listed = append(listed, id)
			}
		}
		for id, t := range add {
			d[id] = t
		}
		return d
	})
	return d, listed, err
}

// UpdateDates is MergeDates that also removes drop from the table:
// versions about to be erased.
func UpdateDates(ctx context.Context, store Store, base string, add Dates, drop ...string) (Dates, error) {
	return writeDates(ctx, store, base, false, true, func(d Dates) Dates {
		for _, id := range drop {
			delete(d, id)
		}
		for id, t := range add {
			d[id] = t
		}
		return d
	})
}

// datedKeys are the objects of item base every version of which its
// .version lists: its content and its .metadata.
func datedKeys(base string) []string { return []string{base, base + ".metadata"} }

// storeDates is every version base's dated keys have now, at the store's
// times to the second: the record of an item that had none.
func storeDates(ctx context.Context, store Store, base string) (Dates, error) {
	d := Dates{}
	for _, key := range datedKeys(base) {
		history, err := store.History(ctx, key)
		if err != nil {
			return nil, err
		}
		for _, o := range history {
			if !o.Deleted {
				d[o.VersionID] = StoreSecond(o.LastModified) // as an unlisted version is dated (Dates.Time)
			}
		}
	}
	return d, nil
}

// SetDates replaces base's override table with dates, under
// compare-and-swap; an empty dates removes the object.
func SetDates(ctx context.Context, store Store, base string, dates Dates) error {
	if len(dates) == 0 {
		err := store.Delete(WithoutDeclaredTime(ctx), base+VersionSuffix)
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	_, err := writeDates(ctx, store, base, false, false, func(Dates) Dates { return dates })
	return err
}

// writeDates writes mutate's base's table and returns it; with fresh, it
// takes there to be none for the first try, reading nothing. With
// complete, a table written for the first time over versions written
// before starts with them (storeDates).
func writeDates(ctx context.Context, store Store, base string, fresh, complete bool, mutate func(Dates) Dates) (Dates, error) {
	ctx = WithoutDeclaredTime(ctx) // the record is news now, not part of the history it records
	key := base + VersionSuffix
	seen := datesReadFrom(ctx)
	for attempt := 0; attempt < 8; attempt++ {
		cur, obj := Dates{}, (*Object)(nil)
		if attempt > 0 {
			seen.forget(base) // lost a race: read it as it is now
		}
		if !fresh || attempt > 0 {
			var err error
			if cur, obj, err = readDates(ctx, store, base); err != nil {
				return nil, err
			}
			if obj == nil && complete {
				if s, ok := seen.get(base); ok && s.full {
					cur = s.dates
				} else if cur, err = storeDates(ctx, store, base); err != nil {
					return nil, err
				}
			}
		}
		next := mutate(cur)
		data, err := encodeDates(next)
		if err != nil {
			return nil, err
		}
		var wrote *Object
		if obj == nil {
			wrote, err = store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil)
		} else {
			wrote, err = store.Put(ctx, key, bytes.NewReader(data), nil, obj.ETag)
		}
		if err == nil {
			if wrote != nil {
				seen.put(base, datesSeen{dates: next, obj: wrote})
			} else {
				seen.forget(base)
			}
			return next, nil
		}
		if !IsPreconditionFailed(err) {
			seen.forget(base)
			return nil, err
		}
	}
	return nil, fmt.Errorf("write %s: lost the race %d times", key, 8)
}

// --- Recording what a dated operation writes ---

// Written is one version a store wrote under a declared time. Dir, if
// set, says it's a block of that directory (AsBlock), whose place in the
// directory's history is the directory's to put right, not the key's.
type Written struct {
	Key, VersionID string
	At             time.Time
	Dir            string
	// Over, if set, is the version it was written on top of, as the write
	// read it (Over): the one its conditional write replaced.
	Over *Object
	// Links, for a directory block Over, is the keys of the blocks it
	// points to (its children and its right sibling): if Over was never
	// dated, neither, it may be, were they (OverLinks).
	Links []string
}

type overLinksKey struct{}

// OverLinks is ctx for a write on top of a directory block that points
// to links (Written.Links).
func OverLinks(ctx context.Context, links []string) context.Context {
	if len(links) == 0 {
		return ctx
	}
	return context.WithValue(ctx, overLinksKey{}, links)
}

type overKey struct{}

// Over says the write under ctx is made on top of version o of its key
// (the version it read and is conditional on): Written.Over.
func Over(ctx context.Context, o *Object) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, overKey{}, o)
}

type blockKey struct{}

type keepDateKey struct{}

type replacingKey struct{}

type datesItselfKey struct{}

// DatesItself marks the writes under ctx as dated by the caller, once
// written (UpdateDates): DateAll leaves them.
func DatesItself(ctx context.Context) context.Context {
	return context.WithValue(ctx, datesItselfKey{}, true)
}

// KeepDate marks the writes under ctx as bookkeeping (checksums, an
// access time): each version is the state of the one it replaces, and
// takes its date (when it's undated: DateAll).
func KeepDate(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepDateKey{}, true)
}

// Replacing says the write under ctx replaces version id, for KeepDate.
func Replacing(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, replacingKey{}, id)
}

// AsBlock marks the writes under ctx as blocks of directory dir
// (Written.Dir).
func AsBlock(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, blockKey{}, dir)
}

// A Recorder collects the versions written under a declared time through
// a Recording store, for the caller to date (AddDates) once the operation
// is done: no write path needs to hand its version ids out.
type Recorder struct {
	mu      sync.Mutex
	written []Written
	overs   []Written
	made    map[string]bool
	absent  map[string]bool
}

// NoteNew tells ctx's Recorder, if it has one, that base is an item the
// operation made: a key just minted, with no history before it.
func NoteNew(ctx context.Context, base string) {
	if rec, _ := ctx.Value(recorderKey{}).(*Recorder); rec != nil {
		rec.mu.Lock()
		if rec.made == nil {
			rec.made = map[string]bool{}
		}
		rec.made[base] = true
		rec.mu.Unlock()
	}
}

// ForgetNew tells ctx's Recorder, if it has one, that base, an item the
// operation made, is gone, every version erased (a create that failed
// before its name: T82-10 round 5): nothing it wrote under base is dated,
// so no .version record is written for a key that has nothing.
func ForgetNew(ctx context.Context, base string) {
	rec, _ := ctx.Value(recorderKey{}).(*Recorder)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	kept := rec.written[:0]
	for _, w := range rec.written {
		if BaseKey(w.Key) != base {
			kept = append(kept, w)
		}
	}
	rec.written = kept
	delete(rec.made, base)
	delete(rec.absent, base)
}

// Fresh reports whether base likely has no .version record: an item
// the operation made, or a key it wrote where there was none
// (PutIfAbsent). A hint for MergeDates, which reads the record after all
// if it's there.
func (r *Recorder) Fresh(base string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made[base] || r.absent[base]
}

// IsNew reports whether base is an item the operation made (NoteNew):
// it has no .version record yet, and every version it has is one the
// operation wrote, dated alike.
func (r *Recorder) IsNew(base string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made[base]
}

// Written is what was recorded so far.
func (r *Recorder) Written() []Written {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Written(nil), r.written...)
}

// NoteOver tells ctx's Recorder, if ctx has one and a declared time,
// that the operation read version o of key and built on it without
// writing key itself (a directory's root block above the block it
// changed): its dating lists o if no table does (Recorder.Overs).
func NoteOver(ctx context.Context, key string, o *Object, links []string) {
	rec, _ := ctx.Value(recorderKey{}).(*Recorder)
	t, ok := DeclaredTime(ctx)
	if rec == nil || !ok || o == nil {
		return
	}
	rec.mu.Lock()
	rec.overs = append(rec.overs, Written{Key: key, At: t, Over: o, Links: links})
	rec.mu.Unlock()
}

// Overs is what NoteOver noted, and every written version's Over.
func (r *Recorder) Overs() []Written {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]Written(nil), r.overs...)
	for _, w := range r.written {
		if w.Over != nil {
			out = append(out, w)
		}
	}
	return out
}

// Dates groups what was recorded by base key: what AddDates takes.
func (r *Recorder) Dates() map[string]Dates {
	out := map[string]Dates{}
	for _, w := range r.Written() {
		base := BaseKey(w.Key)
		if out[base] == nil {
			out[base] = Dates{}
		}
		out[base][w.VersionID] = w.At
	}
	return out
}

// Note records w as written: for a caller that dates, after the fact,
// what a write left undated (icbfs's DateLeftovers).
func (r *Recorder) Note(w Written) { r.add(w) }

func (r *Recorder) add(w Written) {
	r.mu.Lock()
	r.written = append(r.written, w)
	r.mu.Unlock()
}

type recorderKey struct{}

// WithRecorder makes rec record what's written under ctx (through a
// Recording store, while ctx has a declared time).
func WithRecorder(ctx context.Context, rec *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, rec)
}

// RecorderFrom is ctx's Recorder (WithRecorder), or nil.
func RecorderFrom(ctx context.Context) *Recorder {
	rec, _ := ctx.Value(recorderKey{}).(*Recorder)
	return rec
}

// Recording wraps store so that every version it writes under a context
// with a declared time and a Recorder is recorded. A .version object's
// own writes aren't: they're the record, not the history.
func Recording(store Store) Store {
	if _, ok := store.(*recording); ok {
		return store
	}
	return &recording{Store: store}
}

// DateAll has a Recording store date every version of an object dated
// says is one (a filesystem written in a source's time: its items'
// content and .metadata) that it writes with no declared time: at the
// time the store wrote it, or for a KeepDate write the date of the
// version it replaced. So an item's .version has all its versions. A
// version it can't date is erased, and the write fails.
func DateAll(store Store, dated func(key string) bool) {
	if r, ok := store.(*recording); ok {
		r.dated = dated
	}
}

// Unwrap is the store under a Recording one, else store itself.
func Unwrap(store Store) Store {
	if r, ok := store.(*recording); ok {
		return r.Store
	}
	return store
}

type recording struct {
	Store
	dated func(key string) bool // DateAll's
}

// ListPages lists a page at a time if the store under it can: a listing
// writes nothing, so there is nothing to record.
func (r *recording) ListPages(ctx context.Context, prefix string, fn func([]Object) error) error {
	return ListEach(ctx, r.Store, prefix, fn)
}

// VersionsSince reads back only to since if the store under it can: a
// listing writes nothing.
func (r *recording) VersionsSince(ctx context.Context, key string, since time.Time) ([]Object, error) {
	return VersionsSince(ctx, r.Store, key, since)
}

// note records a version written under a declared time, or dates one
// written without that DateAll says must be.
func (r *recording) note(ctx context.Context, key string, obj *Object, err error) error {
	if err != nil || obj == nil || strings.HasSuffix(key, VersionSuffix) {
		return nil
	}
	rec, _ := ctx.Value(recorderKey{}).(*Recorder)
	dir, _ := ctx.Value(blockKey{}).(string)
	if t, ok := DeclaredTime(ctx); ok && rec != nil {
		over, _ := ctx.Value(overKey{}).(*Object)
		if over != nil && over.Key != "" && over.Key != key {
			over = nil
		}
		links, _ := ctx.Value(overLinksKey{}).([]string)
		rec.add(Written{Key: key, VersionID: obj.VersionID, At: t, Dir: dir, Over: over, Links: links})
		return nil
	}
	if self, _ := ctx.Value(datesItselfKey{}).(bool); self || dir != "" || r.dated == nil || !r.dated(key) {
		return nil
	}
	keep, _ := ctx.Value(keepDateKey{}).(bool)
	replaced, _ := ctx.Value(replacingKey{}).(string)
	_, err = writeDates(WithoutDeclaredTime(ctx), r.Store, BaseKey(key), false, true, func(d Dates) Dates {
		t := obj.LastModified
		if prev, ok := d[replaced]; keep && ok {
			t = prev
		}
		d[obj.VersionID] = t
		return d
	})
	if err != nil {
		_ = r.Store.Erase(context.WithoutCancel(ctx), key, obj.VersionID)
		return fmt.Errorf("date %s: %w", key, err)
	}
	return nil
}

// PutAtKey is the store metadata every version written under a declared
// time carries (withPutAt): that time. A version a
// write left and never dated (the write was killed between its put and
// its dating) says its date itself, to the write made on top of it
// (RecordDates), which lists it there.
const PutAtKey = "icbfsat"

// PutAt is the declared time o's write carried (PutAtKey), if any.
func PutAt(o *Object) (time.Time, bool) {
	if o == nil || o.Metadata == nil {
		return time.Time{}, false
	}
	v, ok := o.Metadata[PutAtKey]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil
}

// withPutAt is metadata for a write of key under ctx: every version
// written under a declared time (a directory's block, a .metadata, an
// item's content, an origin record) says that time, or, written with
// none, says none (a copy of what the version before said is not its).
// Content says it since interloc T84-18 (a write killed before its
// dating is dated from it at the next start: icbfs DateLeftovers);
// blocks and .metadata since T83-1.
func withPutAt(ctx context.Context, key string, metadata map[string]string) map[string]string {
	t, dated := DeclaredTime(ctx)
	if _, had := metadata[PutAtKey]; !dated && !had {
		return metadata
	}
	out := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	delete(out, PutAtKey)
	if dated {
		out[PutAtKey] = t.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func (r *recording) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error) {
	metadata = withPutAt(ctx, key, metadata)
	obj, err := r.Store.Put(ctx, key, body, metadata, ifMatch)
	if err := r.note(ctx, key, obj, err); err != nil {
		return nil, err
	}
	return obj, err
}

func (r *recording) PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error) {
	metadata = withPutAt(ctx, key, metadata)
	obj, err := r.Store.PutIfAbsent(ctx, key, body, metadata)
	if err := r.note(ctx, key, obj, err); err != nil {
		return nil, err
	}
	if rec, _ := ctx.Value(recorderKey{}).(*Recorder); rec != nil && err == nil && key == BaseKey(key) {
		rec.mu.Lock()
		if rec.absent == nil {
			rec.absent = map[string]bool{}
		}
		rec.absent[key] = true
		rec.mu.Unlock()
	}
	return obj, err
}

func (r *recording) PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatch string) (*Object, error) {
	metadata = withPutAt(ctx, key, metadata)
	obj, err := r.Store.PutParts(ctx, key, parts, metadata, ifMatch)
	if err := r.note(ctx, key, obj, err); err != nil {
		return nil, err
	}
	return obj, err
}

// AbortCopy passes the store's through (CopyAborter).
func (r *recording) AbortCopy(ctx context.Context, key, id string) error {
	return AbortCopy(ctx, r.Store, key, id)
}
