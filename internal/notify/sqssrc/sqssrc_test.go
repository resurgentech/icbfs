package sqssrc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// fakeQueue implements receiver over an in-memory message list, for
// exercising the consumption/delete logic without a real AWS account
// — task E6's explicitly-chosen test strategy (ARCHITECTURE.md's
// Testing reality note: no real account is provisionable here).
type fakeQueue struct {
	mu       sync.Mutex
	pending  []types.Message
	deleted  []string // ReceiptHandles passed to DeleteMessage, in order
	receives int
}

func (q *fakeQueue) enqueueBody(body string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	handle := fmt.Sprintf("handle-%d", len(q.pending)+len(q.deleted))
	q.pending = append(q.pending, types.Message{
		Body:          aws.String(body),
		ReceiptHandle: aws.String(handle),
	})
}

func (q *fakeQueue) ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.receives++
	if len(q.pending) == 0 {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	msgs := q.pending
	q.pending = nil
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (q *fakeQueue) DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, aws.ToString(params.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

func (q *fakeQueue) deletedSoFar() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.deleted...)
}

func s3EventBody(key string) string {
	return fmt.Sprintf(`{"Records":[{"s3":{"object":{"key":%q}}}]}`, key)
}

// TestSourceFiltersByPrefixAndDeletesConsumedMessages covers
// ROADMAP.md's task E6 "Done when": the consumption logic has a
// passing test against a fake queue. A matching key is forwarded as a
// Signal and its message deleted afterward; a non-matching key (the
// client-side safety-net filter — see Source's doc comment) is never
// forwarded, but its message is still deleted (it was consumed either
// way, same as a real queue consumer would).
func TestSourceFiltersByPrefixAndDeletesConsumedMessages(t *testing.T) {
	queue := &fakeQueue{}
	queue.enqueueBody(s3EventBody("0000-match"))
	queue.enqueueBody(s3EventBody("9999-no-match"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := New(ctx, queue, "https://sqs.example/queue", "0000-")
	defer src.Close()

	select {
	case sig := <-src.Signals():
		if sig.Key != "0000-match" {
			t.Fatalf("got signal for key %q, want %q", sig.Key, "0000-match")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the matching signal")
	}

	select {
	case sig := <-src.Signals():
		t.Fatalf("received an unexpected second signal for key %q — prefix filtering isn't working", sig.Key)
	case <-time.After(500 * time.Millisecond):
		// Expected: the non-matching key should never be forwarded.
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(queue.deletedSoFar()) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d deleted messages, want 2 (both messages should be consumed and deleted, matching or not)", len(queue.deletedSoFar()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSourceIgnoresMalformedMessageBody confirms a message whose body
// isn't valid S3 event JSON is dropped rather than panicking or
// wedging the poll loop — a real, if unlikely, possibility for
// anything consuming from a queue it doesn't fully control the
// producer side of.
func TestSourceIgnoresMalformedMessageBody(t *testing.T) {
	queue := &fakeQueue{}
	queue.enqueueBody("not valid json")
	queue.enqueueBody(s3EventBody("0000-after-bad-message"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := New(ctx, queue, "https://sqs.example/queue", "0000-")
	defer src.Close()

	select {
	case sig := <-src.Signals():
		if sig.Key != "0000-after-bad-message" {
			t.Fatalf("got signal for key %q, want %q", sig.Key, "0000-after-bad-message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — a malformed message body may have wedged the poll loop")
	}
}
