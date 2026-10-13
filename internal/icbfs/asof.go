package icbfs

import (
	"fmt"
	"time"
)

// ParseAsOf parses an as-of mount's time, in loc unless it says
// otherwise: a bare date (2026-10-12) means the end of that day — the
// filesystem as it was when the day ended; a date-time with a zone
// (RFC 3339) is taken as given; one without a zone (2026-10-12T15:04,
// 2026-10-12 15:04:05, fractional seconds allowed) is in loc.
func ParseAsOf(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return d.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("as-of %q: want a date (2006-01-02), a local date-time (2006-01-02T15:04:05), or RFC 3339", s)
}
