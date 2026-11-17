package icbfsdrive

// Round 5 of T83-1: tests of what round 5 adds (settling in parts, a
// folder written behind found), apart from the reds of round 4.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestSettleInParts: more changes than MaxSettleChanges are settled in
// parts, each its own settle (record, write-out, Done); one cut in a
// later part is finished by a rerun given every change and the record.
// STATES: ST-18
func TestSettleInParts(t *testing.T) {
	saved := MaxSettleChanges
	MaxSettleChanges = 3
	t.Cleanup(func() { MaxSettleChanges = saved })
	store := &failStore{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, cutCaseSettings("parts"), Config{DeferLateFolders: true})
	for i, y := range []int{2016, 2018, 2020, 2022, 2024} {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), year(y))
	}
	var rows []drive.LateChange
	want := map[int]string{}
	for i := 0; i < 8; i++ {
		rows = append(rows, r5Write(t, d, fmt.Sprintf("/dir/l%d.txt", i), year(2016).AddDate(0, i*3, 1))...)
	}
	folder := rows[0].Folder
	// Cut the third part's window.
	parts, puts, failing := 0, 0, false
	store.mu.Lock()
	store.watch = func(op, key string) error {
		if op == "HISTORY" && key == folder {
			parts++
			puts = 0
		}
		if parts == 3 && op == "PUT" && (key == folder || key == folder+".metadata") {
			puts++
		}
		if parts == 3 && puts >= 3 || failing {
			failing = true
			return errFail
		}
		return nil
	}
	store.mu.Unlock()
	k := &keeper{}
	if _, err := settleAt(d, system(), rows, k.opts()); err == nil {
		t.Fatalf("not cut")
	}
	store.mu.Lock()
	store.watch = nil
	store.mu.Unlock()
	if k.held() == nil || k.done != 2 {
		t.Fatalf("after a cut in the third part: record held %v, done %d, want held and 2", k.held() != nil, k.done)
	}
	d = newDriveWith(t, store, cutCaseSettings("parts"), Config{DeferLateFolders: true})
	if _, err := settleAt(d, system(), rows, k.opts()); err != nil {
		t.Fatalf("the rerun: %v", err)
	}
	if k.held() != nil {
		t.Errorf("record held after the rerun")
	}
	for y := 2016; y <= 2025; y++ {
		var names []string
		for i, fy := range []int{2016, 2018, 2020, 2022, 2024} {
			if fy <= y {
				names = append(names, fmt.Sprintf("f%d.txt", i))
			}
		}
		for i := 0; i < 8; i++ {
			if !midYear(y).Before(year(2016).AddDate(0, i*3, 1)) {
				names = append(names, fmt.Sprintf("l%d.txt", i))
			}
		}
		sort.Strings(names)
		want[y] = strings.Join(names, ",")
	}
	pastIs(t, d, want)
	settleLeftovers(t, d, folder, 0)
}

// TestWrittenBehindPutRightByReplace (for R1, no policy): a folder whose
// root block has a version stored behind a later-dated one (as an older
// build's late write cut short leaves it, made here by hand) is found
// (FolderWrittenBehind) and refused by a settle; rewritten from its views
// (FolderHistory, then ReplaceFolderHistory) it is not, and a late write
// and a settle then succeed.
func TestWrittenBehindPutRightByReplace(t *testing.T) {
	store := miniotest.Store(t)
	d := newDriveWith(t, store, cutCaseSettings("behind"), Config{DeferLateFolders: true})
	for _, y := range []int{2020, 2022, 2024} {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", y), year(y))
	}
	folder := folderID(t, d, "/dir")
	if behind, err := d.FolderWrittenBehind(system(), drive.ByPath("/dir")); err != nil || behind {
		t.Fatalf("a folder written in order: behind %v, %v", behind, err)
	}
	ctx := context.Background()
	body, _, err := store.Get(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	v, err := store.Put(ctx, folder, bytes.NewReader(data), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.UpdateDates(ctx, store, folder, objstore.Dates{v.VersionID: midYear(2021)}); err != nil {
		t.Fatal(err)
	}
	if behind, err := d.FolderWrittenBehind(system(), drive.ByPath("/dir")); err != nil || !behind {
		t.Fatalf("a folder written behind: behind %v, %v", behind, err)
	}
	rows := r5Write(t, d, "/dir/x.txt", year(2021))
	if _, err := settleAt(d, system(), rows, drive.SettleOptions{}); !errors.Is(err, drive.ErrSettleRefused) {
		t.Fatalf("the settle of a folder written behind: %v, want ErrSettleRefused", err)
	}
	hist, err := d.FolderHistory(system(), drive.ByPath("/dir"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceFolderHistory(system(), drive.ByPath("/dir"), hist.States, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if behind, err := d.FolderWrittenBehind(system(), drive.ByPath("/dir")); err != nil || behind {
		t.Fatalf("after ReplaceFolderHistory: behind %v, %v", behind, err)
	}
	more := r5Write(t, d, "/dir/y.txt", year(2023))
	if _, err := settleAt(d, system(), append(rows, more...), drive.SettleOptions{}); err != nil {
		t.Fatalf("the settle after ReplaceFolderHistory: %v", err)
	}
	// The past as the views read it (the hand-made 2021 state included),
	// with x from 2021 and y from 2023 put in.
	for _, y := range []int{2021, 2022, 2023, 2024} {
		got := sortedAt(t, d, "/dir", midYear(y))
		if !strings.Contains(got, "x.txt") || (y >= 2023) != strings.Contains(got, "y.txt") {
			t.Errorf("/dir as of mid-%d: %s", y, got)
		}
	}
	if got := liveNames(t, d, folder); got != "f2020.txt,f2022.txt,f2024.txt,x.txt,y.txt" {
		t.Errorf("/dir now: %s", got)
	}
}
