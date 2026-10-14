//go:build windows

// Package winfspserver's Windows-only files implement go-winfsp's native
// Behaviour interfaces directly on icbfs.Filesystem (through resolve.go's
// portable resolvePath), not through the gofs wrapper.
package winfspserver

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// driver implements winfsp.BehaviourBase plus every optional Behaviour*
// interface it wires. One driver backs one mounted
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

// sharedOpen is the one icbfs.OpenFile every handle on a file in this mount
// reads and writes through, counted by handle. Windows keeps one cache per
// file, so every handle must see the others' writes at once; a private
// OpenFile per handle made a non-cached read miss what a cached handle had
// just written (winfsp-tests' rdwr_mixed_test). OpenFile's isolation still
// applies between mounts and machines.
type sharedOpen struct {
	key  string
	refs int // guarded by driver.openMu

	mu   sync.Mutex
	open *icbfs.OpenFile // nil until first real I/O, or after invalidate
}

// fileHandle is this driver's state for one handle: what Open and Create
// return as a uintptr, and every later call receives.
//
// shared is attached on first read or write (openFile). Most handles
// (directory listings, attribute queries) never need it.
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

// Root builds the winfsp.BehaviourBase for fsys, for winfsp.Mount. fsys
// must already be attached.
//
// caseInsensitive must match the winfsp.CaseSensitive option passed to
// winfsp.Mount. They are two different settings: WinFsp's decides whether
// the kernel folds case before calling this driver (it may then pass a name
// in different case from the stored one), and this one decides whether
// resolvePath tolerates a case mismatch against icbfs's case-sensitive
// storage. Mismatched, either the fallback never runs or the kernel's
// folded names never match.
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

// ctx is the context for every icbfs.Filesystem call this driver makes:
// go-winfsp's callbacks carry none.
func ctx() context.Context { return context.Background() }

// toWinError maps icbfs's errors onto the syscall.Errno and os.Err values
// go-winfsp's convertNTStatus turns into NTSTATUS codes; anything else
// becomes STATUS_INTERNAL_ERROR.
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
	case errors.Is(err, icbfs.ErrReadOnly):
		// A read-only volume (an as-of view of the past).
		return windows.STATUS_MEDIA_WRITE_PROTECTED
	case errors.Is(err, icbfs.ErrArchived):
		// There's no read-only-volume NTSTATUS in convertNTStatus's map;
		// access denied is the closest.
		return os.ErrPermission
	case errors.Is(err, icbfs.ErrInvalidName):
		// convertNTStatus has no sentinel for this; return the real
		// NTSTATUS directly rather than stretching an os.Err*/
		// syscall.Errno value to fit.
		return windows.STATUS_OBJECT_NAME_INVALID
	case errors.Is(err, icbfs.ErrFileTooLarge):
		// What Windows reports for a file too big for its volume
		// (ERROR_FILE_TOO_LARGE), e.g. over 4 GiB on FAT32.
		return windows.STATUS_FILE_TOO_LARGE
	case errors.Is(err, icbfs.ErrNameTooLong):
		// What NTFS answers for a component over 255 characters. WinFsp
		// refuses those itself before they reach this driver (its
		// MaxComponentLength, 255 by default); this covers anything else.
		return windows.STATUS_OBJECT_NAME_INVALID
	case errors.Is(err, icbfs.ErrInvalidRename):
		// Moving a directory into one of its own descendants — no more specific NTSTATUS confirmed for this exact
		// condition; STATUS_INVALID_PARAMETER is the closest real fit.
		return windows.STATUS_INVALID_PARAMETER
	case errors.Is(err, icbfs.ErrInvalidArgument):
		// A negative file size.
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

// fileOpenReparsePoint is NtCreateFile's FILE_OPEN_REPARSE_POINT option:
// the caller wants the reparse point itself, not what it points to.
const fileOpenReparsePoint = 0x00200000

// Open resolves name and returns a new handle; its OpenFile is made on
// first read or write (see fileHandle).
//
// It follows a symlink at the end of the path unless the caller asked for
// the reparse point itself. The kernel follows an absolute symlink target
// on its own, but not a relative one, so this is what makes relative
// symlinks work (see resolvePathFollow).
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
	// A non-symlink reparse point carries data this driver doesn't
	// interpret. As on NTFS (winfsp-tests' reparse_guid_test and
	// reparse_nfs_test), opening one without FILE_FLAG_OPEN_REPARSE_POINT
	// is refused. Symlinks were already followed above.
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

// Create makes a new file or directory (FILE_DIRECTORY_FILE in
// createOptions). WinFsp refuses every create and open
// (STATUS_INVALID_DEVICE_REQUEST) unless Create, Open and Overwrite are all
// implemented.
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
	// A directory symlink (SYMBOLIC_LINK_FLAG_DIRECTORY) is created with
	// exactly the createOptions of a mkdir: winfsp-tests'
	// reparse_symlink_relative_test sends 0x02200021 for both. So this
	// always makes a directory, and SetReparsePoint turns it into a symlink
	// if a symlink reparse buffer follows.
	if createOptions&fileDirectoryFile != 0 {
		newUUID, attr, err := d.fsys.Mkdir(ctx(), parent.Key, base, 0755, uid, gid)
		if err != nil {
			return 0, toWinError(err)
		}
		// A directory's security descriptor and attribute bits are stored
		// as a file's are (without Archive, as on NTFS), so a restrictive
		// ACL given at creation is enforced (winfsp-tests'
		// create_notraverse, create_restore, getfileattr).
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
	// Store the attribute bits and the security descriptor from the create
	// itself, in one metadata write, so a file created hidden reports it at
	// once, and one created with a restrictive descriptor enforces it
	// (winfsp-tests' getfileattr_test).
	//
	// Windows sets Archive on every new file whatever the caller asked for
	// (winfsp-tests' create_fileattr_test).
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

// Overwrite truncates an existing file to empty (CREATE_ALWAYS,
// TRUNCATE_EXISTING). WinFsp requires it (see Create).
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

// Cleanup performs the delete CanDelete allowed, when the last handle is
// cleaned up with FspCleanupDelete set: WinFsp's delete protocol, as gofs
// implements it.
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
	// The kernel can pass a name in different case from the stored one (see
	// realStoredName).
	base = realStoredName(ctx(), d.fsys, parent.Key, base, d.caseInsensitive)
	dctx := icbfs.WithDeleteInfo(ctx(), icbfs.DeleteInfo{Path: strings.ReplaceAll(name, `\`, "/")})
	if h.typ == icbfs.TypeDir {
		_ = d.fsys.Rmdir(dctx, parent.Key, base)
	} else {
		_ = d.fsys.Unlink(dctx, parent.Key, base)
	}
}

// CanDelete reports whether file may be deleted; WinFsp asks before marking
// a delete pending (Cleanup does it). A non-empty directory can't be.
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

// realStoredName returns name as it's spelled in dirKey, if an entry
// matches it (case-insensitively when caseInsensitive), and name unchanged
// otherwise.
//
// icbfs.Filesystem's operations look names up exactly, but with
// CaseSensitive(false) the kernel can pass "OLD.TXT" for a file stored as
// "old.txt". resolvePath already handles this for directories
// (lookupCaseAware); callers that pass a bare name to icbfs.Filesystem need
// this.
func realStoredName(ctx context.Context, fsys *icbfs.Filesystem, dirKey, name string, caseInsensitive bool) string {
	entry, _, err := lookupCaseAware(ctx, fsys, dirKey, name, caseInsensitive)
	if err != nil {
		return name
	}
	return entry.Name
}

// Rename splits source and target into parent and name, and has
// icbfs.Filesystem.Rename move the entry; file isn't needed.
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

// flushShared writes key's shared OpenFile's pending writes, if any, before
// an operation that changes the content behind it (Overwrite, SetFileSize),
// so they are neither lost nor flushed on top of that change.
func (d *driver) flushShared(key string) error {
	if open := d.currentOpen(key); open != nil {
		if _, err := open.Flush(ctx()); err != nil {
			return err
		}
	}
	return nil
}

// invalidateShared drops key's shared OpenFile after its content was
// changed directly in the store; the next I/O on any handle reopens it. It
// also drops any handle's unused Create seed for key: after a resize, a
// seed made a newly extended file read as empty, and lost memory-mapped
// writes (winfsp-tests' rdwr_mmap_test).
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
			// Releases the private local scratch file — otherwise it leaks for the life of
			// the mount process.
			_ = s.open.Close()
			s.open = nil
		}
		s.mu.Unlock()
	}
}

// callerOwner returns the uid/gid new entries this driver creates get.
// There is no SID -> uid/gid mapping, so it's root's: on Windows, who
// owns an item is in its security descriptor.
func callerOwner() (uid, gid uint32) { return 0, 0 }
