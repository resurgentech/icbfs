// Package testutil holds small, dependency-free helpers shared across
// this project's test suites.
package testutil

import (
	"testing"
	"time"
)

// RetryUntilReady calls fn repeatedly, with a short fixed backoff
// between attempts, until it returns nil or timeout elapses — then
// fails the test with the last error if it never succeeded.
//
// Exists to close a real, confirmed gap between a just-started test
// container reporting "ready" and its API actually being able to
// serve a request: testcontainers-go's MinIO module waits on MinIO's
// own /minio/health/live endpoint (a liveness probe — "the process is
// up"), not a readiness one, and a real S3 API call immediately
// afterward (e.g. CreateBucket) can still intermittently fail for a
// brief window with a connection-refused/timeout error. Retrying the
// first real API call each test helper makes, rather than trusting
// the container's own "ready" signal, is the actual fix — not a
// documented, accepted flake.
func RetryUntilReady(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = fn()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still failing after %v of retrying (container reported ready, but the real API call never succeeded): %v", timeout, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
