package syncer_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/internal/syncer/memside"
)

// ntfsPair is an NTFS-like folder (ids, 100ns, creation times) and a
// drive that keeps creation times.
func ntfsPair(t *testing.T, mode syncer.Mode) *pair {
	t.Helper()
	p := newPair(t, mode)
	p.local = memside.NewFolder(true, 100*time.Nanosecond).WithCreated()
	p.remote = memside.NewDrive().WithCreated()
	p.engine = syncer.New(syncer.Config{Name: "test", Mode: mode, Local: p.local, Remote: p.remote.Drive(), State: p.state,
		CaseInsensitive: true, SpoolDir: t.TempDir()})
	return p
}

func created(t *testing.T, s *memside.Side, p string, want time.Time) {
	t.Helper()
	it, ok := s.Get(p)
	if !ok {
		t.Fatalf("%s: missing", p)
	}
	if !it.Created.Equal(want) {
		t.Fatalf("%s: created %v, want %v", p, it.Created, want)
	}
}

var born = time.Date(2019, 3, 4, 5, 6, 7, 800, time.UTC)

// TestCreationTimesAreCopied: a file's and a folder's creation times go
// to the drive with them; a later change to one alone is a metadata
// write, not a copy; and a drive seeded by a copy that didn't keep them
// gets them without copying anything.
func TestCreationTimesAreCopied(t *testing.T) {
	p := ntfsPair(t, syncer.OneWay)
	p.local.Put("Matters/brief.docx", "brief", t0).Created = born
	dir, _ := p.local.Get("Matters")
	dir.Created = born.Add(-time.Hour)
	p.pass(t)
	created(t, p.remote, "Matters/brief.docx", born)
	created(t, p.remote, "Matters", born.Add(-time.Hour))
	if rep := p.pass(t); rep.Planned != 0 {
		t.Fatalf("second pass planned %d", rep.Planned)
	}

	// Only the creation time changes (set by a tool): copied, no upload.
	writes := p.remote.Writes["Matters/brief.docx"]
	it, _ := p.local.Get("Matters/brief.docx")
	it.Created = born.AddDate(-1, 0, 0)
	if rep := p.pass(t); rep.Planned != 1 {
		t.Fatalf("planned %d, want one set-created", rep.Planned)
	}
	created(t, p.remote, "Matters/brief.docx", born.AddDate(-1, 0, 0))
	if p.remote.Writes["Matters/brief.docx"] != writes || p.local.Reads["Matters/brief.docx"] != 1 {
		t.Fatalf("a creation time change copied the file: %d writes, %d reads", p.remote.Writes["Matters/brief.docx"], p.local.Reads["Matters/brief.docx"])
	}

	// Seeded by a copy that kept the content but not the creation times:
	// adopted, the times set, nothing copied.
	s := ntfsPair(t, syncer.OneWay)
	s.local.Put("a.txt", "a", t0).Created = born
	s.remote.Put("a.txt", "a", t0).Created = t0.Add(time.Hour)
	s.pass(t)
	created(t, s.remote, "a.txt", born)
	if len(s.remote.Writes) != 0 || len(s.local.Reads) != 0 {
		t.Fatalf("adopting copied: writes %v, reads %v", s.remote.Writes, s.local.Reads)
	}
}

// TestCreationTimesComeDown: two-way, a file made on the drive keeps its
// creation time in the folder, and the next pass finds nothing to do.
func TestCreationTimesComeDown(t *testing.T) {
	p := ntfsPair(t, syncer.TwoWay)
	p.remote.Put("Clients/new.txt", "n", t0).Created = born
	d, _ := p.remote.Get("Clients")
	d.Created = born
	p.pass(t)
	created(t, p.local, "Clients/new.txt", born)
	created(t, p.local, "Clients", born)
	if rep := p.pass(t); rep.Planned != 0 {
		t.Fatalf("second pass planned %d", rep.Planned)
	}
}

// TestNoCreationTimesNoCopies: a folder that can't keep creation times
// (Linux, or a side without them) never asks the drive to set one.
func TestNoCreationTimesNoCopies(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.remote.WithCreated()
	p.local.Put("a.txt", "a", t0).Created = born // not reported: the folder keeps none
	p.pass(t)
	created(t, p.remote, "a.txt", time.Time{})
	if rep := p.pass(t); rep.Planned != 0 {
		t.Fatalf("second pass planned %d", rep.Planned)
	}
}

// TestSQLStateKeepsCreationTimes: the base's creation times survive a
// restart, and a state from before they were kept opens, with none.
func TestSQLStateKeepsCreationTimes(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", dir+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE base (
			key TEXT PRIMARY KEY, path TEXT NOT NULL, dir INTEGER NOT NULL, size INTEGER NOT NULL,
			mtime INTEGER NOT NULL, local_id TEXT NOT NULL, remote_id TEXT NOT NULL,
			remote_version TEXT NOT NULL, hash TEXT NOT NULL, acl_hash TEXT NOT NULL);
		INSERT INTO base VALUES ('a', 'a', 0, 1, 0, '', '', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st, err := syncer.OpenState(dir + "/old.db")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base, err := st.Load(ctx)
	if err != nil || len(base) != 1 || !base["a"].Created.IsZero() {
		t.Fatalf("old state: %+v, %v", base, err)
	}
	b := base["a"]
	b.Created = born
	base["a"] = b
	if err := st.Save(ctx, base); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = syncer.OpenState(dir + "/old.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if base, err = st.Load(ctx); err != nil || !base["a"].Created.Equal(born) {
		t.Fatalf("after a restart: %+v, %v", base, err)
	}
}
