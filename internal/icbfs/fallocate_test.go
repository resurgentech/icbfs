package icbfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestAllocateModes runs each fallocate mode on a session and checks the
// content after the flush.
func TestAllocateModes(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	const blk = 4096
	// pattern is 4 blocks: block i filled with 'a'+i.
	pattern := func() []byte {
		var b []byte
		for i := 0; i < 4; i++ {
			b = append(b, bytes.Repeat([]byte{byte('a' + i)}, blk)...)
		}
		return b
	}
	blocks := func(s string) []byte { // "a0c" -> block of a, block of zeros, block of c
		var b []byte
		for _, c := range s {
			if c == '0' {
				c = 0
			}
			b = append(b, bytes.Repeat([]byte{byte(c)}, blk)...)
		}
		return b
	}
	for _, c := range []struct {
		name      string
		off, len  int64
		mode      uint32
		want      []byte
		wantError error
	}{
		{"allocate grows", 4 * blk, blk, 0, blocks("abcd0"), nil},
		{"allocate keep size", 4 * blk, blk, FallocKeepSize, blocks("abcd"), nil},
		{"allocate inside", 0, blk, 0, blocks("abcd"), nil},
		{"punch hole", blk, blk, FallocPunchHole | FallocKeepSize, blocks("a0cd"), nil},
		{"punch hole past end", 3 * blk, 2 * blk, FallocPunchHole | FallocKeepSize, blocks("abc0"), nil},
		{"punch without keep size", blk, blk, FallocPunchHole, nil, ErrInvalidArgument},
		{"zero range", blk, 2 * blk, FallocZeroRange, blocks("a00d"), nil},
		{"zero range grows", 3 * blk, 2 * blk, FallocZeroRange, blocks("abc00"), nil},
		{"zero range keep size", 3 * blk, 2 * blk, FallocZeroRange | FallocKeepSize, blocks("abc0"), nil},
		{"unknown mode", 0, blk, 0x40, nil, ErrNotSupported},
	} {
		key, _, _, err := fsys.Create(ctx, fsys.RootKey(), c.name, 0o644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		o, err := fsys.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.WriteAt(pattern(), 0); err != nil {
			t.Fatal(err)
		}
		err = o.Allocate(ctx, c.off, c.len, c.mode)
		if c.wantError != nil {
			if !errors.Is(err, c.wantError) {
				t.Errorf("%s: %v, want %v", c.name, err, c.wantError)
			}
			o.Close()
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			o.Close()
			continue
		}
		if a, err := fsys.Stat(ctx, key, TypeFile); err != nil || a.Size != int64(len(c.want)) {
			t.Errorf("%s: size before the flush %d (%v), want %d", c.name, a.Size, err, len(c.want))
		}
		if _, err := o.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		o.Close()
		got, _, _, err := fsys.ReadFile(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s: content differs (%d bytes, want %d)", c.name, len(got), len(c.want))
		}
	}
}

// TestAllocateNoSpace: as on Linux, an allocation past the max file size
// is "file too large", and one that grows the file by more than the
// filesystem has free is "no space" and changes nothing; one that fits
// goes ahead, and one that keeps the size isn't counted.
func TestAllocateNoSpace(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	const capacity = 1 << 20
	if err := Resize(ctx, store, "test", capacity); err != nil {
		t.Fatal(err)
	}
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := o.Allocate(ctx, 0, fsys.maxFileSize+1, 0); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("past the max file size: %v, want ErrFileTooLarge", err)
	}
	if err := o.Allocate(ctx, 0, 2*capacity, 0); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("twice the capacity: %v, want ErrNoSpace", err)
	}
	if err := o.Allocate(ctx, 0, 2*capacity, FallocZeroRange); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("zero range of twice the capacity: %v, want ErrNoSpace", err)
	}
	if o.Size() != 0 {
		t.Fatalf("size after refusals = %d, want 0", o.Size())
	}
	if err := o.Allocate(ctx, 0, 2*capacity, FallocKeepSize); err != nil {
		t.Fatalf("keep size: %v", err)
	}
	if err := o.Allocate(ctx, 0, capacity/2, 0); err != nil || o.Size() != capacity/2 {
		t.Fatalf("half the capacity: %v, size %d", err, o.Size())
	}
}
