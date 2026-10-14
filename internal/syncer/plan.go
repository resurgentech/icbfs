package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// Kind is what an action does.
type Kind int

const (
	// Adopt records an item both sides already have, identical, without
	// copying anything (the first sync after seeding the drive).
	Adopt Kind = iota
	// MoveRemote renames or moves an item on the drive, keeping its id
	// and history: the folder renamed it.
	MoveRemote
	// MoveLocal renames or moves an item in the folder: the drive
	// renamed it (two-way).
	MoveLocal
	MkdirRemote
	MkdirLocal
	// Upload copies a file from the folder to the drive. With
	// Candidates, it may turn out to be a rename: if its content hashes
	// the same as one of those files, gone from the folder, that file is
	// moved here instead.
	Upload
	// Download copies a file from the drive to the folder (two-way).
	Download
	// SetACLRemote copies an item's ACL from the folder to the drive.
	SetACLRemote
	RemoveRemote
	RemoveLocal
	// Forget drops the base of an item gone from both sides.
	Forget
)

var kindNames = [...]string{"adopt", "move-remote", "move-local", "mkdir-remote", "mkdir-local",
	"upload", "download", "set-acl-remote", "remove-remote", "remove-local", "forget"}

func (k Kind) String() string { return kindNames[k] }

// Action is one step of a plan.
type Action struct {
	Kind Kind
	Key  string // the item's key after the action
	Path string // its path, as spelled, after the action
	Dir  bool
	// From is the path moved from (MoveRemote, MoveLocal).
	From string
	// Candidates are the keys of files gone from the folder, of this
	// upload's size, that it may be a rename of (Upload).
	Candidates []string
	// Note says why, for the log: a conflict, an overwritten edit.
	Note string
}

func (a Action) String() string {
	s := a.Kind.String() + " " + a.Path
	if a.From != "" {
		s = a.Kind.String() + " " + a.From + " -> " + a.Path
	}
	if a.Note != "" {
		s += " (" + a.Note + ")"
	}
	return s
}

// Plan is what a pass will do.
type Plan struct {
	Actions []Action
	// RemoteRemovals and LocalRemovals count the files the plan deletes
	// on each side, for the safety check.
	RemoteRemovals, LocalRemovals int
	// Base is the base as the plan's moves leave it: what the actions
	// are relative to.
	Base map[string]Base
}

// PlanOptions describe the pair.
type PlanOptions struct {
	Mode            Mode
	CaseInsensitive bool
	// Resolution is how close two times must be to count as equal: the
	// coarser of the sides' resolutions.
	Resolution time.Duration
	// LocalIDs: the folder's entries carry stable ids.
	LocalIDs bool
	// LocalACLs: the folder's entries carry real ACLs.
	LocalACLs bool
}

// HashACL is the hash of an ACL, as kept in the base.
func HashACL(sddl string) string {
	if sddl == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sddl))
	return hex.EncodeToString(sum[:8])
}

// MakePlan compares the folder's and the drive's listings with the base
// and says what to do. remote nil means the drive wasn't listed this
// pass: it's taken to be as the base left it.
func MakePlan(opt PlanOptions, local, remote []Entry, base map[string]Base) *Plan {
	p := &planner{opt: opt, base: map[string]Base{}, l: map[string]Entry{}, r: map[string]Entry{}}
	for k, b := range base {
		p.base[k] = b
	}
	for _, e := range local {
		p.l[p.key(e.Path)] = e
	}
	if remote == nil {
		for k, b := range p.base {
			p.r[k] = Entry{Path: b.Path, Dir: b.Dir, Size: b.Size, ModTime: b.ModTime, ID: b.RemoteID, Version: b.RemoteVersion}
		}
	} else {
		for _, e := range remote {
			p.r[p.key(e.Path)] = e
		}
	}
	p.moves()
	p.compare()
	return p.finish()
}

type planner struct {
	opt     PlanOptions
	base    map[string]Base
	l, r    map[string]Entry
	actions []Action
	plan    Plan
}

func (p *planner) key(s string) string { return Key(s, p.opt.CaseInsensitive) }

func (p *planner) add(a Action) { p.actions = append(p.actions, a) }

func (p *planner) timeEq(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	res := p.opt.Resolution
	if res <= 0 {
		res = time.Nanosecond
	}
	if d < res {
		return true
	}
	// Both truncated to the resolution: a whole-second side against a
	// finer one.
	return a.Truncate(res).Equal(b.Truncate(res))
}

// sameFile: two listings, or a listing and the base, say the same file.
func (p *planner) sameFile(size1 int64, t1 time.Time, size2 int64, t2 time.Time) bool {
	return size1 == size2 && p.timeEq(t1, t2)
}

func (p *planner) localChanged(l Entry, b Base) bool {
	if l.Dir || b.Dir {
		return l.Dir != b.Dir
	}
	if !p.sameFile(l.Size, l.ModTime, b.Size, b.ModTime) {
		return true
	}
	// Replaced under the same name (a save that writes a new file and
	// renames it over the old one).
	return p.opt.LocalIDs && b.LocalID != "" && l.ID != "" && l.ID != b.LocalID
}

func (p *planner) remoteChanged(r Entry, b Base) bool {
	if r.Dir || b.Dir {
		return r.Dir != b.Dir
	}
	if r.Version != "" && b.RemoteVersion != "" {
		return r.Version != b.RemoteVersion
	}
	return !p.sameFile(r.Size, r.ModTime, b.Size, b.ModTime)
}

// --- renames ---

// under reports whether key k is inside directory key dir.
func under(k, dir string) bool { return strings.HasPrefix(k, dir+"/") }

// rekey moves the base and the drive's listing from oldKey (and, for a
// directory, everything under it) to newKey, at newPath: what the
// planned move will do.
func (p *planner) rekey(oldKey, newKey, newPath string) {
	oldPath := p.base[oldKey].Path
	move := func(m map[string]Base) {
		for k, b := range m {
			switch {
			case k == oldKey:
				delete(m, k)
				b.Path = newPath
				m[newKey] = b
			case under(k, oldKey):
				delete(m, k)
				b.Path = newPath + b.Path[len(oldPath):]
				m[newKey+k[len(oldKey):]] = b
			}
		}
	}
	move(p.base)
	for k, e := range p.r {
		switch {
		case k == oldKey:
			delete(p.r, k)
			e.Path = newPath
			p.r[newKey] = e
		case under(k, oldKey):
			delete(p.r, k)
			if len(e.Path) >= len(oldPath) {
				e.Path = newPath + e.Path[len(oldPath):]
			}
			p.r[newKey+k[len(oldKey):]] = e
		}
	}
}

// rekeyLocal is rekey for a move the drive made (two-way): the base and
// the folder's listing follow it.
func (p *planner) rekeyLocal(oldKey, newKey, newPath string) {
	oldPath := p.base[oldKey].Path
	for k, b := range p.base {
		switch {
		case k == oldKey:
			delete(p.base, k)
			b.Path = newPath
			p.base[newKey] = b
		case under(k, oldKey):
			delete(p.base, k)
			b.Path = newPath + b.Path[len(oldPath):]
			p.base[newKey+k[len(oldKey):]] = b
		}
	}
	for k, e := range p.l {
		switch {
		case k == oldKey:
			delete(p.l, k)
			e.Path = newPath
			p.l[newKey] = e
		case under(k, oldKey):
			delete(p.l, k)
			if len(e.Path) >= len(oldPath) {
				e.Path = newPath + e.Path[len(oldPath):]
			}
			p.l[newKey+k[len(oldKey):]] = e
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// Shallow first, then by name: a directory before what's in it.
	sort.Slice(ks, func(i, j int) bool {
		di, dj := strings.Count(ks[i], "/"), strings.Count(ks[j], "/")
		if di != dj {
			return di < dj
		}
		return ks[i] < ks[j]
	})
	return ks
}

// moves finds renames and moves that can be told without reading any
// content, and plans them first: by the folder's ids, by the drive's
// ids, by a directory's whole contents, and by spelling alone.
func (p *planner) moves() {
	// By the folder's stable ids.
	if p.opt.LocalIDs {
		byID := map[string]string{}
		for k, e := range p.l {
			if e.ID != "" {
				byID[e.ID] = k
			}
		}
		for _, k := range sortedKeys(p.base) {
			b, ok := p.base[k]
			if !ok || b.LocalID == "" {
				continue
			}
			if _, still := p.l[k]; still {
				continue
			}
			nk, found := byID[b.LocalID]
			if !found || nk == k {
				continue
			}
			if _, taken := p.base[nk]; taken {
				continue
			}
			if _, onRemote := p.r[k]; !onRemote {
				continue
			}
			to := p.l[nk].Path
			p.add(Action{Kind: MoveRemote, Key: nk, Path: to, From: p.r[k].Path, Dir: b.Dir})
			p.rekey(k, nk, to)
		}
	}
	// By the drive's ids (two-way: the drive renamed it).
	if p.opt.Mode == TwoWay {
		byID := map[string]string{}
		for k, e := range p.r {
			if e.ID != "" {
				byID[e.ID] = k
			}
		}
		for _, k := range sortedKeys(p.base) {
			b, ok := p.base[k]
			if !ok || b.RemoteID == "" {
				continue
			}
			if _, still := p.r[k]; still {
				continue
			}
			nk, found := byID[b.RemoteID]
			if !found || nk == k {
				continue
			}
			if _, taken := p.base[nk]; taken {
				continue
			}
			if _, inFolder := p.l[k]; !inFolder {
				continue
			}
			to := p.r[nk].Path
			p.add(Action{Kind: MoveLocal, Key: nk, Path: to, From: p.l[k].Path, Dir: b.Dir})
			p.rekeyLocal(k, nk, to)
		}
	}
	// A directory gone from the folder, and one new there with exactly
	// the same files (names, sizes and times): the directory was renamed
	// or moved.
	p.directoryMoves()
	// Spelling only (a case-insensitive pair): same key, new spelling.
	if p.opt.CaseInsensitive {
		for _, k := range sortedKeys(p.base) {
			b, ok := p.base[k]
			if !ok {
				continue
			}
			l, inFolder := p.l[k]
			r, onRemote := p.r[k]
			if !inFolder || !onRemote || l.Path == b.Path || path.Base(l.Path) == path.Base(b.Path) {
				continue
			}
			p.add(Action{Kind: MoveRemote, Key: k, Path: l.Path, From: r.Path, Dir: b.Dir})
			p.rekey(k, k, l.Path)
		}
	}
}

// signature is a directory's files, relative, with sizes and times.
func (p *planner) signature(dir string, files map[string]sigEntry) string {
	var rows []string
	for k, e := range files {
		if under(k, dir) {
			rows = append(rows, fmt.Sprintf("%s\x00%d\x00%d", k[len(dir):], e.size, e.mod.Truncate(p.res()).Unix()))
		}
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

type sigEntry struct {
	size int64
	mod  time.Time
}

func (p *planner) res() time.Duration {
	if p.opt.Resolution < time.Second {
		return time.Second
	}
	return p.opt.Resolution
}

func (p *planner) directoryMoves() {
	baseFiles := map[string]sigEntry{}
	for k, b := range p.base {
		if !b.Dir {
			baseFiles[k] = sigEntry{b.Size, b.ModTime}
		}
	}
	localFiles := map[string]sigEntry{}
	for k, e := range p.l {
		if !e.Dir {
			localFiles[k] = sigEntry{e.Size, e.ModTime}
		}
	}
	// New directories in the folder, by their contents.
	newDirs := map[string][]string{}
	for _, k := range sortedKeys(p.l) {
		e := p.l[k]
		if !e.Dir {
			continue
		}
		if _, known := p.base[k]; known {
			continue
		}
		if sig := p.signature(k, localFiles); sig != "" {
			newDirs[sig] = append(newDirs[sig], k)
		}
	}
	var matched []string
	for _, k := range sortedKeys(p.base) {
		b, ok := p.base[k]
		if !ok || !b.Dir {
			continue
		}
		if _, still := p.l[k]; still {
			continue
		}
		if _, onRemote := p.r[k]; !onRemote {
			continue
		}
		inside := false
		for _, m := range matched {
			if under(k, m) {
				inside = true
			}
		}
		if inside {
			continue
		}
		sig := p.signature(k, baseFiles)
		cands := newDirs[sig]
		if sig == "" || len(cands) != 1 {
			continue
		}
		nk := cands[0]
		delete(newDirs, sig)
		to := p.l[nk].Path
		p.add(Action{Kind: MoveRemote, Key: nk, Path: to, From: p.r[k].Path, Dir: true})
		p.rekey(k, nk, to)
		matched = append(matched, nk)
	}
}

// --- everything else ---

func (p *planner) compare() {
	keys := map[string]bool{}
	for k := range p.base {
		keys[k] = true
	}
	for k := range p.l {
		keys[k] = true
	}
	for k := range p.r {
		keys[k] = true
	}
	var gone []string // files the plan removes from the drive because the folder lost them
	var uploads []int // new uploads, which may be renames of those
	for _, k := range sortedKeys(keys) {
		b, hasB := p.base[k]
		l, hasL := p.l[k]
		r, hasR := p.r[k]
		two := p.opt.Mode == TwoWay
		switch {
		case !hasB && hasL && hasR:
			switch {
			case l.Dir && r.Dir:
				p.add(Action{Kind: Adopt, Key: k, Path: l.Path, Dir: true})
			case !l.Dir && !r.Dir && p.sameFile(l.Size, l.ModTime, r.Size, r.ModTime):
				p.add(Action{Kind: Adopt, Key: k, Path: l.Path})
			case l.Dir != r.Dir:
				p.add(Action{Kind: RemoveRemote, Key: k, Path: r.Path, Dir: r.Dir, Note: "replaced by the folder's " + kindOf(l)})
				p.create(k, l)
			default:
				note := ""
				if two {
					note = "new on both sides; the folder's kept, the drive's in its history"
				}
				p.add(Action{Kind: Upload, Key: k, Path: l.Path, Note: note})
			}
		case !hasB && hasL:
			p.create(k, l)
			if !l.Dir {
				uploads = append(uploads, len(p.actions)-1)
			}
		case !hasB && hasR:
			if two {
				if r.Dir {
					p.add(Action{Kind: MkdirLocal, Key: k, Path: r.Path, Dir: true})
				} else {
					p.add(Action{Kind: Download, Key: k, Path: r.Path})
				}
			} else {
				p.add(Action{Kind: RemoveRemote, Key: k, Path: r.Path, Dir: r.Dir, Note: "not in the folder"})
			}
		case hasB && hasL && hasR:
			lch, rch := p.localChanged(l, b), p.remoteChanged(r, b)
			switch {
			case l.Dir != r.Dir:
				p.add(Action{Kind: RemoveRemote, Key: k, Path: r.Path, Dir: r.Dir, Note: "replaced by the folder's " + kindOf(l)})
				p.create(k, l)
			case l.Dir:
				p.acl(k, l, b)
			case b.Dir: // a directory then, a file on both sides now
				if p.sameFile(l.Size, l.ModTime, r.Size, r.ModTime) {
					p.add(Action{Kind: Adopt, Key: k, Path: l.Path})
				} else {
					p.add(Action{Kind: Upload, Key: k, Path: l.Path})
				}
			case lch && rch:
				p.add(Action{Kind: Upload, Key: k, Path: l.Path, Note: "changed on both sides; the folder's kept, the drive's in its history"})
			case lch:
				p.add(Action{Kind: Upload, Key: k, Path: l.Path})
			case rch && two:
				p.add(Action{Kind: Download, Key: k, Path: r.Path})
			case rch:
				p.add(Action{Kind: Upload, Key: k, Path: l.Path, Note: "changed on the drive; overwritten, the drive's in its history"})
			default:
				p.acl(k, l, b)
			}
		case hasB && hasL: // gone from the drive
			switch {
			case l.Dir:
				if two {
					p.add(Action{Kind: RemoveLocal, Key: k, Path: l.Path, Dir: true, Note: "removed on the drive"})
				} else {
					p.add(Action{Kind: MkdirRemote, Key: k, Path: l.Path, Dir: true, Note: "removed on the drive; put back"})
				}
			case two && !p.localChanged(l, b):
				p.add(Action{Kind: RemoveLocal, Key: k, Path: l.Path, Note: "removed on the drive"})
			default:
				note := "removed on the drive; put back"
				if two {
					note = "changed in the folder, removed on the drive; the change wins"
				}
				p.add(Action{Kind: Upload, Key: k, Path: l.Path, Note: note})
			}
		case hasB && hasR: // gone from the folder
			switch {
			case r.Dir:
				p.add(Action{Kind: RemoveRemote, Key: k, Path: r.Path, Dir: true})
			case two && p.remoteChanged(r, b):
				p.add(Action{Kind: Download, Key: k, Path: r.Path, Note: "removed in the folder, changed on the drive; the change wins"})
			default:
				p.add(Action{Kind: RemoveRemote, Key: k, Path: r.Path})
				gone = append(gone, k)
			}
		case hasB:
			p.add(Action{Kind: Forget, Key: k, Path: b.Path, Dir: b.Dir})
		}
	}
	// A new file the size of one that's gone may be it, renamed.
	bySize := map[int64][]string{}
	for _, k := range gone {
		bySize[p.base[k].Size] = append(bySize[p.base[k].Size], k)
	}
	for _, i := range uploads {
		a := &p.actions[i]
		size := p.l[a.Key].Size
		if size == 0 {
			continue // every empty file looks like every other
		}
		a.Candidates = append([]string(nil), bySize[size]...)
	}
}

func kindOf(e Entry) string {
	if e.Dir {
		return "directory"
	}
	return "file"
}

// create puts a folder item on the drive.
func (p *planner) create(k string, l Entry) {
	if l.Dir {
		p.add(Action{Kind: MkdirRemote, Key: k, Path: l.Path, Dir: true})
		return
	}
	p.add(Action{Kind: Upload, Key: k, Path: l.Path})
}

// acl copies a changed ACL, where the folder has real ones.
func (p *planner) acl(k string, l Entry, b Base) {
	if p.opt.LocalACLs && l.ACL != "" && HashACL(l.ACL) != b.ACLHash {
		p.add(Action{Kind: SetACLRemote, Key: k, Path: l.Path, Dir: l.Dir})
	}
}

// phase orders a plan: moves, then directories made (shallow first),
// then content, then removals (files, then directories deepest first).
func phase(a Action) int {
	switch a.Kind {
	case MoveRemote, MoveLocal:
		return 0
	case MkdirRemote, MkdirLocal:
		return 1
	case Adopt, Upload, Download, SetACLRemote:
		return 2
	case RemoveRemote, RemoveLocal:
		if !a.Dir {
			return 3
		}
		return 4
	}
	return 5
}

func (p *planner) finish() *Plan {
	acts := p.actions
	// Moves keep the order they were found in (each relative to the
	// last); the rest sort by phase and depth.
	sort.SliceStable(acts, func(i, j int) bool {
		pi, pj := phase(acts[i]), phase(acts[j])
		if pi != pj {
			return pi < pj
		}
		if pi == 0 {
			return false
		}
		di, dj := strings.Count(acts[i].Key, "/"), strings.Count(acts[j].Key, "/")
		if di != dj {
			if pi == 4 {
				return di > dj
			}
			return di < dj
		}
		return acts[i].Key < acts[j].Key
	})
	for _, a := range acts {
		if a.Dir {
			continue
		}
		switch a.Kind {
		case RemoveRemote:
			p.plan.RemoteRemovals++
		case RemoveLocal:
			p.plan.LocalRemovals++
		}
	}
	p.plan.Actions = acts
	p.plan.Base = p.base
	return &p.plan
}
