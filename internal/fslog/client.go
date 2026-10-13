package fslog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// asyncQueue is how many events async logging holds while the daemon is
// unreachable before it starts dropping (and counting) them.
const asyncQueue = 4096

// Client is a mount's connection to the daemon. Requests survive the
// daemon going away: the client redials and resends until it gets an
// answer (events carry their own ID, so a resend that the server already
// logged isn't logged twice).
//
// Log either blocks until the server has written the event (the
// default) or, async, queues it and returns at once — never slowing or
// failing the filesystem operation; while the daemon is unreachable the
// queue fills, then events are dropped and counted, and the count is
// logged (EventDropped) once delivery resumes.
type Client struct {
	socket string
	fs     string
	host   string

	mu      sync.Mutex // guards conn, enc, nextID, pending
	conn    net.Conn
	enc     *json.Encoder
	nextID  uint64
	pending map[uint64]chan Frame

	async   bool
	queue   chan Event
	dropped atomic.Uint64
	drained chan struct{}
}

// NewClient returns a client for the daemon listening on socket, for
// filesystem fs. It connects lazily, on the first request.
func NewClient(socket, fs string, async bool) *Client {
	host, _ := os.Hostname()
	c := &Client{socket: socket, fs: fs, host: host, pending: map[uint64]chan Frame{}, async: async}
	if async {
		c.queue = make(chan Event, asyncQueue)
		c.drained = make(chan struct{})
		go c.drain()
	}
	return c
}

// Credentials asks the server, through the daemon, how to reach this
// filesystem's object store. The mount can't proceed without them: an
// error means the server refused, or no answer came before ctx ended.
func (c *Client) Credentials(ctx context.Context) (Creds, error) {
	reply, err := c.request(ctx, Frame{Op: OpCreds, FS: c.fs, Host: c.host})
	if err != nil {
		return Creds{}, err
	}
	if !reply.OK || reply.Creds == nil {
		return Creds{}, fmt.Errorf("server refused credentials for %q: %s", c.fs, reply.Error)
	}
	return *reply.Creds, nil
}

// Log records op on path (bytes for a write), by the caller uid/gid/pid.
func (c *Client) Log(op, path string, bytes int64, uid, gid, pid uint32) {
	ev := Event{
		ID: uuid.NewString(), Time: time.Now().UTC(), Host: c.host, FS: c.fs,
		Op: op, Path: path, Bytes: bytes, Uid: uid, Gid: gid, Pid: pid,
	}
	if !c.async {
		c.deliver(ev)
		return
	}
	select {
	case c.queue <- ev:
	default:
		c.dropped.Add(1)
	}
}

// Close delivers what async logging still holds (waiting up to wait for
// it) and disconnects.
func (c *Client) Close(wait time.Duration) {
	if c.async {
		close(c.queue)
		select {
		case <-c.drained:
		case <-time.After(wait):
		}
	}
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func (c *Client) drain() {
	defer close(c.drained)
	for ev := range c.queue {
		if n := c.dropped.Swap(0); n > 0 {
			c.deliver(Event{ID: uuid.NewString(), Time: time.Now().UTC(), Host: c.host, FS: c.fs, Op: EventDropped, Bytes: int64(n)})
		}
		c.deliver(ev)
	}
}

// deliver sends ev until the server confirms it's logged.
func (c *Client) deliver(ev Event) {
	for {
		reply, err := c.request(context.Background(), Frame{Op: OpEvent, Event: &ev})
		if err == nil && reply.OK {
			return
		}
		time.Sleep(time.Second) // the server couldn't write it; try again
	}
}

// request sends f and waits for its reply, redialing and resending
// whenever the connection to the daemon breaks, until ctx ends.
func (c *Client) request(ctx context.Context, f Frame) (Frame, error) {
	backoff := 100 * time.Millisecond
	for {
		ch, err := c.send(f)
		if err == nil {
			select {
			case reply, ok := <-ch:
				if ok {
					return reply, nil
				}
				// the connection broke before the reply: resend
			case <-ctx.Done():
				return Frame{}, ctx.Err()
			}
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			if err != nil {
				return Frame{}, fmt.Errorf("daemon at %s: %w", c.socket, err)
			}
			return Frame{}, ctx.Err()
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// send writes f under a fresh id on the current connection (dialing one
// if needed) and returns the channel its reply arrives on; the channel
// is closed without a reply if the connection breaks first.
func (c *Client) send(f Frame) (chan Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		conn, err := net.Dial("unix", c.socket)
		if err != nil {
			return nil, err
		}
		c.conn, c.enc = conn, json.NewEncoder(conn)
		go c.read(conn)
	}
	c.nextID++
	f.ID = c.nextID
	ch := make(chan Frame, 1)
	c.pending[f.ID] = ch
	if err := c.enc.Encode(f); err != nil {
		c.dropConnLocked(c.conn)
		return nil, err
	}
	return ch, nil
}

// read routes replies on conn to their requests until it breaks.
func (c *Client) read(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var f Frame
		if json.Unmarshal(sc.Bytes(), &f) != nil || f.Op != OpReply {
			continue
		}
		c.mu.Lock()
		ch := c.pending[f.ID]
		delete(c.pending, f.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
	c.mu.Lock()
	c.dropConnLocked(conn)
	c.mu.Unlock()
}

// dropConnLocked forgets conn, if it's still current, and fails every
// request waiting on it so they resend.
func (c *Client) dropConnLocked(conn net.Conn) {
	if c.conn != conn {
		return
	}
	conn.Close()
	c.conn, c.enc = nil, nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}
