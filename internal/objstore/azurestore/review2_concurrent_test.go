package azurestore_test

// Review 2 of T82-10 (reviewer's scratch test, not in any repository).
// Two PutParts to one blob at once (two assemblies of one path, or an
// assembly and a large plain write of it): both name their blocks
// icbfs-part-000000, -000001, ... so one writer's commit takes the other's
// blocks of the same ids. The first to commit succeeds with mixed bytes.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// gated is a part body whose first Read waits for open.
type gated struct {
	*bytes.Reader
	open <-chan struct{}
	once sync.Once
}

func (g *gated) Read(p []byte) (int, error) {
	g.once.Do(func() { <-g.open })
	return g.Reader.Read(p)
}

func (g *gated) Seek(off int64, whence int) (int64, error) { return g.Reader.Seek(off, whence) }

var _ io.ReadSeeker = (*gated)(nil)

func TestReview2ConcurrentPutPartsMixBlocks(t *testing.T) {
	s, f := newBlockRules(t)
	openA, openB := make(chan struct{}), make(chan struct{})
	partsA := []objstore.Part{
		{Size: 2, Body: bytes.NewReader([]byte("A0"))},
		{Size: 2, Body: bytes.NewReader([]byte("A1"))},
		{Size: 2, Body: &gated{Reader: bytes.NewReader([]byte("A2")), open: openA}},
	}
	partsB := []objstore.Part{
		{Size: 2, Body: bytes.NewReader([]byte("B0"))},
		{Size: 2, Body: bytes.NewReader([]byte("B1"))},
		{Size: 2, Body: &gated{Reader: bytes.NewReader([]byte("B2")), open: openB}},
	}
	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.PutParts(context.Background(), "k", partsA, nil, ""); errA <- err }()
	// A has staged A0 and A1 and waits on A2.
	waitPuts(t, f, 2)
	go func() { _, err := s.PutParts(context.Background(), "k", partsB, nil, ""); errB <- err }()
	// B has staged B0 and B1 (the same ids as A0, A1) and waits on B2.
	waitPuts(t, f, 4)
	close(openA)
	if err := <-errA; err != nil {
		t.Fatalf("A: %v", err)
	}
	f.mu.Lock()
	got := string(bytes.Join(f.committed, nil))
	f.mu.Unlock()
	close(openB)
	eb := <-errB
	t.Logf("A succeeded and committed %q (A sent A0A1A2); B: %v", got, eb)
	if got != "A0A1A2" {
		t.Fatalf("A's PutParts succeeded with another writer's blocks in its version: %q", got)
	}
}

func waitPuts(t *testing.T, f *blockRules, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		f.mu.Lock()
		p := f.puts
		f.mu.Unlock()
		if p >= n {
			return
		}
	}
	t.Fatalf("waited for %d Put Block", n)
}

// TestReview2StaleIDsEqualSwitchedIDs: stale blocks whose ids are exactly
// the ids icbfs switches to (8-byte digits: a repaired PutParts that was
// cut), with stale bytes. One writer: every id committed is staged again
// by this PutParts after the switch, so no stale byte is committed.
func TestReview2StaleIDsEqualSwitchedIDs(t *testing.T) {
	for name, copies := range map[string]bool{"uploaded": false, "copied": true} {
		t.Run(name, func(t *testing.T) {
			s, f := newBlockRules(t, "00000000", "00000001", "00000002", "00000003")
			var parts []objstore.Part
			for i := 0; i < 3; i++ {
				if copies {
					parts = append(parts, objstore.Part{Size: 5 << 20, CopyKey: "src", CopyVersion: "2026-11-17T00:00:00.0000000Z"})
				} else {
					parts = append(parts, objstore.Part{Size: 2, Body: bytes.NewReader([]byte{'N', byte('0' + i)})})
				}
			}
			if _, err := s.PutParts(context.Background(), "k", parts, nil, ""); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for i, b := range f.committed {
				if string(b) == "stale" {
					t.Fatalf("block %d committed with stale bytes", i)
				}
			}
			if len(f.committed) != 3 || f.versions != 1 {
				t.Fatalf("%d blocks, %d versions", len(f.committed), f.versions)
			}
			if !copies && string(bytes.Join(f.committed, nil)) != "N0N1N2" {
				t.Fatalf("committed %q", bytes.Join(f.committed, nil))
			}
		})
	}
}
