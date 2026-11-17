package azurestore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
// icbfs's ids are 17 bytes (blockIDs). On a staging failure that
// is a 400, PutParts reads the blob's uncommitted blocks once (Get Block
// List, blocklisttype=uncommitted); if they are of another length, it
// takes that length for its own ids (blockIDs.id), stages the block again,
// and every later block under the new ids. Its commit lists only its own
// blocks, so the stale ones are garbage-collected by it: one version is
// written, the PutParts's own; nothing else is committed. A healthy
// PutParts makes no request more; a failure that is not this one, or a
// second failure, is the error as before.

// blockIDs are a PutParts's block ids (T82-10 round 3, review 2's B3):
// unique to the call, so that two writes of one blob at once, each
// committing only the ids it staged, never commit each other's blocks
// (Azure commits, under an id, whatever was staged under it last). An id
// is the call's own random tag (lowercase letters and digits, from
// crypto/rand), cut to fit, then the block's number in 4 base-36 digits
// (50,000 blocks need 4: 36^4 = 1,679,616). All of a call's ids have one
// length: icbfs's own 17 bytes (13 characters of tag, about 67 bits), or,
// once repaired, the length the blob's stale blocks force, whose tag is
// then that length less 4. The shortest taken is minIDLen, 8 bytes: a
// 4-character tag (36^4 = 1,679,616 tags, a 1 in 1.7 million chance that
// two writes of one blob at once, both repaired to 8 bytes, share one);
// stale blocks of shorter ids are not matched (the store's error, as
// before). icbfs-git also writes one path at a time (its assemblies), so
// that chance is met only by two ordinary large writes of one file at
// once over stale blocks of an 8-byte length.
type blockIDs struct {
	mu       sync.Mutex
	tag      string // the call's own, 60 characters; cut to the width
	width    int    // the raw id's length in bytes; 0: icbfs's own (17)
	repaired bool   // a repair was tried (once a PutParts)
}

// ownIDLen is the raw length of icbfs's own ids, as before round 3 (the
// encoded length, 24 characters, stays: stale blocks of icbfs's older ids
// are of the same length and never fail a write).
const ownIDLen = 17

// numLen is the base-36 digits of a block's number; minIDLen the shortest
// raw id (a 4-character tag and the number).
const (
	numLen   = 4
	minIDLen = numLen + 4
)

func newBlockIDs() *blockIDs {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var raw [60]byte
	_, _ = rand.Read(raw[:])
	for i, c := range raw {
		raw[i] = alphabet[int(c)%len(alphabet)]
	}
	return &blockIDs{tag: string(raw[:])}
}

// id is block i's id, base64 as Azure takes it.
func (b *blockIDs) id(i int) string {
	b.mu.Lock()
	w := b.width
	b.mu.Unlock()
	return base64.StdEncoding.EncodeToString([]byte(b.rawID(i, w)))
}

// rawID is block i's id of raw length w (0: icbfs's own): the call's tag
// cut to w-numLen, then i in numLen base-36 digits. A width past the tag
// and the number is padded with '-' between them.
func (b *blockIDs) rawID(i, w int) string {
	if w == 0 {
		w = ownIDLen
	}
	num := strconv.FormatInt(int64(i), 36)
	num = strings.Repeat("0", numLen-len(num)) + num
	tag := b.tag
	if t := w - numLen; t <= len(tag) {
		tag = tag[:t]
	} else {
		tag += strings.Repeat("-", t-len(tag))
	}
	return tag + num
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
	own := len(base64.StdEncoding.EncodeToString([]byte(b.rawID(0, b.width))))
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
