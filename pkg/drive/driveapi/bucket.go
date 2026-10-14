package driveapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/resurgentech/icbfs/pkg/drive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// sreq is one bucket request, authorized.
type sreq struct {
	c       fiber.Ctx
	ctx     context.Context
	store   pkgicbfs.Store
	actor   string
	profile drive.Profile
}

func (s *server) bucket(permission string, h func(*sreq) error) fiber.Handler {
	return func(c fiber.Ctx) error {
		if s.cfg.AuthorizeBucket == nil || s.cfg.Bucket == nil {
			return writeErr(c, fmt.Errorf("buckets: %w", drive.ErrNotSupported))
		}
		bucket := strings.Clone(c.Params("bucket"))
		actor, p, err := s.cfg.AuthorizeBucket(c, bucket, permission)
		if err != nil {
			return writeErr(c, err)
		}
		store, err := s.cfg.Bucket(c.Context(), bucket)
		if err != nil {
			return writeErr(c, err)
		}
		return h(&sreq{c: c, ctx: c.Context(), store: store, actor: actor, profile: p})
	}
}

func filesystemOut(info pkgicbfs.Info, used *int64) Filesystem {
	mode := FilesystemPrimaryModePosix
	if info.PrimaryWindows {
		mode = FilesystemPrimaryModeWindows
	}
	out := Filesystem{Name: info.Name, Id: &info.ID, PrimaryMode: mode, MaxFileSize: info.MaxFileSize,
		Size: int64(info.Size), Archived: info.Archived, Used: used}
	if !info.Created.IsZero() {
		created := info.Created
		out.Created = &created
	}
	return out
}

func (s *server) listFilesystems(r *sreq) error {
	all, err := pkgicbfs.ListFilesystems(r.ctx, r.store)
	if err != nil {
		return writeErr(r.c, err)
	}
	out := Filesystems{Filesystems: []Filesystem{}}
	for _, info := range all {
		out.Filesystems = append(out.Filesystems, filesystemOut(info, nil))
	}
	return r.c.JSON(out)
}

func (s *server) getFilesystem(r *sreq) error {
	info, used, err := pkgicbfs.DescribeFilesystem(r.ctx, r.store, r.param("name"))
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(filesystemOut(info, &used))
}

// rootFor is a new filesystem's root: the creating admin's profile has
// full control (Windows: and SYSTEM; the SDDL is just the root folder's
// ACL, changeable like any folder's).
func rootFor(p drive.Profile, windows bool) (pkgicbfs.Settings, error) {
	var set pkgicbfs.Settings
	if windows {
		if p.Windows == nil {
			return set, fmt.Errorf("a primary-Windows filesystem's root needs a Windows profile: %w", drive.ErrNoAccess)
		}
		sid := p.Windows.User
		set.RootSDDL = "O:" + sid + "G:" + sid + "D:PAI(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)"
		return set, nil
	}
	if p.POSIX == nil {
		return set, fmt.Errorf("a primary-POSIX filesystem's root needs a POSIX profile: %w", drive.ErrNoAccess)
	}
	set.RootMode, set.RootUid = 0o755, p.POSIX.Uid
	if len(p.POSIX.Gids) > 0 {
		set.RootGid = p.POSIX.Gids[0]
	}
	return set, nil
}

func (s *server) createFilesystem(r *sreq) error {
	var in CreateFilesystemRequest
	if err := (&req{c: r.c}).bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	if in.Name == "" || in.Size <= 0 || in.MaxFileSize <= 0 || in.PrimaryMode != CreateFilesystemRequestPrimaryModeWindows && in.PrimaryMode != CreateFilesystemRequestPrimaryModePosix {
		return writeErr(r.c, fmt.Errorf("name, primaryMode, maxFileSize and size are all required: %w", drive.ErrInvalid))
	}
	windows := in.PrimaryMode == CreateFilesystemRequestPrimaryModeWindows
	set, err := rootFor(r.profile, windows)
	if err != nil {
		return writeErr(r.c, err)
	}
	set.Name, set.Size, set.PrimaryWindows, set.MaxFileSize = in.Name, uint64(in.Size), windows, uint64(in.MaxFileSize)
	info, err := pkgicbfs.CreateFilesystem(r.ctx, r.store, set)
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.Status(http.StatusCreated).JSON(filesystemOut(info, nil))
}

func (s *server) resizeFilesystem(r *sreq) error {
	var in ResizeRequest
	if err := (&req{c: r.c}).bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	if in.Size <= 0 {
		return writeErr(r.c, fmt.Errorf("size: %w", drive.ErrInvalid))
	}
	name := r.param("name")
	if err := pkgicbfs.Resize(r.ctx, r.store, name, uint64(in.Size)); err != nil {
		return writeErr(r.c, err)
	}
	return s.getFilesystem(r)
}

func (s *server) archiveFilesystem(r *sreq) error {
	if err := pkgicbfs.Archive(r.ctx, r.store, r.param("name")); err != nil {
		return writeErr(r.c, err)
	}
	return s.getFilesystem(r)
}

func (s *server) pruneFilesystem(r *sreq) error {
	if err := pkgicbfs.Prune(r.ctx, r.store, r.param("name")); err != nil {
		return writeErr(r.c, err)
	}
	return r.c.SendStatus(http.StatusNoContent)
}
