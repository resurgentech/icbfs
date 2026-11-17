package objstore

import (
	"context"
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
