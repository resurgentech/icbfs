package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// listPagesAfterAKey: a store lists by prefix starting after a key
// (objstore.AfterLister), the key itself left out whether it exists or
// not, a page at a time, in key order; "" lists everything (interloc
// T84: the trash a page at a time).
func listPagesAfterAKey(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 2
	for i := 0; i < 7; i++ {
		if _, err := store.Put(ctx, fmt.Sprintf("p/%d", i), strings.NewReader("x"), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Put(ctx, "pq", strings.NewReader("y"), nil, ""); err != nil {
		t.Fatal(err)
	}
	al, ok := store.(objstore.AfterLister)
	if !ok {
		t.Fatalf("%T cannot start a listing after a key: want objstore.AfterLister", store)
	}
	for after, want := range map[string]string{
		"":     "p/0 p/1 p/2 p/3 p/4 p/5 p/6",
		"p/2":  "p/3 p/4 p/5 p/6",
		"p/25": "p/3 p/4 p/5 p/6", // not a key
		"p/6":  "",
	} {
		var got []string
		if err := al.ListPagesAfter(ctx, "p/", after, func(p []objstore.Object) error {
			if len(p) > int(objstore.HistoryPageSize) {
				t.Errorf("after %q: a page of %d", after, len(p))
			}
			for _, o := range p {
				got = append(got, o.Key)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("after %q: %v, want %s", after, got, want)
		}
	}
}
