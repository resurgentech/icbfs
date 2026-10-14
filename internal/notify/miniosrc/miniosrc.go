// Package miniosrc is the MinIO change-notification source
// (docs/architecture.md's Change notifications section).
//
// It uses github.com/minio/minio-go/v7, the one place icbfs does:
// ListenBucketNotification is MinIO's own API, not part of S3, so
// aws-sdk-go-v2 can't reach it. It's a separate package so nothing else
// pulls the dependency in.
package miniosrc

import (
	"context"

	"github.com/minio/minio-go/v7"

	"github.com/resurgentech/icbfs/internal/notify"
)

// Source streams create and remove events for one bucket, filtered by MinIO
// itself to prefix (the filesystem's ID prefix).
type Source struct {
	cancel context.CancelFunc
	ch     chan notify.Signal
	done   chan struct{}
}

var _ notify.Source = (*Source)(nil)

// New starts listening for create and remove events on bucket under prefix.
// Cancelling ctx stops it, as Close does.
//
// ListenBucketNotification is a plain long poll with no delivery-mode
// setting; whether MinIO delivers synchronously is the server's own
// notification configuration.
func New(ctx context.Context, client *minio.Client, bucket, prefix string) *Source {
	listenCtx, cancel := context.WithCancel(ctx)
	infoCh := client.ListenBucketNotification(listenCtx, bucket, prefix, "", []string{
		"s3:ObjectCreated:*",
		"s3:ObjectRemoved:*",
	})

	s := &Source{
		cancel: cancel,
		ch:     make(chan notify.Signal),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(s.ch)
		defer close(s.done)
		for info := range infoCh {
			if info.Err != nil {
				// A failed poll only delays a wake-up;
				// ListenBucketNotification retries on its own.
				continue
			}
			for _, rec := range info.Records {
				select {
				case s.ch <- notify.Signal{Key: rec.S3.Object.Key}:
				case <-listenCtx.Done():
					return
				}
			}
		}
	}()
	return s
}

func (s *Source) Signals() <-chan notify.Signal { return s.ch }

// Close stops listening and waits for the internal forwarding
// goroutine to exit. Safe to call more than once.
func (s *Source) Close() error {
	s.cancel()
	<-s.done
	return nil
}
