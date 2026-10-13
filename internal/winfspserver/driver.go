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

	openMu sync.Mutex
	opens  map[string]*sharedOpen // by key; see sharedOpen

	// Per-file allocation size (FILE_ALLOCATION_INFORMATION, or a
	// create's AllocationSize), kept only while the file has open
	// handles — as on NTFS, unused allocation is released at last
	// close (winfsp-tests' create_allocation_test). Guarded by mu.
	keyOpens map[string]int
	allocs   map[string]uint64
}

// sharedOpen is the one icbfs.OpenFile every handle on the same file
// within this mount reads and writes through, reference-counted by
// handle. Windows keeps one cache/FCB per file, so all handles on it
// must see each other's writes immediately — with a private OpenFile
// per handle, a non-cached read through one handle missed data a
// cached handle had just written (winfsp-tests' rdwr_mixed_test).
// icbfs.OpenFile's per-session isolation still applies across mounts
// and clients, which is what its CAS-retry logic is about.
type sharedOpen struct {
	key  string
	refs int // guarded by driver.openMu

	mu   sync.Mutex
	open *icbfs.OpenFile // nil until first real I/O, or after invalidate
}

// fileHandle is the file context WinFsp's Open/Create hand back a
// uintptr for and every later call (Read, Write, Close, GetFileInfo,
// ...) receives verbatim — this driver's own bookkeeping, opaque to
// WinFsp itself, exactly like internal/fuseserver's FileHandle plays
// the same role for go-fuse.
//
// shared is non-nil only once this handle has actually been read from
// or written to (lazily attached via openFile) — most handles
// (directory listings, a plain GetFileInfo-only open) never need
// icbfs.OpenFile's buffering/CAS-retry machinery at all.
type fileHandle struct {
	key  string
	typ  icbfs.EntryType
	path string // as opened; may be stale after a rename — only used for ReadDirectory's ".."

	// A named-stream handle (stream.go): key/typ are the stream's own
	// object; the owner's key/type carry its metadata.
	stream   string
	ownerKey string
	ownerTyp icbfs.EntryType

	mu       sync.Mutex
	shared   *sharedOpen
	allocKey string // key counted in driver.keyOpens; fixed at newHandle, unlike key
	hasSeed  bool
	etag     string // seeded from Create, used to avoid a redundant read-back on first open
	seeded   []byte // seeded content from Create, paired with etag

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
	return &driver{fsys: fsys, caseInsensitive: caseInsensitive, handles: make(map[uintptr]*fileHandle), opens: make(map[string]*sharedOpen),
		keyOpens: make(map[string]int), allocs: make(map[string]uint64)}
}

var (
	_ winfsp.BehaviourBase                  = (*driver)(nil)
	_ winfsp.BehaviourCreate                = (*driver)(nil)
	_ winfsp.BehaviourRename                = (*driver)(nil)
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
	_ winfsp.BehaviourSetSecurity           = (*driver)(nil)
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
	case errors.Is(err, icbfs.ErrInvalidName):
		// convertNTStatus has no sentinel for this; return the real
		// NTSTATUS directly rather than stretching an os.Err*/
		// syscall.Errno value to fit (task F6).
		return windows.STATUS_OBJECT_NAME_INVALID
	case errors.Is(err, icbfs.ErrNameTooLong):
		// What NTFS answers for a component over 255 characters. WinFsp
		// refuses those itself before they reach this driver (its
		// MaxComponentLength, 255 by default); this covers anything else.
		return windows.STATUS_OBJECT_NAME_INVALID
	case errors.Is(err, icbfs.ErrInvalidRename):
		// Moving a directory into one of its own descendants (task
		// F9) — no more specific NTSTATUS confirmed for this exact
		// condition; STATUS_INVALID_PARAMETER is the closest real fit.
		return windows.STATUS_INVALID_PARAMETER
	case errors.Is(err, icbfs.ErrInvalidArgument):
		// A negative SetFileSize request — found as a real crash
		// (make([]byte, negative) panics) via pjdfstest on the FUSE
		// side; mapped here too since icbfs.Filesystem.SetAttr is
		// shared by both access layers.
		return windows.STATUS_INVALID_PARAMETER
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
	h.allocKey = h.key
	d.keyOpens[h.key]++
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
	h, ok := d.handles[file]
	if !ok {
		return
	}
	delete(d.handles, file)
	if d.keyOpens[h.allocKey]--; d.keyOpens[h.allocKey] <= 0 {
		delete(d.keyOpens, h.allocKey)
		delete(d.allocs, h.allocKey)
	}
}

// setAlloc records key's allocation size while it has open handles.
func (d *driver) setAlloc(key string, size uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keyOpens[key] > 0 {
		d.allocs[key] = size
	}
}

// allocFor is key's reported allocation size: at least its data size.
func (d *driver) allocFor(key string, size int64) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return max(d.allocs[key], uint64(size))
}

// setNormalizedName reports the opened name with each component's
// stored case. info must be the one passed to Create or Open. On a
// case-insensitive mount the kernel otherwise keeps the upper-cased name
// it looked the file up by, and reports that back — e.g. "\FILE0" from
// GetFinalPathNameByHandle for a file created as "file0" (winfsp-tests'
// getfileinfo_test/getfileinfo_name_test). Same as WinFsp's own memfs.
func setNormalizedName(info *winfsp.FSP_FSCTL_FILE_INFO, name string) {
	winfsp.GetOpenFileInfo(info).SetNormalizedName(name)
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
	path, stream, ok := splitStream(name)
	if !ok {
		return 0, windows.STATUS_OBJECT_NAME_INVALID
	}
	resolve := resolvePathFollow
	if createOptions&fileOpenReparsePoint != 0 {
		resolve = resolvePath
	}
	r, err := resolve(ctx(), d.fsys, path, d.caseInsensitive)
	if err != nil {
		return 0, toWinError(err)
	}
	if stream != "" {
		return d.openStream(r, path, stream, info)
	}
	// A generic (non-symlink) reparse point (task G4) carries opaque
	// data this driver never interprets — real NTFS, and winfsp-tests'
	// own reparse_guid_test/reparse_nfs_test (confirmed against this
	// exact driver), both require that a normal open of such a file,
	// without FILE_FLAG_OPEN_REPARSE_POINT, be refused outright rather
	// than silently handed back as if the reparse data weren't there:
	// a caller with no way to interpret an opaque tag has no business
	// reading or writing "through" it. Symlinks are exempt — they're a
	// first-class type this driver does interpret, and resolve has
	// already followed them above when appropriate.
	if createOptions&fileOpenReparsePoint == 0 && r.Type != icbfs.TypeSymlink {
		if _, ok := icbfs.GenericReparseFromXattrs(r.Attr.Xattrs); ok {
			return 0, windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED
		}
	}
	d.fillFileInfo(info, r.Key, r.Type, r.Attr)
	setNormalizedName(info, r.Path)
	h := &fileHandle{key: r.Key, typ: r.Type, path: name}
	return d.newHandle(h), nil
}

func (d *driver) Close(fs *winfsp.FileSystemRef, file uintptr) {
	h, ok := d.handle(file)
	d.dropHandle(file)
	if !ok {
		return
	}
	d.releaseShared(h)
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
	path, stream, ok := splitStream(name)
	if !ok {
		return 0, windows.STATUS_OBJECT_NAME_INVALID
	}
	if stream != "" {
		return d.createStream(path, stream, allocationSize, info)
	}
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir, d.caseInsensitive)
	if err != nil {
		return 0, toWinError(err)
	}
	uid, gid := callerOwner()

	const fileDirectoryFile = 0x00000001 // FILE_DIRECTORY_FILE, low byte of NtCreateFile's CreateOptions
	// A *directory* symlink (CreateSymbolicLinkW with
	// SYMBOLIC_LINK_FLAG_DIRECTORY, ROADMAP.md's Part G, task G4/G5)
	// sets FILE_DIRECTORY_FILE too, identically to a real mkdir — there
	// is no reliable createOptions/fileAttributes signal at this point
	// to tell the two apart. Confirmed empirically, not assumed: a real
	// debug trace of this exact test (winfsp-tests' own
	// reparse_symlink_relative_test, --external) showed its own plain
	// CreateDirectoryW("\1") and its CreateSymbolicLinkW("\ld", ...,
	// SYMBOLIC_LINK_FLAG_DIRECTORY) placeholder create produce the
	// *identical* createOptions value (0x02200021) — FILE_DIRECTORY_FILE
	// and FILE_OPEN_REPARSE_POINT both set in both cases,
	// FILE_OPEN_FOR_BACKUP_INTENT absent from both. (A separate manual
	// probe via PowerShell's New-Item/raw CreateDirectoryW did set
	// FILE_OPEN_FOR_BACKUP_INTENT — a caller-dependent detail, not a
	// stable protocol signal, and chasing it broke plain mkdir outright
	// before this was caught.) So: always make a real directory here,
	// same as before this task; SetReparsePoint (reparse.go) is what
	// actually resolves the ambiguity, by converting *that* directory
	// into a symlink if a symlink reparse buffer arrives for it next.
	if createOptions&fileDirectoryFile != 0 {
		newUUID, attr, err := d.fsys.Mkdir(ctx(), parent.Key, base, 0755, uid, gid)
		if err != nil {
			return 0, toWinError(err)
		}
		// A directory's security descriptor and attribute bits are
		// stored like a file's (no Archive bit, as on NTFS) — so a
		// directory created with a restrictive ACL actually enforces it
		// (winfsp-tests' create_notraverse/create_restore/getfileattr).
		if securityDescriptor != nil || fileAttributes&^windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			var sddl []byte
			if securityDescriptor != nil {
				sddl = []byte(securityDescriptor.String())
			}
			if attr, err = d.fsys.SetWindowsCreateMetadata(ctx(), newUUID, icbfs.TypeDir, fileAttributes, sddl); err != nil {
				return 0, toWinError(err)
			}
		}
		d.fillFileInfo(info, newUUID, icbfs.TypeDir, attr)
		setNormalizedName(info, joinPath(parent.Path, base))
		return d.newHandle(&fileHandle{key: newUUID, typ: icbfs.TypeDir, path: name}), nil
	}

	mode := modeFromAttributes(fileAttributes)
	newUUID, attr, etag, err := d.fsys.Create(ctx(), parent.Key, base, mode, uid, gid)
	if err != nil {
		return 0, toWinError(err)
	}
	// Persist the real Hidden/System/ReadOnly/Archive byte from
	// creation time too, not just from a later SetBasicInfo (task F7)
	// — a file created with e.g. FILE_ATTRIBUTE_HIDDEN set should
	// report it back immediately, not only after an explicit
	// SetFileAttributes call.
	//
	// FILE_ATTRIBUTE_ARCHIVE is always set on a newly created file by
	// a real Windows filesystem, regardless of what the caller
	// requested — confirmed a real, previously-missing requirement by
	// winfsp-tests' own create_fileattr_test (--external, against this
	// driver), not assumed: it asserts FILE_ATTRIBUTE_ARCHIVE ==
	// GetFileAttributes() after a plain CreateFileW(...,
	// FILE_ATTRIBUTE_NORMAL, 0), which this driver failed before this
	// fix (it only ever stored exactly what the caller passed, masked
	// to the four real bits — a NORMAL-only create stored and reported
	// back nothing set at all).
	//
	// The security descriptor WinFsp passes for the new file is stored
	// too, in the same metadata write — previously it was ignored and
	// every new file got the default full-access descriptor, so e.g. a
	// file created with DELETE-only access could still be opened for
	// reading (winfsp-tests' getfileattr_test).
	var sddl []byte
	if securityDescriptor != nil {
		sddl = []byte(securityDescriptor.String())
	}
	if attr, err = d.fsys.SetWindowsCreateMetadata(ctx(), newUUID, icbfs.TypeFile, fileAttributes|icbfs.WindowsAttrArchive, sddl); err != nil {
		return 0, toWinError(err)
	}
	// seeded empty, not nil: a just-created file's content really is
	// zero bytes, distinct from "not seeded yet" — openFile below
	// tells the two apart via hasSeed, not a nil check on this slice.
	id := d.newHandle(&fileHandle{key: newUUID, typ: icbfs.TypeFile, path: name, etag: etag, seeded: []byte{}, hasSeed: true})
	if allocationSize > 0 {
		d.setAlloc(newUUID, allocationSize)
	}
	d.fillFileInfo(info, newUUID, icbfs.TypeFile, attr)
	setNormalizedName(info, joinPath(parent.Path, base))
	return id, nil
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
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, nil, nil, nil, &zero, nil, nil, nil)
	if err != nil {
		return toWinError(err)
	}
	// Buffered writes from before the truncate are discarded, not
	// flushed — an overwrite replaces the content wholesale.
	d.invalidateShared(h.key)

	if h.stream != "" {
		d.fillHandleInfo(info, h, attr)
		return nil
	}

	// Superseding a file removes its named streams, except any that are
	// open right now — same as WinFsp's memfs (winfsp-tests'
	// stream_create_overwrite_test).
	if len(attr.Streams) > 0 {
		var drop []string
		d.mu.Lock()
		for name, id := range attr.Streams {
			if d.keyOpens[icbfs.StreamKey(h.key, id)] == 0 {
				drop = append(drop, name)
			}
		}
		d.mu.Unlock()
		if len(drop) > 0 {
			if _, err := d.fsys.DeleteStreams(ctx(), h.key, h.typ, drop); err != nil {
				return toWinError(err)
			}
		}
	}

	// CREATE_ALWAYS/TRUNCATE_EXISTING carry attributes too: replace or
	// merge them per replaceAttributes, with Archive always set, as on
	// NTFS (winfsp-tests' create_fileattr_test).
	bits := attributes
	if !replaceAttributes {
		if old, ok := icbfs.WindowsAttributesFromXattrs(attr.Xattrs); ok {
			bits |= old
		} else {
			bits |= d.attributesFromAttr(h.typ, attr)
		}
	}
	bits |= icbfs.WindowsAttrArchive
	mode := modeFromAttributes(bits)
	if attr, err = d.fsys.SetAttr(ctx(), h.key, h.typ, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		return toWinError(err)
	}
	if attr, err = d.fsys.SetWindowsAttributes(ctx(), h.key, h.typ, bits); err != nil {
		return toWinError(err)
	}
	d.fillHandleInfo(info, h, attr)
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
	if h.stream != "" {
		_ = d.fsys.DeleteStream(ctx(), h.ownerKey, h.ownerTyp, h.stream)
		return
	}
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir, d.caseInsensitive)
	if err != nil {
		return
	}
	// Same case-folding gotcha Rename hit (task F9, see
	// realStoredName's own doc comment): the kernel can hand this
	// callback a differently-cased name than what's actually stored
	// when CaseSensitive(false) is in effect, which Rmdir/Unlink's
	// exact-match lookup would otherwise silently fail to find.
	base = realStoredName(ctx(), d.fsys, parent.Key, base, d.caseInsensitive)
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
	if h.typ != icbfs.TypeDir || h.stream != "" {
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

// Rename resolves both source and target (full WinFsp paths) to their
// own parent directory + base name, same splitParent/resolvePath
// pattern Create/Cleanup already use, then delegates to
// icbfs.Filesystem.Rename (task F9 — see its own doc comment for why
// this didn't already exist at the core layer). file (the open handle
// on source) isn't needed here: Rename resolves the entry by
// (oldParent, oldBase) itself.
// realStoredName resolves name to its actual stored (real-case)
// spelling within dirKey, if an entry matching it (case-aware per
// caseInsensitive) exists; otherwise returns name unchanged — the
// caller (e.g. Rename, below, for a target that doesn't exist yet)
// decides what that means.
//
// Needed because icbfs.Filesystem's own operations always do an
// *exact* (case-sensitive) lookup internally, but the kernel can hand
// this driver a differently-cased name than what's actually stored
// when CaseSensitive(false) is in effect — the exact F3-era gotcha
// (see resolve.go's lookupCaseAware doc comment), confirmed recurring
// here for Rename specifically: WinFsp/the kernel passed this driver
// "OLD.TXT" for a file actually stored as "old.txt", which
// icbfs.Filesystem.Rename's exact-match findInTree could never locate
// on its own (unlike resolvePath's own directory-traversal, which
// already runs every segment through lookupCaseAware).
func realStoredName(ctx context.Context, fsys *icbfs.Filesystem, dirKey, name string, caseInsensitive bool) string {
	entry, _, err := lookupCaseAware(ctx, fsys, dirKey, name, caseInsensitive)
	if err != nil {
		return name
	}
	return entry.Name
}

func (d *driver) Rename(fs *winfsp.FileSystemRef, file uintptr, source, target string, replaceIfExist bool) error {
	// A stream renames only to another stream of the same file or
	// directory; a stream can't become a file or vice versa here.
	_, srcStream, ok1 := splitStream(source)
	dstPath, dstStream, ok2 := splitStream(target)
	if !ok1 || !ok2 {
		return windows.STATUS_OBJECT_NAME_INVALID
	}
	if srcStream != "" || dstStream != "" {
		h, ok := d.handle(file)
		if !ok {
			return os.ErrNotExist
		}
		if srcStream == "" || dstStream == "" || h.stream == "" {
			return windows.STATUS_INVALID_PARAMETER
		}
		dst, err := resolvePath(ctx(), d.fsys, dstPath, d.caseInsensitive)
		if err != nil {
			return toWinError(err)
		}
		if dst.Key != h.ownerKey {
			return windows.STATUS_INVALID_PARAMETER
		}
		if real, _, found := lookupStream(dst.Attr.Streams, dstStream, d.caseInsensitive); found {
			if real == h.stream {
				return nil
			}
			if !replaceIfExist {
				return windows.STATUS_OBJECT_NAME_COLLISION
			}
			dstStream = real
		}
		if err := d.fsys.RenameStream(ctx(), h.ownerKey, h.ownerTyp, h.stream, dstStream, replaceIfExist); err != nil {
			return toWinError(err)
		}
		h.mu.Lock()
		h.stream = dstStream
		h.mu.Unlock()
		return nil
	}
	oldDir, oldBase := splitParent(source)
	oldParent, err := resolvePath(ctx(), d.fsys, oldDir, d.caseInsensitive)
	if err != nil {
		return toWinError(err)
	}
	oldBase = realStoredName(ctx(), d.fsys, oldParent.Key, oldBase, d.caseInsensitive)

	newDir, newBase := splitParent(target)
	newParent, err := resolvePath(ctx(), d.fsys, newDir, d.caseInsensitive)
	if err != nil {
		return toWinError(err)
	}
	newBase = realStoredName(ctx(), d.fsys, newParent.Key, newBase, d.caseInsensitive)

	// Windows never lets a rename replace an existing directory, even
	// with MOVEFILE_REPLACE_EXISTING — STATUS_ACCESS_DENIED, as on NTFS
	// and WinFsp's own memfs (winfsp-tests' rename_test). POSIX
	// rename(2) does allow replacing an empty directory, which is what
	// icbfs.Filesystem.Rename implements, so this is Windows-only.
	if replaceIfExist {
		if e, _, err := d.fsys.Lookup(ctx(), newParent.Key, newBase); err == nil && e.Type == icbfs.TypeDir {
			return windows.STATUS_ACCESS_DENIED
		}
	}

	return toWinError(d.fsys.Rename(ctx(), oldParent.Key, oldBase, newParent.Key, newBase, replaceIfExist))
}

// openFile returns the OpenFile shared by every handle on h's file,
// attaching h to it (and taking a reference) on first real I/O — most
// handles (a bare GetFileInfo, a directory listing) never touch this.
func (d *driver) openFile(h *fileHandle) (*icbfs.OpenFile, error) {
	h.mu.Lock()
	if h.shared == nil {
		d.openMu.Lock()
		s := d.opens[h.key]
		if s == nil {
			s = &sharedOpen{key: h.key}
			d.opens[h.key] = s
		}
		s.refs++
		d.openMu.Unlock()
		h.shared = s
	}
	s := h.shared
	hasSeed, etag, seeded := h.hasSeed, h.etag, h.seeded
	h.hasSeed, h.seeded = false, nil
	h.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open != nil {
		return s.open, nil
	}
	if hasSeed {
		s.open = d.fsys.NewOpenFile(h.key, etag, seeded)
		return s.open, nil
	}
	open, err := d.fsys.Open(ctx(), h.key)
	if err != nil {
		return nil, err
	}
	s.open = open
	return open, nil
}

// currentOpen returns key's shared OpenFile if some handle already has
// one in use, without creating one.
func (d *driver) currentOpen(key string) *icbfs.OpenFile {
	d.openMu.Lock()
	s := d.opens[key]
	d.openMu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// flushShared commits key's shared OpenFile's pending writes, if any.
// Called before an operation that changes content behind OpenFile's
// back (Overwrite, SetFileSize), so buffered writes are neither lost
// nor later flushed on top of that change.
func (d *driver) flushShared(key string) error {
	if open := d.currentOpen(key); open != nil {
		if _, err := open.Flush(ctx()); err != nil {
			return err
		}
	}
	return nil
}

// invalidateShared drops key's shared OpenFile after its content was
// changed directly in the store; the next I/O on any handle reopens it.
// Also discards any handle's unused Create seed for key: a seed is the
// content as of creation, and using it after a resize made a freshly
// extended file read as empty — page-in reads got EOF inside the file's
// reported size, and memory-mapped writes were lost (winfsp-tests'
// rdwr_mmap_test).
func (d *driver) invalidateShared(key string) {
	d.mu.Lock()
	for _, h := range d.handles {
		if h.key == key {
			h.mu.Lock()
			h.hasSeed, h.seeded = false, nil
			h.mu.Unlock()
		}
	}
	d.mu.Unlock()

	d.openMu.Lock()
	s := d.opens[key]
	d.openMu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open != nil {
		_ = s.open.Close()
		s.open = nil
	}
}

// releaseShared drops h's reference, flushing (every close, as before)
// and releasing the scratch file once the last handle is gone.
func (d *driver) releaseShared(h *fileHandle) {
	h.mu.Lock()
	s := h.shared
	h.shared = nil
	h.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.open != nil {
		// Best-effort: a close that can't flush has nowhere left to
		// report the error to (BehaviourBase.Close returns nothing).
		_, _ = s.open.Flush(ctx())
	}
	s.mu.Unlock()

	d.openMu.Lock()
	s.refs--
	last := s.refs == 0
	if last && d.opens[s.key] == s {
		delete(d.opens, s.key)
	}
	d.openMu.Unlock()
	if last {
		s.mu.Lock()
		if s.open != nil {
			// Releases the private local scratch file (ROADMAP.md's
			// Part G, task G3) — otherwise it leaks for the life of
			// the mount process.
			_ = s.open.Close()
			s.open = nil
		}
		s.mu.Unlock()
	}
}

// callerOwner returns the uid/gid new entries this driver creates get.
// Real per-caller identity mapping (SID -> uid/gid) is F8's job, same
// as the rest of this driver's ACL story — placeholder root ownership
// here is consistent with this task's own scope (F3 is core operation
// wiring, not ACL design).
func callerOwner() (uid, gid uint32) { return 0, 0 }
