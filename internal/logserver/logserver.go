// Package logserver is a mock of the server end of icbfs's logging
// (cmd/icbfs-mockserver): daemons connect over a websocket with a shared
// secret; it answers credential requests for the filesystems it allows,
// and appends every event to a JSON-lines log, one event per line,
// before acknowledging it — so a mount logging synchronously knows its
// event is on disk. An event ID already logged (a resend after a
// reconnect) is acknowledged without being written again.
package logserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/resurgentech/icbfs/internal/fslog"
)

// Config is what the server hands out and where it logs.
type Config struct {
	Secret string      // the bearer secret daemons must present
	Log    string      // the JSON-lines event log, appended to
	Creds  fslog.Creds // the store credentials every allowed filesystem gets
	Allow  []string    // filesystem names allowed to mount; empty allows any
}

// Server is the mock server.
type Server struct {
	cfg      Config
	upgrader websocket.Upgrader

	mu    sync.Mutex // guards f, seen and conns
	f     *os.File
	seen  map[string]bool
	conns map[*websocket.Conn]bool
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
	return &Server{cfg: cfg, f: f, seen: seen, conns: map[*websocket.Conn]bool{}}, nil
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
	for {
		var f fslog.Frame
		if err := ws.ReadJSON(&f); err != nil {
			return
		}
		reply := fslog.Frame{Op: fslog.OpReply, ID: f.ID}
		switch f.Op {
		case fslog.OpCreds:
			if len(s.cfg.Allow) == 0 || slices.Contains(s.cfg.Allow, f.FS) {
				creds := s.cfg.Creds
				reply.OK, reply.Creds = true, &creds
				log.Printf("logserver: credentials for %q to %s", f.FS, f.Host)
			} else {
				reply.Error = fmt.Sprintf("filesystem %q is not allowed to mount", f.FS)
				log.Printf("logserver: refused credentials for %q to %s", f.FS, f.Host)
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
		if err := ws.WriteJSON(reply); err != nil {
			return
		}
	}
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
