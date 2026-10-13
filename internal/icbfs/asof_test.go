package icbfs

import (
	"testing"
	"time"
)

func TestParseAsOf(t *testing.T) {
	loc := time.FixedZone("test", -7*3600)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"2026-10-12", time.Date(2026, 10, 12, 23, 59, 59, 999999999, loc)},
		{"2026-10-12T15:04:05", time.Date(2026, 10, 12, 15, 4, 5, 0, loc)},
		{"2026-10-12 15:04:05.25", time.Date(2026, 10, 12, 15, 4, 5, 250000000, loc)},
		{"2026-10-12T15:04", time.Date(2026, 10, 12, 15, 4, 0, 0, loc)},
		{"2026-10-12T15:04:05Z", time.Date(2026, 10, 12, 15, 4, 5, 0, time.UTC)},
		{"2026-10-12T15:04:05+02:00", time.Date(2026, 10, 12, 13, 4, 5, 0, time.UTC)},
	} {
		got, err := ParseAsOf(tc.in, loc)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("ParseAsOf(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "2026-13-01", "12/10/2026"} {
		if _, err := ParseAsOf(bad, loc); err == nil {
			t.Errorf("ParseAsOf(%q) succeeded", bad)
		}
	}
}
