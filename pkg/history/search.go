package history

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// Hit is an item whose name matched a search.
type Hit struct {
	ID      string    `json:"id"`   // the item's (Versions takes it, for a deleted one too)
	Path    string    `json:"path"` // drive-relative; for a deleted item, where it was
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Deleted bool      `json:"deleted,omitempty"`
	Time    time.Time `json:"time"` // when it was last modified, or deleted
	// See is the moment to show it at: zero (now) for an item that's
	// there now, just before its deletion for a deleted one.
	See     time.Time `json:"see"`
	TrashID string    `json:"trashId,omitempty"`
}

// Results are a search's hits, and whether it stopped before looking
// everywhere (at the limit, or after maxSearched items).
type Results struct {
	Hits      []Hit `json:"hits"`
	Truncated bool  `json:"truncated,omitempty"`
}

// maxSearched is how many items a search looks at before it stops.
const maxSearched = 200_000

// Search finds items whose names contain query, ignoring case: what's
// there now, by walking the tree, and what's in the trash, deleted since
// the trash began. Items renamed or deleted without the trash in between
// aren't found: that needs an index of names over time, not built yet.
// limit caps the hits (0: 1000).
func (h *History) Search(ctx context.Context, query string, limit int) (Results, error) {
	if limit <= 0 {
		limit = 1000
	}
	q := strings.ToLower(query)
	var r Results
	full := func() bool {
		if len(r.Hits) >= limit {
			r.Truncated = true
			return true
		}
		return false
	}

	opts := drive.TrashOptions{}
	for !full() {
		page, err := h.drive.Trash(ctx, opts)
		if err != nil {
			return r, err
		}
		for _, e := range page.Entries {
			if strings.Contains(strings.ToLower(e.Name), q) && !full() {
				path := e.Path
				if path == "" {
					path = e.Name
				}
				r.Hits = append(r.Hits, Hit{ID: e.ItemID, Path: path, Name: e.Name, Type: e.Type, Deleted: true,
					Time: e.DeletedAt, See: e.DeletedAt.Add(-time.Second), TrashID: e.ID})
			}
		}
		if page.Next == "" {
			break
		}
		opts.Cursor = page.Next
	}

	queue, searched := []string{"/"}, 0
	for len(queue) > 0 && !full() {
		dir := queue[0]
		queue = queue[1:]
		items, err := h.List(ctx, dir, time.Time{})
		if err != nil {
			if skippable(err) {
				continue // a directory this user can't read, or gone since
			}
			return r, err
		}
		for _, it := range items {
			if searched++; searched > maxSearched {
				r.Truncated = true
				return r, nil
			}
			if strings.Contains(strings.ToLower(it.Name), q) && !full() {
				r.Hits = append(r.Hits, Hit{ID: it.ID, Path: it.Path, Name: it.Name, Type: it.Type, Time: it.Modified})
			}
			if it.Type == drive.TypeDir {
				queue = append(queue, it.Path)
			}
		}
	}
	return r, nil
}

// skippable: a search walks past what it may not read, or what went
// while it walked.
func skippable(err error) bool {
	return IsNotFound(err) || errors.Is(err, drive.ErrForbidden) || errors.Is(err, drive.ErrNoAccess)
}
