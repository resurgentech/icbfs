// Package testutil holds small, dependency-free helpers shared across
// this project's test suites.
package testutil

import (
	"testing"
	"time"
)

// RetryUntilReady calls fn, with a short fixed backoff, until it returns
// nil or timeout passes, then fails the test with the last error.
//
// testcontainers' MinIO module waits for MinIO's liveness endpoint, not
// readiness, so the first S3 call just after can still be refused for a
// moment. Test helpers retry their first call with this.
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
