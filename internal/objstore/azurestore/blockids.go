package azurestore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

// A PutParts's block ids (T82-10 round 2). Azure takes a blob's blocks
// under ids of one length: "For a specified blob, all block IDs must be
// the same length. If a block is uploaded with a block ID of a different
// length than the block IDs for any existing uncommitted blocks, the
// service returns error response code 400 (Bad Request)" (Put Block,
// learn.microsoft.com/en-us/rest/api/storageservices/put-block, Remarks).
// Uncommitted blocks live until the blob's next commit ("Any uncommitted
// blocks that weren't specified in the block list for the blob are
// garbage collected"), or a week after the last successful Put Block on
// the blob, or a Put Blob. So a write cut half way (the SDK's stream
// upload, whose ids are another length, or a PutParts of icbfs's own)
// leaves blocks that make the blob's next PutParts fail for up to a week.
//
// icbfs's ids are "icbfs-part-%06d" (17 bytes). On a staging failure that
// is a 400, PutParts reads the blob's uncommitted blocks once (Get Block
// List, blocklisttype=uncommitted); if they are of another length, it
// takes that length for its own ids (blockIDs.id), stages the block again,
// and every later block under the new ids. Its commit lists only its own
// blocks, so the stale ones are garbage-collected by it: one version is
// written, the PutParts's own; nothing else is committed. A healthy
// PutParts makes no request more; a failure that is not this one, or a
// second failure, is the error as before.

// blockIDs are a PutParts's block ids: icbfs's own, or, once repaired, of
// the length the blob's uncommitted blocks have.
type blockIDs struct {
	mu       sync.Mutex
	width    int  // the raw id's length in bytes; 0: icbfs's own (17)
	repaired bool // a repair was tried (once a PutParts)
}

// ownIDLen is the raw length of icbfs's own ids.
const ownIDLen = len("icbfs-part-000000")

// minIDLen is the shortest raw id that numbers 50,000 blocks.
const minIDLen = 5

// id is block i's id, base64 as Azure takes it.
func (b *blockIDs) id(i int) string {
	b.mu.Lock()
	w := b.width
	b.mu.Unlock()
	return base64.StdEncoding.EncodeToString([]byte(rawID(i, w)))
}

// rawID is block i's id of raw length w (0: icbfs's own).
func rawID(i, w int) string {
	switch {
	case w == 0 || w == ownIDLen:
		return fmt.Sprintf("icbfs-part-%06d", i)
	case w > ownIDLen:
		return fmt.Sprintf("icbfs-part-%0*d", w-len("icbfs-part-"), i)
	}
	return fmt.Sprintf("%0*d", w, i)
}

// repair is called on a failure err of staging a block of blob bb. It
// says whether the block should be staged again, under the ids b now
// gives: once a PutParts, for a 400 when the blob's uncommitted blocks
// have ids of a length other than b's. Concurrent callers wait for the
// one repair and are told the same.
func (b *blockIDs) repair(ctx context.Context, bb *blockblob.Client, err error) bool {
	var re *azcore.ResponseError
	if !errors.As(err, &re) || re.StatusCode != http.StatusBadRequest {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.repaired {
		return b.width != 0
	}
	b.repaired = true
	list, lerr := bb.GetBlockList(ctx, blockblob.BlockListTypeUncommitted, nil)
	if lerr != nil {
		return false
	}
	// Azure compares the ids as sent: base64, so 16 to 18 raw bytes are
	// all 24 characters.
	own := len(base64.StdEncoding.EncodeToString([]byte(rawID(0, b.width))))
	for _, blk := range list.UncommittedBlocks {
		if blk == nil || blk.Name == nil || len(*blk.Name) == own {
			continue
		}
		raw, derr := base64.StdEncoding.DecodeString(*blk.Name)
		if derr != nil || len(raw) < minIDLen {
			return false
		}
		b.width = len(raw)
		return true
	}
	return false
}
