// Package sqssrc is the AWS S3 (via SQS) backend adapter for Change
// notifications (ARCHITECTURE.md's Change notifications section,
// ROADMAP.md's task E6): a bucket notification configuration routes
// S3 events to an SQS queue; this package long-polls that queue.
//
// Stays within aws-sdk-go-v2 (service/sqs) — no new dependency,
// unlike MinIO's adapter (task E4), which genuinely needed one
// because the feature it wraps isn't exposed anywhere in the generic
// S3 API aws-sdk-go-v2 targets. SQS, by contrast, is just another
// AWS service with its own aws-sdk-go-v2 client, same family as
// everything else in this project.
//
// Setting up the actual S3-bucket-to-SQS notification configuration
// (PutBucketNotificationConfiguration, with whatever prefix/suffix
// filter — AWS S3 supports server-side filtering here, unlike Azure's
// Change Feed) is not this package's job and isn't implemented here:
// ROADMAP.md's task E6 scopes this adapter to the consumption side
// only, treating that configuration as an externally-provisioned
// concern. There is no real AWS account available in this environment
// to test against (ARCHITECTURE.md's Testing reality note) — same
// constraint task E5 documents for Azure — so this package's test
// coverage is a fake queue exercising the consumption/delete logic,
// with real-account testing an explicit, documented manual gap, not a
// silently-assumed one.
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

// receiver is the minimal SQS client surface this adapter needs,
// verified directly against the installed aws-sdk-go-v2/service/sqs
// source (same "verified not assumed" bar as every other backend API
// this project touches) — a real *sqs.Client satisfies it (see the
// assertion below), and a test fake can too, without needing real AWS
// credentials or network access, per this task's prescribed test
// strategy.
type receiver interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

var _ receiver = (*sqs.Client)(nil)

// s3EventNotification is the subset of AWS's S3 Event Notification
// JSON schema (what a bucket notification configuration delivers as
// an SQS message body) this adapter needs — just enough to pull each
// record's object key out. This is the same well-established schema
// MinIO's own notification format mirrors for S3 compatibility
// (confirmed by inspecting minio-go/v7/pkg/notification's types
// directly while building task E4's adapter); defined locally here
// rather than importing that MinIO-specific package from an AWS
// adapter.
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
// uses — ROADMAP.md's task E6 explicitly calls for "long-polling
// ReceiveMessage (WaitTimeSeconds up to 20)"; 20 is SQS's own maximum
// and the one actually worth using here, since this feature is
// latency-tolerant by design.
const waitTimeSeconds = 20

// Source long-polls queueURL, parses each message's body as an S3
// event notification, filters each record's key against prefix
// client-side (a redundant safety net — AWS S3 supports server-side
// prefix filtering on the bucket notification configuration itself,
// per ARCHITECTURE.md, but that configuration lives outside this
// package, so this check costs little and keeps this Source's
// filtering contract consistent with the other two backend adapters'),
// and deletes each message afterward on a best-effort basis (a failed
// delete just risks redelivery later, which this feature's "just a
// wake-up hint" governing principle already tolerates — not treated
// as fatal).
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
			// Per ARCHITECTURE.md's governing principle, a failed
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

// deliver parses msg's body and forwards each matching record's key
// as a Signal. Returns false if ctx ended while trying to send (the
// caller should stop immediately in that case, without deleting the
// message it never finished delivering).
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
