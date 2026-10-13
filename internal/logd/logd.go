// Package logd is the local daemon between icbfs mounts and the logging
// server (cmd/icbfs-logd): the same shape as host's hostd, with
// only what this needs. Mounts on the machine connect to its Unix
// socket; it holds the machine's one websocket to the server,
// authenticated with a shared secret, and relays fslog Frames both ways
// — credential requests and log events up, replies down.
//
// Requests outlive the websocket: one not yet answered when the
// connection drops is resent, in order, once it's back (the server
// skips events it already logged). A mount's own disconnect doesn't
// cancel what it already sent.
package logd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/resurgentech/icbfs/internal/fslog"
)

// Config is where the daemon listens and what it connects to.
type Config struct {
	Socket string // the Unix socket mounts connect to
	Server string // the server's websocket URL, e.g. ws://host:8090/ws/daemon
	Secret string // the bearer secret the server expects
}

// Daemon relays between local mounts and the server.
type Daemon struct {
	cfg Config
	ln  net.Listener

	mu      sync.Mutex // guards everything below
	clients map[*client]bool
	ws      *websocket.Conn
	nextID  uint64
	pending map[uint64]*relay // by upstream id
	order   []uint64          // upstream ids in send order, for resending
}

// relay is one mount request on its way to the server.
type relay struct {
	frame    fslog.Frame // as sent upstream (its ID is the daemon's)
	client   *client
	clientID uint64
}

// client is one mount's connection.
type client struct {
	conn net.Conn
	mu   sync.Mutex
	enc  *json.Encoder
}

func (c *client) reply(f fslog.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.enc.Encode(f) // a mount that went away just misses its reply
}

// New starts listening on cfg.Socket (replacing a stale socket file).
func New(cfg Config) (*Daemon, error) {
	if err := os.Remove(cfg.Socket); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, err
	}
	return &Daemon{cfg: cfg, ln: ln, pending: map[uint64]*relay{}, clients: map[*client]bool{}}, nil
}

// Run serves mounts and keeps the server connection up until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	go d.serverLoop(ctx)
	go func() {
		<-ctx.Done()
		d.ln.Close()
		d.mu.Lock()
		if d.ws != nil {
			d.ws.Close()
		}
		// As if the process had exited: mounts see their connection
		// close, and resend to the next daemon.
		for c := range d.clients {
			c.conn.Close()
		}
		d.mu.Unlock()
	}()
	defer os.Remove(d.cfg.Socket)
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serveClient(conn)
	}
}

// serveClient reads one mount's requests and queues them upstream.
func (d *Daemon) serveClient(conn net.Conn) {
	c := &client{conn: conn, enc: json.NewEncoder(conn)}
	d.mu.Lock()
	d.clients[c] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.clients, c)
		d.mu.Unlock()
		conn.Close()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var f fslog.Frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if f.Op != fslog.OpCreds && f.Op != fslog.OpEvent {
			c.reply(fslog.Frame{Op: fslog.OpReply, ID: f.ID, Error: "unknown op " + f.Op})
			continue
		}
		d.mu.Lock()
		d.nextID++
		r := &relay{frame: f, client: c, clientID: f.ID}
		r.frame.ID = d.nextID
		d.pending[r.frame.ID] = r
		d.order = append(d.order, r.frame.ID)
		ws := d.ws
		if ws != nil {
			d.writeLocked(ws, r.frame)
		}
		d.mu.Unlock()
	}
}

// writeLocked sends f on ws; a failed write closes ws, and the read loop
// then reconnects and resends.
func (d *Daemon) writeLocked(ws *websocket.Conn, f fslog.Frame) {
	if err := ws.WriteJSON(f); err != nil {
		ws.Close()
	}
}

// serverLoop keeps one websocket to the server, resending everything
// unanswered on each connect and routing replies to their mounts.
func (d *Daemon) serverLoop(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, d.cfg.Server, http.Header{"Authorization": {"Bearer " + d.cfg.Secret}})
		if err != nil {
			log.Printf("logd: connect %s: %v", d.cfg.Server, err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		backoff = 250 * time.Millisecond
		log.Printf("logd: connected to %s", d.cfg.Server)

		d.mu.Lock()
		d.ws = ws
		for _, id := range d.order {
			if r := d.pending[id]; r != nil {
				d.writeLocked(ws, r.frame)
			}
		}
		d.mu.Unlock()

		err = d.readReplies(ws)
		d.mu.Lock()
		d.ws = nil
		d.mu.Unlock()
		ws.Close()
		if ctx.Err() == nil {
			log.Printf("logd: lost %s: %v", d.cfg.Server, err)
		}
	}
}

func (d *Daemon) readReplies(ws *websocket.Conn) error {
	for {
		var f fslog.Frame
		if err := ws.ReadJSON(&f); err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				continue
			}
			return err
		}
		if f.Op != fslog.OpReply {
			continue
		}
		d.mu.Lock()
		r := d.pending[f.ID]
		delete(d.pending, f.ID)
		d.compactLocked()
		d.mu.Unlock()
		if r != nil {
			f.ID = r.clientID
			r.client.reply(f)
		}
	}
}

// compactLocked drops answered ids from the front of the resend order.
func (d *Daemon) compactLocked() {
	i := 0
	for i < len(d.order) && d.pending[d.order[i]] == nil {
		i++
	}
	d.order = d.order[i:]
	if len(d.order) > 1024 && len(d.pending) < len(d.order)/2 {
		live := d.order[:0]
		for _, id := range d.order {
			if d.pending[id] != nil {
				live = append(live, id)
			}
		}
		d.order = live
	}
}
