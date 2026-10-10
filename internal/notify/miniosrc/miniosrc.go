// Package miniosrc is the MinIO backend adapter for Change
// notifications (ARCHITECTURE.md's Change notifications section,
// ROADMAP.md's task E4).
//
// New dependency: github.com/minio/minio-go/v7. Confirmed non-
// standard, not an assumption — ListenBucketNotification/
// ListenNotification are explicitly MinIO-specific APIs (per their
// own doc comments in minio-go's source: "this is a MinIO specific
// API"), not reachable via the aws-sdk-go-v2 client this project
// otherwise uses exclusively for every S3-API call elsewhere. This is
// a deliberate, documented exception to the project's otherwise-
// single-SDK approach: the feature itself (a server-side, long-poll
// bucket event stream) simply isn't exposed anywhere in the generic
// S3 API aws-sdk-go-v2 targets. Kept in its own subpackage (not
// internal/notify itself) so a consumer that only needs the core
// notify.Source interface, or a different backend's adapter, doesn't
// have to pull this dependency in transitively.
package miniosrc

import (
	"context"

	"github.com/minio/minio-go/v7"

	"github.com/resurgentech/icbfs/internal/notify"
)

// Source streams object-created/removed events for one bucket,
// scoped server-side to prefix (the owning filesystem's ID prefix,
// per ARCHITECTURE.md's Multiple filesystems per bucket section) —
// MinIO supports this filtering directly, unlike Azure's Change Feed
// (task E5), which has no server-side filtering at all.
type Source struct {
	cancel context.CancelFunc
	ch     chan notify.Signal
	done   chan struct{}
}

var _ notify.Source = (*Source)(nil)

// New starts listening for create/remove events on bucket under
// prefix and returns a Source wrapping the stream. ctx bounds the
// listen's lifetime in addition to Close — cancelling it has the same
// effect as calling Close.
//
// There is no separate sync/async delivery-mode parameter to set
// here, despite ARCHITECTURE.md's Change notifications section
// calling out MinIO's sync mode as worth preferring: confirmed by
// reading ListenBucketNotification's implementation directly that it
// is a direct, unqueued client HTTP long-poll with no such parameter
// exposed at this API surface at all. If MinIO's sync/async delivery
// distinction applies here, it would be a server-side deployment
// setting (the MinIO server's own notification configuration), not
// something this Go client call controls — flagged in ASSUMPTIONS.md
// as worth clarifying against a real deployment, not silently
// asserted as handled.
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
				// Per ARCHITECTURE.md's governing principle, a missed
				// or errored poll is never a correctness problem for
				// this feature — just a slower wake-up next time
				// around. The underlying retry/backoff loop lives
				// inside ListenBucketNotification itself.
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
