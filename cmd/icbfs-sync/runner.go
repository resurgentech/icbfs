package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/internal/syncer/folderside"
	"github.com/resurgentech/icbfs/internal/syncer/icbfsside"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Runner runs every pair's passes, each on its own schedule.
type Runner struct {
	paths Paths
	store objstore.Store
	actor string
	log   *Log

	mu    sync.Mutex
	pairs []*pairRun
	stop  context.CancelFunc
	wg    sync.WaitGroup
}

// pairRun is one pair, running.
type pairRun struct {
	cfg  Pair
	r    *Runner
	wake chan struct{}

	mu      sync.Mutex
	engine  *syncer.Engine
	closers []func()
	paused  bool
	state   string // what it's doing or waiting for, for the tray
	last    syncer.Report
	recent  []string
}

// PairStatus is a pair, for the tray.
type PairStatus struct {
	Name, Folder, Mode, State string
	Paused                    bool
	Busy                      bool
	Last                      syncer.Report
	BytesUp, BytesDown        int64
	PendingRemoval            int
	Recent                    []string
}

func newRunner(cfg *Config, paths Paths, log *Log) *Runner {
	host, _ := os.Hostname()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	r := &Runner{paths: paths, log: log, actor: fmt.Sprintf("icbfs-sync:%s/%s", host, user),
		store: pkgicbfs.OpenS3(pkgicbfs.S3{Endpoint: cfg.Store.Endpoint, Region: cfg.Store.Region, Bucket: cfg.Store.Bucket,
			AccessKey: cfg.Store.AccessKey, SecretKey: cfg.Store.SecretKey})}
	for _, p := range cfg.Pairs {
		r.pairs = append(r.pairs, &pairRun{cfg: p, r: r, wake: make(chan struct{}, 1), paused: p.Paused, state: "starting"})
	}
	return r
}

// Start runs the pairs until Stop.
func (r *Runner) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.stop = cancel
	for _, p := range r.pairs {
		r.wg.Add(1)
		go func(p *pairRun) {
			defer r.wg.Done()
			p.run(ctx)
		}(p)
	}
}

// Stop ends every pair, waiting for passes in progress to stop.
func (r *Runner) Stop() {
	if r.stop != nil {
		r.stop()
	}
	r.wg.Wait()
}

func (r *Runner) pair(name string) *pairRun {
	for _, p := range r.pairs {
		if p.cfg.Name == name {
			return p
		}
	}
	return nil
}

// SyncNow starts a pass of a pair (or every pair, "") now.
func (r *Runner) SyncNow(name string) {
	for _, p := range r.pairs {
		if name == "" || p.cfg.Name == name {
			p.poke()
		}
	}
}

// SetPaused pauses or resumes a pair.
func (r *Runner) SetPaused(name string, paused bool) {
	if p := r.pair(name); p != nil {
		p.mu.Lock()
		p.paused = paused
		p.mu.Unlock()
		p.poke()
	}
}

// Confirm lets a pair's held removals go ahead, and runs it.
func (r *Runner) Confirm(name string) {
	if p := r.pair(name); p != nil {
		p.mu.Lock()
		e := p.engine
		p.mu.Unlock()
		if e != nil {
			e.Confirm()
			p.poke()
		}
	}
}

// Status is every pair's state.
func (r *Runner) Status() []PairStatus {
	var out []PairStatus
	for _, p := range r.pairs {
		p.mu.Lock()
		s := PairStatus{Name: p.cfg.Name, Folder: p.cfg.Folder, Mode: p.cfg.Mode, State: p.state, Paused: p.paused,
			Last: p.last, PendingRemoval: p.last.PendingRemoval, Recent: append([]string(nil), p.recent...)}
		if p.engine != nil {
			st := p.engine.Status()
			s.Busy, s.BytesUp, s.BytesDown = st.Busy, st.BytesUp, st.BytesDown
			if st.Busy && st.Doing != "" {
				s.State = st.Doing
			}
		}
		p.mu.Unlock()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *pairRun) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pairRun) setState(s string) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
}

func (p *pairRun) note(line string) {
	p.mu.Lock()
	p.recent = append(p.recent, line)
	if len(p.recent) > 20 {
		p.recent = p.recent[len(p.recent)-20:]
	}
	p.mu.Unlock()
}

// wait sleeps for d, or until poked or stopped.
func (p *pairRun) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-p.wake:
		return true
	case <-t.C:
		return true
	}
}

// open makes the pair's engine, once the folder and the drive can be
// reached: a network drive may not be mounted yet.
func (p *pairRun) open(ctx context.Context) (*syncer.Engine, error) {
	c := p.cfg
	if _, err := os.Stat(c.Folder); err != nil {
		return nil, fmt.Errorf("the folder %s isn't there (not connected?)", c.Folder)
	}
	local, err := folderside.Open(c.Folder, folderside.Options{IDs: c.IDs, ACLs: c.ACLs})
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(p.r.paths.Data, safeName(c.Name))
	spool := filepath.Join(dataDir, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, err
	}
	remote, err := icbfsside.Open(ctx, p.r.store, c.Filesystem, c.Root, p.r.actor, filepath.Join(dataDir, "cache"))
	if err != nil {
		return nil, fmt.Errorf("the drive %s%s: %w", c.Filesystem, c.Root, err)
	}
	state, err := syncer.OpenState(filepath.Join(dataDir, "state.db"))
	if err != nil {
		remote.Close()
		return nil, err
	}
	p.mu.Lock()
	p.closers = append(p.closers, remote.Close, func() { state.Close() })
	p.mu.Unlock()
	mode := syncer.OneWay
	if c.Mode == "two-way" {
		mode = syncer.TwoWay
	}
	caps := local.Caps()
	p.r.log.Printf("%s: %s %s ↔ %s%s (folder: ids %v, ACLs %v, times to %v)", c.Name, mode, c.Folder, c.Filesystem, c.Root,
		caps.IDs, caps.ACLs, caps.TimeResolution)
	return syncer.New(syncer.Config{
		Name: c.Name, Mode: mode, Local: local, Remote: remote, State: state,
		CaseInsensitive: !c.CaseSensitive, Ignore: append(append([]string(nil), syncer.DefaultIgnore...), c.Ignore...),
		RequirePresent: c.RequirePresent, MaxRemovals: c.MaxRemovals, RemoteListEvery: c.remoteEvery(),
		SpoolDir: spool, Log: p.event,
	}), nil
}

func (p *pairRun) event(e syncer.Event) {
	line := e.Action
	if e.Note != "" {
		if line != "" {
			line += " — "
		}
		line += e.Note
	}
	if e.Err != nil {
		line += ": " + e.Err.Error()
	}
	p.r.log.Printf("%s: %s", e.Pair, line)
	if e.Action != "" || e.Err != nil {
		p.note(e.Time.Local().Format("15:04:05") + " " + line)
	}
}

func (p *pairRun) run(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.closers {
			c()
		}
		p.closers = nil
	}()
	for ctx.Err() == nil {
		p.mu.Lock()
		paused, e := p.paused, p.engine
		p.mu.Unlock()
		if paused {
			p.setState("paused")
			p.wait(ctx, 24*time.Hour)
			continue
		}
		if e == nil {
			var err error
			e, err = p.open(ctx)
			if err != nil {
				p.setState("waiting: " + err.Error())
				p.r.log.Printf("%s: %v", p.cfg.Name, err)
				p.wait(ctx, 30*time.Second)
				continue
			}
			p.mu.Lock()
			p.engine = e
			p.mu.Unlock()
		}
		rep := e.Pass(ctx)
		p.mu.Lock()
		p.last = rep
		switch {
		case rep.Refused != "":
			p.state = "held: " + rep.Refused
		case len(rep.Errors) > 0:
			p.state = fmt.Sprintf("last pass: %d done, %d failed", rep.Done, len(rep.Errors))
		default:
			p.state = "up to date (" + rep.Finished.Local().Format("15:04") + ")"
		}
		p.mu.Unlock()
		if rep.Done > 0 || len(rep.Errors) > 0 {
			p.r.log.Printf("%s: pass: %d of %d done, %d errors, %d renames, %s up, %s down", p.cfg.Name,
				rep.Done, rep.Planned, len(rep.Errors), rep.Renames, size(rep.BytesUp), size(rep.Down))
		}
		p.wait(ctx, p.cfg.interval())
	}
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, s)
}

// size is a number of bytes for people.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
