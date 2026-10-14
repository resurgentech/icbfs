package driveapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

const token = "test-token"

var (
	auditMu sync.Mutex
	audited []AuditEvent
)

// serve starts the standalone API on a MinIO with drive "docs" (on
// filesystem docs, not yet created) and a side store, and returns its
// base URL.
func serve(t *testing.T) string {
	t.Helper()
	_, srv := miniotest.Start(t)
	st := &Standalone{
		Token: token,
		// "shared" is registered separately but is the same bucket, so
		// both side stores can be seen from "main"'s filesystem list.
		Buckets: map[string]pkgicbfs.S3{
			"main":   {Endpoint: srv.Endpoint, Bucket: srv.Bucket, AccessKey: srv.AccessKey, SecretKey: srv.SecretKey},
			"shared": {Endpoint: srv.Endpoint, Bucket: srv.Bucket, AccessKey: srv.AccessKey, SecretKey: srv.SecretKey},
		},
		Drives: map[string]StandaloneDrive{
			"docs":        {Name: "Docs", Bucket: "main", Filesystem: "docs", Root: "/"},
			"shared-docs": {Name: "Docs, via the shared bucket", Bucket: "shared", Filesystem: "docs", Root: "/"},
			"nowhere":     {Name: "Nowhere", Bucket: "main", Filesystem: "missing", Root: "/"},
		},
		SideStore:        &SideStoreAt{Bucket: "main", Filesystem: "host-side"},
		BucketSideStores: map[string]string{"main": "main-side"},
		Profiles: map[string]drive.Profile{
			"admin": {POSIX: &drive.POSIXProfile{Uid: 1000, Gids: []uint32{1000}}},
			"alice": {POSIX: &drive.POSIXProfile{Uid: 1000, Gids: []uint32{1000}}},
			"bob":   {POSIX: &drive.POSIXProfile{Uid: 1001, Gids: []uint32{1001}}},
		},
		Permissions: map[string][]string{
			"bob": {"drive.read"},
			"eve": {"drive.*"}, // no profile
		},
		CacheDir: t.TempDir(),
	}
	cfg, cleanup, err := st.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	stop := make(chan struct{})
	cfg.Stop = stop
	cfg.Audit = func(e AuditEvent) {
		auditMu.Lock()
		audited = append(audited, e)
		auditMu.Unlock()
	}
	app := fiber.New(fiber.Config{BodyLimit: 1 << 30, StreamRequestBody: true})
	Mount(app, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { close(stop); _ = app.Shutdown() })
	return "http://" + ln.Addr().String() + "/api/v1"
}

func client(t *testing.T, base, actor string) *ClientWithResponses {
	t.Helper()
	c, err := NewClientWithResponses(base, WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Drive-Actor", actor)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ptr[T any](v T) *T { return &v }

func code(t *testing.T, body []byte) string {
	t.Helper()
	var e Error
	if err := jsonUnmarshal(body, &e); err != nil {
		t.Fatalf("not an error body: %s", body)
	}
	return string(e.Code)
}

func TestAPI(t *testing.T) {
	base := serve(t)
	ctx := context.Background()
	admin := client(t, base, "admin")
	alice := client(t, base, "alice")
	bob := client(t, base, "bob")

	// storage: create the filesystem the drive is on
	cr, err := admin.CreateFilesystemWithResponse(ctx, "main", CreateFilesystemJSONRequestBody{
		Name: "docs", PrimaryMode: CreateFilesystemRequestPrimaryModePosix, MaxFileSize: 1 << 30, Size: 1 << 32})
	if err != nil || cr.StatusCode() != 201 || cr.JSON201.Name != "docs" {
		t.Fatalf("create filesystem: %v %d %s", err, cr.StatusCode(), cr.Body)
	}
	if cr, _ := admin.CreateFilesystemWithResponse(ctx, "main", CreateFilesystemJSONRequestBody{
		Name: "docs", PrimaryMode: CreateFilesystemRequestPrimaryModePosix, MaxFileSize: 1 << 30, Size: 1 << 32}); cr.StatusCode() != 409 {
		t.Fatalf("create again: %d", cr.StatusCode())
	}
	if lr, _ := admin.ListFilesystemsWithResponse(ctx, "main"); lr.StatusCode() != 200 || len(lr.JSON200.Filesystems) < 1 {
		t.Fatalf("list filesystems: %s", lr.Body)
	}

	dr, err := alice.GetDriveWithResponse(ctx, "docs")
	if err != nil || dr.StatusCode() != 200 || !dr.JSON200.Capabilities.Trash || dr.JSON200.Capabilities.Permissions != "posix" {
		t.Fatalf("drive: %v %s", err, dr.Body)
	}

	// write, then conditional writes
	put := func(c *ClientWithResponses, p, content string, params PutContentParams) *PutContentResponse {
		t.Helper()
		params.Path = p
		r, err := c.PutContentWithBodyWithResponse(ctx, "docs", &params, "text/markdown", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1 := put(alice, "/notes/a.md", "one\n", PutContentParams{Parents: ptr(true)})
	if r1.StatusCode() != 200 || r1.JSON200.Version == nil || r1.JSON200.ContentType == nil || *r1.JSON200.ContentType != "text/markdown" {
		t.Fatalf("put: %d %s", r1.StatusCode(), r1.Body)
	}
	v1, id := r1.JSON200.Version.Version, r1.JSON200.Id
	time.Sleep(1100 * time.Millisecond)
	r2 := put(alice, "/notes/a.md", "one\ntwo\n", PutContentParams{IfMatch: ptr(`"` + v1 + `"`)})
	if r2.StatusCode() != 200 {
		t.Fatalf("conditional put: %s", r2.Body)
	}
	v2 := r2.JSON200.Version.Version
	stale := put(alice, "/notes/a.md", "x", PutContentParams{IfMatch: ptr(v1)})
	if stale.StatusCode() != 412 || stale.HTTPResponse.Header.Get("x-drive-current-version") != v2 || code(t, stale.Body) != "precondition_failed" {
		t.Fatalf("stale put: %d %s", stale.StatusCode(), stale.Body)
	}
	if r := put(alice, "/notes/a.md", "x", PutContentParams{IfNoneMatch: ptr("*")}); r.StatusCode() != 412 {
		t.Fatalf("If-None-Match * on an existing file: %d", r.StatusCode())
	}

	// content: whole, a range, not modified, an old version, a download
	gc, err := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Path: ptr("/notes/a.md")})
	if err != nil || gc.StatusCode() != 200 || string(gc.Body) != "one\ntwo\n" || gc.HTTPResponse.Header.Get("ETag") != `"`+v2+`"` ||
		gc.HTTPResponse.Header.Get("x-drive-item-id") != id {
		t.Fatalf("content: %v %d %q %v", err, gc.StatusCode(), gc.Body, gc.HTTPResponse.Header)
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Path: ptr("/notes/a.md"), Range: ptr("bytes=4-6")}); gc.StatusCode() != 206 || string(gc.Body) != "two" {
		t.Fatalf("range: %d %q", gc.StatusCode(), gc.Body)
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Path: ptr("/notes/a.md"), IfNoneMatch: ptr(`"` + v2 + `"`)}); gc.StatusCode() != 304 {
		t.Fatalf("If-None-Match: %d", gc.StatusCode())
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Id: ptr(id), Version: ptr(v1), Download: ptr(true)}); string(gc.Body) != "one\n" ||
		!strings.HasPrefix(gc.HTTPResponse.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("old version: %q %v", gc.Body, gc.HTTPResponse.Header)
	}

	// stat, list, versions, diff
	sr, _ := alice.StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/notes/a.md")})
	if sr.StatusCode() != 200 || sr.JSON200.Rights == nil || !sr.JSON200.Rights.Write || *sr.JSON200.Owner != "uid:1000" {
		t.Fatalf("stat: %s", sr.Body)
	}
	if sr, _ := bob.StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/notes/a.md")}); sr.JSON200.Rights.Write {
		t.Fatalf("bob's rights: %s", sr.Body)
	}
	lr, _ := alice.ListItemsWithResponse(ctx, "docs", &ListItemsParams{Path: ptr("/"), Depth: ptr(2)})
	if lr.StatusCode() != 200 || len(lr.JSON200.Items) != 2 {
		t.Fatalf("list: %s", lr.Body)
	}
	vr, _ := alice.ListVersionsWithResponse(ctx, "docs", &ListVersionsParams{Id: ptr(id)})
	if vr.StatusCode() != 200 || len(vr.JSON200.Versions) != 2 || vr.JSON200.Id != id {
		t.Fatalf("versions: %s", vr.Body)
	}
	df, _ := alice.DiffVersionsWithResponse(ctx, "docs", &DiffVersionsParams{Id: id, From: ptr(v1), To: ptr(v2)})
	if df.StatusCode() != 200 || !strings.Contains(string(df.Body), "+two") {
		t.Fatalf("diff: %s", df.Body)
	}

	// permission and role errors
	if r := put(bob, "/notes/b.md", "bob", PutContentParams{}); r.StatusCode() != 403 || code(t, r.Body) != "forbidden" {
		t.Fatalf("bob (drive.read only) writes: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := client(t, base, "eve").StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/")}); r.StatusCode() != 403 || code(t, r.Body) != "no_access" {
		t.Fatalf("eve (no profile): %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := alice.StatItemWithResponse(ctx, "nope", &StatItemParams{Path: ptr("/")}); r.StatusCode() != 404 {
		t.Fatalf("unknown drive: %d", r.StatusCode())
	}
	if r, _ := alice.StatItemWithResponse(ctx, "nowhere", &StatItemParams{Path: ptr("/")}); r.StatusCode() != 404 {
		t.Fatalf("a drive on a missing filesystem: %d %s", r.StatusCode(), r.Body)
	}
	// Read-only for someone is not holding drive.write: no switch on the
	// drive itself.
	if r, _ := bob.MkdirWithResponse(ctx, "docs", MkdirJSONRequestBody{Path: "/x"}); r.StatusCode() != 403 || code(t, r.Body) != "forbidden" {
		t.Fatalf("mkdir without drive.write: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := alice.StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/missing")}); r.StatusCode() != 404 || code(t, r.Body) != "not_found" {
		t.Fatalf("missing item: %d %s", r.StatusCode(), r.Body)
	}
	noAuth, _ := NewClientWithResponses(base)
	if r, _ := noAuth.StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/")}); r.StatusCode() != 403 {
		t.Fatalf("no token: %d", r.StatusCode())
	}

	// names
	time.Sleep(1100 * time.Millisecond)
	nr, _ := alice.CreateNameWithResponse(ctx, "docs", CreateNameJSONRequestBody{Name: "draft-1"})
	if nr.StatusCode() != 201 {
		t.Fatalf("create name: %s", nr.Body)
	}
	time.Sleep(1100 * time.Millisecond)
	put(alice, "/notes/a.md", "three\n", PutContentParams{})
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Path: ptr("/notes/a.md"), Version: ptr("draft-1")}); string(gc.Body) != "one\ntwo\n" {
		t.Fatalf("content as of a name: %d %q", gc.StatusCode(), gc.Body)
	}
	if ln, _ := alice.ListNamesWithResponse(ctx, "docs"); len(ln.JSON200.Names) != 1 {
		t.Fatalf("names: %s", ln.Body)
	}
	if r, _ := alice.DeleteNameWithResponse(ctx, "docs", "draft-1"); r.StatusCode() != 204 {
		t.Fatalf("delete name: %d %s", r.StatusCode(), r.Body)
	}

	// move, copy, restore a version
	mv, _ := alice.MoveWithResponse(ctx, "docs", MoveJSONRequestBody{From: ptr("/notes/a.md"), To: "/notes/renamed.md"})
	if mv.StatusCode() != 200 || mv.JSON200.Id != id {
		t.Fatalf("move: %s", mv.Body)
	}
	cp, _ := alice.CopyWithResponse(ctx, "docs", CopyJSONRequestBody{FromId: ptr(id), Version: ptr(v1), To: "/notes/copy.md"})
	if cp.StatusCode() != 200 || cp.JSON200.Origin == nil || cp.JSON200.Origin.Version == nil || *cp.JSON200.Origin.Version != v1 {
		t.Fatalf("copy: %s", cp.Body)
	}
	rv, _ := alice.RestoreVersionWithResponse(ctx, "docs", RestoreVersionJSONRequestBody{Id: id, Version: v1})
	if rv.StatusCode() != 200 {
		t.Fatalf("restore version: %s", rv.Body)
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Id: ptr(id)}); string(gc.Body) != "one\n" {
		t.Fatalf("after restoring a version: %q", gc.Body)
	}

	// drafts
	draftParams := &PutDraftParams{Id: id, XDriveBaseVersion: ptr(rv.JSON200.Version.Version)}
	if r, _ := alice.PutDraftWithBodyWithResponse(ctx, "docs", draftParams, "application/octet-stream", strings.NewReader("drafty")); r.StatusCode() != 200 {
		t.Fatalf("put draft: %s", r.Body)
	}
	if sr, _ := alice.StatItemWithResponse(ctx, "docs", &StatItemParams{Id: ptr(id)}); sr.JSON200.HasDraft == nil || !*sr.JSON200.HasDraft {
		t.Fatalf("stat doesn't show the draft: %s", sr.Body)
	}
	if r, _ := alice.GetDraftWithResponse(ctx, "docs", &GetDraftParams{Id: id}); string(r.Body) != "drafty" {
		t.Fatalf("get draft: %s", r.Body)
	}
	if r, _ := alice.PublishDraftWithResponse(ctx, "docs", &PublishDraftParams{Id: id}); r.StatusCode() != 200 {
		t.Fatalf("publish: %s", r.Body)
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Id: ptr(id)}); string(gc.Body) != "drafty" {
		t.Fatalf("after publish: %q", gc.Body)
	}

	// Side stores: a bucket with its own keeps its drives' drafts there;
	// a drive on a bucket without one uses the deployment's.
	sideStores := func() string {
		t.Helper()
		lr, _ := admin.ListFilesystemsWithResponse(ctx, "main")
		var names []string
		for _, f := range lr.JSON200.Filesystems {
			if strings.HasSuffix(f.Name, "-side") {
				names = append(names, f.Name)
			}
		}
		return strings.Join(names, ",")
	}
	if got := sideStores(); got != "main-side" {
		t.Fatalf("side stores after a draft on main: %q", got)
	}
	if r, _ := alice.PutDraftWithBodyWithResponse(ctx, "shared-docs", draftParams, "application/octet-stream", strings.NewReader("elsewhere")); r.StatusCode() != 200 {
		t.Fatalf("put draft on the shared bucket: %s", r.Body)
	}
	if got := sideStores(); got != "main-side,host-side" && got != "host-side,main-side" {
		t.Fatalf("side stores after a draft on shared: %q", got)
	}
	if r, _ := alice.GetDraftWithResponse(ctx, "docs", &GetDraftParams{Id: id}); r.StatusCode() != 404 {
		t.Fatalf("the shared bucket's draft showed up in main's side store: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := alice.DiscardDraftWithResponse(ctx, "shared-docs", &DiscardDraftParams{Id: id}); r.StatusCode() != 204 {
		t.Fatalf("discard: %d %s", r.StatusCode(), r.Body)
	}

	// delete, trash, restore, purge
	if r, _ := alice.DeleteItemWithResponse(ctx, "docs", &DeleteItemParams{Path: ptr("/notes")}); r.StatusCode() != 409 || code(t, r.Body) != "not_empty" {
		t.Fatalf("delete a full directory: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := alice.DeleteItemWithResponse(ctx, "docs", &DeleteItemParams{Path: ptr("/notes"), Recursive: ptr(true)}); r.StatusCode() != 204 {
		t.Fatalf("recursive delete: %d %s", r.StatusCode(), r.Body)
	}
	tr, _ := alice.ListTrashWithResponse(ctx, "docs", &ListTrashParams{})
	if tr.StatusCode() != 200 || len(tr.JSON200.Entries) != 3 || *tr.JSON200.Entries[0].Path != "/notes" {
		t.Fatalf("trash: %s", tr.Body)
	}
	if r, _ := alice.RestoreTrashWithResponse(ctx, "docs", tr.JSON200.Entries[0].Id, RestoreTrashJSONRequestBody{}); r.StatusCode() != 200 {
		t.Fatalf("restore: %s", r.Body)
	}
	if gc, _ := alice.GetContentWithResponse(ctx, "docs", &GetContentParams{Path: ptr("/notes/copy.md")}); string(gc.Body) != "one\n" {
		t.Fatalf("restored batch: %q", gc.Body)
	}
	if r, _ := alice.PurgeWithResponse(ctx, "docs", &PurgeParams{Path: ptr("/notes/copy.md")}); r.StatusCode() != 204 {
		t.Fatalf("purge: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := alice.StatItemWithResponse(ctx, "docs", &StatItemParams{Path: ptr("/notes/copy.md")}); r.StatusCode() != 404 {
		t.Fatalf("after purge: %d", r.StatusCode())
	}

	// permissions and the identities scan
	pr, _ := alice.GetPermissionsWithResponse(ctx, "docs", &GetPermissionsParams{Id: ptr(id)})
	if pr.StatusCode() != 200 || pr.JSON200.Kind != PermissionsKindPosix || *pr.JSON200.Owner != "uid:1000" {
		t.Fatalf("permissions: %s", pr.Body)
	}
	jr, _ := alice.StartIdentitiesWithResponse(ctx, "docs")
	if jr.StatusCode() != 202 {
		t.Fatalf("start identities: %s", jr.Body)
	}
	var job *IdentitiesJob
	for i := 0; i < 50; i++ {
		g, _ := alice.GetIdentitiesWithResponse(ctx, "docs", jr.JSON202.Job)
		if g.JSON200 != nil && g.JSON200.Done {
			job = g.JSON200
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job == nil || job.Identities == nil || len(*job.Identities) == 0 {
		t.Fatalf("identities never finished: %+v", job)
	}

	// events: a write shows up as a change to its directory
	req, _ := http.NewRequest("GET", base+"/drives/docs/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Drive-Actor", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)
	if l, _ := lines.ReadString('\n'); !strings.HasPrefix(l, ": connected") {
		t.Fatalf("events start: %q", l)
	}
	put(alice, "/notes/event.md", "e", PutContentParams{})
	got := make(chan string, 1)
	go func() {
		for {
			l, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(l, "data: ") {
				got <- l
				return
			}
		}
	}()
	select {
	case l := <-got:
		if !strings.Contains(l, `"path":"/notes"`) {
			t.Fatalf("event: %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}

	// storage: resize, archive, prune
	if r, _ := admin.ResizeFilesystemWithResponse(ctx, "main", "docs", ResizeFilesystemJSONRequestBody{Size: 1 << 33}); r.StatusCode() != 200 || r.JSON200.Size != 1<<33 {
		t.Fatalf("resize: %s", r.Body)
	}
	if r, _ := admin.PruneFilesystemWithResponse(ctx, "main", "docs"); r.StatusCode() != 409 {
		t.Fatalf("prune before archive: %d %s", r.StatusCode(), r.Body)
	}
	if r, _ := admin.ArchiveFilesystemWithResponse(ctx, "main", "docs"); r.StatusCode() != 200 || !r.JSON200.Archived {
		t.Fatalf("archive: %s", r.Body)
	}
	if r := put(alice, "/after-archive", "x", PutContentParams{}); r.StatusCode() != 409 || code(t, r.Body) != "read_only" {
		t.Fatalf("write after archive: %d %s", r.StatusCode(), r.Body)
	}
	_ = bytes.MinRead
	_ = io.EOF

	auditMu.Lock()
	defer auditMu.Unlock()
	var sawStale bool
	for _, e := range audited {
		if e.Method == "PUT" && e.Status == 412 && e.Actor == "alice" && e.Path == "/notes/a.md" && e.Profile.POSIX != nil {
			sawStale = true
		}
	}
	if !sawStale || len(audited) < 40 {
		t.Fatalf("audit: %d events, the stale put among them: %v", len(audited), sawStale)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
