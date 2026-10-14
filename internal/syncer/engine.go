package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// State keeps a pair's base between passes.
type State interface {
	Load(ctx context.Context) (map[string]Base, error)
	// Save replaces the base.
	Save(ctx context.Context, base map[string]Base) error
}

// MemState is a State in memory.
type MemState struct {
	mu   sync.Mutex
	base map[string]Base
}

func (m *MemState) Load(context.Context) (map[string]Base, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Base{}
	for k, b := range m.base {
		out[k] = b
	}
	return out, nil
}

func (m *MemState) Save(_ context.Context, base map[string]Base) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.base = map[string]Base{}
	for k, b := range base {
		m.base[k] = b
	}
	return nil
}

// DefaultIgnore are names never synced: editors' lock and temporary
// files, the shell's own files.
var DefaultIgnore = []string{"~$*", "~*.tmp", "*.tmp", ".~lock.*", "Thumbs.db", "desktop.ini", ".DS_Store", "$RECYCLE.BIN", "System Volume Information"}

// Config is a pair.
type Config struct {
	Name   string
	Mode   Mode
	Local  Side // the folder
	Remote Side // the icbfs drive; Hashes and Trash if it has them
	State  State
	// CaseInsensitive pairs match names regardless of case (Windows).
	CaseInsensitive bool
	// Ignore are name patterns (path.Match, case-insensitively) skipped
	// on both sides, with everything under a matching directory.
	Ignore []string
	// RequirePresent are paths that must be in the folder's listing for
	// a pass to go ahead: a marker file that shows the folder is really
	// there (a network drive that isn't mounted lists as empty).
	RequirePresent []string
	// MaxRemovals is how many files a pass may remove (from either side)
	// without Confirm; 0 means no limit.
	MaxRemovals int
	// RemoteListEvery: one-way passes list the drive only this often
	// (and on the first pass, and after a failure), trusting the base in
	// between; 0 lists it every pass. Two-way passes always list it.
	RemoteListEvery time.Duration
	// SpoolDir holds a new file while it's checked for being a rename,
	// so it's read from the folder only once ("" for the temp directory).
	SpoolDir string
	// Log receives what each pass does.
	Log func(Event)
}

// Event is something a pass did, or found.
type Event struct {
	Time   time.Time
	Pair   string
	Action string // the action, as text; "" for a pass-level event
	Err    error
	Note   string
}

// Report is one pass.
type Report struct {
	Started, Finished time.Time
	Planned, Done     int
	Errors            []error
	// Refused says why the pass did nothing, if it refused.
	Refused        string
	BytesUp, Down  int64
	ListedRemote   bool
	Renames        int // files kept, not re-uploaded, because they were renames
	PendingRemoval int // removals waiting for Confirm
}

// Status is the engine's state, for a tray or a log.
type Status struct {
	Pair      string
	Mode      Mode
	Busy      bool
	Doing     string // the action in progress
	LastPass  Report
	BytesUp   int64 // since the engine started
	BytesDown int64
}

// Engine runs a pair's passes.
type Engine struct {
	cfg Config

	mu         sync.Mutex
	status     Status
	lastRemote time.Time
	forceList  bool
	confirmed  bool

	up, down atomic.Int64
}

// New makes an engine for a pair.
func New(cfg Config) *Engine {
	if cfg.Ignore == nil {
		cfg.Ignore = DefaultIgnore
	}
	return &Engine{cfg: cfg, status: Status{Pair: cfg.Name, Mode: cfg.Mode}}
}

// Status is a copy of the engine's state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.BytesUp, s.BytesDown = e.up.Load(), e.down.Load()
	return s
}

// Confirm lets the next pass make the removals the last one refused.
func (e *Engine) Confirm() {
	e.mu.Lock()
	e.confirmed = true
	e.mu.Unlock()
}

// ListRemoteNext makes the next pass list the drive.
func (e *Engine) ListRemoteNext() {
	e.mu.Lock()
	e.forceList = true
	e.mu.Unlock()
}

func (e *Engine) log(ev Event) {
	ev.Time, ev.Pair = time.Now(), e.cfg.Name
	if e.cfg.Log != nil {
		e.cfg.Log(ev)
	}
}

func (e *Engine) doing(s string) {
	e.mu.Lock()
	e.status.Doing = s
	e.mu.Unlock()
}

// ignored reports whether a path is skipped.
func (e *Engine) ignored(p string) bool {
	for _, part := range strings.Split(p, "/") {
		for _, pat := range e.cfg.Ignore {
			if ok, _ := path.Match(strings.ToLower(pat), strings.ToLower(part)); ok {
				return true
			}
		}
	}
	return false
}

func (e *Engine) filter(es []Entry) []Entry {
	out := es[:0:0]
	for _, x := range es {
		if !e.ignored(x.Path) {
			out = append(out, x)
		}
	}
	return out
}

func (e *Engine) resolution() time.Duration {
	r := e.cfg.Local.Caps().TimeResolution
	if rr := e.cfg.Remote.Caps().TimeResolution; rr > r {
		r = rr
	}
	if r <= 0 {
		r = time.Nanosecond
	}
	return r
}

// Pass lists, plans, checks and carries out one sync.
func (e *Engine) Pass(ctx context.Context) (rep Report) {
	rep.Started = time.Now()
	e.mu.Lock()
	e.status.Busy = true
	e.mu.Unlock()
	defer func() {
		rep.Finished = time.Now()
		e.mu.Lock()
		e.status.Busy, e.status.Doing, e.status.LastPass = false, "", rep
		e.mu.Unlock()
	}()
	fail := func(err error) Report {
		rep.Errors = append(rep.Errors, err)
		e.log(Event{Err: err})
		e.ListRemoteNext()
		return rep
	}

	base, err := e.cfg.State.Load(ctx)
	if err != nil {
		return fail(fmt.Errorf("load state: %w", err))
	}
	e.doing("listing the folder")
	local, err := e.cfg.Local.List(ctx)
	if err != nil {
		return fail(fmt.Errorf("list the folder: %w", err))
	}
	local = e.filter(local)
	if why := e.present(local, base); why != "" {
		rep.Refused = why
		e.log(Event{Note: "refused: " + why})
		return rep
	}

	var remote []Entry
	e.mu.Lock()
	listRemote := e.cfg.Mode == TwoWay || e.forceList || e.lastRemote.IsZero() ||
		e.cfg.RemoteListEvery == 0 || time.Since(e.lastRemote) >= e.cfg.RemoteListEvery
	e.mu.Unlock()
	if listRemote {
		e.doing("listing the drive")
		remote, err = e.cfg.Remote.List(ctx)
		if err != nil {
			return fail(fmt.Errorf("list the drive: %w", err))
		}
		remote = e.filter(remote)
		if remote == nil {
			remote = []Entry{}
		}
		rep.ListedRemote = true
	}

	plan := MakePlan(PlanOptions{
		Mode: e.cfg.Mode, CaseInsensitive: e.cfg.CaseInsensitive, Resolution: e.resolution(),
		LocalIDs: e.cfg.Local.Caps().IDs, LocalACLs: e.cfg.Local.Caps().ACLs && e.cfg.Remote.Caps().ACLs,
	}, local, remote, base)
	rep.Planned = len(plan.Actions)

	removals := plan.RemoteRemovals + plan.LocalRemovals
	e.mu.Lock()
	confirmed := e.confirmed
	e.mu.Unlock()
	if e.cfg.MaxRemovals > 0 && removals > e.cfg.MaxRemovals && !confirmed {
		rep.Refused = fmt.Sprintf("%d files would be removed (more than %d): confirm to go ahead", removals, e.cfg.MaxRemovals)
		rep.PendingRemoval = removals
		e.log(Event{Note: "refused: " + rep.Refused})
		return rep
	}

	x := &executor{e: e, ctx: ctx, base: base, rep: &rep,
		local: map[string]Entry{}, remote: map[string]Entry{}, consumed: map[string]bool{}}
	for _, l := range local {
		x.local[Key(l.Path, e.cfg.CaseInsensitive)] = l
	}
	for _, r := range remote {
		x.remote[Key(r.Path, e.cfg.CaseInsensitive)] = r
	}
	for _, a := range plan.Actions {
		if ctx.Err() != nil {
			rep.Errors = append(rep.Errors, ctx.Err())
			break
		}
		e.doing(a.String())
		err := x.do(a)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: %w", a, err))
			e.log(Event{Action: a.String(), Err: err})
			if a.Kind == MoveRemote || a.Kind == MoveLocal {
				// What follows was planned on the move having happened.
				break
			}
			continue
		}
		rep.Done++
		if a.Kind != Adopt && a.Kind != Forget {
			e.log(Event{Action: a.String(), Note: a.Note})
		}
	}
	if err := e.cfg.State.Save(ctx, x.base); err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("save state: %w", err))
	}
	e.mu.Lock()
	if listRemote {
		e.lastRemote = time.Now()
		e.forceList = false
	}
	if len(rep.Errors) > 0 {
		e.forceList = true
	}
	if removals > 0 {
		e.confirmed = false
	}
	e.mu.Unlock()
	return rep
}

// present checks the folder is really there: the marker files are, and
// it isn't empty when the base says it had things.
func (e *Engine) present(local []Entry, base map[string]Base) string {
	have := map[string]bool{}
	for _, l := range local {
		have[Key(l.Path, e.cfg.CaseInsensitive)] = true
	}
	for _, p := range e.cfg.RequirePresent {
		if !have[Key(p, e.cfg.CaseInsensitive)] {
			return fmt.Sprintf("%s isn't in the folder: is it connected?", p)
		}
	}
	if len(local) == 0 && len(base) > 0 {
		return "the folder lists as empty: is it connected?"
	}
	return ""
}

// executor carries out a plan, keeping the base current as it goes.
type executor struct {
	e        *Engine
	ctx      context.Context
	base     map[string]Base
	rep      *Report
	local    map[string]Entry
	remote   map[string]Entry
	consumed map[string]bool // files a rename used, not to be removed

	trashBySize map[int64][]Trashed
}

func (x *executor) ci() bool { return x.e.cfg.CaseInsensitive }

// rekey is the planner's: the base follows a move.
func rekeyBase(m map[string]Base, oldKey, newKey, newPath string) {
	b, ok := m[oldKey]
	if !ok {
		return
	}
	oldPath := b.Path
	for k, v := range m {
		switch {
		case k == oldKey:
			delete(m, k)
			v.Path = newPath
			m[newKey] = v
		case under(k, oldKey):
			delete(m, k)
			v.Path = newPath + v.Path[len(oldPath):]
			m[newKey+k[len(oldKey):]] = v
		}
	}
}

func (x *executor) do(a Action) error {
	e := x.e
	L, R := e.cfg.Local, e.cfg.Remote
	switch a.Kind {
	case Adopt:
		l, r := x.local[a.Key], x.remote[a.Key]
		x.base[a.Key] = Base{Path: l.Path, Dir: l.Dir, Size: l.Size, ModTime: l.ModTime, LocalID: l.ID,
			RemoteID: r.ID, RemoteVersion: r.Version, ACLHash: x.aclHash(l)}
		return nil
	case Forget:
		delete(x.base, a.Key)
		return nil
	case MoveRemote:
		if _, err := R.Move(x.ctx, a.From, a.Path); err != nil {
			return err
		}
		rekeyBase(x.base, Key(a.From, x.ci()), a.Key, a.Path)
		x.rep.Renames++
		return nil
	case MoveLocal:
		if _, err := L.Move(x.ctx, a.From, a.Path); err != nil {
			return err
		}
		rekeyBase(x.base, Key(a.From, x.ci()), a.Key, a.Path)
		return nil
	case MkdirRemote:
		r, err := R.Mkdir(x.ctx, a.Path)
		if err != nil {
			return err
		}
		l := x.local[a.Key]
		if l.ACL != "" && x.acls() {
			if err := R.SetACL(x.ctx, a.Path, l.ACL); err != nil {
				return err
			}
		}
		x.base[a.Key] = Base{Path: a.Path, Dir: true, LocalID: l.ID, RemoteID: r.ID, ACLHash: x.aclHash(l)}
		return nil
	case MkdirLocal:
		l, err := L.Mkdir(x.ctx, a.Path)
		if err != nil {
			return err
		}
		x.base[a.Key] = Base{Path: a.Path, Dir: true, LocalID: l.ID, RemoteID: x.remote[a.Key].ID}
		return nil
	case Upload:
		return x.upload(a)
	case Download:
		return x.download(a)
	case SetACLRemote:
		l := x.local[a.Key]
		if err := R.SetACL(x.ctx, a.Path, l.ACL); err != nil {
			return err
		}
		b := x.base[a.Key]
		b.ACLHash = x.aclHash(l)
		x.base[a.Key] = b
		return nil
	case RemoveRemote:
		if x.consumed[a.Key] {
			return nil
		}
		if err := R.Remove(x.ctx, a.Path); err != nil {
			if a.Dir && !errors.Is(err, os.ErrNotExist) {
				// Something is in it still (a file kept by an edit that
				// beat a delete): leave the directory.
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		delete(x.base, a.Key)
		return nil
	case RemoveLocal:
		if err := L.Remove(x.ctx, a.Path); err != nil {
			if a.Dir && !errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		delete(x.base, a.Key)
		return nil
	}
	return fmt.Errorf("unknown action %v", a.Kind)
}

func (x *executor) acls() bool {
	return x.e.cfg.Local.Caps().ACLs && x.e.cfg.Remote.Caps().ACLs
}

func (x *executor) aclHash(l Entry) string {
	if !x.acls() {
		return ""
	}
	return HashACL(l.ACL)
}

// counting counts bytes into an atomic as they're read.
type counting struct {
	r io.Reader
	n *atomic.Int64
	m *int64
}

func (c counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	*c.m += int64(n)
	return n, err
}

// upload copies a folder file to the drive — unless it turns out to be
// a file gone from the folder, renamed: then that file is moved (or
// brought back from the drive's trash) instead, keeping its history.
func (x *executor) upload(a Action) error {
	R := x.e.cfg.Remote
	l := x.local[a.Key]
	_, hasBase := x.base[a.Key]

	var trashed []Trashed
	if !hasBase {
		trashed = x.trashed(l.Size)
	}
	if len(a.Candidates) == 0 && len(trashed) == 0 {
		return x.copyUp(a, l, nil)
	}

	// It may be a rename: read it once, into the spool, hashing it.
	spool, hash, err := x.spool(a.Path)
	if err != nil {
		return err
	}
	defer func() { spool.Close(); os.Remove(spool.Name()) }()

	hashes, _ := R.(Hashes)
	for _, k := range a.Candidates {
		b, ok := x.base[k]
		if !ok || x.consumed[k] {
			continue
		}
		h := b.Hash
		if h == "" && hashes != nil {
			h, _ = hashes.StoredHash(x.ctx, b.Path)
		}
		if h == "" {
			h, err = x.hashRemote(b.Path)
			if err != nil {
				continue
			}
		}
		if h != hash {
			continue
		}
		moved, err := R.Move(x.ctx, b.Path, a.Path)
		if err != nil {
			return err
		}
		x.consumed[k] = true
		delete(x.base, k)
		x.rep.Renames++
		return x.recordUp(a, l, moved, hash)
	}
	if tr, ok := R.(Trash); ok {
		for _, t := range trashed {
			h, err := tr.TrashedHash(x.ctx, t)
			if err != nil || h != hash {
				continue
			}
			restored, err := tr.Restore(x.ctx, t, a.Path)
			if err != nil {
				return err
			}
			x.dropTrashed(t)
			x.rep.Renames++
			return x.recordUp(a, l, restored, hash)
		}
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return x.copyUp(a, l, spool)
}

// recordUp fills in the base after a rename made the drive's file (now
// entry) the folder's new one: its hash and ACL kept.
func (x *executor) recordUp(a Action, l Entry, entry Entry, hash string) error {
	R := x.e.cfg.Remote
	if h, ok := R.(Hashes); ok {
		_ = h.StoreHash(x.ctx, a.Path, hash)
	}
	if l.ACL != "" && x.acls() {
		if err := R.SetACL(x.ctx, a.Path, l.ACL); err != nil {
			return err
		}
	}
	x.base[a.Key] = Base{Path: a.Path, Size: l.Size, ModTime: l.ModTime, LocalID: l.ID,
		RemoteID: entry.ID, RemoteVersion: entry.Version, Hash: hash, ACLHash: x.aclHash(l)}
	return nil
}

// copyUp writes the folder file to the drive, from src (the spool) or
// read from the folder now, hashing it on the way.
func (x *executor) copyUp(a Action, l Entry, src io.Reader) error {
	e := x.e
	L, R := e.cfg.Local, e.cfg.Remote
	if src == nil {
		rc, err := L.Read(x.ctx, a.Path)
		if err != nil {
			return err
		}
		defer rc.Close()
		src = rc
	}
	h := sha256.New()
	r := io.TeeReader(counting{src, &e.up, &x.rep.BytesUp}, h)
	entry, err := R.Write(x.ctx, a.Path, r, l.Size, l.ModTime)
	if err != nil {
		return err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if hs, ok := R.(Hashes); ok {
		_ = hs.StoreHash(x.ctx, a.Path, hash)
	}
	if l.ACL != "" && x.acls() {
		if err := R.SetACL(x.ctx, a.Path, l.ACL); err != nil {
			return err
		}
	}
	x.base[a.Key] = Base{Path: a.Path, Size: l.Size, ModTime: l.ModTime, LocalID: l.ID,
		RemoteID: entry.ID, RemoteVersion: entry.Version, Hash: hash, ACLHash: x.aclHash(l)}
	return nil
}

// download writes a drive file to the folder.
func (x *executor) download(a Action) error {
	e := x.e
	L, R := e.cfg.Local, e.cfg.Remote
	r := x.remote[a.Key]
	rc, err := R.Read(x.ctx, a.Path)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	l, err := L.Write(x.ctx, a.Path, io.TeeReader(counting{rc, &e.down, &x.rep.Down}, h), r.Size, r.ModTime)
	if err != nil {
		return err
	}
	x.base[a.Key] = Base{Path: a.Path, Size: l.Size, ModTime: l.ModTime, LocalID: l.ID,
		RemoteID: r.ID, RemoteVersion: r.Version, Hash: hex.EncodeToString(h.Sum(nil)), ACLHash: x.aclHash(l)}
	return nil
}

// spool copies a folder file into a temporary file, hashing it.
func (x *executor) spool(p string) (*os.File, string, error) {
	rc, err := x.e.cfg.Local.Read(x.ctx, p)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp(x.e.cfg.SpoolDir, "icbfs-sync-")
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, "", err
	}
	return f, hex.EncodeToString(h.Sum(nil)), nil
}

// hashRemote hashes a drive file's content.
func (x *executor) hashRemote(p string) (string, error) {
	rc, err := x.e.cfg.Remote.Read(x.ctx, p)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// trashed is the drive's trashed files of a size (asked once per size
// per pass).
func (x *executor) trashed(size int64) []Trashed {
	tr, ok := x.e.cfg.Remote.(Trash)
	if !ok || size == 0 {
		return nil
	}
	if x.trashBySize == nil {
		x.trashBySize = map[int64][]Trashed{}
	}
	if ts, ok := x.trashBySize[size]; ok {
		return ts
	}
	ts, err := tr.TrashedFiles(x.ctx, size)
	if err != nil {
		ts = nil
	}
	x.trashBySize[size] = ts
	return ts
}

func (x *executor) dropTrashed(t Trashed) {
	ts := x.trashBySize[t.Size]
	for i, y := range ts {
		if y.ID == t.ID {
			x.trashBySize[t.Size] = append(ts[:i:i], ts[i+1:]...)
			return
		}
	}
}
