//go:build windows

// Package winfspserver's Windows-only files wire github.com/winfsp/
// go-winfsp's native Behaviour* interfaces directly to icbfs.Filesystem
// (via resolve.go's portable resolvePath) — see ROADMAP.md's Part F,
// task F3, and F1's entry for why the native interfaces, not the
// higher-level gofs wrapper, are the right layer for this driver.
package winfspserver

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// driver implements winfsp.BehaviourBase plus every optional Behaviour*
// interface this task wires. One driver backs one mounted
// icbfs.Filesystem; Root builds it.
type driver struct {
	fsys            *icbfs.Filesystem
	caseInsensitive bool

	mu      sync.Mutex
	handles map[uintptr]*fileHandle
	nextID  uintptr
}

// fileHandle is the file context WinFsp's Open/Create hand back a
// uintptr for and every later call (Read, Write, Close, GetFileInfo,
// ...) receives verbatim — this driver's own bookkeeping, opaque to
// WinFsp itself, exactly like internal/fuseserver's FileHandle plays
// the same role for go-fuse.
//
// open is non-nil only once this handle has actually been read from or
// written to (lazily opened via ensureOpen) — most handles (directory
// listings, a plain GetFileInfo-only open) never need icbfs.OpenFile's
// buffering/CAS-retry machinery at all.
type fileHandle struct {
	key string
	typ icbfs.EntryType

	mu      sync.Mutex
	open    *icbfs.OpenFile
	hasSeed bool
	etag    string // seeded from Create, used to avoid a redundant read-back on first ensureOpen
	seeded  []byte // seeded content from Create, paired with etag

	dirBuf winfsp.DirBuffer // directories only; zero value until first use
}

// Root builds the winfsp.BehaviourBase for fsys, ready to pass to
// winfsp.Mount. fsys must already be bootstrapped (see icbfs.Filesystem.
// Bootstrap) — this driver does no bootstrapping of its own, same
// division of responsibility as internal/fuseserver.Root.
//
// caseInsensitive (task F4) must match whatever the caller also passes
// to winfsp.Mount's own winfsp.CaseSensitive option — the two are
// separate, both load-bearing settings, not one flag with two names:
// WinFsp's own CaseSensitive controls whether the *kernel* folds case
// before ever calling this driver (confirmed in task F3: with it off,
// the kernel can hand this driver a canonicalized, differently-cased
// name than what's actually stored), while this field controls whether
// resolvePath's own lookupCaseAware fallback additionally tolerates a
// case mismatch against icbfs.Filesystem's always-case-sensitive
// storage. Mismatching the two (e.g. kernel case-sensitive, driver
// case-insensitive) leaves this fallback dead code; the other way
// around leaves names the kernel already case-folded unable to match
// at all.
func Root(fsys *icbfs.Filesystem, caseInsensitive bool) winfsp.BehaviourBase {
	return &driver{fsys: fsys, caseInsensitive: caseInsensitive, handles: make(map[uintptr]*fileHandle)}
}

var (
	_ winfsp.BehaviourBase                  = (*driver)(nil)
	_ winfsp.BehaviourCreate                = (*driver)(nil)
	_ winfsp.BehaviourOverwrite             = (*driver)(nil)
	_ winfsp.BehaviourCleanup               = (*driver)(nil)
	_ winfsp.BehaviourGetFileInfo           = (*driver)(nil)
	_ winfsp.BehaviourSetBasicInfo          = (*driver)(nil)
	_ winfsp.BehaviourSetFileSize           = (*driver)(nil)
	_ winfsp.BehaviourCanDelete             = (*driver)(nil)
	_ winfsp.BehaviourRead                  = (*driver)(nil)
	_ winfsp.BehaviourWrite                 = (*driver)(nil)
	_ winfsp.BehaviourFlush                 = (*driver)(nil)
	_ winfsp.BehaviourReadDirectory         = (*driver)(nil)
	_ winfsp.BehaviourGetSecurityByName     = (*driver)(nil)
	_ winfsp.BehaviourGetSecurity           = (*driver)(nil)
	_ winfsp.BehaviourGetVolumeInfo         = (*driver)(nil)
	_ winfsp.BehaviourGetReparsePoint       = (*driver)(nil)
	_ winfsp.BehaviourGetReparsePointByName = (*driver)(nil)
	_ winfsp.BehaviourSetReparsePoint       = (*driver)(nil)
)

// ctx is a placeholder for every icbfs.Filesystem call this driver
// makes: go-winfsp's native interface, unlike go-fuse's, passes no
// per-request context.Context at all (a synchronous, C-callback-driven
// dispatch model) — there is nothing to derive a real one from. Same
// choice gofs itself makes internally.
func ctx() context.Context { return context.Background() }

// toWinError maps icbfs's own error sentinels onto syscall.Errno/
// os.Err* values go-winfsp's convertNTStatus already knows how to turn
// into the right NTSTATUS (confirmed by reading it directly, not
// guessed) — anything unrecognized passes through unchanged and
// becomes a generic STATUS_INTERNAL_ERROR, the same fallback every
// other unmapped error gets.
func toWinError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, icbfs.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, icbfs.ErrExists):
		return syscall.EEXIST
	case errors.Is(err, icbfs.ErrNotEmpty):
		return syscall.ERROR_DIR_NOT_EMPTY
	case errors.Is(err, icbfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, icbfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, icbfs.ErrArchived):
		// No exact NTSTATUS for "this whole volume is read-only" is
		// wired through convertNTStatus's map; STATUS_ACCESS_DENIED
		// (via os.ErrPermission) is the closest real fit, same as
		// internal/fuseserver's errnoFromErr maps it to EROFS on the
		// POSIX side — a different errno, same underlying intent.
		return os.ErrPermission
	default:
		return err
	}
}

func (d *driver) newHandle(h *fileHandle) uintptr {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := d.nextID
	d.handles[id] = h
	return id
}

func (d *driver) handle(file uintptr) (*fileHandle, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.handles[file]
	return h, ok
}

func (d *driver) dropHandle(file uintptr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.handles, file)
}

// fileOpenReparsePoint is NTCreateFile's FILE_OPEN_REPARSE_POINT
// CreateOptions bit: when set, a caller wants the reparse point itself
// (e.g. to delete it, or to read its raw target), not whatever it
// points to.
const fileOpenReparsePoint = 0x00200000

// Open resolves name (a WinFsp path) and hands back a fresh handle —
// no icbfs.OpenFile is created yet (see fileHandle's doc comment);
// that happens lazily on first real Read/Write.
//
// Follows a symlink at the end of the path (resolvePathFollow, not
// plain resolvePath) unless the caller explicitly asked for the
// reparse point itself. Needed for real symlink read-through, not
// just create/list: confirmed empirically that the kernel transparently
// re-resolves an *absolute* symlink target on its own, without ever
// calling this Open again for the target, but does not do the
// equivalent relative-to-the-link's-own-directory resolution for a
// *relative* target — this follow call is what makes that second case
// actually work, not just the first. See resolvePathFollow's own doc
// comment and ROADMAP.md's F3 entry.
func (d *driver) Open(fs *winfsp.FileSystemRef, name string, createOptions, grantedAccess uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	resolve := resolvePathFollow
	if createOptions&fileOpenReparsePoint != 0 {
		resolve = resolvePath
	}
	r, err := resolve(ctx(), d.fsys, name, d.caseInsensitive)
	if err != nil {
		return 0, toWinError(err)
	}
	fillFileInfo(info, r.Type, r.Attr)
	h := &fileHandle{key: r.Key, typ: r.Type}
	return d.newHandle(h), nil
}

func (d *driver) Close(fs *winfsp.FileSystemRef, file uintptr) {
	h, ok := d.handle(file)
	d.dropHandle(file)
	if !ok {
		return
	}
	h.mu.Lock()
	open := h.open
	h.mu.Unlock()
	if open != nil {
		// Best-effort: a close that can't flush has nowhere left to
		// report the error to (BehaviourBase.Close returns nothing) —
		// same "the lease/next real write is the backstop" reasoning
		// this project already accepts elsewhere for best-effort
		// cleanup paths.
		_, _ = open.Flush(ctx())
	}
}

// Create makes a new file or directory. WinFsp's own dispatcher
// (fsop.c's FspFileSystemOpCreate) refuses every create/open outright
// with STATUS_INVALID_DEVICE_REQUEST unless Create (or CreateEx),
// Open, and Overwrite (or OverwriteEx) are ALL wired, regardless of
// what a given request actually needs — confirmed directly against
// WinFsp's own C source after this exact failure mode cost real time
// in task F1 (see ASSUMPTIONS.md/ROADMAP.md's F1 entry). Create itself
// distinguishes a directory request via createOptions' FILE_DIRECTORY_
// FILE bit.
func (d *driver) Create(fs *winfsp.FileSystemRef, name string, createOptions, grantedAccess, fileAttributes uint32, securityDescriptor *windows.SECURITY_DESCRIPTOR, allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir, d.caseInsensitive)
	if err != nil {
		return 0, toWinError(err)
	}
	uid, gid := callerOwner()

	const fileDirectoryFile = 0x00000001 // FILE_DIRECTORY_FILE, low byte of NtCreateFile's CreateOptions
	if createOptions&fileDirectoryFile != 0 {
		newUUID, attr, err := d.fsys.Mkdir(ctx(), parent.Key, base, 0755, uid, gid)
		if err != nil {
			return 0, toWinError(err)
		}
		fillFileInfo(info, icbfs.TypeDir, attr)
		return d.newHandle(&fileHandle{key: newUUID, typ: icbfs.TypeDir}), nil
	}

	mode := modeFromAttributes(fileAttributes)
	newUUID, attr, etag, err := d.fsys.Create(ctx(), parent.Key, base, mode, uid, gid)
	if err != nil {
		return 0, toWinError(err)
	}
	fillFileInfo(info, icbfs.TypeFile, attr)
	// seeded empty, not nil: a just-created file's content really is
	// zero bytes, distinct from "not seeded yet" — ensureOpen below
	// tells the two apart via hasSeed, not a nil check on this slice.
	return d.newHandle(&fileHandle{key: newUUID, typ: icbfs.TypeFile, etag: etag, seeded: []byte{}, hasSeed: true}), nil
}

// Overwrite truncates an existing file back to empty, same as Windows'
// CREATE_ALWAYS/TRUNCATE_EXISTING disposition against an existing
// target. Wired purely because Create's doc comment's FspFileSystemOpCreate
// requirement demands it exist at all; this driver has no separate
// "replace attributes" behavior beyond what SetBasicInfo already does.
func (d *driver) Overwrite(fs *winfsp.FileSystemRef, file uintptr, attributes uint32, replaceAttributes bool, allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	var zero int64
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, nil, nil, nil, &zero)
	if err != nil {
		return toWinError(err)
	}
	h.mu.Lock()
	h.open = nil // drop any stale buffered OpenFile; next I/O reopens against the now-truncated content
	h.mu.Unlock()
	fillFileInfo(info, h.typ, attr)
	return nil
}

// Cleanup performs the deferred delete a prior CanDelete allowed, per
// WinFsp's own delete protocol (confirmed against gofs's own Cleanup,
// the one tested reference implementation available — see F1's entry):
// a real delete only happens here, when FspCleanupDelete is set on the
// last handle's cleanup, not at CanDelete time itself.
func (d *driver) Cleanup(fs *winfsp.FileSystemRef, file uintptr, name string, cleanupFlags uint32) {
	if cleanupFlags&winfsp.FspCleanupDelete == 0 {
		return
	}
	h, ok := d.handle(file)
	if !ok {
		return
	}
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir, d.caseInsensitive)
	if err != nil {
		return
	}
	if h.typ == icbfs.TypeDir {
		_ = d.fsys.Rmdir(ctx(), parent.Key, base)
	} else {
		_ = d.fsys.Unlink(ctx(), parent.Key, base)
	}
}

// CanDelete reports whether file could be deleted, checked by WinFsp
// before it commits to a pending delete (the real delete happens later,
// in Cleanup) — the one real check: a non-empty directory can't be
// removed, matching rmdir(2)/icbfs.Filesystem.Rmdir's own rule.
func (d *driver) CanDelete(fs *winfsp.FileSystemRef, file uintptr, name string) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	if h.typ != icbfs.TypeDir {
		return nil
	}
	entries, err := d.fsys.ReadDir(ctx(), h.key)
	if err != nil {
		return toWinError(err)
	}
	if len(entries) > 0 {
		return syscall.ERROR_DIR_NOT_EMPTY
	}
	return nil
}

// ensureOpen lazily creates h's icbfs.OpenFile on first real I/O —
// most handles (a bare GetFileInfo, a directory listing) never touch
// this at all.
func (h *fileHandle) ensureOpen(fsys *icbfs.Filesystem) (*icbfs.OpenFile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open != nil {
		return h.open, nil
	}
	if h.hasSeed {
		h.open = fsys.NewOpenFile(h.key, h.etag, h.seeded)
		h.hasSeed = false
		return h.open, nil
	}
	open, err := fsys.Open(ctx(), h.key)
	if err != nil {
		return nil, err
	}
	h.open = open
	return open, nil
}

// callerOwner returns the uid/gid new entries this driver creates get.
// Real per-caller identity mapping (SID -> uid/gid) is F8's job, same
// as the rest of this driver's ACL story — placeholder root ownership
// here is consistent with this task's own scope (F3 is core operation
// wiring, not ACL design).
func callerOwner() (uid, gid uint32) { return 0, 0 }
