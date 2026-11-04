// Package dedupe scans a filesystem for content versions with identical
// bytes (icbfs-cli fs dedupe): the scan, the dump of what it saw, and the
// dry run's numbers; and the writing run, Replace, which replaces the
// duplicate current versions by stubs, or with Options.History every
// duplicate version, rewriting histories (history.go). The scan is the same pass for
// every mode, and the writes work on its Result (groups, canonical
// choice, per-version records with their referrer counts).
package dedupe

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// DefaultMinSize is the smallest version dedupe considers, in bytes.
const DefaultMinSize = 2 << 20

// Options say what to scan.
type Options struct {
	Path    string // a subtree (or one file) of the filesystem; "" for all of it
	MinSize int64  // versions smaller than this are not looked at
	Current bool   // scan current versions only, not histories
	History bool   // Replace: rewrite histories (history.go), not only current versions
}

// Ref is a stub's target.
type Ref struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

// Record is one content version of a file in scope: a line of the dump.
type Record struct {
	Path      string    `json:"path"`
	ItemID    string    `json:"itemId"`
	VersionID string    `json:"versionId"`
	ETag      string    `json:"etag"`
	Date      time.Time `json:"date"`
	Size      int64     `json:"size"`

	MD5            string `json:"md5"`
	MD5Computed    bool   `json:"md5_computed"`
	SHA256         string `json:"sha256"`
	SHA256Computed bool   `json:"sha256_computed"`

	Ref *Ref `json:"ref,omitempty"`

	Current   bool `json:"-"` // the file's current version
	Referrers int  `json:"-"` // stubs elsewhere that name this version
}

// Group is the versions of one content (SHA-256 and size) that could be
// one. Records are indexes into Result.Records.
type Group struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Canonical  int    `json:"canonical"`  // the copy that stays
	Duplicates []int  `json:"duplicates"` // the other real copies
}

// Tally counts versions and their bytes.
type Tally struct {
	Versions int   `json:"versions"`
	Bytes    int64 `json:"bytes"`
}

// ModeReport is the numbers for one mode: current versions only, or
// every version.
type ModeReport struct {
	Versions    int   `json:"versions"` // real (not stub) versions of the minimum size or over
	Stubs       int   `json:"stubs"`    // versions that already are stubs
	Groups      int   `json:"groups"`   // contents with more than one real copy
	Duplicates  int   `json:"duplicates"`
	Reclaimable int64 `json:"reclaimable"` // bytes of the duplicates
	BelowMin    Tally `json:"below_min"`
}

// Report is the dry run's numbers, and a writing run's writes.
type Report struct {
	Files    int         `json:"files"`    // files in scope
	Computed int         `json:"computed"` // versions whose checksums this run computed and recorded
	Skipped  int         `json:"skipped"`  // files and versions it could not read (fs check says why)
	Current  ModeReport  `json:"current"`
	History  *ModeReport `json:"history,omitempty"` // nil for a current-only scan
	Writes   *Writes     `json:"writes,omitempty"`  // nil for a dry run
}

// Writes is what a writing run (Replace) did.
type Writes struct {
	Replaced  Tally  `json:"replaced"`  // versions replaced by stubs, and the bytes they held
	Finished  int    `json:"finished"`  // replacements a killed run left, finished
	Repointed int    `json:"repointed"` // stubs re-pointed to a canonical copy (-history)
	Skipped   []Skip `json:"skipped"`   // duplicates left as they are, by path
}

// Skip is a current version a writing run left as it is, and why.
type Skip struct {
	Path      string `json:"path"`
	ItemID    string `json:"itemId"`
	VersionID string `json:"versionId"`
	Reason    string `json:"reason"` // one of the Skip reasons
	Detail    string `json:"detail,omitempty"`
}

// The reasons a writing run skips a version.
const (
	SkipReferrers = "referrers" // stubs elsewhere name it
	SkipChanged   = "changed"   // a writer got there first (a lost If-Match), or it went
	SkipStub      = "stub"      // it is a stub already
	SkipTarget    = "target"    // its canonical copy can't be a stub's target (gone, or not the same content)
	SkipOpen      = "open"      // the file is open here (-history)
)

// Pending is a .refs entry still pending, of a referrer in scope: what a
// killed writing run leaves (icbfs.Filesystem.FinishReplacements).
type Pending struct {
	Target, Referrer string
}

// Result is what a scan found.
type Result struct {
	Report  Report    `json:"report"`
	Records []Record  `json:"records"` // by path, each file's versions newest first
	Current []Group   `json:"current_groups"`
	History []Group   `json:"history_groups,omitempty"`
	Pending []Pending `json:"-"`
}

// Scan scans f, whose objects are in store: the files under o.Path, the
// live ones only (the trash is not walked). It lists the filesystem's
// objects once (every version of every key), walks the directory tree
// for the files and their paths, and reads each file's .metadata once.
// A version with no recorded checksums gets them computed and recorded
// (icbfs.Filesystem.VersionChecksums, which leaves the item's dates
// alone); nothing else is written.
func Scan(ctx context.Context, store objstore.Store, f *icbfs.Filesystem, o Options) (*Result, error) {
	files, err := walk(ctx, f, o.Path)
	if err != nil {
		return nil, err
	}
	inScope := make(map[string]bool, len(files))
	for _, fl := range files {
		inScope[fl.key] = true
	}
	all, err := store.HistoryUnder(ctx, f.Prefix())
	if err != nil {
		return nil, err
	}
	versions := map[string][]objstore.Object{} // by item key, newest first
	hasDates, hasRefs := map[string]bool{}, map[string]bool{}
	seen, deleted := map[string]bool{}, map[string]bool{} // keys out of scope, and those whose newest entry is a deletion
	for _, v := range all {
		switch {
		case inScope[v.Key]:
			versions[v.Key] = append(versions[v.Key], v)
		case strings.HasSuffix(v.Key, objstore.VersionSuffix):
			hasDates[strings.TrimSuffix(v.Key, objstore.VersionSuffix)] = true
		case strings.HasSuffix(v.Key, ".refs"):
			hasRefs[strings.TrimSuffix(v.Key, ".refs")] = true
		case !seen[v.Key]:
			seen[v.Key], deleted[v.Key] = true, v.Deleted
		}
	}

	res := &Result{}
	if !o.Current {
		res.Report.History = &ModeReport{}
	}
	rep := &res.Report
	for _, fl := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hist := versions[fl.key]
		if len(hist) == 0 || hist[0].Deleted {
			continue
		}
		attr, err := f.FileMetadata(ctx, fl.key)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			rep.Skipped++
			continue
		}
		var dates objstore.Dates
		if hasDates[fl.key] {
			if dates, err = objstore.ReadDates(ctx, store, fl.key); err != nil {
				return nil, err
			}
		}
		var counts map[string]int
		if hasRefs[fl.key] {
			var pending []string
			if counts, pending, err = f.ReferrerCounts(ctx, fl.key); err != nil {
				return nil, err
			}
			res.pending(fl.key, pending, inScope)
		}
		rep.Files++
		current := hist[0].VersionID
		live := make([]objstore.Object, 0, len(hist))
		for _, v := range hist {
			if !v.Deleted && (!o.Current || v.VersionID == current) {
				live = append(live, v)
			}
		}
		// Newest first by effective date, the store's order on ties.
		eff := func(v *objstore.Object) time.Time { return dates.Time(v) }
		for i := 1; i < len(live); i++ {
			for j := i; j > 0 && eff(&live[j]).After(eff(&live[j-1])); j-- {
				live[j], live[j-1] = live[j-1], live[j]
			}
		}
		for i := range live {
			v := &live[i]
			isCurrent := v.VersionID == current
			rec := Record{Path: fl.path, ItemID: fl.key, VersionID: v.VersionID, ETag: v.ETag, Date: eff(v), Size: v.Size, Current: isCurrent}
			ref, stub := attr.Refs[v.ETag]
			if stub {
				rec.Size, rec.Ref = ref.Size, &Ref{Key: ref.Key, Version: ref.VersionID}
			}
			if rec.Size < o.MinSize {
				rep.Current.below(isCurrent, rec.Size)
				if rep.History != nil {
					rep.History.below(true, rec.Size)
				}
				continue
			}
			c := attr.Checksums[v.ETag]
			recordedMD5, recordedSHA := len(c.MD5) > 0, len(c.SHA256) > 0
			if !c.Complete() {
				if c, err = f.VersionChecksums(ctx, fl.key, v.VersionID); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					rep.Skipped++
					continue
				}
				rep.Computed++
			}
			rec.MD5, rec.MD5Computed = hex.EncodeToString(c.MD5), !recordedMD5
			rec.SHA256, rec.SHA256Computed = hex.EncodeToString(c.SHA256), !recordedSHA
			rec.Referrers = counts[v.VersionID]
			if stub {
				rep.Current.stub(isCurrent)
				if rep.History != nil {
					rep.History.stub(true)
				}
			}
			res.Records = append(res.Records, rec)
		}
	}

	// A retired holder (a purged target that stubs still name) is out of
	// the tree, behind a deletion; an entry a killed run left may be in
	// its .refs.
	for base := range hasRefs {
		if !inScope[base] && deleted[base] {
			_, pending, err := f.ReferrerCounts(ctx, base)
			if err != nil {
				return nil, err
			}
			res.pending(base, pending, inScope)
		}
	}
	sort.Slice(res.Pending, func(i, j int) bool {
		a, b := res.Pending[i], res.Pending[j]
		return a.Target < b.Target || a.Target == b.Target && a.Referrer < b.Referrer
	})

	res.Current = groups(res.Records, func(r *Record) bool { return r.Current }, &rep.Current)
	if rep.History != nil {
		res.History = groups(res.Records, func(*Record) bool { return true }, rep.History)
	}
	return res, nil
}

// pending notes target's pending entries of referrers in scope.
func (r *Result) pending(target string, referrers []string, inScope map[string]bool) {
	for _, ref := range referrers {
		if inScope[ref] {
			r.Pending = append(r.Pending, Pending{Target: target, Referrer: ref})
		}
	}
}

func (m *ModeReport) below(counted bool, size int64) {
	if counted {
		m.BelowMin.Versions++
		m.BelowMin.Bytes += size
	}
}

func (m *ModeReport) stub(counted bool) {
	if counted {
		m.Stubs++
	}
}

// groups groups the real (not stub) records pick by content, and makes
// the group of each content with more than one copy, filling m's counts.
// The canonical copy is, of those with referrers, the one with the most
// (the oldest of equals), else the oldest.
func groups(records []Record, pick func(*Record) bool, m *ModeReport) []Group {
	type content struct {
		sha  string
		size int64
	}
	members := map[content][]int{}
	for i := range records {
		r := &records[i]
		if r.Ref != nil || !pick(r) {
			continue
		}
		m.Versions++
		c := content{r.SHA256, r.Size}
		members[c] = append(members[c], i)
	}
	// older reports whether record a is older than b: earlier by date;
	// of one file, listed later (they are newest first); else by path.
	older := func(a, b int) bool {
		ra, rb := &records[a], &records[b]
		switch {
		case !ra.Date.Equal(rb.Date):
			return ra.Date.Before(rb.Date)
		case ra.ItemID == rb.ItemID:
			return a > b
		case ra.Path != rb.Path:
			return ra.Path < rb.Path
		}
		return ra.VersionID < rb.VersionID
	}
	var out []Group
	for c, idx := range members {
		if len(idx) < 2 {
			continue
		}
		canon := idx[0]
		for _, i := range idx[1:] {
			ri, rc := records[i].Referrers, records[canon].Referrers
			if ri > rc || ri == rc && older(i, canon) {
				canon = i
			}
		}
		g := Group{SHA256: c.sha, Size: c.size, Canonical: canon}
		for _, i := range idx {
			if i != canon {
				g.Duplicates = append(g.Duplicates, i)
			}
		}
		sort.Ints(g.Duplicates)
		m.Groups++
		m.Duplicates += len(g.Duplicates)
		m.Reclaimable += c.size * int64(len(g.Duplicates))
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Canonical < out[j].Canonical })
	return out
}

// testHookBeforeReplace, if set, runs just before each replacement.
var testHookBeforeReplace func(Record)

// Replace is the writing run: Scan, then each current version that isn't
// its content's canonical copy is replaced by a stub naming that copy
// (icbfs.Filesystem.ReplaceWithRef: the same bytes, the history the same
// at the same dates). Before that, what a killed run left (the pending
// entries the scan found) is finished, or taken back, and the scan run
// again. A version stubs name, one a writer changed meanwhile, one that is
// a stub already and one whose canonical copy can't be a target are
// skipped, and reported in Report.Writes. The filesystem's past is marked
// changed (MarkPastChanged) once, at the end, if anything was replaced.
// A run with nothing left to do writes nothing. With o.History, the
// history run (see histRun) takes the place of the replacements of
// current versions.
func Replace(ctx context.Context, store objstore.Store, f *icbfs.Filesystem, o Options) (res *Result, err error) {
	if o.History && o.Current {
		return nil, errors.New("dedupe: rewriting histories needs the scan of every version, not current ones only")
	}
	if res, err = Scan(ctx, store, f, o); err != nil {
		return nil, err
	}
	w := &Writes{}
	// Rewrites mark the past changed each (ReplaceFileHistory): held back
	// to the one mark at the end.
	pastChanged := false
	if o.History {
		prev := f.PastChangedFunc()
		f.SetPastChanged(func(context.Context) { pastChanged = true })
		defer f.SetPastChanged(prev)
	}
	defer func() {
		if w.Replaced.Versions+w.Finished+w.Repointed > 0 || pastChanged {
			if merr := f.MarkPastChanged(ctx); merr != nil && err == nil {
				res, err = nil, merr
			}
		}
	}()
	if len(res.Pending) > 0 {
		for _, p := range res.Pending {
			n, err := f.FinishReplacements(ctx, p.Target, p.Referrer)
			w.Finished += n
			if err != nil {
				return nil, fmt.Errorf("finishing %s's replacement: %w", p.Referrer, err)
			}
		}
		computed := res.Report.Computed
		if res, err = Scan(ctx, store, f, o); err != nil {
			return nil, err
		}
		res.Report.Computed += computed
	}
	if o.History {
		if err := replaceHistory(ctx, f, res, w); err != nil {
			return nil, err
		}
		sort.SliceStable(w.Skipped, func(i, j int) bool { return w.Skipped[i].Path < w.Skipped[j].Path })
		res.Report.Writes = w
		return res, nil
	}
	skip := func(r *Record, reason, detail string) {
		w.Skipped = append(w.Skipped, Skip{Path: r.Path, ItemID: r.ItemID, VersionID: r.VersionID, Reason: reason, Detail: detail})
	}
	for _, g := range res.Current {
		c := &res.Records[g.Canonical]
		sha, err := hex.DecodeString(c.SHA256)
		if err != nil {
			return nil, err
		}
		for _, d := range g.Duplicates {
			r := &res.Records[d]
			if r.Referrers > 0 {
				skip(r, SkipReferrers, fmt.Sprintf("%d stubs name it", r.Referrers))
				continue
			}
			if testHookBeforeReplace != nil {
				testHookBeforeReplace(*r)
			}
			_, err := f.ReplaceWithRef(ctx, r.ItemID, r.VersionID, c.ItemID, c.VersionID, sha)
			if err == nil {
				w.Replaced.Versions++
				w.Replaced.Bytes += r.Size
				continue
			}
			reason := skipReason(err)
			if reason == "" {
				return nil, fmt.Errorf("%s: %w", r.Path, err)
			}
			skip(r, reason, err.Error())
		}
	}
	for i := range res.Records {
		if r := &res.Records[i]; r.Current && r.Ref != nil {
			skip(r, SkipStub, "")
		}
	}
	sort.SliceStable(w.Skipped, func(i, j int) bool { return w.Skipped[i].Path < w.Skipped[j].Path })
	res.Report.Writes = w
	return res, nil
}

// skipReason is the reason ReplaceWithRef's err skips a version, or ""
// for an error that ends the run.
func skipReason(err error) string {
	switch {
	case errors.Is(err, icbfs.ErrReferenced):
		return SkipReferrers
	case errors.Is(err, icbfs.ErrFileChanged), errors.Is(err, icbfs.ErrNotFound):
		return SkipChanged
	case errors.Is(err, icbfs.ErrIsRef):
		return SkipStub
	case errors.Is(err, icbfs.ErrRefTarget), errors.Is(err, icbfs.ErrRefChecksum), errors.Is(err, icbfs.ErrRefToRef), errors.Is(err, icbfs.ErrBadRef):
		return SkipTarget
	}
	return ""
}

// file is a file of the tree: its item key and a path to it.
type file struct{ key, path string }

// walk lists the files at path in f's tree (the path itself, if it is
// one), by path; an item with several names is listed once.
func walk(ctx context.Context, f *icbfs.Filesystem, path string) ([]file, error) {
	dirKey, dirPath := f.RootKey(), "/"
	var out []file
	seen := map[string]bool{}
	add := func(key, p string) {
		if !seen[key] {
			seen[key] = true
			out = append(out, file{key, p})
		}
	}
	if parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' }); len(parts) > 0 {
		for i, name := range parts {
			e, _, err := f.Lookup(ctx, dirKey, name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", "/"+strings.Join(parts[:i+1], "/"), err)
			}
			dirPath = strings.TrimSuffix(dirPath, "/") + "/" + name
			switch {
			case e.Type == block.TypeFile && i == len(parts)-1:
				return []file{{e.UUID, dirPath}}, nil
			case e.Type != block.TypeDir:
				return nil, fmt.Errorf("%s: not a directory or a file: %w", dirPath, icbfs.ErrNotFound)
			}
			dirKey = e.UUID
		}
	}
	type dir struct{ key, path string }
	queue := []dir{{dirKey, dirPath}}
	for len(queue) > 0 {
		d := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := f.ReadDir(ctx, d.key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.path, err)
		}
		for _, e := range entries {
			p := strings.TrimSuffix(d.path, "/") + "/" + e.Name
			switch e.Type {
			case block.TypeDir:
				queue = append(queue, dir{e.UUID, p})
			case block.TypeFile:
				add(e.UUID, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// WriteDump writes records as JSON lines.
func WriteDump(w io.Writer, records []Record) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i := range records {
		if err := enc.Encode(&records[i]); err != nil {
			return err
		}
	}
	return nil
}
