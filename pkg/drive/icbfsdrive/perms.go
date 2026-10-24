package icbfsdrive

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/resurgentech/icbfs/internal/acl"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// The file-level check, by the filesystem's primary mode: Windows ACLs
// on a primary-Windows filesystem, POSIX modes on a primary-POSIX one.

// what an operation needs of an item.
type need int

const (
	needRead        need = iota // a file's content, a directory's listing
	needWrite                   // a file's content
	needAddFile                 // a directory: create a file in it
	needAddDir                  // a directory: create a directory in it
	needDelete                  // the item itself (with its parent, see canDelete)
	needReadControl             // its permissions
	needChangePerms
)

func profile(ctx context.Context) (drive.Profile, error) {
	p, ok := drive.ProfileFrom(ctx)
	if !ok {
		return drive.Profile{}, drive.ErrNoAccess
	}
	return p, nil
}

// sdOf is the security descriptor that applies to n: its own, or its
// nearest ancestor's (the item inherits it as it stands), and the
// drive-relative path it came from ("" for its own; "(above)" above
// the drive's root).
func sdOf(n *node) (*acl.SecurityDescriptor, string) {
	if sddl, ok := n.attr.WindowsACL(); ok {
		if sd, err := acl.Parse(string(sddl)); err == nil {
			return sd, ""
		}
	}
	for i := len(n.chain) - 1; i >= 0; i-- {
		if sddl, ok := n.chain[i].attr.WindowsACL(); ok {
			if sd, err := acl.Parse(string(sddl)); err == nil {
				from := n.chain[i].path
				if from == "" {
					from = "(above)"
				}
				return sd, from
			}
		}
	}
	return nil, ""
}

func principal(p *drive.WindowsProfile) acl.Principal {
	return acl.Principal{User: p.User, Groups: p.Groups, Domain: p.Domain}
}

func posixUser(p *drive.POSIXProfile) acl.POSIXUser {
	return acl.POSIXUser{Uid: p.Uid, Gids: p.Gids}
}

// allowed reports whether ctx's profile may do what to n.
func (d *Drive) allowed(ctx context.Context, n *node, what need) (bool, error) {
	p, err := profile(ctx)
	if err != nil {
		return false, err
	}
	if p.System {
		return true, nil
	}
	isDir := n.typ == icbfs.TypeDir
	if d.windows {
		if p.Windows == nil {
			return false, drive.ErrNoAccess
		}
		sd, _ := sdOf(n)
		var want uint32
		switch what {
		case needRead:
			want = acl.ReadData
		case needWrite, needAddFile:
			want = acl.WriteData
		case needAddDir:
			want = acl.AppendData
		case needDelete:
			want = acl.Delete
		case needReadControl:
			want = acl.ReadControl
		case needChangePerms:
			want = acl.WriteDAC
		}
		return sd.Allows(principal(p.Windows), want), nil
	}
	if p.POSIX == nil {
		if p.ByMode {
			return allowedByMode(n.attr.Mode, what), nil
		}
		return false, drive.ErrNoAccess
	}
	u := posixUser(p.POSIX)
	a := n.attr
	switch what {
	case needRead:
		bits := acl.PosixRead
		if isDir {
			bits |= acl.PosixExec
		}
		return acl.POSIXAllows(a.Mode, a.Uid, a.Gid, isDir, u, bits), nil
	case needWrite:
		return acl.POSIXAllows(a.Mode, a.Uid, a.Gid, isDir, u, acl.PosixWrite), nil
	case needAddFile, needAddDir:
		return acl.POSIXAllows(a.Mode, a.Uid, a.Gid, isDir, u, acl.PosixWrite|acl.PosixExec), nil
	case needReadControl:
		return true, nil
	case needChangePerms, needDelete:
		return u.Uid == 0 || u.Uid == a.Uid, nil
	}
	return false, nil
}

// allowedByMode is allowed for a ByMode profile: a Windows mount's DACL
// for a primary-POSIX item (winfspserver's approximateDACLFromMode),
// granted to everyone.
func allowedByMode(mode uint32, what need) bool {
	switch what {
	case needRead, needReadControl:
		return mode&0o444 != 0
	case needWrite, needAddFile, needAddDir, needDelete:
		return mode&0o200 != 0
	}
	return false // changing permissions needs an owner, which this has none of
}

// check is allowed, as an error.
func (d *Drive) check(ctx context.Context, n *node, what need) error {
	ok, err := d.allowed(ctx, n, what)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w", n.path, drive.ErrForbidden)
	}
	return nil
}

// checkTraverse: a POSIX directory needs execute to look inside it.
// (Windows grants traversal to everyone by default — bypass traverse
// checking — so there's no check there.)
func (d *Drive) checkTraverse(ctx context.Context, dir *node) error {
	if d.windows {
		return nil
	}
	p, err := profile(ctx)
	if err != nil {
		return err
	}
	if p.System || (p.POSIX == nil && p.ByMode) {
		return nil // a Windows mount has no traverse check either
	}
	if p.POSIX == nil {
		return drive.ErrNoAccess
	}
	a := dir.attr
	if !acl.POSIXAllows(a.Mode, a.Uid, a.Gid, true, posixUser(p.POSIX), acl.PosixExec) {
		return fmt.Errorf("%s: %w", dir.path, drive.ErrForbidden)
	}
	return nil
}

// parentNode is n's parent as a node (for checks on it).
func parentNode(n *node) *node {
	if len(n.chain) == 0 {
		return nil
	}
	a := n.chain[len(n.chain)-1]
	return &node{fsys: n.fsys, key: a.key, typ: icbfs.TypeDir, path: a.path, attr: a.attr, chain: n.chain[:len(n.chain)-1]}
}

// canDelete: Windows allows a delete with DELETE on the item or
// FILE_DELETE_CHILD on its directory; POSIX needs write and execute on
// the directory, and with the sticky bit, owning the item or directory.
func (d *Drive) canDelete(ctx context.Context, n *node) error {
	parent := parentNode(n)
	if parent == nil {
		return fmt.Errorf("the filesystem's root: %w", drive.ErrForbidden)
	}
	p, err := profile(ctx)
	if err != nil {
		return err
	}
	if p.System {
		return nil
	}
	if d.windows {
		if p.Windows == nil {
			return drive.ErrNoAccess
		}
		own, _ := sdOf(n)
		dir, _ := sdOf(parent)
		pr := principal(p.Windows)
		if own.Allows(pr, acl.Delete) || dir.Allows(pr, acl.DeleteChild) {
			return nil
		}
		return fmt.Errorf("%s: %w", n.path, drive.ErrForbidden)
	}
	if p.POSIX == nil {
		if p.ByMode && allowedByMode(parent.attr.Mode, needWrite) {
			return nil
		}
		if p.ByMode {
			return fmt.Errorf("%s: %w", n.path, drive.ErrForbidden)
		}
		return drive.ErrNoAccess
	}
	u := posixUser(p.POSIX)
	pa := parent.attr
	if !acl.POSIXAllows(pa.Mode, pa.Uid, pa.Gid, true, u, acl.PosixWrite|acl.PosixExec) {
		return fmt.Errorf("%s: %w", n.path, drive.ErrForbidden)
	}
	if pa.Mode&0o1000 != 0 && u.Uid != 0 && u.Uid != n.attr.Uid && u.Uid != pa.Uid {
		return fmt.Errorf("%s (sticky directory): %w", n.path, drive.ErrForbidden)
	}
	return nil
}

// rights is what ctx's profile may do to n, for Stat.
func (d *Drive) rights(ctx context.Context, n *node) *drive.Rights {
	r := &drive.Rights{}
	r.Read, _ = d.allowed(ctx, n, needRead)
	if n.typ == icbfs.TypeDir {
		r.Write, _ = d.allowed(ctx, n, needAddFile)
	} else {
		r.Write, _ = d.allowed(ctx, n, needWrite)
	}
	r.Delete = d.canDelete(ctx, n) == nil
	r.ChangePermissions, _ = d.allowed(ctx, n, needChangePerms)
	return r
}

// newOwnership is what a new item in parent gets: on Windows the
// security descriptor inherited from the parent's (owned by the
// profile), on POSIX the profile's uid and gid (the directory's gid
// under setgid) and the default mode.
func (d *Drive) newOwnership(ctx context.Context, parent *node, isDir bool) (sddl []byte, mode, uid, gid uint32) {
	mode = 0o644
	if isDir {
		mode = 0o755
	}
	p, _ := drive.ProfileFrom(ctx)
	if d.windows {
		owner := "SY"
		if p.Windows != nil {
			owner = p.Windows.User
		}
		sd, _ := sdOf(parent)
		group := owner
		if sd != nil && sd.Group != "" {
			group = sd.Group
		}
		return []byte(acl.Inherit(sd, isDir, owner, group).String()), mode, 0, 0
	}
	if p.POSIX != nil {
		uid = p.POSIX.Uid
		if len(p.POSIX.Gids) > 0 {
			gid = p.POSIX.Gids[0]
		}
	} else {
		uid, gid = parent.attr.Uid, parent.attr.Gid
	}
	if parent.attr.Mode&0o2000 != 0 {
		gid = parent.attr.Gid
		if isDir {
			mode |= 0o2000
		}
	}
	return nil, mode, uid, gid
}

// Permissions is an item's own permissions, or on Windows the ones it
// inherits when it has none.
func (d *Drive) Permissions(ctx context.Context, ref drive.ItemRef) (drive.Permissions, error) {
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return drive.Permissions{}, err
	}
	if err := d.check(ctx, n, needReadControl); err != nil {
		return drive.Permissions{}, err
	}
	if d.windows {
		sd, from := sdOf(n)
		out := drive.Permissions{Kind: "windows", Inherited: from}
		if sd != nil {
			out.SDDL, out.Owner, out.Group = sd.String(), sd.Owner, sd.Group
		}
		return out, nil
	}
	return drive.Permissions{Kind: "posix", Owner: "uid:" + strconv.Itoa(int(n.attr.Uid)),
		Group: "gid:" + strconv.Itoa(int(n.attr.Gid)), Mode: n.attr.Mode}, nil
}

// Identities scans the drive for every principal its items name.
func (d *Drive) Identities(ctx context.Context, progress func(int)) ([]drive.Identity, error) {
	root, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return nil, err
	}
	if err := d.check(ctx, root, needReadControl); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	kinds := map[string]string{}
	scanned := 0
	add := func(id, kind string) {
		counts[id]++
		kinds[id] = kind
	}
	var walk func(key string, attr icbfs.Attr) error
	walk = func(key string, attr icbfs.Attr) error {
		scanned++
		if progress != nil && scanned%100 == 0 {
			progress(scanned)
		}
		if d.windows {
			if sddl, ok := attr.WindowsACL(); ok {
				if sd, err := acl.Parse(string(sddl)); err == nil {
					seen := map[string]bool{}
					for _, sid := range append([]string{sd.Owner, sd.Group}, aceSIDs(sd)...) {
						if sid != "" && !seen[sid] {
							seen[sid] = true
							add(sid, "sid")
						}
					}
				}
			}
		} else {
			add("uid:"+strconv.Itoa(int(attr.Uid)), "uid")
			add("gid:"+strconv.Itoa(int(attr.Gid)), "gid")
		}
		entries, err := d.fs.ReadDir(ctx, key)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			a, err := d.fs.Stat(ctx, e.UUID, e.Type)
			if err != nil {
				continue
			}
			if e.Type == icbfs.TypeDir {
				if err := walk(e.UUID, a); err != nil {
					return err
				}
				continue
			}
			scanned++
			if d.windows {
				if sddl, ok := a.WindowsACL(); ok {
					if sd, err := acl.Parse(string(sddl)); err == nil {
						seen := map[string]bool{}
						for _, sid := range append([]string{sd.Owner, sd.Group}, aceSIDs(sd)...) {
							if sid != "" && !seen[sid] {
								seen[sid] = true
								add(sid, "sid")
							}
						}
					}
				}
			} else {
				add("uid:"+strconv.Itoa(int(a.Uid)), "uid")
				add("gid:"+strconv.Itoa(int(a.Gid)), "gid")
			}
		}
		return nil
	}
	if err := walk(root.key, root.attr); err != nil {
		return nil, err
	}
	if progress != nil {
		progress(scanned)
	}
	out := make([]drive.Identity, 0, len(counts))
	for id, n := range counts {
		out = append(out, drive.Identity{ID: id, Kind: kinds[id], Items: n})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Items > out[j].Items || out[i].Items == out[j].Items && out[i].ID < out[j].ID
	})
	return out, nil
}

func aceSIDs(sd *acl.SecurityDescriptor) []string {
	var out []string
	for _, a := range sd.DACL {
		out = append(out, a.SID)
	}
	return out
}
