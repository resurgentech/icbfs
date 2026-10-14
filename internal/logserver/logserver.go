// Package logserver is a mock of the server end of icbfs's logging
// (cmd/icbfs-mockserver): daemons connect over a websocket with a shared
// secret; it answers mount requests for the drives in its registry,
// runs side commands (icbfs-cli through the daemon; internal/sidecmd) on
// the configured store, and appends every event to a JSON-lines log, one event per line,
// before acknowledging it — so a mount logging synchronously knows its
// event is on disk. An event ID already logged (a resend after a
// reconnect) is acknowledged without being written again.
package logserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/sidecmd"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Config is what the server hands out and where it logs.
type Config struct {
	Secret string      // the bearer secret daemons must present
	Log    string      // the JSON-lines event log, appended to
	Creds  fslog.Creds // the store credentials every drive's mount gets
	// Drives is the registry: drive id → its filesystem, root and
	// whether machines may only read it. A drive not in it can't be
	// mounted, nor named by a command.
	Drives map[string]fslog.Mount
	// AllowCommands are the side commands daemons may run ("names
	// create", ...); empty allows every one.
	AllowCommands []string
	// StoreEndpoint, if set, is the store's endpoint as the server itself
	// reaches it, for side commands (Creds.Endpoint is as mounts do).
	StoreEndpoint string
}

// Server is the mock server.
type Server struct {
	cfg      Config
	upgrader websocket.Upgrader

	mu    sync.Mutex // guards f, seen and conns
	f     *os.File
	seen  map[string]bool
	conns map[*websocket.Conn]bool
	store objstore.Store // the store side commands run on

	drivesMu sync.Mutex
	drives   map[string]*icbfsdrive.Drive // opened on first use
}

// New opens cfg.Log for appending, remembering the IDs it already holds
// so an event resent across a server restart isn't logged twice.
func New(cfg Config) (*Server, error) {
	seen := map[string]bool{}
	if old, err := os.Open(cfg.Log); err == nil {
		sc := bufio.NewScanner(old)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var ev fslog.Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.ID != "" {
				seen[ev.ID] = true
			}
		}
		old.Close()
	}
	f, err := os.OpenFile(cfg.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	endpoint := cfg.Creds.Endpoint
	if cfg.StoreEndpoint != "" {
		endpoint = cfg.StoreEndpoint
	}
	store := pkgicbfs.OpenS3(pkgicbfs.S3{Endpoint: endpoint, Region: cfg.Creds.Region, Bucket: cfg.Creds.Bucket,
		AccessKey: cfg.Creds.AccessKey, SecretKey: cfg.Creds.SecretKey})
	return &Server{cfg: cfg, f: f, seen: seen, conns: map[*websocket.Conn]bool{}, store: store}, nil
}

// Close drops every daemon connection and closes the log.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ws := range s.conns {
		ws.Close()
	}
	return s.f.Close()
}

// Handler serves the daemon websocket at /ws/daemon.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/daemon", s.serveDaemon)
	return mux
}

func (s *Server) serveDaemon(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conns[ws] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, ws)
		s.mu.Unlock()
		ws.Close()
	}()
	log.Printf("logserver: daemon connected from %s", r.RemoteAddr)
	var wmu sync.Mutex
	write := func(f fslog.Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return ws.WriteJSON(f)
	}
	for {
		var f fslog.Frame
		if err := ws.ReadJSON(&f); err != nil {
			return
		}
		reply := fslog.Frame{Op: fslog.OpReply, ID: f.ID}
		if f.Op == fslog.OpCommand {
			// Commands can take a while; events don't wait for them.
			go func(f fslog.Frame) {
				reply.Result, reply.Error = s.command(f)
				reply.OK = reply.Error == ""
				_ = write(reply)
			}(f)
			continue
		}
		switch f.Op {
		case fslog.OpCreds:
			if m, ok := s.cfg.Drives[f.Drive]; ok {
				creds := s.cfg.Creds
				reply.OK, reply.Creds, reply.Mount = true, &creds, &m
				log.Printf("logserver: mount of drive %q (%s:%s) for %s", f.Drive, m.Filesystem, m.Root, f.Host)
			} else {
				reply.Error = fmt.Sprintf("drive %q is not allowed to mount", f.Drive)
				log.Printf("logserver: refused to mount drive %q for %s", f.Drive, f.Host)
			}
		case fslog.OpEvent:
			if err := s.record(f.Event); err != nil {
				reply.Error = err.Error()
			} else {
				reply.OK = true
			}
		default:
			reply.Error = "unknown op " + f.Op
		}
		if err := write(reply); err != nil {
			return
		}
	}
}

// command runs a side command, if it's allowed, and logs it.
func (s *Server) command(f fslog.Frame) (json.RawMessage, string) {
	c := f.Command
	if c == nil {
		return nil, "a command frame without a command"
	}
	if len(s.cfg.AllowCommands) > 0 && !slices.Contains(s.cfg.AllowCommands, c.Name) {
		log.Printf("logserver: refused %q for %s", c.Name, c.Actor)
		return nil, fmt.Sprintf("%s may not %s", c.Actor, c.Name)
	}
	if m := s.cfg.Drives[c.Args["drive"]]; m.ReadOnly && c.Name == "trash restore" {
		return nil, fmt.Sprintf("drive %q is read-only here", c.Args["drive"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := sidecmd.Run(ctx, sidecmd.Env{Store: s.store, Drive: s.drive}, sidecmd.Request{Name: c.Name, Args: c.Args, Actor: c.Actor})
	ev := &fslog.Event{ID: uuid.NewString(), Time: time.Now().UTC(), Host: f.Host, Drive: c.Args["drive"], FS: c.Args["fs"], Op: fslog.EventCommand, Path: c.Name, Actor: c.Actor}
	if m, ok := s.cfg.Drives[c.Args["drive"]]; ok {
		ev.FS = m.Filesystem
	}
	if err != nil {
		ev.Error = err.Error()
	}
	if lerr := s.record(ev); lerr != nil {
		log.Printf("logserver: logging %q: %v", c.Name, lerr)
	}
	if err != nil {
		return nil, err.Error()
	}
	return result, ""
}

// drive is the registry's drive a command names (args["drive"]).
func (s *Server) drive(ctx context.Context, args map[string]string) (drive.Drive, error) {
	id := args["drive"]
	m, ok := s.cfg.Drives[id]
	if !ok {
		return nil, fmt.Errorf("no drive %q", id)
	}
	s.drivesMu.Lock()
	defer s.drivesMu.Unlock()
	if d, ok := s.drives[id]; ok {
		return d, nil
	}
	d, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: s.store, Filesystem: m.Filesystem, Root: m.Root, ID: id})
	if err != nil {
		return nil, fmt.Errorf("drive %q: %w", id, err)
	}
	if s.drives == nil {
		s.drives = map[string]*icbfsdrive.Drive{}
	}
	s.drives[id] = d
	return d, nil
}

// record appends ev to the log and syncs it, once per event ID.
func (s *Server) record(ev *fslog.Event) error {
	if ev == nil || ev.ID == "" {
		return fmt.Errorf("event without an id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[ev.ID] {
		return nil
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.seen[ev.ID] = true
	return nil
}
