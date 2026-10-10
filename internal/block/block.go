// Package block implements the directory block format.
//
// A directory's entries are stored as a median-key B-tree, per
// ARCHITECTURE.md's large-directory design: a Leaf block holds a sorted
// list of name -> UUID rows directly; once a node overflows a size
// threshold, it splits at its median key into two children, and an
// Internal block holds sorted range pointers to its children instead of
// entries directly. Every directory's own UUID (or, for the filesystem
// root, its fixed name-key) is always the entry point into its own
// B-tree, whether that tree is currently one flat Leaf or several levels
// of Internal nodes underneath it.
//
// Known simplification: splitting on insert-overflow is implemented;
// merging underfull nodes back together on delete is not. An
// over-sharded, mostly-empty directory just stays taller than necessary
// rather than being compacted — see ARCHITECTURE.md.
package block

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/pb"
)

// EntryType is the kind of node a directory row points at.
type EntryType uint8

const (
	TypeFile EntryType = iota
	TypeDir
	TypeSymlink
)

// Entry is one row of a leaf block.
type Entry struct {
	Name string
	UUID string
	Type EntryType
}

// Kind distinguishes a leaf (holds entries directly) from an internal
// node (holds range pointers to children).
type Kind uint8

const (
	Leaf Kind = iota
	Internal
)

// Child is one range pointer in an internal node: the subtree reachable
// through UUID covers every name >= MinKey (up to the next child's
// MinKey, or unbounded for the last child). The first child in any node
// always has MinKey == "", meaning "no additional lower bound beyond
// whatever this node's own position in the tree already implies" — this
// convention holds even after a split, so split halves never need their
// MinKey rewritten, only the new sibling inserted into the parent needs a
// real MinKey (see Block.SplitLeaf/SplitInternal).
type Child struct {
	MinKey string
	UUID   string
}

// Block is the decoded body of a directory object: either a Leaf (a
// sorted list of entries) or an Internal node (a sorted list of child
// pointers).
type Block struct {
	Kind     Kind
	Entries  []Entry
	Children []Child
}

// Decode parses a directory block body (Protobuf-encoded, per
// spec/proto/icbfs/v1/block.proto). Empty input decodes to an empty leaf
// block, so a freshly created directory's body can just be nil bytes.
func Decode(data []byte) (*Block, error) {
	if len(data) == 0 {
		return &Block{Kind: Leaf}, nil
	}
	var wire pb.Block
	if err := proto.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode directory block: %w", err)
	}
	return fromWire(&wire), nil
}

// Encode serializes the block back to bytes.
func (b *Block) Encode() ([]byte, error) {
	data, err := proto.Marshal(toWire(b))
	if err != nil {
		return nil, fmt.Errorf("encode directory block: %w", err)
	}
	return data, nil
}

func toWire(b *Block) *pb.Block {
	w := &pb.Block{Kind: pb.BlockKind(b.Kind)}
	for _, e := range b.Entries {
		w.Entries = append(w.Entries, &pb.Entry{
			Name: e.Name,
			Uuid: e.UUID,
			Type: pb.EntryType(e.Type),
		})
	}
	for _, c := range b.Children {
		w.Children = append(w.Children, &pb.Child{
			MinKey: c.MinKey,
			Uuid:   c.UUID,
		})
	}
	return w
}

func fromWire(w *pb.Block) *Block {
	b := &Block{Kind: Kind(w.Kind)}
	for _, e := range w.Entries {
		b.Entries = append(b.Entries, Entry{
			Name: e.Name,
			UUID: e.Uuid,
			Type: EntryType(e.Type),
		})
	}
	for _, c := range w.Children {
		b.Children = append(b.Children, Child{
			MinKey: c.MinKey,
			UUID:   c.Uuid,
		})
	}
	return b
}

// --- Leaf operations ---

// Find returns the entry named name, if present. b must be a Leaf.
func (b *Block) Find(name string) (Entry, bool) {
	i := sort.Search(len(b.Entries), func(i int) bool { return b.Entries[i].Name >= name })
	if i < len(b.Entries) && b.Entries[i].Name == name {
		return b.Entries[i], true
	}
	return Entry{}, false
}

// Insert adds a new entry in sorted position. It fails if name already
// exists. b must be a Leaf.
func (b *Block) Insert(e Entry) error {
	i := sort.Search(len(b.Entries), func(i int) bool { return b.Entries[i].Name >= e.Name })
	if i < len(b.Entries) && b.Entries[i].Name == e.Name {
		return fmt.Errorf("insert %q: %w", e.Name, ErrExists)
	}
	b.Entries = append(b.Entries, Entry{})
	copy(b.Entries[i+1:], b.Entries[i:])
	b.Entries[i] = e
	return nil
}

// Remove deletes the entry named name. It fails if name is not present.
// b must be a Leaf.
func (b *Block) Remove(name string) (Entry, error) {
	i := sort.Search(len(b.Entries), func(i int) bool { return b.Entries[i].Name >= name })
	if i >= len(b.Entries) || b.Entries[i].Name != name {
		return Entry{}, fmt.Errorf("remove %q: %w", name, ErrNotFound)
	}
	e := b.Entries[i]
	b.Entries = append(b.Entries[:i], b.Entries[i+1:]...)
	return e, nil
}

// SplitLeaf splits an overflowing leaf at its median key. The left half
// keeps the keys below the median; the right half (and its MinKey, to be
// inserted as a new Child in the parent) holds the rest.
func (b *Block) SplitLeaf() (left, right *Block, rightMinKey string) {
	mid := len(b.Entries) / 2
	left = &Block{Kind: Leaf, Entries: append([]Entry{}, b.Entries[:mid]...)}
	right = &Block{Kind: Leaf, Entries: append([]Entry{}, b.Entries[mid:]...)}
	return left, right, right.Entries[0].Name
}

// --- Internal node operations ---

// ChildFor returns the child whose subtree covers name. b must be
// Internal and have at least one child (true for every Internal node,
// which is only ever created by a split that produces exactly two).
func (b *Block) ChildFor(name string) Child {
	i := sort.Search(len(b.Children), func(i int) bool { return b.Children[i].MinKey > name })
	if i == 0 {
		i = 1 // Children[0].MinKey == "" always matches, so i is never truly 0
	}
	return b.Children[i-1]
}

// InsertChild adds a new range pointer in sorted position.
func (b *Block) InsertChild(c Child) {
	i := sort.Search(len(b.Children), func(i int) bool { return b.Children[i].MinKey > c.MinKey })
	b.Children = append(b.Children, Child{})
	copy(b.Children[i+1:], b.Children[i:])
	b.Children[i] = c
}

// SplitInternal splits an overflowing internal node at its median key,
// the same way SplitLeaf does for entries.
func (b *Block) SplitInternal() (left, right *Block, rightMinKey string) {
	mid := len(b.Children) / 2
	left = &Block{Kind: Internal, Children: append([]Child{}, b.Children[:mid]...)}
	right = &Block{Kind: Internal, Children: append([]Child{}, b.Children[mid:]...)}
	return left, right, right.Children[0].MinKey
}

// ErrExists and ErrNotFound are the sentinel errors Insert/Remove/Find
// callers match against with errors.Is.
var (
	ErrExists   = fmt.Errorf("entry already exists")
	ErrNotFound = fmt.Errorf("entry not found")
)
