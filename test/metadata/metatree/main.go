// Command metatree builds a tree of files whose metadata is the hard
// cases (times well in the past, a creation time after the modified time,
// modes, owners, extended attributes, POSIX and Windows ACLs, Windows
// attributes, alternate data streams, links), from a description of it
// (test/metadata/tree.json), and compares trees: with the description, or
// one copy with another. It's how the copy tests check that a copy onto
// icbfs keeps what the same tool keeps onto a local disk.
//
//	metatree build SPEC DIR        make the tree in DIR (empty or missing)
//	metatree check SPEC DIR        compare DIR with the description
//	metatree dump DIR [FILE]       print DIR's metadata as JSON, or write it to FILE
//	metatree diff [-since T] [-skip F,...] A B   compare two trees, field by field
//
// check and diff take a dump (a .json file) in place of a directory:
// reading a tree moves its access times, so a tree read once, dumped, is
// compared as it was.
//
// Each runs with the system's own calls (setxattr, utimensat, SetFileTime,
// SetNamedSecurityInfo), so nothing between it and the disk drops
// anything. Fields the platform can't set are skipped: on Linux, creation
// times and Windows attributes, ACLs and streams; on Windows, modes,
// owners, extended attributes and POSIX ACLs. Building everything needs
// root (other owners, trusted. attributes) or Administrator (other
// owners); without, check reports what couldn't be set.
//
// diff -since T: a time at or after T in both trees counts as the same,
// both set by the copy (a copy's own creation time on Linux, say), since
// the two copies weren't made at the same instant. One at or after T in
// only one of them is a difference: one copy kept it, the other didn't.
// Status-change times are never compared: no tool can set them.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Spec is a tree's description.
type Spec struct {
	Description string `json:"description,omitempty"`
	Items       []Item `json:"items"`
}

// Item is one file, directory or link: in a description, what to make it;
// in a dump, what it is. In a description only what's set is made and
// checked.
type Item struct {
	Path string `json:"path"` // '/'-separated, relative to the tree
	Type string `json:"type"` // file, dir, symlink, hardlink (description only: a second name for Target)

	Content string `json:"content,omitempty"` // a file's content, as text
	Fill    int64  `json:"fill,omitempty"`    // or this many bytes of a fixed pattern
	Target  string `json:"target,omitempty"`  // a symlink's target; a hardlink's other name

	Size *int64 `json:"size,omitempty"`
	SHA  string `json:"sha256,omitempty"`

	Mtime *time.Time `json:"mtime,omitempty"`
	Atime *time.Time `json:"atime,omitempty"`
	Btime *time.Time `json:"btime,omitempty"` // creation time

	// Links is every name of a file with more than one, sorted, in a dump.
	Links []string `json:"links,omitempty"`

	POSIX   *POSIX   `json:"posix,omitempty"`
	Windows *Windows `json:"windows,omitempty"`
}

// POSIX is what Linux keeps.
type POSIX struct {
	Mode       string            `json:"mode,omitempty"` // octal, with setuid, setgid and sticky: "2775"
	Uid        *uint32           `json:"uid,omitempty"`
	Gid        *uint32           `json:"gid,omitempty"`
	Xattrs     map[string]string `json:"xattrs,omitempty"` // text, or "base64:..."
	ACL        string            `json:"acl,omitempty"`    // short text form: u::rw-,u:1001:r--,g::r--,m::r--,o::---
	DefaultACL string            `json:"defaultAcl,omitempty"`
}

// Windows is what NTFS keeps.
type Windows struct {
	Attributes string            `json:"attributes,omitempty"` // of R, H, S, A, in that order; "-" for none
	SDDL       string            `json:"sddl,omitempty"`       // owner, group and DACL
	Streams    map[string]string `json:"streams,omitempty"`    // alternate data streams' contents (a dump: their SHA-256)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "build":
		args := need(os.Args[2:], 2)
		err = build(load(args[0]), args[1])
	case "check":
		args := need(os.Args[2:], 2)
		var diffs []string
		if diffs, err = check(load(args[0]), args[1]); err == nil {
			err = report(diffs)
		}
	case "dump":
		args := os.Args[2:]
		if len(args) != 1 && len(args) != 2 {
			usage()
		}
		var items []Item
		if items, err = dump(args[0]); err == nil {
			out, _ := json.MarshalIndent(items, "", " ")
			if len(args) == 2 {
				err = os.WriteFile(args[1], append(out, '\n'), 0o644)
			} else {
				fmt.Println(string(out))
			}
		}
	case "diff":
		fset := flag.NewFlagSet("diff", flag.ExitOnError)
		since := fset.String("since", "", "times at or after this (RFC 3339) in both trees are the same: set by the copy")
		skip := fset.String("skip", "", "fields not to compare, comma-separated: what the target can't keep (links, on a Windows mount)")
		fset.Parse(os.Args[2:])
		args := need(fset.Args(), 2)
		var t time.Time
		if *since != "" {
			if t, err = time.Parse(time.RFC3339Nano, *since); err != nil {
				fail(err)
			}
		}
		var diffs []string
		skipped := map[string]bool{}
		for _, f := range strings.Split(*skip, ",") {
			if f != "" {
				skipped[f] = true
			}
		}
		if diffs, err = diffTrees(args[0], args[1], t, skipped); err == nil {
			err = report(diffs)
		}
	default:
		usage()
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: metatree build SPEC DIR | check SPEC DIR | dump DIR [FILE] | diff [-since T] A B")
	os.Exit(2)
}

func need(args []string, n int) []string {
	if len(args) != n {
		usage()
	}
	return args
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "metatree:", err)
	os.Exit(2)
}

// report prints the differences and fails if there are any.
func report(diffs []string) error {
	for _, d := range diffs {
		fmt.Println(d)
	}
	if len(diffs) > 0 {
		fmt.Fprintf(os.Stderr, "%d differences\n", len(diffs))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "no differences")
	return nil
}

func load(path string) *Spec {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		fail(fmt.Errorf("%s: %w", path, err))
	}
	return &s
}

// fill is n bytes of a fixed pattern.
func fill(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

func (it Item) content() []byte {
	if it.Fill > 0 {
		return fill(it.Fill)
	}
	return []byte(it.Content)
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// xattrValue decodes a description's attribute value.
func xattrValue(v string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(v, "base64:"); ok {
		return base64.StdEncoding.DecodeString(rest)
	}
	if rest, ok := strings.CutPrefix(v, "repeat:"); ok {
		// repeat:N:text, text repeated to N bytes: a large value.
		n, text, _ := strings.Cut(rest, ":")
		var size int
		fmt.Sscan(n, &size)
		return []byte(strings.Repeat(text, size/len(text)+1)[:size]), nil
	}
	return []byte(v), nil
}

// showXattr is how a dump shows a value: text if it is, base64 if not, a
// hash if it's long.
func showXattr(b []byte) string {
	if len(b) > 256 {
		return "sha256:" + hash(b)
	}
	if utf8.Valid(b) && !strings.ContainsRune(string(b), 0) {
		return string(b)
	}
	return "base64:" + base64.StdEncoding.EncodeToString(b)
}

// normXattrs is a description's attributes as a dump shows them.
func normXattrs(m map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m {
		b, err := xattrValue(v)
		if err != nil {
			return nil, fmt.Errorf("xattr %s: %w", k, err)
		}
		out[k] = showXattr(b)
	}
	return out, nil
}

// --- building ---

func build(s *Spec, root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := prepare(); err != nil {
		return err
	}
	items := append([]Item(nil), s.Items...)
	sort.SliceStable(items, func(i, j int) bool { return depth(items[i].Path) < depth(items[j].Path) })
	// Everything exists first, then what's set on it, then the times,
	// deepest first: making an entry changes its directory's.
	for _, it := range items {
		p := filepath.Join(root, filepath.FromSlash(it.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		var err error
		switch it.Type {
		case "dir":
			err = os.Mkdir(p, 0o755)
		case "file":
			err = os.WriteFile(p, it.content(), 0o644)
		case "symlink":
			err = os.Symlink(filepath.FromSlash(it.Target), p)
		case "hardlink":
			err = os.Link(filepath.Join(root, filepath.FromSlash(it.Target)), p)
		default:
			err = fmt.Errorf("unknown type %q", it.Type)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}
	}
	for _, it := range items {
		if it.Type == "hardlink" {
			continue // its file's
		}
		if err := setMeta(filepath.Join(root, filepath.FromSlash(it.Path)), it); err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}
	}
	// ACLs from the top down: a folder's inheritable entries spread to
	// what's in it, before that gets its own.
	for _, it := range items {
		if it.Type == "hardlink" {
			continue
		}
		if err := setSecurity(filepath.Join(root, filepath.FromSlash(it.Path)), it); err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}
	}
	// Then attributes, which spreading ACLs can change (Archive), and
	// the times last, deepest first, since everything else can move them.
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Type == "hardlink" {
			continue
		}
		if err := setAttributes(filepath.Join(root, filepath.FromSlash(it.Path)), it); err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Type == "hardlink" {
			continue
		}
		if err := setTimes(filepath.Join(root, filepath.FromSlash(it.Path)), it); err != nil {
			return fmt.Errorf("%s: times: %w", it.Path, err)
		}
	}
	return nil
}

func depth(p string) int { return strings.Count(p, "/") }

// --- reading ---

// dump reads every item under root (not root itself), sorted by path; or,
// for a .json file, the dump in it.
func dump(root string) ([]Item, error) {
	if strings.HasSuffix(root, ".json") {
		data, err := os.ReadFile(root)
		if err != nil {
			return nil, err
		}
		var items []Item
		// A dump written through Windows PowerShell may start with a BOM.
		if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &items); err != nil {
			return nil, fmt.Errorf("%s: %w", root, err)
		}
		return items, nil
	}
	// Past ACLs a copy left that shut its reader out (xcopy /O's).
	if err := prepare(); err != nil {
		return nil, err
	}
	var items []Item
	ids := map[string][]string{} // file identity -> paths, for hard links
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		it, id, err := readItem(p)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		it.Path = filepath.ToSlash(rel)
		if id != "" {
			ids[id] = append(ids[id], it.Path)
		}
		items = append(items, it)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range items {
		for _, paths := range ids {
			if len(paths) > 1 && contains(paths, items[i].Path) {
				items[i].Links = append([]string(nil), paths...)
				sort.Strings(items[i].Links)
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- comparing ---

// check compares a tree with its description: only what the description
// sets, and only what this platform keeps.
func check(s *Spec, root string) ([]string, error) {
	got, err := dump(root)
	if err != nil {
		return nil, err
	}
	byPath := map[string]Item{}
	for _, it := range got {
		byPath[it.Path] = it
	}
	links := map[string][]string{}
	for _, it := range s.Items {
		if it.Type == "hardlink" {
			links[it.Target] = append(links[it.Target], it.Path)
		}
	}
	var diffs []string
	add := func(p, field, want, got string) {
		diffs = append(diffs, fmt.Sprintf("%s: %s: want %s, got %s", p, field, want, got))
	}
	for _, want := range s.Items {
		g, ok := byPath[want.Path]
		if !ok {
			diffs = append(diffs, want.Path+": missing")
			continue
		}
		typ := want.Type
		if typ == "hardlink" {
			typ = "file"
		}
		if g.Type != typ {
			add(want.Path, "type", typ, g.Type)
			continue
		}
		if want.Type == "file" {
			if h := hash(want.content()); g.SHA != h {
				add(want.Path, "content", h, g.SHA)
			}
		}
		if want.Type == "symlink" && filepath.ToSlash(want.Target) != g.Target {
			add(want.Path, "target", want.Target, g.Target)
		}
		if names := links[want.Path]; len(names) > 0 {
			all := append([]string{want.Path}, names...)
			sort.Strings(all)
			if strings.Join(all, ",") != strings.Join(g.Links, ",") {
				add(want.Path, "links", strings.Join(all, ","), strings.Join(g.Links, ","))
			}
		}
		if want.Type == "hardlink" {
			continue
		}
		cmpTime := func(field string, w, g *time.Time) {
			if w != nil && (g == nil || !w.Equal(*g)) {
				add(want.Path, field, w.UTC().Format(time.RFC3339Nano), showTime(g))
			}
		}
		cmpTime("mtime", want.Mtime, g.Mtime)
		if want.Type != "symlink" || keepsSymlinkAtime {
			cmpTime("atime", want.Atime, g.Atime)
		}
		if keepsBtime {
			cmpTime("btime", want.Btime, g.Btime)
		}
		for _, d := range checkPlatform(want, g) {
			diffs = append(diffs, want.Path+": "+d)
		}
	}
	return diffs, nil
}

func showTime(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// diffTrees compares two trees, every field either keeps.
func diffTrees(a, b string, since time.Time, skip map[string]bool) ([]string, error) {
	da, err := dump(a)
	if err != nil {
		return nil, err
	}
	db, err := dump(b)
	if err != nil {
		return nil, err
	}
	ma, mb := map[string]Item{}, map[string]Item{}
	for _, it := range da {
		ma[it.Path] = it
	}
	for _, it := range db {
		mb[it.Path] = it
	}
	var paths []string
	for p := range ma {
		paths = append(paths, p)
	}
	for p := range mb {
		if _, ok := ma[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var diffs []string
	for _, p := range paths {
		x, okx := ma[p]
		y, oky := mb[p]
		switch {
		case !okx:
			diffs = append(diffs, p+": only in "+b)
			continue
		case !oky:
			diffs = append(diffs, p+": only in "+a)
			continue
		}
		fields := flatten(x, since)
		other := flatten(y, since)
		var names []string
		for k := range fields {
			names = append(names, k)
		}
		for k := range other {
			if _, ok := fields[k]; !ok {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		for _, k := range names {
			if skip[k] {
				continue
			}
			if fields[k] != other[k] {
				diffs = append(diffs, fmt.Sprintf("%s: %s: %s, %s", p, k, orNone(fields[k]), orNone(other[k])))
			}
		}
	}
	return diffs, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// flatten is an item's fields as strings, for diffTrees. A time at or
// after since is "set by the copy".
func flatten(it Item, since time.Time) map[string]string {
	f := map[string]string{"type": it.Type, "sha256": it.SHA, "target": it.Target, "links": strings.Join(it.Links, ",")}
	if it.Size != nil {
		f["size"] = fmt.Sprint(*it.Size)
	}
	t := func(name string, v *time.Time) {
		switch {
		case v == nil:
		case !since.IsZero() && !v.Before(since):
			f[name] = "(set by the copy)"
		default:
			f[name] = v.UTC().Format(time.RFC3339Nano)
		}
	}
	t("mtime", it.Mtime)
	if it.Type != "symlink" || keepsSymlinkAtime {
		t("atime", it.Atime)
	}
	t("btime", it.Btime)
	if p := it.POSIX; p != nil {
		f["mode"] = p.Mode
		if p.Uid != nil {
			f["uid"] = fmt.Sprint(*p.Uid)
		}
		if p.Gid != nil {
			f["gid"] = fmt.Sprint(*p.Gid)
		}
		for k, v := range p.Xattrs {
			f["xattr "+k] = v
		}
		f["acl"] = p.ACL
		f["defaultAcl"] = p.DefaultACL
	}
	if w := it.Windows; w != nil {
		f["attributes"] = w.Attributes
		f["sddl"] = w.SDDL
		for k, v := range w.Streams {
			f["stream "+k] = v
		}
	}
	for k, v := range f {
		if v == "" {
			delete(f, k)
		}
	}
	return f
}
