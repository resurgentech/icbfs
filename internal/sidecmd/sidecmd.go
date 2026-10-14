// Package sidecmd runs icbfs-cli's side commands — what isn't a
// filesystem operation: filesystems (fs), named versions (names), the
// trash — against a store. icbfs-cli runs them itself when it has the
// store's credentials; on an host machine it sends them through the
// daemon (icbfs-logd), and the server runs them here, as the actor the
// daemon vouches for. Arguments are strings, results JSON.
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

// Names is every command, for the server's permission check.
var Names = []string{"fs create", "fs list", "fs describe", "fs resize", "fs archive", "fs prune",
	"names create", "names list", "names delete", "trash list", "trash restore"}

// Run runs r against store.
func Run(ctx context.Context, store objstore.Store, r Request) (json.RawMessage, error) {
	a := args(r.Args)
	var out any
	var err error
	switch r.Name {
	case "fs create":
		var s icbfs.Settings
		s.Name = a.str("fs")
		s.PrimaryWindows = a.bool("primary-windows")
		s.Size, err = a.uint("size", 100<<30)
		if err == nil {
			s.MaxFileSize, err = a.uint("max-file-size", 0)
		}
		var mode, uid, gid uint64
		if err == nil {
			mode, err = a.uint("root-mode", 0o755)
		}
		if err == nil {
			uid, err = a.uint("root-uid", 0)
		}
		if err == nil {
			gid, err = a.uint("root-gid", 0)
		}
		if err != nil {
			return nil, err
		}
		s.RootMode, s.RootUid, s.RootGid, s.RootSDDL = uint32(mode), uint32(uid), uint32(gid), a.str("root-sddl")
		out, err = icbfs.CreateFilesystem(ctx, store, s)
	case "fs list":
		var all []icbfs.Info
		all, err = icbfs.ListFilesystems(ctx, store)
		if all == nil {
			all = []icbfs.Info{}
		}
		out = all
	case "fs describe":
		var info icbfs.Info
		info, err = icbfs.DescribeFilesystem(ctx, store, a.str("fs"))
		if err != nil {
			break
		}
		f := icbfs.New(store, info.Name)
		if err = f.Attach(ctx); err != nil {
			break
		}
		var used uint64
		_, used, err = f.StatFS(ctx)
		out = Described{info, used}
	case "fs resize":
		var size uint64
		if size, err = a.uint("size", 0); err == nil {
			if size == 0 {
				return nil, errors.New("size is required")
			}
			err = icbfs.Resize(ctx, store, a.str("fs"), size)
		}
		out = struct{}{}
	case "fs archive":
		err = icbfs.Archive(ctx, store, a.str("fs"))
		out = struct{}{}
	case "fs prune":
		var info icbfs.Info
		if info, err = icbfs.DescribeFilesystem(ctx, store, a.str("fs")); err == nil {
			if !info.Archived {
				return nil, fmt.Errorf("filesystem %q isn't archived: archive it first (fs archive), then prune", info.Name)
			}
			err = icbfs.Prune(ctx, store, info.Name)
		}
		out = struct{}{}
	case "names create":
		var at time.Time
		if s := a.str("at"); s != "" {
			if at, err = time.Parse(time.RFC3339Nano, s); err != nil {
				return nil, fmt.Errorf("at: %w", err)
			}
		}
		out, err = icbfs.CreateName(ctx, store, a.str("fs"), icbfs.NamedVersion{Name: a.str("name"), Scope: a.str("scope"), CreatedBy: r.Actor}, at)
	case "names list":
		var names []icbfs.NamedVersion
		names, err = icbfs.ListNames(ctx, store, a.str("fs"))
		if names == nil {
			names = []icbfs.NamedVersion{}
		}
		out = names
	case "names delete":
		err = icbfs.DeleteName(ctx, store, a.str("fs"), a.str("name"))
		out = struct{}{}
	case "trash list":
		f := icbfs.New(store, a.str("fs"))
		if err = f.Attach(ctx); err != nil {
			break
		}
		var limit uint64
		if limit, err = a.uint("limit", 0); err != nil {
			break
		}
		var entries []icbfs.TrashEntry
		entries, err = f.ListTrash(ctx, icbfs.TrashOptions{PathPrefix: a.str("path"), Limit: int(limit)})
		if entries == nil {
			entries = []icbfs.TrashEntry{}
		}
		out = entries
	case "trash restore":
		f := icbfs.New(store, a.str("fs"))
		if err = f.Attach(ctx); err != nil {
			break
		}
		rctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: r.Actor})
		out, err = f.Restore(rctx, a.str("id"), icbfs.RestoreOptions{ToName: a.str("as"), Replace: a.bool("replace")})
	default:
		return nil, fmt.Errorf("unknown command %q", r.Name)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
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
