// Package driveapi is the drive API's HTTP layer: Fiber v3 handlers over
// package drive, mounted by host-server (Mount) or served standalone
// (cmd/drive-api). spec/openapi/drive.yaml is the contract; its models
// and a Go client are generated into api.gen.go.
package driveapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// DriveEntry is what host's registry knows of a drive.
type DriveEntry struct {
	Name     string
	Kind     string // "icbfs", ...
	Writable bool
}

// Config is what the handlers need from their host.
type Config struct {
	// Drive finds a drive by its registry id (drive.ErrNotFound if
	// there's none, or the actor may not know of it).
	Drive func(ctx context.Context, id string) (drive.Drive, DriveEntry, error)
	// Authorize is host's check: may this request's actor do
	// permission (drive.read, drive.write, ...) on the drive? It answers
	// the actor and its profile on the drive (drive.ErrNoAccess when it
	// has none), or drive.ErrNotFound / drive.ErrForbidden.
	Authorize func(c fiber.Ctx, driveID, permission string) (actor string, p drive.Profile, err error)
	// AuthorizeStorage is the same for a registered bucket
	// (storage.read, storage.admin); the profile is who a new
	// filesystem's root starts out owned by.
	AuthorizeStorage func(c fiber.Ctx, bucket, permission string) (actor string, p drive.Profile, err error)
	// Bucket opens a registered bucket.
	Bucket func(ctx context.Context, id string) (pkgicbfs.Store, error)
	// Drafts finds a drive's drafts: in the side store of the drive's own
	// bucket where it has one, else the deployment's. nil (or a nil
	// answer) means none: the draft endpoints answer not_supported.
	Drafts func(ctx context.Context, driveID string) (*drive.Drafts, error)
	// Stop, when closed, ends every event stream (before the app shuts
	// down, which otherwise waits for them).
	Stop <-chan struct{}
	// Audit, if set, is told of every drive operation once it's answered
	// — for host's audit log (the logging chain).
	Audit func(AuditEvent)
}

// AuditEvent is one drive operation, as answered.
type AuditEvent struct {
	Time       time.Time     `json:"time"`
	Drive      string        `json:"drive"`
	Permission string        `json:"permission"`
	Method     string        `json:"method"`
	Route      string        `json:"route"`
	Path       string        `json:"path,omitempty"`
	ID         string        `json:"id,omitempty"`
	Actor      string        `json:"actor"`
	Profile    drive.Profile `json:"profile"`
	Status     int           `json:"status"`
}

// Mount adds the API's routes under r (at /api/v1).
func Mount(r fiber.Router, cfg Config) {
	s := &server{cfg: cfg, jobs: map[string]*job{}}
	g := r.Group("/api/v1")
	d := g.Group("/drives/:drive")
	d.Get("", s.op("drive.read", s.getDrive))
	d.Get("/list", s.op("drive.read", s.list))
	d.Get("/stat", s.op("drive.read", s.stat))
	d.Get("/content", s.op("drive.read", s.getContent))
	d.Head("/content", s.op("drive.read", s.getContent))
	d.Put("/content", s.op("drive.write", s.putContent))
	d.Delete("/content", s.op("drive.delete", s.deleteItem))
	d.Get("/versions", s.op("drive.read", s.versions))
	d.Get("/diff", s.op("drive.read", s.diff))
	d.Post("/mkdir", s.op("drive.write", s.mkdir))
	d.Post("/move", s.op("drive.write", s.move))
	d.Post("/copy", s.op("drive.write", s.copy))
	d.Post("/restore-version", s.op("drive.write", s.restoreVersion))
	d.Get("/names", s.op("drive.read", s.names))
	d.Post("/names", s.op("drive.name", s.createName))
	d.Delete("/names/:name", s.op("drive.name", s.deleteName))
	d.Get("/trash", s.op("drive.read", s.trash))
	d.Post("/trash/:entry/restore", s.op("drive.restore", s.restore))
	d.Delete("/purge", s.op("drive.purge", s.purge))
	d.Get("/drafts", s.op("drive.read", s.getDraft))
	d.Put("/drafts", s.op("drive.write", s.putDraft))
	d.Delete("/drafts", s.op("drive.write", s.discardDraft))
	d.Post("/drafts/publish", s.op("drive.write", s.publishDraft))
	d.Post("/identities", s.op("drive.permissions.read", s.startIdentities))
	d.Get("/identities/:job", s.op("drive.permissions.read", s.getIdentities))
	d.Get("/permissions", s.op("drive.permissions.read", s.permissions))
	d.Get("/events", s.op("drive.read", s.events))

	st := g.Group("/storage/:bucket/filesystems")
	st.Get("", s.storage("storage.read", s.listFilesystems))
	st.Post("", s.storage("storage.admin", s.createFilesystem))
	st.Get("/:name", s.storage("storage.read", s.getFilesystem))
	st.Patch("/:name", s.storage("storage.admin", s.resizeFilesystem))
	st.Delete("/:name", s.storage("storage.admin", s.pruneFilesystem))
	st.Post("/:name/archive", s.storage("storage.admin", s.archiveFilesystem))
}

type server struct {
	cfg  Config
	mu   sync.Mutex
	jobs map[string]*job
}

// req is one drive request, authorized.
type req struct {
	c       fiber.Ctx
	ctx     context.Context
	id      string
	d       drive.Drive
	info    DriveEntry
	actor   string
	profile drive.Profile
}

// writes are the permissions that change the drive.
var writes = map[string]bool{"drive.write": true, "drive.delete": true, "drive.restore": true, "drive.purge": true, "drive.name": true}

func (s *server) op(permission string, h func(*req) error) fiber.Handler {
	return func(c fiber.Ctx) error {
		// Fiber's strings point into the request's buffers, reused once
		// the request is answered; what outlives it (in a context, a
		// job, an audit event) is copied.
		id := strings.Clone(c.Params("drive"))
		actor, p, err := s.cfg.Authorize(c, id, permission)
		if err != nil {
			return writeErr(c, err)
		}
		actor = strings.Clone(actor)
		d, info, err := s.cfg.Drive(c.Context(), id)
		if err != nil {
			return writeErr(c, err)
		}
		if writes[permission] && !info.Writable {
			return writeErr(c, fmt.Errorf("drive %s: %w", id, drive.ErrReadOnly))
		}
		ctx := drive.WithProfile(drive.WithActor(c.Context(), actor), p)
		err = h(&req{c: c, ctx: ctx, id: id, d: d, info: info, actor: actor, profile: p})
		if s.cfg.Audit != nil {
			s.cfg.Audit(AuditEvent{Time: time.Now().UTC(), Drive: id, Permission: permission, Method: strings.Clone(c.Method()),
				Route: strings.Clone(c.Route().Path), Path: strings.Clone(c.Query("path")), ID: strings.Clone(c.Query("id")), Actor: actor, Profile: p,
				Status: c.Response().StatusCode()})
		}
		return err
	}
}

// ref is the item a request names (id wins over path).
func (r *req) ref() drive.ItemRef {
	return drive.ItemRef{ID: r.query("id"), Path: r.query("path")}
}

func (r *req) flag(name string) bool {
	v, _ := strconv.ParseBool(r.query(name))
	return v
}

// at reads asOf and version: a version that's one of the drive's names
// is the name; otherwise it's a version id.
func (r *req) at() (drive.At, error) {
	var at drive.At
	if s := r.query("asOf"); s != "" {
		t, err := icbfs.ParseAsOf(s, time.UTC)
		if err != nil {
			return at, fmt.Errorf("%w: %v", drive.ErrInvalid, err)
		}
		at.Time = t
	}
	if v := r.query("version"); v != "" {
		if r.isName(v) {
			at.Name = v
		} else {
			at.Version = v
		}
	}
	return at, nil
}

func (r *req) isName(v string) bool {
	if icbfs.CheckVersionName(v) != nil {
		return false
	}
	names, err := r.d.Names(r.ctx)
	if err != nil {
		return false
	}
	for _, n := range names {
		if n.Name == v {
			return true
		}
	}
	return false
}

func (r *req) bind(v any) error {
	body := r.c.Body()
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %v", drive.ErrInvalid, err)
	}
	return nil
}

// --- errors ---

type apiError struct {
	Error          string `json:"error"`
	Code           string `json:"code"`
	CurrentVersion string `json:"currentVersion,omitempty"`
}

var errCodes = []struct {
	err    error
	status int
	code   string
}{
	{drive.ErrNotFound, http.StatusNotFound, "not_found"},
	{drive.ErrForbidden, http.StatusForbidden, "forbidden"},
	{drive.ErrNoAccess, http.StatusForbidden, "no_access"},
	{drive.ErrNotSupported, http.StatusNotImplemented, "not_supported"},
	{drive.ErrExists, http.StatusConflict, "exists"},
	{drive.ErrNotEmpty, http.StatusConflict, "not_empty"},
	{drive.ErrNotDir, http.StatusConflict, "not_dir"},
	{drive.ErrIsDir, http.StatusConflict, "is_dir"},
	{drive.ErrHasLinks, http.StatusConflict, "has_other_names"},
	{drive.ErrPrecondition, http.StatusPreconditionFailed, "precondition_failed"},
	{drive.ErrInvalidName, http.StatusBadRequest, "invalid_name"},
	{drive.ErrInvalid, http.StatusBadRequest, "invalid"},
	{drive.ErrTooLarge, http.StatusRequestEntityTooLarge, "too_large"},
	{drive.ErrReadOnly, http.StatusConflict, "read_only"},
	{drive.ErrBusy, http.StatusServiceUnavailable, "busy"},
	{pkgicbfs.ErrNotFound, http.StatusNotFound, "not_found"},
	{pkgicbfs.ErrExists, http.StatusConflict, "exists"},
	{pkgicbfs.ErrNotArchived, http.StatusConflict, "read_only"},
}

func writeErr(c fiber.Ctx, err error) error {
	status, code := http.StatusInternalServerError, "internal"
	for _, e := range errCodes {
		if errors.Is(err, e.err) {
			status, code = e.status, e.code
			break
		}
	}
	body := apiError{Error: err.Error(), Code: code}
	var pe *drive.PreconditionError
	if errors.As(err, &pe) && pe.Current != "" {
		body.CurrentVersion = pe.Current
		c.Set("x-drive-current-version", pe.Current)
	}
	return c.Status(status).JSON(body)
}

// --- the drive ---

func (s *server) getDrive(r *req) error {
	out := map[string]any{"id": r.id, "name": r.info.Name, "kind": r.info.Kind, "writable": r.info.Writable,
		"capabilities": r.d.Capabilities(), "profile": r.profile, "actor": r.actor}
	return r.c.JSON(out)
}

// withDrafts marks items that have drafts.
func (s *server) withDrafts(r *req, items []drive.Item) {
	if len(items) == 0 {
		return
	}
	ds, err := s.draftsOf(r)
	if err != nil || ds == nil {
		return
	}
	only := map[string]bool{}
	for _, it := range items {
		if it.Type == drive.TypeFile {
			only[it.ID] = true
		}
	}
	by, err := ds.ByItem(r.ctx, r.id, only)
	if err != nil {
		return
	}
	for i := range items {
		if users, ok := by[items[i].ID]; ok {
			items[i].HasDraft, items[i].DraftsBy = true, users
		}
	}
}

func (s *server) list(r *req) error {
	at, err := r.at()
	if err != nil {
		return writeErr(r.c, err)
	}
	depth, _ := strconv.Atoi(r.query("depth"))
	limit, _ := strconv.Atoi(r.query("limit"))
	ref := r.ref()
	if ref.ID == "" && ref.Path == "" {
		ref.Path = "/"
	}
	page, err := r.d.List(r.ctx, ref, drive.ListOptions{At: at, Depth: depth, Limit: limit, Cursor: r.query("cursor"), Sort: r.query("sort")})
	if err != nil {
		return writeErr(r.c, err)
	}
	if at.Now() {
		s.withDrafts(r, page.Items)
	}
	return r.c.JSON(page)
}

func (s *server) stat(r *req) error {
	at, err := r.at()
	if err != nil {
		return writeErr(r.c, err)
	}
	it, err := r.d.Stat(r.ctx, r.ref(), at)
	if err != nil {
		return writeErr(r.c, err)
	}
	if at.Now() {
		items := []drive.Item{it}
		s.withDrafts(r, items)
		it = items[0]
	}
	return r.c.JSON(it)
}

func (s *server) getContent(r *req) error {
	at, err := r.at()
	if err != nil {
		return writeErr(r.c, err)
	}
	it, err := r.d.Stat(r.ctx, r.ref(), at)
	if err != nil {
		return writeErr(r.c, err)
	}
	if it.Type != drive.TypeFile {
		return writeErr(r.c, drive.ErrIsDir)
	}
	etag := ""
	if it.Version != nil {
		etag = `"` + it.Version.ID + `"`
		r.c.Set("ETag", etag)
		r.c.Set("Last-Modified", it.Version.Time.UTC().Format(http.TimeFormat))
	}
	r.c.Set("x-drive-item-id", it.ID)
	r.c.Set("Accept-Ranges", "bytes")
	r.c.Set("Content-Type", it.ContentType)
	if r.flag("download") {
		r.c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", it.Name))
	}
	if inm := r.c.Get("If-None-Match"); inm != "" && etag != "" && (inm == etag || strings.Trim(inm, `"`) == it.Version.ID) {
		return r.c.SendStatus(http.StatusNotModified)
	}
	var rng *drive.Range
	status := http.StatusOK
	if h := r.c.Get("Range"); h != "" {
		off, length, ok := parseRange(h, it.Size)
		if !ok {
			r.c.Set("Content-Range", fmt.Sprintf("bytes */%d", it.Size))
			return r.c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
		}
		rng = &drive.Range{Offset: off, Length: length}
		status = http.StatusPartialContent
		r.c.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+length-1, it.Size))
	}
	size := it.Size
	if rng != nil {
		size = rng.Length
	}
	if r.c.Method() == http.MethodHead {
		r.c.Set("Content-Length", strconv.FormatInt(size, 10))
		return r.c.SendStatus(status)
	}
	if it.Version != nil && at.Version == "" {
		at.Version = it.Version.ID // read exactly what was described
	}
	body, _, err := r.d.Read(r.ctx, drive.ItemRef{ID: it.ID, Path: it.Path}, at, rng)
	if err != nil {
		return writeErr(r.c, err)
	}
	r.c.Status(status)
	return r.c.SendStream(body, int(size))
}

// parseRange reads a single "bytes=a-b" range.
func parseRange(h string, size int64) (off, length int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, size > 0
	default:
		start, err := strconv.ParseInt(a, 10, 64)
		if err != nil || start >= size {
			return 0, 0, false
		}
		end := size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
				return 0, 0, false
			}
			if end >= size {
				end = size - 1
			}
		}
		return start, end - start + 1, true
	}
}

func condition(c fiber.Ctx) drive.Condition {
	return drive.Condition{IfMatch: strings.Trim(c.Get("If-Match"), `"`), IfNoneMatch: c.Get("If-None-Match") == "*"}
}

// body is the request body, streamed when the app streams bodies.
func body(c fiber.Ctx) io.Reader {
	if s := c.Request().BodyStream(); s != nil {
		return s
	}
	return bytes.NewReader(c.Body())
}

func (s *server) putContent(r *req) error {
	p := r.query("path")
	if p == "" {
		return writeErr(r.c, fmt.Errorf("path is required: %w", drive.ErrInvalid))
	}
	ct := r.c.Get("Content-Type")
	if ct == "application/octet-stream" {
		ct = "" // the default: the name's type decides
	}
	it, err := r.d.Write(r.ctx, p, body(r.c), condition(r.c), drive.WriteOptions{Parents: r.flag("parents"), ContentType: ct})
	if err != nil {
		return writeErr(r.c, err)
	}
	if it.Version != nil {
		r.c.Set("ETag", `"`+it.Version.ID+`"`)
	}
	return r.c.JSON(it)
}

func (s *server) deleteItem(r *req) error {
	if err := r.d.Delete(r.ctx, r.ref(), r.flag("recursive"), condition(r.c)); err != nil {
		return writeErr(r.c, err)
	}
	return r.c.SendStatus(http.StatusNoContent)
}

func (s *server) versions(r *req) error {
	ref := r.ref()
	vs, err := r.d.Versions(r.ctx, ref)
	if err != nil {
		return writeErr(r.c, err)
	}
	id := ref.ID
	if id == "" {
		if it, err := r.d.Stat(r.ctx, ref, drive.At{}); err == nil {
			id = it.ID
		}
	}
	return r.c.JSON(map[string]any{"id": id, "versions": vs})
}

func (s *server) diff(r *req) error {
	out, err := drive.Diff(r.ctx, r.d, drive.ItemRef{ID: r.query("id")}, r.query("from"), r.query("to"))
	if err != nil {
		return writeErr(r.c, err)
	}
	r.c.Set("Content-Type", "text/plain; charset=utf-8")
	return r.c.SendString(out)
}

func (s *server) mkdir(r *req) error {
	var in MkdirRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	it, err := r.d.Mkdir(r.ctx, in.Path, in.Parents != nil && *in.Parents)
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolv(p *bool) bool { return p != nil && *p }

func (s *server) move(r *req) error {
	var in MoveRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	it, err := r.d.Move(r.ctx, drive.ItemRef{ID: str(in.FromId), Path: str(in.From)}, in.To, boolv(in.Replace))
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

func (s *server) copy(r *req) error {
	var in CopyRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	var at drive.At
	if in.AsOf != nil {
		at.Time = *in.AsOf
	}
	if v := str(in.Version); v != "" {
		if r.isName(v) {
			at.Name = v
		} else {
			at.Version = v
		}
	}
	if t := str(in.ToDrive); t != "" && t != r.id {
		actor, p, err := s.cfg.Authorize(r.c, t, "drive.write")
		if err != nil {
			return writeErr(r.c, err)
		}
		d, info, err := s.cfg.Drive(r.ctx, t)
		if err != nil {
			return writeErr(r.c, err)
		}
		if !info.Writable {
			return writeErr(r.c, fmt.Errorf("drive %s: %w", t, drive.ErrReadOnly))
		}
		// The target's checks are made as the actor's profile there.
		dctx := drive.WithProfile(drive.WithActor(r.ctx, actor), p)
		it, err := copyAcross(r.ctx, dctx, r.d, r.id, drive.ItemRef{ID: str(in.FromId), Path: str(in.From)}, at, d, in.To)
		if err != nil {
			return writeErr(r.c, err)
		}
		return r.c.JSON(it)
	}
	it, err := drive.Copy(r.ctx, r.d, r.id, drive.ItemRef{ID: str(in.FromId), Path: str(in.From)}, at, r.d, in.To, drive.Condition{})
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

// copyAcross copies between drives where the actor has different
// profiles: reads as the source's, writes as the target's.
func copyAcross(srcCtx, dstCtx context.Context, src drive.Drive, srcID string, from drive.ItemRef, at drive.At, dst drive.Drive, to string) (drive.Item, error) {
	return drive.Copy(srcCtx, profileSwitch{src, srcCtx}, srcID, from, at, profileSwitch{dst, dstCtx}, to, drive.Condition{})
}

// profileSwitch runs every call with its own context's profile.
type profileSwitch struct {
	drive.Drive
	ctx context.Context
}

func (p profileSwitch) with(ctx context.Context) context.Context {
	prof, _ := drive.ProfileFrom(p.ctx)
	return drive.WithProfile(drive.WithActor(ctx, drive.Actor(p.ctx)), prof)
}

func (p profileSwitch) Stat(ctx context.Context, ref drive.ItemRef, at drive.At) (drive.Item, error) {
	return p.Drive.Stat(p.with(ctx), ref, at)
}
func (p profileSwitch) List(ctx context.Context, ref drive.ItemRef, o drive.ListOptions) (drive.Page, error) {
	return p.Drive.List(p.with(ctx), ref, o)
}
func (p profileSwitch) Read(ctx context.Context, ref drive.ItemRef, at drive.At, rng *drive.Range) (io.ReadCloser, drive.Item, error) {
	return p.Drive.Read(p.with(ctx), ref, at, rng)
}
func (p profileSwitch) Write(ctx context.Context, path string, b io.Reader, c drive.Condition, o drive.WriteOptions) (drive.Item, error) {
	return p.Drive.Write(p.with(ctx), path, b, c, o)
}
func (p profileSwitch) Mkdir(ctx context.Context, path string, parents bool) (drive.Item, error) {
	return p.Drive.Mkdir(p.with(ctx), path, parents)
}
func (p profileSwitch) SetOrigin(ctx context.Context, ref drive.ItemRef, o drive.Origin) error {
	return p.Drive.SetOrigin(p.with(ctx), ref, o)
}

func (s *server) restoreVersion(r *req) error {
	var in RestoreVersionRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	ref := drive.ItemRef{ID: in.Id}
	cur, err := r.d.Stat(r.ctx, ref, drive.At{})
	if err != nil {
		return writeErr(r.c, err)
	}
	old, _, err := r.d.Read(r.ctx, ref, drive.At{Version: in.Version}, nil)
	if err != nil {
		return writeErr(r.c, err)
	}
	defer old.Close()
	cond := drive.Condition{}
	if cur.Version != nil {
		cond.IfMatch = cur.Version.ID
	}
	it, err := r.d.Write(r.ctx, cur.Path, old, cond, drive.WriteOptions{})
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

func (s *server) names(r *req) error {
	ns, err := r.d.Names(r.ctx)
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(map[string]any{"names": ns})
}

func (s *server) createName(r *req) error {
	var in CreateNameRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	var at time.Time
	if in.At != nil {
		at = *in.At
	}
	fsWide := in.Scope != nil && *in.Scope == CreateNameRequestScopeFilesystem
	n, err := r.d.CreateName(r.ctx, in.Name, at, fsWide)
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.Status(http.StatusCreated).JSON(n)
}

func (s *server) deleteName(r *req) error {
	if err := r.d.DeleteName(r.ctx, r.param("name")); err != nil {
		return writeErr(r.c, err)
	}
	return r.c.SendStatus(http.StatusNoContent)
}

func (s *server) trash(r *req) error {
	limit, _ := strconv.Atoi(r.query("limit"))
	page, err := r.d.Trash(r.ctx, drive.TrashOptions{Path: r.query("path"), Limit: limit, Cursor: r.query("cursor")})
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(page)
}

func (s *server) restore(r *req) error {
	var in RestoreRequest
	if err := r.bind(&in); err != nil {
		return writeErr(r.c, err)
	}
	it, err := r.d.Restore(r.ctx, r.param("entry"), drive.RestoreOptions{To: str(in.To), Parents: boolv(in.Parents), Replace: boolv(in.Replace)})
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

func (s *server) purge(r *req) error {
	ref := r.ref()
	id := ref.ID
	if id == "" {
		it, err := r.d.Stat(r.ctx, ref, drive.At{})
		if err != nil {
			return writeErr(r.c, err)
		}
		id = it.ID
	}
	if err := r.d.Purge(r.ctx, ref, r.flag("allNames")); err != nil {
		return writeErr(r.c, err)
	}
	ds, err := s.draftsOf(r)
	if err != nil {
		return writeErr(r.c, err)
	}
	if ds != nil {
		if err := ds.DeleteAll(r.ctx, r.id, id); err != nil {
			return writeErr(r.c, err)
		}
	}
	return r.c.SendStatus(http.StatusNoContent)
}

// --- drafts ---

// draftsOf is the request's drive's drafts, or nil if it has none.
func (s *server) draftsOf(r *req) (*drive.Drafts, error) {
	if s.cfg.Drafts == nil {
		return nil, nil
	}
	return s.cfg.Drafts(r.ctx, r.id)
}

func (s *server) drafts(r *req) (*drive.Drafts, string, error) {
	ds, err := s.draftsOf(r)
	if err != nil {
		return nil, "", err
	}
	if ds == nil {
		return nil, "", fmt.Errorf("drafts: %w", drive.ErrNotSupported)
	}
	if r.actor == "" {
		return nil, "", fmt.Errorf("drafts need an actor: %w", drive.ErrInvalid)
	}
	id := r.query("id")
	if id == "" {
		return nil, "", fmt.Errorf("id is required: %w", drive.ErrInvalid)
	}
	// The file must exist, and the actor see it.
	if _, err := r.d.Stat(r.ctx, drive.ItemRef{ID: id}, drive.At{}); err != nil {
		return nil, "", err
	}
	return ds, id, nil
}

func (s *server) getDraft(r *req) error {
	ds, id, err := s.drafts(r)
	if err != nil {
		return writeErr(r.c, err)
	}
	d, data, err := ds.Get(r.ctx, r.id, id, r.actor)
	if err != nil {
		return writeErr(r.c, err)
	}
	r.c.Set("x-drive-base-version", d.Base)
	r.c.Set("Last-Modified", d.Saved.UTC().Format(http.TimeFormat))
	r.c.Set("Content-Type", "application/octet-stream")
	return r.c.Send(data)
}

func (s *server) putDraft(r *req) error {
	ds, id, err := s.drafts(r)
	if err != nil {
		return writeErr(r.c, err)
	}
	data, err := io.ReadAll(io.LimitReader(body(r.c), 256<<20+1))
	if err != nil {
		return writeErr(r.c, err)
	}
	if len(data) > 256<<20 {
		return writeErr(r.c, fmt.Errorf("a draft: %w", drive.ErrTooLarge))
	}
	d, err := ds.Save(r.ctx, r.id, id, r.actor, strings.Trim(r.c.Get("x-drive-base-version"), `"`), data)
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(d)
}

func (s *server) discardDraft(r *req) error {
	ds, id, err := s.drafts(r)
	if err != nil {
		return writeErr(r.c, err)
	}
	if err := ds.Discard(r.ctx, r.id, id, r.actor); err != nil {
		return writeErr(r.c, err)
	}
	return r.c.SendStatus(http.StatusNoContent)
}

func (s *server) publishDraft(r *req) error {
	ds, id, err := s.drafts(r)
	if err != nil {
		return writeErr(r.c, err)
	}
	it, err := ds.Publish(r.ctx, r.d, r.id, id, r.actor, r.flag("force"))
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(it)
}

// --- permissions ---

type job struct {
	mu      sync.Mutex
	id      string
	done    bool
	scanned int
	err     string
	found   []drive.Identity
	created time.Time
}

func (s *server) startIdentities(r *req) error {
	id, err := icbfs.NewUUIDv7()
	if err != nil {
		return writeErr(r.c, err)
	}
	j := &job{id: id, created: time.Now()}
	s.mu.Lock()
	for k, old := range s.jobs { // jobs are kept for an hour
		if time.Since(old.created) > time.Hour {
			delete(s.jobs, k)
		}
	}
	s.jobs[r.id+"/"+id] = j
	s.mu.Unlock()
	ctx := context.WithoutCancel(r.ctx)
	go func() {
		found, err := r.d.Identities(ctx, func(n int) {
			j.mu.Lock()
			j.scanned = n
			j.mu.Unlock()
		})
		j.mu.Lock()
		defer j.mu.Unlock()
		j.done, j.found = true, found
		if err != nil {
			j.err = err.Error()
		}
	}()
	return r.c.Status(http.StatusAccepted).JSON(map[string]any{"job": id, "done": false, "scanned": 0})
}

func (s *server) getIdentities(r *req) error {
	s.mu.Lock()
	j := s.jobs[r.id+"/"+r.param("job")]
	s.mu.Unlock()
	if j == nil {
		return writeErr(r.c, fmt.Errorf("job: %w", drive.ErrNotFound))
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]any{"job": j.id, "done": j.done, "scanned": j.scanned}
	if j.err != "" {
		out["error"] = j.err
	}
	if j.done && j.err == "" {
		out["identities"] = j.found
	}
	return r.c.JSON(out)
}

func (s *server) permissions(r *req) error {
	p, err := r.d.Permissions(r.ctx, r.ref())
	if err != nil {
		return writeErr(r.c, err)
	}
	return r.c.JSON(p)
}

// --- events ---

func (s *server) events(r *req) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.ctx))
	ch, err := r.d.Changes(ctx)
	if err != nil {
		cancel()
		return writeErr(r.c, err)
	}
	r.c.Set("Content-Type", "text/event-stream")
	r.c.Set("Cache-Control", "no-cache")
	r.c.Set("Connection", "keep-alive")
	return r.c.SendStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		fmt.Fprint(w, ": connected\n\n")
		if w.Flush() != nil {
			return
		}
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-s.cfg.Stop:
				return
			case change, ok := <-ch:
				if !ok {
					return
				}
				data, _ := json.Marshal(change)
				fmt.Fprintf(w, "event: changed\ndata: %s\n\n", data)
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
			}
			if w.Flush() != nil {
				return // the client went away
			}
		}
	})
}

// query and param copy what they return, out of the request's buffers
// (see op), since drives may keep it (an id in a cache, say).
func (r *req) query(k string) string  { return strings.Clone(r.c.Query(k)) }
func (r *req) param(k string) string  { return strings.Clone(r.c.Params(k)) }
func (r *sreq) param(k string) string { return strings.Clone(r.c.Params(k)) }
