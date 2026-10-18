// Package block is the directory block format: a median-key B-tree. A Leaf
// holds sorted name -> UUID rows; a node that overflows splits at its
// median key into two, and an Internal block holds sorted pointers to its
// children. A directory's own key is always the entry point to its tree,
// however deep it is. Nodes split but are never merged.
package block

import (
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/pb"
)

// EntryType is the kind of item a row points at. FIFOs, devices and sockets
// exist only on Linux; winfspserver never makes them. The order must match
// spec/proto/icbfs/v1/block.proto's EntryType, which Encode and Decode cast
// to and from numerically.
type EntryType uint8

const (
	TypeFile EntryType = iota
	TypeDir
	TypeSymlink
	TypeFifo
	TypeCharDevice
	TypeBlockDevice
	TypeSocket
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

// Child is one pointer in an internal node: the subtree at UUID covers
// every name >= MinKey, up to the next child's MinKey. A node's first child
// always has MinKey "" ("nothing below what the node's position implies"),
// even after a split, so only the new sibling put in the parent needs a
// real MinKey (SplitLeaf, SplitInternal).
type Child struct {
	MinKey string
	UUID   string
}

// Block is a decoded directory object: a Leaf (sorted entries) or an
// Internal node (sorted child pointers).
//
// Collation is the order they're sorted in. It isn't stored in the block
// but is the filesystem's (Folded for primary-Windows, Exact otherwise),
// set by whoever decodes the block.
type Block struct {
	Kind      Kind
	Collation Collation
	Entries   []Entry
	Children  []Child

	// HighKey is the exclusive upper bound of the names this block covers,
	// and Right the key of the next block at the same level: a B-link
	// tree. A split commits with one write of the splitting block (its
	// left half, linked to the new right half), so a name at or past
	// HighKey is found by moving right (Past), whether or not the parent
	// lists the right half yet. Both are "" for a block with nothing after
	// it, and in blocks written before they existed.
	HighKey string
	Right   string
}

// Past reports whether name sorts at or after b's HighKey: the name
// belongs to a block to the right (b.Right).
func (b *Block) Past(name string) bool {
	return b.HighKey != "" && b.Collation.Compare(name, b.HighKey) >= 0
}

// Layout is the block format this package reads and writes. A block
// written before blocks carried one reads as layout 0, the same format.
const Layout = 1

// ErrNewerFormat is a stored object in a format newer than this build
// knows: refused, not guessed at, so an older binary can't misread it or
// write over it in the old format.
var ErrNewerFormat = errors.New("stored in a newer format than this build reads; upgrade icbfs")

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
	if wire.Layout > Layout {
		return nil, fmt.Errorf("directory block layout %d: %w", wire.Layout, ErrNewerFormat)
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
	w := &pb.Block{Kind: pb.BlockKind(b.Kind), HighKey: b.HighKey, Right: b.Right, Layout: Layout}
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
	b := &Block{Kind: Kind(w.Kind), HighKey: w.HighKey, Right: w.Right}
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

// search returns the index of the first entry not below name under b's
// collation.
func (b *Block) search(name string) int {
	return sort.Search(len(b.Entries), func(i int) bool { return b.Collation.Compare(b.Entries[i].Name, name) >= 0 })
}

// Find returns the entry named name, if present. b must be a Leaf.
func (b *Block) Find(name string) (Entry, bool) {
	i := b.search(name)
	if i < len(b.Entries) && b.Entries[i].Name == name {
		return b.Entries[i], true
	}
	return Entry{}, false
}

// Insert adds a new entry in sorted position. It fails if name already
// exists. b must be a Leaf.
func (b *Block) Insert(e Entry) error {
	i := b.search(e.Name)
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
	i := b.search(name)
	if i >= len(b.Entries) || b.Entries[i].Name != name {
		return Entry{}, fmt.Errorf("remove %q: %w", name, ErrNotFound)
	}
	e := b.Entries[i]
	b.Entries = append(b.Entries[:i], b.Entries[i+1:]...)
	return e, nil
}

// SeekFold returns the index of the first entry that doesn't sort below
// name case-insensitively: in a Folded leaf, where the run of entries
// matching name case-insensitively starts, if it starts in this leaf at
// all. b must be a Leaf.
func (b *Block) SeekFold(name string) int {
	return sort.Search(len(b.Entries), func(i int) bool { return CompareFold(b.Entries[i].Name, name) >= 0 })
}

// SplitLeaf splits an overflowing leaf at its median key. The left half
// keeps the keys below the median and ends at it (HighKey); the right half
// holds the rest and takes over b's HighKey and Right. The caller links
// left.Right to wherever it stores the right half.
func (b *Block) SplitLeaf() (left, right *Block, rightMinKey string) {
	mid := len(b.Entries) / 2
	sep := b.Entries[mid].Name
	left = &Block{Kind: Leaf, Collation: b.Collation, Entries: append([]Entry{}, b.Entries[:mid]...), HighKey: sep}
	right = &Block{Kind: Leaf, Collation: b.Collation, Entries: append([]Entry{}, b.Entries[mid:]...), HighKey: b.HighKey, Right: b.Right}
	return left, right, sep
}

// --- Internal node operations ---

// ChildFor returns the child whose subtree covers name. b must be
// Internal and have at least one child (true for every Internal node,
// which is only ever created by a split that produces exactly two).
func (b *Block) ChildFor(name string) Child {
	i := sort.Search(len(b.Children), func(i int) bool { return b.Collation.Compare(b.Children[i].MinKey, name) > 0 })
	if i == 0 {
		i = 1 // Children[0].MinKey == "" always matches, so i is never truly 0
	}
	return b.Children[i-1]
}

// ChildIndexFold returns the index of the child whose subtree covers the
// lowest possible name matching name case-insensitively: in a Folded
// node, the subtree where that run of entries would start. The run can
// continue into the children after it. b must be Internal.
func (b *Block) ChildIndexFold(name string) int {
	i := sort.Search(len(b.Children)-1, func(i int) bool { return CompareFold(b.Children[i+1].MinKey, name) >= 0 })
	return i // Children[0] has no lower bound, so the answer is never below it
}

// InsertChild adds a new range pointer in sorted position.
func (b *Block) InsertChild(c Child) {
	i := sort.Search(len(b.Children), func(i int) bool { return b.Collation.Compare(b.Children[i].MinKey, c.MinKey) > 0 })
	b.Children = append(b.Children, Child{})
	copy(b.Children[i+1:], b.Children[i:])
	b.Children[i] = c
}

// SplitInternal splits an overflowing internal node at its median key,
// the same way SplitLeaf does for entries.
func (b *Block) SplitInternal() (left, right *Block, rightMinKey string) {
	mid := len(b.Children) / 2
	sep := b.Children[mid].MinKey
	left = &Block{Kind: Internal, Collation: b.Collation, Children: append([]Child{}, b.Children[:mid]...), HighKey: sep}
	right = &Block{Kind: Internal, Collation: b.Collation, Children: append([]Child{}, b.Children[mid:]...), HighKey: b.HighKey, Right: b.Right}
	return left, right, sep
}

// ErrExists and ErrNotFound are the sentinel errors Insert/Remove/Find
// callers match against with errors.Is.
var (
	ErrExists   = fmt.Errorf("entry already exists")
	ErrNotFound = fmt.Errorf("entry not found")
)
