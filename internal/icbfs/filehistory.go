package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// A file's history is its content's versions and its .metadata's. Like a
// directory's (ReplaceDirHistory), it's normally only added to;
// ReplaceFileHistory rewrites it, for a caller correcting it (a date the
// source got wrong, a version that shouldn't be there).

// FileState is one state of a file: from At, content Version (an
// existing version of the file, copied), Content (new bytes) or a stub
// naming RefKey's version RefVersion (see PutRef; RefSHA256, if given,
// must be the target's recorded SHA-256), one of the three, with
// attributes Attr (nil: the file's attributes now), whose Mtime, if
// zero, is At.
type FileState struct {
	At      time.Time
	Version string
	Content io.Reader
	Attr    *Attr

	RefKey     string
	RefVersion string
	RefSHA256  []byte
}

// FileVersion is one content version of a file with its effective date.
type FileVersion struct {
	VersionID string
	ETag      string
	Size      int64 // the content's: a stub's target's, not the stub's own
	At        time.Time
	Stored    time.Time
	// Ref is a stub's target, when the version is one (as .metadata's
	// Refs records it); nil otherwise.
	Ref *Ref
}

// ErrFileChanged is ReplaceFileHistory's refusal when the file isn't the
// version the caller read: it has changed since.
var (
	ErrFileChanged = errors.New("the file has changed since its history was read")
	// ErrFileOpen is its refusal while the file is open through a mount or
	// a session here: its content is in flux.
	ErrFileOpen = errors.New("the file is open")
	// ErrReferenced is its refusal of a file with live referrers (stubs
	// elsewhere name its versions): a rewrite erases the versions they
	// name, and can't retire them as a purge does. The referrers are
	// re-pointed first.
	ErrReferenced = errors.New("stubs name the file's versions")
)

// FileHistory is file key's content versions, newest first by effective
// date, and the id of its current version (what ReplaceFileHistory's
// ifMatch takes).
func (f *Filesystem) FileHistory(ctx context.Context, key string) ([]FileVersion, string, error) {
	history, err := f.store.History(ctx, key)
	if err != nil {
		return nil, "", mapNotFound(err)
	}
	dates, err := objstore.ReadDates(ctx, f.store, key)
	if err != nil {
		return nil, "", err
	}
	// A history listing carries no metadata, so a stub's target and real
	// size come from .metadata's Refs, by its ETag, not a Head per version.
	refs, err := f.contentRefs(ctx, key)
	if err != nil {
		return nil, "", err
	}
	current := ""
	var out []FileVersion
	for i, o := range history {
		if o.Deleted {
			continue
		}
		if i == 0 {
			current = o.VersionID
		}
		v := FileVersion{VersionID: o.VersionID, ETag: o.ETag, Size: o.Size, At: dates.Time(&o), Stored: o.LastModified}
		if r, ok := refs[o.ETag]; ok {
			v.Size, v.Ref = r.Size, &r
		}
		out = append(out, v)
	}
	for i := 1; i < len(out); i++ { // insertion sort: newest first by date, the store's order on ties
		for j := i; j > 0 && out[j].At.After(out[j-1].At); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, current, nil
}

// ContentRefs is file key's stubs' targets, by each stub's ETag, as its
// .metadata's Refs records them: for a reader of its history listing,
// which carries no metadata. A deleted (trashed) file's are its last
// .metadata's; a key with no .metadata at all has none.
func (f *Filesystem) ContentRefs(ctx context.Context, key string) (map[string]Ref, error) {
	return f.contentRefs(ctx, key)
}

func (f *Filesystem) contentRefs(ctx context.Context, key string) (map[string]Ref, error) {
	mk := metadataObjectKey(key)
	body, _, err := f.store.Get(objstore.CacheContent(ctx), mk)
	if objstore.IsNotFound(err) {
		history, herr := f.store.History(ctx, mk)
		if objstore.IsNotFound(herr) {
			return nil, nil
		}
		if herr != nil {
			return nil, herr
		}
		i := 0
		for i < len(history) && history[i].Deleted {
			i++
		}
		if i == len(history) {
			return nil, nil
		}
		body, _, err = f.store.GetVersion(objstore.CacheContent(ctx), mk, history[i].VersionID)
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, err
	}
	attr, err := decodeFileMetadata(data)
	if err != nil {
		return nil, err
	}
	return attr.Refs, nil
}

// ReplaceFileHistory erases file key's history (its content's versions
// and its .metadata's) and writes states in its place, oldest first, each
// dated at its time (At, strictly increasing and not in the future); the
// last is the file now. A state's Version is copied (on the server, for
// content past one part), so nothing is downloaded to keep a version.
// ifMatch, if set, is the current version id FileHistory gave:
// ErrFileChanged if the file has changed since. Streams and the file's
// names are left as they are. The caller stops its own writes to the
// file meanwhile.
//
// Stubs: a file with live referrers is refused (ErrReferenced); its own
// stubs naming its own versions are not referrers. A stub state is
// checked and written as PutRef writes one, its target's .refs gaining
// its entry. A kept version that is a stub stays one, its copy's entry
// replacing the original's (copyVersionOnTop); a stub not kept has its
// entry dropped.
//
// A stub state may name a version of the file itself (RefKey key) only
// if an earlier state keeps that version (Version) and it isn't a stub;
// otherwise ErrInvalidArgument. Kept versions are copied to new ids, so
// the stub names the copy's new version id, and the file's own .refs
// lists it as any target's does. A kept version that is such a stub is
// written again the same way, naming its target's copy (which, again,
// an earlier state must keep).
func (f *Filesystem) ReplaceFileHistory(ctx context.Context, key string, states []FileState, ifMatch string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if isStreamKey(key) {
		return fmt.Errorf("a stream's history: %w", ErrInvalidArgument)
	}
	ctx = undeclared(ctx)
	if len(states) == 0 {
		return fmt.Errorf("a file's history needs its state now: %w", ErrInvalidArgument)
	}
	now := time.Now()
	for i, s := range states {
		if s.At.After(now) || i > 0 && !s.At.After(states[i-1].At) {
			return fmt.Errorf("state %d at %s: states go oldest first, none in the future: %w", i, s.At.Format(time.RFC3339Nano), ErrInvalidArgument)
		}
		n := 0
		for _, set := range []bool{s.Version != "", s.Content != nil, s.RefKey != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("state %d: a version to keep, new content or a stub, one of the three: %w", i, ErrInvalidArgument)
		}
	}
	if c := f.openContent(key); c != nil {
		return fmt.Errorf("the file is open: %w", ErrFileOpen)
	}
	if live, err := f.liveReferrers(ctx, key, nil); err != nil {
		return err
	} else if n := len(live) - selfEntries(live, key); n > 0 {
		return fmt.Errorf("%s: %d referrers: %w", key, n, ErrReferenced)
	}
	targets := make([]*stubTarget, len(states))
	for i, s := range states {
		if s.RefKey == "" || s.RefKey == key {
			continue
		}
		t, err := f.checkRefTarget(ctx, s.RefKey, s.RefVersion, s.RefSHA256)
		if err != nil {
			return fmt.Errorf("state %d: %w", i, err)
		}
		targets[i] = t
	}
	oldContent, err := f.store.History(ctx, key)
	if err != nil {
		return mapNotFound(err)
	}
	if ifMatch != "" && (len(oldContent) == 0 || oldContent[0].VersionID != ifMatch) {
		return ErrFileChanged
	}
	mk := metadataObjectKey(key)
	oldMeta, err := f.store.History(ctx, mk)
	if err != nil {
		return mapNotFound(err)
	}
	attrNow, err := f.statFile(ctx, key)
	if err != nil {
		return err
	}
	oldRefs, err := f.contentRefs(ctx, key)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t != nil {
			if err := f.raiseFormat(ctx); err != nil {
				return err
			}
			break
		}
	}
	byID := map[string]objstore.Object{}
	for _, o := range oldContent {
		if !o.Deleted {
			byID[o.VersionID] = o
		}
	}
	self, err := selfTargets(key, states, byID, oldRefs, attrNow.Checksums)
	if err != nil {
		return err
	}
	if len(self) > 0 {
		if err := f.raiseFormat(ctx); err != nil {
			return err
		}
	}
	copies := map[string]*objstore.Object{} // the kept versions' copies, by the version copied

	rec := &objstore.Recorder{}
	// What each state's .metadata learns about its own content version
	// (a kept version's checksums under its copy's ETag, a stub's Ref),
	// every later state's carries too: the last is the .metadata history
	// readers go by.
	var carry []func(*Attr)
	for i, s := range states {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.At), rec)
		var obj *objstore.Object
		var ref *Ref
		if t, ok := self[i]; ok {
			// A stub naming a version of this file kept earlier: its
			// copy, written just now, is the target.
			cp := copies[t.version]
			t = &stubTarget{key: key, version: cp.VersionID, obj: &objstore.Object{Key: key, VersionID: cp.VersionID, ETag: cp.ETag, Size: byID[t.version].Size},
				sums: t.sums, recorded: t.recorded, sha256: t.sha256}
			targets[i] = t
		}
		switch {
		case targets[i] != nil:
			body, meta, err := targets[i].stub()
			if err != nil {
				return err
			}
			if err := f.addReferrer(sctx, targets[i], key); err != nil {
				return err
			}
			if obj, err = f.store.Put(sctx, key, bytes.NewReader(body), meta, ""); err != nil {
				return err // the pending entry stays, for fsck
			}
		case s.Version != "":
			src, ok := byID[s.Version]
			if !ok {
				return fmt.Errorf("state %d: version %s: %w", i, s.Version, ErrNotFound)
			}
			if obj, ref, err = f.copyVersionOnTop(sctx, key, src); err != nil {
				return err
			}
			copies[s.Version] = obj
		default:
			if obj, err = f.store.Put(sctx, key, s.Content, nil, ""); err != nil {
				return err
			}
		}
		attr := attrNow
		if s.Attr != nil {
			attr = *s.Attr
		}
		// Each state's maps are its own (the states may share attrNow's),
		// with what the states so far carry.
		attr.Checksums = maps.Clone(attr.Checksums)
		attr.Refs = maps.Clone(attr.Refs)
		if attr.Mtime.IsZero() {
			attr.Mtime = s.At
		}
		etag := obj.ETag
		if s.Version != "" {
			old := byID[s.Version].ETag
			carry = append(carry, func(a *Attr) { rekeyETag(a, old, etag) }) // the copy's checksums are the original's
		}
		if ref != nil {
			r := *ref
			carry = append(carry, func(a *Attr) { // the stub's own metadata says, whatever Attr did
				if a.Refs == nil {
					a.Refs = map[string]Ref{}
				}
				a.Refs[etag] = r
			})
		}
		if t := targets[i]; t != nil {
			carry = append(carry, func(a *Attr) { t.record(a, etag) })
		}
		for _, c := range carry {
			c(&attr)
		}
		attr.mtimeOverrideETag = obj.ETag
		if attr.Ctime.IsZero() || attr.Ctime.Before(s.At) {
			attr.Ctime = s.At
		}
		data, err := f.encodeMetadata(sctx, attr)
		if err != nil {
			return err
		}
		if _, err := f.store.Put(sctx, mk, bytes.NewReader(data), nil, ""); err != nil {
			return err
		}
		if targets[i] != nil {
			f.finalizeReferrer(ctx, targets[i], key, obj.VersionID)
		}
	}
	if err := objstore.SetDates(ctx, f.store, key, rec.Dates()[key]); err != nil {
		return err
	}
	for _, v := range oldContent {
		if err := f.store.Erase(ctx, key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	// The old stubs' entries go with them (a kept one's went with its
	// copy).
	erased := map[string]map[string]bool{} // by target, the stub versions
	for _, v := range oldContent {
		if r, ok := oldRefs[v.ETag]; ok && !v.Deleted {
			if erased[r.Key] == nil {
				erased[r.Key] = map[string]bool{}
			}
			erased[r.Key][v.VersionID] = true
		}
	}
	for target, versions := range erased {
		if err := f.dropRef(ctx, target, func(e *pb.Referrer) bool {
			return e.ReferrerKey == key && !e.Pending && versions[e.ReferrerVersion]
		}); err != nil {
			return err
		}
	}
	for _, v := range oldMeta {
		if err := f.store.Erase(ctx, mk, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	return f.markPast(ctx)
}

// selfEntries counts the entries of live that are key's own: its stubs
// naming its own versions, which a rewrite of key re-aims, not a
// referrer that holds it back.
func selfEntries(live []*pb.Referrer, key string) int {
	n := 0
	for _, e := range live {
		if e.ReferrerKey == key {
			n++
		}
	}
	return n
}

// selfTargets checks ReplaceFileHistory's states that are stubs naming
// file key itself: a stub state with RefKey key, or a kept Version that is
// a stub naming key (its target moves with the rewrite, so it is written
// again as a stub naming the new copy). Each must name a version that an
// earlier state keeps (Version) and that isn't a stub. It returns them by
// state index, each target's version the old one; its copy's id is known
// only once that state is written.
func selfTargets(key string, states []FileState, byID map[string]objstore.Object, oldRefs map[string]Ref, sums map[string]Checksums) (map[int]*stubTarget, error) {
	out := map[int]*stubTarget{}
	kept := map[string]bool{} // versions the states so far keep
	for i, s := range states {
		target, sha := "", s.RefSHA256
		switch {
		case s.RefKey == key:
			target = s.RefVersion
		case s.Version != "":
			if src, ok := byID[s.Version]; ok {
				if r, ok := oldRefs[src.ETag]; ok && r.Key == key {
					target = r.VersionID
				}
			}
		}
		if target == "" {
			if s.Version != "" {
				kept[s.Version] = true
			}
			continue
		}
		src, ok := byID[target]
		if !ok || !kept[target] {
			return nil, fmt.Errorf("state %d: a stub naming version %s of the file itself, which no earlier state keeps: %w", i, target, ErrInvalidArgument)
		}
		if _, stub := oldRefs[src.ETag]; stub {
			return nil, fmt.Errorf("state %d: a stub naming version %s of the file itself, a stub: %w", i, target, ErrInvalidArgument)
		}
		t := &stubTarget{key: key, version: target, sha256: sha}
		t.sums, t.recorded = sums[src.ETag]
		if t.recorded && len(t.sums.SHA256) > 0 {
			if len(sha) > 0 && !bytes.Equal(sha, t.sums.SHA256) {
				return nil, fmt.Errorf("state %d: %s version %s: SHA-256 %x, given %x: %w", i, key, target, t.sums.SHA256, sha, ErrRefChecksum)
			}
			t.sha256 = t.sums.SHA256
		}
		out[i] = t
	}
	return out, nil
}
