package objstore

import (
	"context"
	"testing"
)

// listOnly is a Store that has only List: ListEach falls back to it.
type listOnly struct {
	Store
	objs  []Object
	lists int
}

func (s *listOnly) List(ctx context.Context, prefix string) ([]Object, error) {
	s.lists++
	return s.objs, nil
}

// paged is a Store that lists a page at a time: ListEach uses that, and
// never List.
type paged struct {
	listOnly
	pages [][]Object
}

func (s *paged) ListPages(ctx context.Context, prefix string, fn func([]Object) error) error {
	for _, p := range s.pages {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func TestListEach_FallsBackToList(t *testing.T) {
	s := &listOnly{objs: []Object{{Key: "a", Size: 1}, {Key: "b", Size: 2}}}
	var got []Object
	if err := ListEach(context.Background(), s, "", func(p []Object) error { got = append(got, p...); return nil }); err != nil {
		t.Fatal(err)
	}
	if s.lists != 1 || len(got) != 2 {
		t.Fatalf("lists %d, got %d objects; want one List and 2 objects", s.lists, len(got))
	}
}

func TestListEach_UsesPagesWhenTheStoreHasThem(t *testing.T) {
	s := &paged{pages: [][]Object{{{Key: "a"}}, {{Key: "b"}, {Key: "c"}}}}
	var pages int
	var got []Object
	if err := ListEach(context.Background(), s, "", func(p []Object) error { pages++; got = append(got, p...); return nil }); err != nil {
		t.Fatal(err)
	}
	if s.lists != 0 || pages != 2 || len(got) != 3 {
		t.Fatalf("lists %d, pages %d, objects %d; want no List, 2 pages, 3 objects", s.lists, pages, len(got))
	}
}
