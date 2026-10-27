package icbfsdrive

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// inMemoryWrite is the largest write sent from memory; a bigger body is
// spooled to a temporary file and written the way a mount writes a
// large file (in parts, through a scratch file).
var inMemoryWrite int64 = 64 << 20 // a variable for the tests

// Write makes the body a file's content, creating the file if need be.
func (d *Drive) Write(ctx context.Context, p string, body io.Reader, cond drive.Condition, opts drive.WriteOptions) (drive.Item, error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return drive.Item{}, err
	}
	defer done()
	if _, err := appData(opts.App, nil); err != nil { // refused before anything is written
		return drive.Item{}, err
	}
	parent, name, err := d.resolveParent(ctx, p, opts.Parents)
	if err != nil {
		return drive.Item{}, err
	}
	data, spool, sums, err := d.readBody(body)
	if err != nil {
		return drive.Item{}, err
	}
	if spool != nil {
		defer func() { spool.Close(); os.Remove(spool.Name()) }()
	}
	if err := checkSums(opts.Checksums, sums); err != nil {
		return drive.Item{}, err
	}

	for attempt := 0; attempt < 3; attempt++ {
		// The first look starts from the block the parent's resolve
		// read; one after a race reads it again.
		lookup := d.step
		if attempt > 0 {
			lookup = d.lookupIn
		}
		e, a, lerr := lookup(ctx, parent, name)
		if lerr == nil {
			if e.Type != icbfs.TypeFile {
				return drive.Item{}, drive.ErrIsDir
			}
			n := parent.child(e, a)
			if err := d.check(ctx, n, needWrite); err != nil {
				return drive.Item{}, err
			}
			cur, ok := a.ContentVersion()
			if !ok {
				head, err := d.fs.Store().Head(ctx, n.key)
				if err != nil {
					return drive.Item{}, mapErr(err)
				}
				cur = *head
			}
			if cond.IfNoneMatch || cond.IfMatch != "" && cond.IfMatch != cur.VersionID {
				return drive.Item{}, &drive.PreconditionError{Current: cur.VersionID}
			}
			written, err := d.writeContent(ctx, n.key, data, spool, cur.ETag)
			if err != nil {
				if objstore.IsPreconditionFailed(err) {
					if cond.IfMatch != "" {
						now, _ := d.fs.Store().Head(ctx, n.key)
						current := ""
						if now != nil {
							current = now.VersionID
						}
						return drive.Item{}, &drive.PreconditionError{Current: current}
					}
					continue // someone else wrote; an unconditional write just goes again
				}
				return drive.Item{}, mapErr(err)
			}
			if n.attr, err = d.afterWrite(ctx, n.key, n.path, written, opts, sums); err != nil {
				return drive.Item{}, err
			}
			d.changed(parent)
			return d.item(ctx, n, true), nil
		}
		if !errors.Is(mapErr(lerr), drive.ErrNotFound) {
			return drive.Item{}, mapErr(lerr)
		}
		if cond.IfMatch != "" {
			return drive.Item{}, &drive.PreconditionError{}
		}
		if err := d.check(ctx, parent, needAddFile); err != nil {
			return drive.Item{}, err
		}
		n, err := d.create(ctx, parent, name, data, spool, opts, sums)
		if errors.Is(err, icbfs.ErrExists) {
			if cond.IfNoneMatch {
				return drive.Item{}, &drive.PreconditionError{}
			}
			continue // made meanwhile: write over it
		}
		if err != nil {
			return drive.Item{}, err
		}
		d.changed(parent)
		d.idPaths.put(n.key, n.path)
		return d.item(ctx, n, true), nil
	}
	return drive.Item{}, drive.ErrBusy
}

// readBody reads up to the filesystem's max file size: into memory when
// small, else into a temporary file; and the content's checksums, as it
// goes by.
func (d *Drive) readBody(body io.Reader) ([]byte, *os.File, icbfs.Checksums, error) {
	max := d.fs.MaxFileSize()
	sum := icbfs.NewChecksummer()
	body = io.TeeReader(body, sum)
	buf := &bytes.Buffer{}
	n, err := io.CopyN(buf, body, inMemoryWrite+1)
	if err != nil && err != io.EOF {
		return nil, nil, icbfs.Checksums{}, err
	}
	if n <= inMemoryWrite {
		if int64(buf.Len()) > max {
			return nil, nil, icbfs.Checksums{}, drive.ErrTooLarge
		}
		return buf.Bytes(), nil, sum.Sum(), nil
	}
	f, err := os.CreateTemp(d.cfg.CacheDir, "drive-write-")
	if err != nil {
		return nil, nil, icbfs.Checksums{}, err
	}
	total, err := io.Copy(f, io.MultiReader(buf, io.LimitReader(body, max-n+1)))
	if err == nil && total > max {
		err = drive.ErrTooLarge
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, icbfs.Checksums{}, err
	}
	return nil, f, sum.Sum(), nil
}

// checkSums fails with ErrChecksum if a checksum the caller gave isn't
// the content's.
func checkSums(want *drive.Checksums, got icbfs.Checksums) error {
	if want == nil {
		return nil
	}
	if want.MD5 != "" && !strings.EqualFold(want.MD5, hex.EncodeToString(got.MD5)) {
		return fmt.Errorf("MD5 %s given, %x arrived: %w", want.MD5, got.MD5, drive.ErrChecksum)
	}
	if want.SHA256 != "" && !strings.EqualFold(want.SHA256, hex.EncodeToString(got.SHA256)) {
		return fmt.Errorf("SHA-256 %s given, %x arrived: %w", want.SHA256, got.SHA256, drive.ErrChecksum)
	}
	return nil
}

// toChecksums is c in the drive API's form; nil for none.
func toChecksums(c icbfs.Checksums) *drive.Checksums {
	if len(c.MD5) == 0 && len(c.SHA256) == 0 {
		return nil
	}
	return &drive.Checksums{MD5: hex.EncodeToString(c.MD5), SHA256: hex.EncodeToString(c.SHA256)}
}

// create makes the file name in parent with its content: in the same two
// writes when the content is in memory (icbfs CreateWith); a large one,
// spooled, as a mount writes it, into a file created empty.
func (d *Drive) create(ctx context.Context, parent *node, name string, data []byte, spool *os.File, opts drive.WriteOptions, sums icbfs.Checksums) (*node, error) {
	sddl, mode, uid, gid := d.newOwnership(ctx, parent, false)
	p := joinPath(parent.path, name)
	if spool == nil {
		info, err := d.writeInfo(ctx, opts, sums)
		if err != nil {
			return nil, err
		}
		key, attr, err := d.fs.CreateWith(ctx, parent.key, name, mode, uid, gid, data, sddl, info)
		if err != nil {
			return nil, mapCreateErr(err)
		}
		if opts.Origin != nil {
			if err := d.recordOrigin(ctx, key, p, *opts.Origin); err != nil {
				return nil, err
			}
		}
		return parent.child(block.Entry{Name: name, UUID: key, Type: icbfs.TypeFile}, attr), nil
	}
	key, _, etag, err := d.fs.Create(ctx, parent.key, name, mode, uid, gid)
	if err != nil {
		return nil, mapCreateErr(err)
	}
	if sddl != nil {
		if _, err := d.fs.SetWindowsACL(ctx, key, icbfs.TypeFile, sddl); err != nil {
			return nil, mapErr(err)
		}
	}
	created, _ := d.fs.Store().Head(ctx, key)
	written, err := d.writeContent(ctx, key, data, spool, etag)
	if err != nil {
		return nil, mapErr(err)
	}
	attr, err := d.afterWrite(ctx, key, p, written, opts, sums)
	if err != nil {
		return nil, err
	}
	// Creating made an empty version before the content's; a file the
	// API makes starts with just its content.
	if created != nil && created.Size == 0 {
		_ = d.fs.Store().Erase(ctx, key, created.VersionID)
	}
	return parent.child(block.Entry{Name: name, UUID: key, Type: icbfs.TypeFile}, attr), nil
}

// mapCreateErr is mapErr, leaving icbfs.ErrExists as it is for Write to
// retry on.
func mapCreateErr(err error) error {
	if errors.Is(err, icbfs.ErrExists) {
		return err
	}
	return mapErr(err)
}

// writeContent replaces key's content, if it's still at etag, and returns
// the version written.
func (d *Drive) writeContent(ctx context.Context, key string, data []byte, spool *os.File, etag string) (*objstore.Object, error) {
	if spool == nil {
		return d.fs.WriteFile(ctx, key, data, etag)
	}
	of, err := d.fs.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer of.Close()
	if err := of.Truncate(0); err != nil {
		return nil, err
	}
	buf := make([]byte, 8<<20)
	var off int64
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		n, rerr := spool.Read(buf)
		if n > 0 {
			if err := of.WriteAt(buf[:n], off); err != nil {
				return nil, err
			}
			off += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if _, err := of.Flush(ctx); err != nil {
		return nil, err
	}
	return d.fs.Store().Head(ctx, key)
}

// writeInfo is what opts say to record about content just written, with
// sums: its content type, its times at the source, its origin, and its
// checksums.
func (d *Drive) writeInfo(ctx context.Context, opts drive.WriteOptions, sums icbfs.Checksums) (icbfs.ItemInfo, error) {
	app, err := appData(opts.App, opts.Origin)
	if err != nil {
		return icbfs.ItemInfo{}, err
	}
	if opts.ContentType != "" {
		app[icbfs.AppDriveContentType] = []byte(opts.ContentType)
	}
	mtime := timeOrNil(opts.ModTime)
	if t, ok := drive.SourceTime(ctx); ok && mtime == nil {
		mtime = &t // a version written in source time is modified then
	}
	return icbfs.ItemInfo{
		Mtime: mtime, Btime: timeOrNil(opts.Created),
		Mode: opts.Mode, WindowsAttrs: opts.WindowsAttrs, App: app, Checksums: &sums,
	}, nil
}

// afterWrite records, in one metadata write, what opts say about content
// just written to the file key at drive path p (writeInfo), and indexes
// the origin. It returns the file's attributes, as a stat would.
func (d *Drive) afterWrite(ctx context.Context, key, p string, written *objstore.Object, opts drive.WriteOptions, sums icbfs.Checksums) (icbfs.Attr, error) {
	info, err := d.writeInfo(ctx, opts, sums)
	if err != nil {
		return icbfs.Attr{}, err
	}
	attr, err := d.fs.SetFileInfo(ctx, key, written, info)
	if err != nil {
		return icbfs.Attr{}, mapErr(err)
	}
	if opts.Origin != nil {
		return attr, d.recordOrigin(ctx, key, p, *opts.Origin)
	}
	return attr, nil
}

func (d *Drive) statItem(ctx context.Context, p string) (drive.Item, error) {
	n, err := d.resolvePath(ctx, d.fs, p)
	if err != nil {
		return drive.Item{}, err
	}
	return d.item(ctx, n, true), nil
}

// Mkdir makes a directory (and its parents, with parents; an existing
// directory is then fine).
func (d *Drive) Mkdir(ctx context.Context, p string, parents bool) (drive.Item, error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return drive.Item{}, err
	}
	defer done()
	if parents {
		n, err := d.mkdirAll(ctx, p)
		if err != nil {
			return drive.Item{}, err
		}
		return d.item(ctx, n, false), nil
	}
	parent, name, err := d.resolveParent(ctx, p, false)
	if err != nil {
		return drive.Item{}, err
	}
	n, err := d.mkdirIn(ctx, parent, name)
	if err != nil {
		return drive.Item{}, err
	}
	return d.item(ctx, n, false), nil
}

func (d *Drive) mkdirIn(ctx context.Context, parent *node, name string) (*node, error) {
	if err := d.check(ctx, parent, needAddDir); err != nil {
		return nil, err
	}
	sddl, mode, uid, gid := d.newOwnership(ctx, parent, true)
	key, _, err := d.fs.Mkdir(ctx, parent.key, name, mode, uid, gid)
	if err != nil {
		return nil, mapErr(err)
	}
	if sddl != nil {
		if _, err := d.fs.SetWindowsACL(ctx, key, icbfs.TypeDir, sddl); err != nil {
			return nil, mapErr(err)
		}
	}
	d.changed(parent)
	n, err := d.resolvePath(ctx, d.fs, joinPath(parent.path, name))
	if err == nil {
		d.idPaths.put(n.key, n.path)
	}
	return n, err
}

// mkdirAll makes p and every missing directory above it.
func (d *Drive) mkdirAll(ctx context.Context, p string) (*node, error) {
	n, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return nil, err
	}
	for _, part := range splitPath(cleanPath(p)) {
		if err := d.checkTraverse(ctx, n); err != nil {
			return nil, err
		}
		// A step down from n, which holds its block; made if it isn't
		// there, and looked up afresh if someone made it meanwhile.
		var next *node
		e, a, err := d.step(ctx, n, part)
		if err = mapErr(err); err == nil {
			next = n.child(e, a)
		} else if errors.Is(err, drive.ErrNotFound) {
			next, err = d.mkdirIn(ctx, n, part)
			if errors.Is(err, drive.ErrExists) {
				e, a, err = d.lookupIn(ctx, n, part)
				if err = mapErr(err); err == nil {
					next = n.child(e, a)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if next.typ != icbfs.TypeDir {
			return nil, fmt.Errorf("%s: %w", next.path, drive.ErrNotDir)
		}
		n = next
	}
	return n, nil
}

// Move renames or moves an item; its id doesn't change.
func (d *Drive) Move(ctx context.Context, from drive.ItemRef, to string, replace bool) (drive.Item, error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return drive.Item{}, err
	}
	defer done()
	src, err := d.resolve(ctx, d.fs, from)
	if err != nil {
		return drive.Item{}, err
	}
	if src.path == "/" {
		return drive.Item{}, fmt.Errorf("the drive's root: %w", drive.ErrInvalidName)
	}
	if err := d.canDelete(ctx, src); err != nil {
		return drive.Item{}, err
	}
	parent, name, err := d.resolveParent(ctx, to, false)
	if err != nil {
		return drive.Item{}, err
	}
	what := needAddFile
	if src.typ == icbfs.TypeDir {
		what = needAddDir
	}
	if err := d.check(ctx, parent, what); err != nil {
		return drive.Item{}, err
	}
	if replace {
		if existing, err := d.resolvePath(ctx, d.fs, joinPath(parent.path, name)); err == nil {
			if err := d.canDelete(ctx, existing); err != nil {
				return drive.Item{}, err
			}
		}
	}
	dctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: drive.Actor(ctx), Path: d.fsPath(joinPath(parent.path, name))})
	if err := d.fs.Rename(dctx, src.parentKey(), src.name, parent.key, name, replace); err != nil {
		return drive.Item{}, mapErr(err)
	}
	d.changed(parentNode(src))
	d.changed(parent)
	return d.statItem(ctx, joinPath(parent.path, name))
}

// Delete moves an item to the trash; recursive for a non-empty
// directory, as one batch.
func (d *Drive) Delete(ctx context.Context, ref drive.ItemRef, recursive bool, cond drive.Condition) error {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return err
	}
	if n.path == "/" {
		return fmt.Errorf("the drive's root: %w", drive.ErrInvalidName)
	}
	if err := d.canDelete(ctx, n); err != nil {
		return err
	}
	if cond.IfMatch != "" {
		cur, err := d.fs.Store().Head(ctx, n.key)
		if err != nil {
			return mapErr(err)
		}
		if n.typ != icbfs.TypeFile || cur.VersionID != cond.IfMatch {
			return &drive.PreconditionError{Current: cur.VersionID}
		}
	}
	by, pass := drive.Actor(ctx), drive.Pass(ctx)
	if n.typ != icbfs.TypeDir {
		dctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: by, Path: d.fsPath(n.path), Pass: pass})
		if err := d.fs.Unlink(dctx, n.parentKey(), n.name); err != nil {
			return mapErr(err)
		}
		d.changed(parentNode(n))
		return nil
	}
	if !recursive {
		dctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: by, Path: d.fsPath(n.path), Pass: pass})
		if err := d.fs.Rmdir(dctx, n.parentKey(), n.name); err != nil {
			return mapErr(err)
		}
		d.changed(parentNode(n))
		return nil
	}
	// Everything under it must be deletable before anything is.
	var below []*node
	if err := d.walkBelow(ctx, n, func(c *node) error {
		below = append(below, c)
		return d.canDelete(ctx, c)
	}); err != nil {
		return err
	}
	batch, err := icbfs.NewUUIDv7()
	if err != nil {
		return err
	}
	// Children before their directory (walkBelow's order is
	// directories first, so go backwards).
	for i := len(below) - 1; i >= 0; i-- {
		c := below[i]
		dctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: by, Path: d.fsPath(c.path), Batch: batch, Pass: pass})
		if c.typ == icbfs.TypeDir {
			err = d.fs.Rmdir(dctx, c.parentKey(), c.name)
		} else {
			err = d.fs.Unlink(dctx, c.parentKey(), c.name)
		}
		if err != nil && !errors.Is(err, icbfs.ErrNotFound) {
			return mapErr(err)
		}
	}
	dctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: by, Path: d.fsPath(n.path), Batch: batch, EntryID: batch, Pass: pass})
	if err := d.fs.Rmdir(dctx, n.parentKey(), n.name); err != nil {
		return mapErr(err)
	}
	d.changed(parentNode(n))
	return nil
}

// walkBelow calls fn for everything under directory n, each directory
// before what's in it.
func (d *Drive) walkBelow(ctx context.Context, n *node, fn func(*node) error) error {
	entries, err := n.fsys.ReadDir(ctx, n.key)
	if err != nil {
		return mapErr(err)
	}
	chain := append(append([]ancestor(nil), n.chain...), ancestor{key: n.key, path: n.path, attr: n.attr})
	for _, e := range entries {
		a, err := n.fsys.Stat(ctx, e.UUID, e.Type)
		if err != nil {
			continue
		}
		c := &node{fsys: n.fsys, key: e.UUID, typ: e.Type, name: e.Name, path: joinPath(n.path, e.Name), attr: a, chain: chain}
		if err := fn(c); err != nil {
			return err
		}
		if e.Type == icbfs.TypeDir {
			if err := d.walkBelow(ctx, c, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// errDriveReadOnly is what a change through a Config.ReadOnly drive gets.
var errDriveReadOnly = fmt.Errorf("the drive is read-only: %w", drive.ErrReadOnly)

// begin starts a change. It fails if the drive is read-only or the
// filesystem has been archived (since this drive attached, too: one read
// of the master block per change), and dates everything the change
// writes at ctx's source time, if it has one (drive.WithSourceTime).
// done, to be deferred, then tells history caches that the past has
// changed.
func (d *Drive) begin(ctx context.Context) (context.Context, func(), error) {
	if d.cfg.ReadOnly {
		return ctx, nil, errDriveReadOnly
	}
	if err := d.fs.Refresh(ctx); err != nil {
		return ctx, nil, mapErr(err)
	}
	if d.fs.Archived() {
		return ctx, nil, fmt.Errorf("the filesystem is archived: %w", drive.ErrReadOnly)
	}
	t, ok := drive.SourceTime(ctx)
	if !ok {
		return ctx, func() {}, nil
	}
	if t.After(time.Now().Add(maxSourceTimeAhead)) {
		return ctx, nil, fmt.Errorf("source time %s is in the future: %w", t.Format(time.RFC3339), drive.ErrInvalid)
	}
	rec := &objstore.Recorder{}
	ctx = objstore.WithRecorder(objstore.WithDeclaredTime(ctx, t), rec)
	return ctx, func() {
		// What was written is dated, and anything written behind its
		// item's current state is put right: the item's current version
		// back on top, and a directory's copies from t on rewritten with
		// the change. Best effort, like a trash entry: the write itself
		// has happened, and the mirror's next pass finds what's undated.
		restored, _ := d.fs.RecordDates(ctx, rec)
		d.repairLateFolders(ctx, rec, restored)
		_ = d.fs.MarkPastChanged(ctx)
		d.views.lookAgain()
	}, nil
}

// maxSourceTimeAhead is how far past this machine's clock a source time
// may be: a source's clock and ours differ a little.
const maxSourceTimeAhead = time.Second

// SetOrigin records where an item was copied from.
func (d *Drive) SetOrigin(ctx context.Context, ref drive.ItemRef, o drive.Origin) error {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := d.fs.SetAppData(ctx, n.key, n.typ, icbfs.AppDriveOrigin, data); err != nil {
		return mapErr(err)
	}
	return d.recordOrigin(ctx, n.key, n.path, o)
}

// SetTimes sets an item's times at its source (drive.TimeSetter), in one
// metadata write: no new version of its content.
func (d *Drive) SetTimes(ctx context.Context, ref drive.ItemRef, modified, created time.Time) (drive.Item, error) {
	return d.SetInfo(ctx, ref, drive.ItemInfo{Modified: modified, Created: created})
}

// SetInfo records info about an item (drive.InfoSetter) in one metadata
// write: no new version of its content.
func (d *Drive) SetInfo(ctx context.Context, ref drive.ItemRef, info drive.ItemInfo) (drive.Item, error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return drive.Item{}, err
	}
	defer done()
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return drive.Item{}, err
	}
	if err := d.check(ctx, n, needWrite); err != nil {
		return drive.Item{}, err
	}
	app, err := appData(info.App, info.Origin)
	if err != nil {
		return drive.Item{}, err
	}
	fi := icbfs.ItemInfo{Btime: timeOrNil(info.Created), Mode: info.Mode, WindowsAttrs: info.WindowsAttrs, App: app}
	if mtime := timeOrNil(info.Modified); mtime != nil {
		fi.Mtime = mtime
		if n.typ != icbfs.TypeDir {
			// Tied to the content version it's set on, as SetAttr's is.
			head, err := d.fs.Store().Head(ctx, n.key)
			if err != nil {
				return drive.Item{}, mapErr(err)
			}
			fi.ContentETag = head.ETag
		}
	}
	if err := d.fs.SetItemInfo(ctx, n.key, n.typ, fi); err != nil {
		return drive.Item{}, mapErr(err)
	}
	if info.Origin != nil {
		if err := d.recordOrigin(ctx, n.key, n.path, *info.Origin); err != nil {
			return drive.Item{}, err
		}
	}
	return d.statItem(ctx, n.path)
}

// appData is the app map a caller's app keys and origin make: each key
// checked (drive.CheckAppKey), the origin as the drive's own key.
func appData(app map[string][]byte, origin *drive.Origin) (map[string][]byte, error) {
	out := make(map[string][]byte, len(app)+2)
	for k, v := range app {
		if err := drive.CheckAppKey(k); err != nil {
			return nil, err
		}
		out[k] = v
	}
	if origin != nil {
		data, err := json.Marshal(origin)
		if err != nil {
			return nil, err
		}
		out[icbfs.AppDriveOrigin] = data
	}
	return out, nil
}

// timeOrNil is t, or nil for the zero time: an icbfs time left as it is.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func decodeOrigin(data []byte) *drive.Origin {
	var o drive.Origin
	if json.Unmarshal(data, &o) != nil {
		return nil
	}
	return &o
}

// ID is the drive's registry id.
func (d *Drive) ID() string { return d.cfg.ID }
