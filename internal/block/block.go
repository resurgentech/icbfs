// Package block implements the directory block format: the body of a
// directory (or root) object, which is just a list of name -> UUID rows.
// Metadata (mode, uid, gid, ...) deliberately does not live here — see
// ARCHITECTURE.md's metadata model. This is the flat, unsharded version;
// median-key B-tree splitting for large directories is not implemented yet.
package block

import (
	"encoding/json"
	"fmt"
)

// EntryType is the kind of node a directory row points at.
type EntryType uint8

const (
	TypeFile EntryType = iota
	TypeDir
	TypeSymlink
)

// Entry is one row of a directory block.
type Entry struct {
	Name string    `json:"name"`
	UUID string    `json:"uuid"`
	Type EntryType `json:"type"`
}

// Block is the decoded body of a directory object.
type Block struct {
	Entries []Entry `json:"entries"`
}

// Decode parses a directory block body. Empty input decodes to an empty
// block, so a freshly created directory's body can just be nil/empty bytes.
func Decode(data []byte) (*Block, error) {
	if len(data) == 0 {
		return &Block{}, nil
	}
	var b Block
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode directory block: %w", err)
	}
	return &b, nil
}

// Encode serializes the block back to bytes.
func (b *Block) Encode() ([]byte, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode directory block: %w", err)
	}
	return data, nil
}

// Find returns the entry named name, if present.
func (b *Block) Find(name string) (Entry, bool) {
	for _, e := range b.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Insert adds a new entry. It fails if name already exists.
func (b *Block) Insert(e Entry) error {
	if _, ok := b.Find(e.Name); ok {
		return fmt.Errorf("insert %q: %w", e.Name, ErrExists)
	}
	b.Entries = append(b.Entries, e)
	return nil
}

// Remove deletes the entry named name. It fails if name is not present.
func (b *Block) Remove(name string) (Entry, error) {
	for i, e := range b.Entries {
		if e.Name == name {
			b.Entries = append(b.Entries[:i], b.Entries[i+1:]...)
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("remove %q: %w", name, ErrNotFound)
}

// ErrExists and ErrNotFound are the sentinel errors Insert/Remove/Find
// callers match against with errors.Is.
var (
	ErrExists   = fmt.Errorf("entry already exists")
	ErrNotFound = fmt.Errorf("entry not found")
)
