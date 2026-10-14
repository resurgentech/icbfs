// Package sidecmd runs icbfs-cli's side commands, what isn't a filesystem
// operation. Two kinds:
//
//   - storage administration (fs create, list, describe, resize, archive,
//     prune), on the filesystems of a bucket;
//   - commands on a drive (names create, list, delete; trash list,
//     restore), through the drive library, so a drive on a directory sees
//     only its own names and trash.
//
// icbfs-cli runs them itself when it has the store's credentials (a drive
// is then a filesystem, or a directory in one); on a machine with the
// daemon it sends them through it (icbfs-logd), and the server runs them
// here, as the actor the daemon vouches for, on the drive its registry
// names. Arguments are strings, results JSON.
package sidecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// Request is one command.
type Request struct {
	Name  string            `json:"name"` // "names create", "fs list", ...
	Args  map[string]string `json:"args,omitempty"`
	Actor string            `json:"actor,omitempty"` // who; set by the daemon, not the caller
}

// Described is fs describe's result.
type Described struct {
	icbfs.Info
	Used uint64 `json:"used"`
}

// Storage commands act on a bucket's filesystems; drive commands on a
// drive.
var (
	StorageCommands = []string{"fs create", "fs list", "fs describe", "fs resize", "fs archive", "fs prune"}
	DriveCommands   = []string{"names create", "names list", "names delete", "trash list", "trash restore"}
)

// IsDriveCommand reports whether name acts on a drive.
func IsDriveCommand(name string) bool {
	for _, n := range DriveCommands {
		if n == name {
			return true
		}
	}
	return false
}

// Env is what commands run against: the bucket's store, and how to get
// the drive a drive command names (from its arguments).
type Env struct {
	Store objstore.Store
	Drive func(ctx context.Context, args map[string]string) (drive.Drive, error)
}

// Run runs r.
func Run(ctx context.Context, env Env, r Request) (json.RawMessage, error) {
	var out any
	var err error
	if IsDriveCommand(r.Name) {
		if env.Drive == nil {
			return nil, fmt.Errorf("%s: no drives here", r.Name)
		}
		d, derr := env.Drive(ctx, r.Args)
		if derr != nil {
			return nil, derr
		}
		// The drive library checks each item's permissions as a profile;
		// these commands come from the CLI or the daemon, already
		// allowed, so they run as the system, recorded as the actor.
		dctx := drive.WithProfile(drive.WithActor(ctx, r.Actor), drive.Profile{System: true})
		out, err = runOnDrive(dctx, d, r)
	} else {
		out, err = runStorage(ctx, env.Store, r)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func runStorage(ctx context.Context, store objstore.Store, r Request) (any, error) {
	a := args(r.Args)
	switch r.Name {
	case "fs create":
		var s icbfs.Settings
		var err error
		s.Name = a.str("fs")
		s.PrimaryWindows = a.bool("primary-windows")
		var mode, uid, gid uint64
		for _, f := range []struct {
			k   string
			def uint64
			to  *uint64
		}{{"size", 100 << 30, &s.Size}, {"max-file-size", 0, &s.MaxFileSize}, {"root-mode", 0o755, &mode}, {"root-uid", 0, &uid}, {"root-gid", 0, &gid}} {
			if *f.to, err = a.uint(f.k, f.def); err != nil {
				return nil, err
			}
		}
		s.RootMode, s.RootUid, s.RootGid, s.RootSDDL = uint32(mode), uint32(uid), uint32(gid), a.str("root-sddl")
		return icbfs.CreateFilesystem(ctx, store, s)
	case "fs list":
		all, err := icbfs.ListFilesystems(ctx, store)
		if all == nil {
			all = []icbfs.Info{}
		}
		return all, err
	case "fs describe":
		info, err := icbfs.DescribeFilesystem(ctx, store, a.str("fs"))
		if err != nil {
			return nil, err
		}
		f := icbfs.New(store, info.Name)
		if err := f.Attach(ctx); err != nil {
			return nil, err
		}
		_, used, err := f.StatFS(ctx)
		return Described{info, used}, err
	case "fs resize":
		size, err := a.uint("size", 0)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, errors.New("size is required")
		}
		return struct{}{}, icbfs.Resize(ctx, store, a.str("fs"), size)
	case "fs archive":
		return struct{}{}, icbfs.Archive(ctx, store, a.str("fs"))
	case "fs prune":
		info, err := icbfs.DescribeFilesystem(ctx, store, a.str("fs"))
		if err != nil {
			return nil, err
		}
		if !info.Archived {
			return nil, fmt.Errorf("filesystem %q isn't archived: archive it first (fs archive), then prune", info.Name)
		}
		return struct{}{}, icbfs.Prune(ctx, store, info.Name)
	}
	return nil, fmt.Errorf("unknown command %q", r.Name)
}

func runOnDrive(ctx context.Context, d drive.Drive, r Request) (any, error) {
	a := args(r.Args)
	switch r.Name {
	case "names create":
		var at time.Time
		if s := a.str("at"); s != "" {
			var err error
			if at, err = time.Parse(time.RFC3339Nano, s); err != nil {
				return nil, fmt.Errorf("at: %w", err)
			}
		}
		return d.CreateName(ctx, a.str("name"), at, a.bool("filesystem-wide"))
	case "names list":
		return d.Names(ctx)
	case "names delete":
		return struct{}{}, d.DeleteName(ctx, a.str("name"))
	case "trash list":
		limit, err := a.uint("limit", 0)
		if err != nil {
			return nil, err
		}
		return d.Trash(ctx, drive.TrashOptions{Path: a.str("path"), Limit: int(limit), Cursor: a.str("cursor")})
	case "trash restore":
		return d.Restore(ctx, a.str("id"), drive.RestoreOptions{To: a.str("to"), Parents: a.bool("parents"), Replace: a.bool("replace")})
	}
	return nil, fmt.Errorf("unknown command %q", r.Name)
}

type args map[string]string

func (a args) str(k string) string { return a[k] }

func (a args) bool(k string) bool {
	v, _ := strconv.ParseBool(a[k])
	return v
}

func (a args) uint(k string, def uint64) (uint64, error) {
	s, ok := a[k]
	if !ok || s == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return v, nil
}
