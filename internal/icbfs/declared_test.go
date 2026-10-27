package icbfs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestUnfinishedErased: in a filesystem written in a source's time, a
// version its item's .version doesn't list (written, never dated: the
// change that wrote it failed) is erased by the next change that has to
// list the item's versions, a late one; the late change still puts the
// latest-dated version back on top.
func TestUnfinishedErased(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	if err := f.MarkSourceTime(ctx); err != nil {
		t.Fatal(err)
	}
	dated := func(y int, change func(ctx context.Context)) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)), rec)
		change(dctx)
		if _, err := f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	write := func(ctx context.Context, key, data string) {
		t.Helper()
		if _, err := f.WriteFile(ctx, key, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
	}
	var key string
	dated(2021, func(ctx context.Context) { key = writeNew(t, ctx, f, f.RootKey(), "doc.txt", "2021") })
	dated(2023, func(ctx context.Context) { write(ctx, key, "2023") })
	unfinished, err := store.Put(ctx, key, strings.NewReader("unfinished"), nil, "")
	if err != nil {
		t.Fatal(err)
	}

	defer func(was time.Duration) { unfinishedAfter = was }(unfinishedAfter)
	unfinishedAfter = 0
	dated(2022, func(ctx context.Context) { write(ctx, key, "2022") })

	history, err := store.History(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range history {
		if o.VersionID == unfinished.VersionID {
			t.Fatal("the unfinished version is still there")
		}
	}
	if got := readName(t, ctx, f, f.RootKey(), "doc.txt"); got != "2023" {
		t.Fatalf("current: %q, want the latest-dated, 2023", got)
	}
}
