package objstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Tests named for the rows of interloc's docs/STATES.md, section 1.3.

// st14Child, set, makes this test binary a settle that is killed: it
// opens the history cache at that path, starts a working table, fills
// it, and exits without clearing it.
const st14Child = "ICBFS_ST14_CHILD"

// TestST14_AKilledSettlesTableIsEmptiedByTheNext: a settle killed while
// its working table holds rows (another process, which exited without
// clearing them) has its rows emptied by the next settle's table on the
// same cache file, at once, not a day later; a run of a live settle of
// this process is left alone.
func TestST14_AKilledSettlesTableIsEmptiedByTheNext(t *testing.T) {
	ctx := context.Background()
	if path := os.Getenv(st14Child); path != "" {
		c, err := OpenHistoryCache(ctx, clockStore{}, path, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tab, err := c.SettleTable(ctx, "killed")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tab.ReadState(ctx, tm(1), []SettleEntry{{Name: "z", Item: "Z", Type: 1}}, []byte("rec"), "sum"); err != nil {
			t.Fatal(err)
		}
		if err := tab.EnsureState(ctx, tm(2)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // killed: nothing cleared, the database not even closed
	}
	path := filepath.Join(t.TempDir(), "history.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestST14_AKilledSettlesTableIsEmptiedByTheNext$", "-test.count=1")
	cmd.Env = append(os.Environ(), st14Child+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the killed settle: %v\n%s", err, out)
	}
	c, err := OpenHistoryCache(ctx, clockStore{}, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	count := func(table string) int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("settle_runs") != 1 || count("settle_entries") == 0 || count("settle_states") == 0 {
		t.Fatalf("the killed settle left no rows: runs %d, entries %d, states %d", count("settle_runs"), count("settle_entries"), count("settle_states"))
	}
	live, err := c.SettleTable(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"settle_entries", "settle_read", "settle_records", "settle_states", "settle_blocks"} {
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE run <> ?`, live.run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows of the killed settle left after the next settle's table", table, n)
		}
	}
	if n := count("settle_runs"); n != 1 {
		t.Errorf("%d runs after the next settle's table, want its own only", n)
	}
	// A live settle of this process keeps its rows when another starts.
	if _, err := live.ReadState(ctx, tm(1), []SettleEntry{{Name: "y", Item: "Y", Type: 1}}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SettleTable(ctx, "another"); err != nil {
		t.Fatal(err)
	}
	if es, _ := live.EntriesAt(ctx, tm(1)); entriesString(es) != "y:Y" {
		t.Errorf("a live settle's rows cleared by another's table: %v", es)
	}
}

// TestST26_ALiveRunIsNeverEmptied (ST-26, ST-14): a run whose process is
// alive on this machine keeps its rows however old it is: this process's
// own live settle and another live process's. A dead process's run on
// this machine goes at once; another machine's, or one with no process
// recorded, after a day.
func TestST26_ALiveRunIsNeverEmptied(t *testing.T) {
	ctx := context.Background()
	c, err := OpenHistoryCache(ctx, clockStore{}, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	own, err := c.SettleTable(ctx, "own")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { own.Clear(ctx) })
	alive := exec.Command("sleep", "120")
	if err := alive.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { alive.Process.Kill(); alive.Wait() })
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	old, recent := nanos(time.Now().Add(-48*time.Hour)), nanos(time.Now().Add(-time.Minute))
	if _, err := c.db.ExecContext(ctx, `UPDATE settle_runs SET started = ? WHERE run = ?`, old, own.run); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		run, host string
		pid       int
		started   int64
	}{
		{"alive-old", settleHost(), alive.Process.Pid, old},
		{"dead-recent", settleHost(), dead.Process.Pid, recent},
		{"elsewhere-recent", "another-machine", 4242, recent},
		{"elsewhere-old", "another-machine", 4242, old},
		{"unknown-old", "", 0, old},
	} {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO settle_runs (run, dir, started, host, pid) VALUES (?, 'd', ?, ?, ?)`, r.run, r.started, r.host, r.pid); err != nil {
			t.Fatal(err)
		}
	}
	next, err := c.SettleTable(ctx, "next")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Clear(ctx) })
	rows, err := c.db.QueryContext(ctx, `SELECT run FROM settle_runs`)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		kept[r] = true
	}
	rows.Close()
	for run, want := range map[string]bool{own.run: true, "alive-old": true, "dead-recent": false, "elsewhere-recent": true, "elsewhere-old": false, "unknown-old": false, next.run: true} {
		if kept[run] != want {
			t.Errorf("run %s: kept %v, want %v", run, kept[run], want)
		}
	}
	if err := own.Held(ctx); err != nil {
		t.Errorf("this process's live run, two days old: %v", err)
	}
}

// TestST26_HeldSaysWhenTheTableWasEmptied (ST-26): a table whose run's
// rows were emptied under it (by another process that took it for dead)
// says so (ErrSettleTableGone); one that holds its rows says nothing.
func TestST26_HeldSaysWhenTheTableWasEmptied(t *testing.T) {
	ctx := context.Background()
	c, err := OpenHistoryCache(ctx, clockStore{}, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	tab, err := c.SettleTable(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Held(ctx); err != nil {
		t.Fatalf("a fresh table: %v", err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM settle_runs WHERE run = ?`, tab.run); err != nil {
		t.Fatal(err)
	}
	if err := tab.Held(ctx); !errors.Is(err, ErrSettleTableGone) {
		t.Errorf("emptied: %v, want ErrSettleTableGone", err)
	}
	tab.Clear(ctx)
}
