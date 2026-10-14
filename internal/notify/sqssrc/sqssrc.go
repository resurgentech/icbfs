// Package sqssrc is the AWS S3 change-notification source
// (docs/architecture.md's Change notifications section): the bucket's event
// notifications go to an SQS queue, and this long-polls the queue. It uses
// aws-sdk-go-v2's SQS client, the same SDK as everything else.
//
// Configuring the bucket to send events to the queue
// (PutBucketNotificationConfiguration, with a server-side prefix filter) is
// left to whoever provisions it. With no AWS account to test against, the
// tests use a fake queue.
package sqssrc

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/resurgentech/icbfs/internal/notify"
)

// receiver is the part of the SQS client this uses; *sqs.Client satisfies
// it (asserted below), and so does the tests' fake.
type receiver interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

var _ receiver = (*sqs.Client)(nil)

// s3EventNotification is the part of S3's event notification JSON (an SQS
// message body) this needs: each record's object key. MinIO's format is the
// same schema; it's defined here rather than importing MinIO's package into
// an AWS adapter.
type s3EventNotification struct {
	Records []struct {
		S3 struct {
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// waitTimeSeconds is the long-poll duration every ReceiveMessage call
// uses: 20 is SQS's own maximum and the one worth using here, since this feature is
// latency-tolerant by design.
const waitTimeSeconds = 20

// Source long-polls queueURL, parses each message as S3 event
// notifications, forwards records whose keys start with prefix (a cheap
// safety net: the bucket's configuration can filter too, but it's outside
// this package), and deletes each message after, best effort (a failed
// delete only means redelivery).
type Source struct {
	cancel context.CancelFunc
	ch     chan notify.Signal
	done   chan struct{}
}

var _ notify.Source = (*Source)(nil)

// New starts long-polling queueURL for events under prefix.
func New(ctx context.Context, client receiver, queueURL, prefix string) *Source {
	pollCtx, cancel := context.WithCancel(ctx)
	s := &Source{
		cancel: cancel,
		ch:     make(chan notify.Signal),
		done:   make(chan struct{}),
	}
	go s.run(pollCtx, client, queueURL, prefix)
	return s
}

func (s *Source) run(ctx context.Context, client receiver, queueURL, prefix string) {
	defer close(s.ch)
	defer close(s.done)
	for {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     waitTimeSeconds,
		})
		if err != nil {
			// Per docs/architecture.md's governing principle, a failed
			// poll is never a correctness problem for this feature —
			// just a slower wake-up next time around.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		for _, msg := range out.Messages {
			if !s.deliver(ctx, msg, prefix) {
				return
			}
			if msg.ReceiptHandle != nil {
				_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl:      &queueURL,
					ReceiptHandle: msg.ReceiptHandle,
				})
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// deliver forwards each matching record in msg as a Signal. It returns
// false if ctx ended while sending; the caller then stops without deleting
// the message.
func (s *Source) deliver(ctx context.Context, msg types.Message, prefix string) bool {
	if msg.Body == nil {
		return true
	}
	var evt s3EventNotification
	if err := json.Unmarshal([]byte(*msg.Body), &evt); err != nil {
		return true // malformed body: drop, consistent with "just a hint"
	}
	for _, rec := range evt.Records {
		if !strings.HasPrefix(rec.S3.Object.Key, prefix) {
			continue
		}
		select {
		case s.ch <- notify.Signal{Key: rec.S3.Object.Key}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (s *Source) Signals() <-chan notify.Signal { return s.ch }

// Close stops polling and waits for the internal goroutine to exit.
// Safe to call more than once.
func (s *Source) Close() error {
	s.cancel()
	<-s.done
	return nil
}
