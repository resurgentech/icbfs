package drive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

// Copy copies an item — a file, or a directory and everything in it —
// from one drive to another (or within one): a read from the source and
// a write to the target, which records where the copy came from when it
// can keep that. at picks the source's time, name or version. srcID is
// the source drive's registry id, for the origin record.
func Copy(ctx context.Context, src Drive, srcID string, from ItemRef, at At, dst Drive, to string, cond Condition) (Item, error) {
	it, err := src.Stat(ctx, from, at)
	if err != nil {
		return Item{}, err
	}
	switch it.Type {
	case TypeFile:
		return copyFile(ctx, src, srcID, ItemRef{ID: it.ID, Path: it.Path}, at, dst, to, cond)
	case TypeDir:
	default:
		return Item{}, fmt.Errorf("%s is a %s: %w", it.Path, it.Type, ErrNotSupported)
	}
	if at.Version != "" {
		return Item{}, fmt.Errorf("a version is a file's: %w", ErrIsDir)
	}
	top, err := dst.Mkdir(ctx, to, false)
	if err != nil {
		return Item{}, err
	}
	page, err := src.List(ctx, ItemRef{ID: it.ID, Path: it.Path}, ListOptions{At: at})
	if err != nil {
		return Item{}, err
	}
	for _, c := range page.Items {
		target := strings.TrimSuffix(to, "/") + "/" + c.Name
		if c.Type == TypeSymlink {
			continue
		}
		if _, err := Copy(ctx, src, srcID, ItemRef{ID: c.ID, Path: c.Path}, at, dst, target, Condition{}); err != nil {
			return Item{}, fmt.Errorf("copy %s: %w", c.Path, err)
		}
	}
	return top, nil
}

func copyFile(ctx context.Context, src Drive, srcID string, from ItemRef, at At, dst Drive, to string, cond Condition) (Item, error) {
	body, it, err := src.Read(ctx, from, at, nil)
	if err != nil {
		return Item{}, err
	}
	defer body.Close()
	out, err := dst.Write(ctx, to, body, cond, WriteOptions{ContentType: it.ContentType})
	if err != nil {
		return Item{}, err
	}
	if dst.Capabilities().Origin {
		o := Origin{Drive: srcID, ID: it.ID, Path: it.Path, Copied: time.Now().UTC()}
		if it.Version != nil {
			o.Version = it.Version.ID
		}
		if err := dst.SetOrigin(ctx, ItemRef{ID: out.ID, Path: out.Path}, o); err != nil {
			return out, err
		}
		out.Origin = &o
	}
	return out, nil
}

// maxDiffBytes bounds each side of a diff.
const maxDiffBytes = 8 << 20

// Diff is a unified diff between two versions of a text file (from and
// to are version ids; "" for the current one). A drive whose backend
// diffs natively would do it itself; this computes it.
func Diff(ctx context.Context, d Drive, item ItemRef, from, to string) (string, error) {
	read := func(version string) (string, Item, error) {
		body, it, err := d.Read(ctx, item, At{Version: version}, nil)
		if err != nil {
			return "", Item{}, err
		}
		defer body.Close()
		data, err := io.ReadAll(io.LimitReader(body, maxDiffBytes+1))
		if err != nil {
			return "", Item{}, err
		}
		if len(data) > maxDiffBytes {
			return "", Item{}, fmt.Errorf("diff: %w", ErrTooLarge)
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return "", Item{}, fmt.Errorf("diff: not text: %w", ErrNotSupported)
		}
		return string(data), it, nil
	}
	a, ai, err := read(from)
	if err != nil {
		return "", err
	}
	b, bi, err := read(to)
	if err != nil {
		return "", err
	}
	label := func(it Item) string {
		if it.Version != nil {
			return it.Path + "@" + it.Version.Time.UTC().Format(time.RFC3339Nano)
		}
		return it.Path
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(a), B: difflib.SplitLines(b),
		FromFile: label(ai), ToFile: label(bi), Context: 3,
	})
}
