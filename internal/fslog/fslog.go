// Package fslog is how an icbfs mount reports what happens on it, and
// gets the credentials it mounts with, through a local daemon
// (internal/logd, cmd/icbfs-logd) that relays to a server over one
// websocket: local clients on a Unix socket, the daemon holding the only
// connection to the server.
//
// Both hops carry the same JSON Frames: one per line on the Unix socket,
// one per message on the websocket. Every request has an id, and its
// reply echoes it.
package fslog

import (
	"encoding/json"
	"time"

	"github.com/resurgentech/icbfs/internal/storeconfig"
)

// Frame ops.
const (
	OpCreds = "creds" // driver → server: how to mount Drive (credentials, filesystem, root)
	OpEvent = "event" // driver → server: log Event
	// OpCommand: icbfs-cli → server: run a side command (Command); the
	// daemon sets who is asking (Command.Actor), from the socket's peer.
	OpCommand = "command"
	OpReply   = "reply" // server → driver: the answer to request ID
)

// Frame is one message on either hop.
type Frame struct {
	Op      string   `json:"op"`
	ID      uint64   `json:"id"`
	Drive   string   `json:"drive,omitempty"`   // OpCreds: the drive being mounted (the server's id for it)
	Host    string   `json:"host,omitempty"`    // OpCreds: the machine asking
	Event   *Event   `json:"event,omitempty"`   // OpEvent
	Command *Command `json:"command,omitempty"` // OpCommand

	// OpReply: OK, or Error saying why not; Creds and Mount for an
	// OpCreds, Result for an OpCommand.
	OK     bool            `json:"ok,omitempty"`
	Error  string          `json:"error,omitempty"`
	Creds  *Creds          `json:"creds,omitempty"`
	Mount  *Mount          `json:"mount,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Mount is what a drive is, for a mount: the filesystem it's on, the
// directory in it that is the drive's root ("" or "/" for the whole
// filesystem), and whether this machine may only read it.
type Mount struct {
	Filesystem string `json:"filesystem"`
	Root       string `json:"root,omitempty"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

// Command is a side command (see internal/sidecmd): a name like
// "names create", string arguments, and — set by the daemon, whatever
// the client sent — the actor.
type Command struct {
	Name  string            `json:"name"`
	Args  map[string]string `json:"args,omitempty"`
	Actor string            `json:"actor,omitempty"`
}

// Event kinds.
const (
	EventRead    = "read"    // a file opened for reading
	EventWrite   = "write"   // a file closed after writing; Bytes is its size
	EventDelete  = "delete"  // a file or directory removed
	EventList    = "ls"      // a directory listed
	EventDropped = "dropped" // async logging dropped Bytes events while the daemon was unreachable
	EventCommand = "command" // a side command was run; Path is its name, Actor who

)

// Event is one logged filesystem action.
type Event struct {
	ID    string    `json:"id"` // unique; the server ignores a repeat (a resend after a reconnect)
	Time  time.Time `json:"time"`
	Host  string    `json:"host"`
	Drive string    `json:"drive,omitempty"` // the drive mounted; Path is from its root
	FS    string    `json:"fs"`
	Op    string    `json:"op"`
	Path  string    `json:"path"`
	Bytes int64     `json:"bytes,omitempty"`
	Uid   uint32    `json:"uid"`
	Gid   uint32    `json:"gid"`
	Pid   uint32    `json:"pid"`
	Actor string    `json:"actor,omitempty"` // EventCommand
	Error string    `json:"error,omitempty"` // EventCommand: it failed
}

// Creds is how to reach a filesystem's object store: which kind, and that
// kind's section ({"kind": "s3", "s3": {...}}).
type Creds = storeconfig.Config
