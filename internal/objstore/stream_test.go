package objstore

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStreamParts(t *testing.T) {
	body := []byte("0123456789abcdefghij") // 20 bytes
	t.Run("in order, sharing one buffer, each seekable for its own retry", func(t *testing.T) {
		parts := StreamParts(bytes.NewReader(body), 20, 8)
		if len(parts) != 3 || parts[0].Size != 8 || parts[2].Size != 4 || !IsStream(parts) {
			t.Fatalf("parts %+v", parts)
		}
		for i, want := range []string{"01234567", "89abcdef", "ghij"} {
			got, err := io.ReadAll(parts[i].Body)
			if err != nil || string(got) != want {
				t.Fatalf("part %d: %q, %v; want %q", i, got, err, want)
			}
			// A retry of the same part seeks back and reads again.
			if _, err := parts[i].Body.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if again, _ := io.ReadAll(parts[i].Body); string(again) != want {
				t.Fatalf("part %d again: %q", i, again)
			}
		}
		// The first part is gone once the second was taken.
		if _, err := parts[0].Body.Seek(0, io.SeekStart); !errors.Is(err, errStreamOrder) {
			t.Fatalf("part 0 after the rest: %v", err)
		}
	})
	t.Run("out of order is refused", func(t *testing.T) {
		parts := StreamParts(bytes.NewReader(body), 20, 8)
		if _, err := io.ReadAll(parts[1].Body); !errors.Is(err, errStreamOrder) {
			t.Fatalf("part 1 first: %v", err)
		}
	})
	t.Run("a short body fails the part it ends in", func(t *testing.T) {
		parts := StreamParts(strings.NewReader("0123456789"), 20, 8)
		if _, err := io.ReadAll(parts[0].Body); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(parts[1].Body); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("part 1 of a short body: %v", err)
		}
	})
	t.Run("a long body fails the last part", func(t *testing.T) {
		parts := StreamParts(strings.NewReader("0123456789abcdefghijKLM"), 20, 8)
		for i := 0; i < 2; i++ {
			if _, err := io.ReadAll(parts[i].Body); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.ReadAll(parts[2].Body); err == nil || !strings.Contains(err.Error(), "longer") {
			t.Fatalf("last part of a long body: %v", err)
		}
	})
	t.Run("none from nothing", func(t *testing.T) {
		if parts := StreamParts(strings.NewReader(""), 0, 8); len(parts) != 0 || IsStream(parts) {
			t.Fatalf("parts %+v", parts)
		}
	})
}
