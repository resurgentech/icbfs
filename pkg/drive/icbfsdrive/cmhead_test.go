package icbfsdrive

import (
	"errors"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestCMHead_AppObjectHeadAndPastMark (T86 lane CM): AppObjectHead tells
// an app object's current version, the one its writer got back, without
// its content, and ErrNotFound for none; PastMark is "" until the past
// is marked changed and moves with each mark. Each is one store request
// once the drive's root is known (the app object's access check reads
// the root, as AppObjectVersion's does).
func TestCMHead_AppObjectHeadAndPastMark(t *testing.T) {
	c := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDrive(t, c, icbfs.Settings{Name: "cmhead", Size: 1 << 30, RootMode: 0o755}, "")
	ctx := system()
	var _ drive.AppObjectHeader = d
	var _ drive.PastMarker = d

	if _, err := d.AppObjectHead(ctx, "clone.workdir.x"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("head of none: %v, want ErrNotFound", err)
	}
	v1, err := d.SetAppObjectIf(ctx, "clone.workdir.x", []byte(`{"a":1}`), drive.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	c.take()
	got, err := d.AppObjectHead(ctx, "clone.workdir.x")
	if err != nil || got != v1 {
		t.Fatalf("head: %q, %v; want %q", got, err, v1)
	}
	n, by := c.take()
	t.Logf("AppObjectHead: %d requests:%s", n, by)
	if n > 3 {
		t.Errorf("AppObjectHead took %d requests, want at most 3 (the root's two conditional reads and one HEAD):%s", n, by)
	}
	v2, err := d.SetAppObjectIf(ctx, "clone.workdir.x", []byte(`{"a":2}`), drive.Condition{IfMatch: v1})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.AppObjectHead(ctx, "clone.workdir.x"); err != nil || got != v2 || got == v1 {
		t.Fatalf("head after a write: %q, %v; want %q", got, err, v2)
	}

	m0, err := d.PastMark(ctx)
	if err != nil || m0 != "" {
		t.Fatalf("past mark before any: %q, %v", m0, err)
	}
	if err := d.fs.MarkPastChanged(ctx); err != nil {
		t.Fatal(err)
	}
	c.take()
	m1, err := d.PastMark(ctx)
	if err != nil || m1 == "" {
		t.Fatalf("past mark after a mark: %q, %v", m1, err)
	}
	c.mu.Lock()
	heads, all := c.n["HEAD past"], len(c.n)
	c.mu.Unlock()
	if heads != 1 || all != 1 {
		_, by := c.take()
		t.Fatalf("PastMark: want one HEAD of the mark and nothing else:%s", by)
	}
	c.take()
	if err := d.fs.MarkPastChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if m2, err := d.PastMark(ctx); err != nil || m2 == m1 {
		t.Fatalf("past mark after a second mark: %q, %v; want other than %q", m2, err, m1)
	}
}
