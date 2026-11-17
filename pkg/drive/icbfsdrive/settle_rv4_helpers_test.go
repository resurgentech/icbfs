package icbfsdrive

// Helpers of the round-4 reviewer's tests (settle_rv4_*_test.go, brought
// in from the review of 2026-11-17 as round 5's reds).

import (
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

func rowsUntil(rows []drive.LateChange) string {
	var s []string
	for _, r := range rows {
		s = append(s, r.Until.Format(time.RFC3339Nano))
	}
	return strings.Join(s, ",")
}

func errShort(err error) string {
	if err == nil {
		return "ok"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func rvKeyOf(t *testing.T, d *Drive, p string) string {
	n, err := d.resolve(system(), d.fs, drive.ByPath(p))
	if err != nil {
		t.Fatal(err)
	}
	return n.key
}
