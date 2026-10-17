package s3store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/objstore"
)

const conflictBody = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>ConditionalRequestConflict</Code><Message>The conditional request cannot succeed due to a conflicting operation against this resource.</Message></Error>`

// conflictServer answers the first conflicts PUTs with AWS's 409
// ConditionalRequestConflict, then the next with status (200, or 412), and
// HEADs with an object. MinIO never answers 409, so only a fake shows it.
func conflictServer(t *testing.T, conflicts int, status int) (*Store, *atomic.Int32) {
	t.Helper()
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("x-amz-version-id", "v1")
		w.Header().Set("ETag", `"e1"`)
		switch r.Method {
		case http.MethodPut:
			n := int(puts.Add(1))
			switch {
			case n <= conflicts:
				w.WriteHeader(http.StatusConflict)
				io.WriteString(w, conflictBody)
			case status == http.StatusPreconditionFailed:
				w.WriteHeader(status)
				io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>no</Message></Error>`)
			default:
				w.WriteHeader(status)
			}
		case http.MethodHead:
			w.Header().Set("Content-Length", "5")
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		}
	}))
	t.Cleanup(srv.Close)
	saved := conflictRetryer
	conflictRetryer = newConflictRetryer(func(o *retry.StandardOptions) {
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	})
	t.Cleanup(func() { conflictRetryer = saved })
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider("ak", "sk", ""),
	})
	return New(client, "b"), &puts
}

func TestConflictIsRetried(t *testing.T) {
	ctx := context.Background()
	for name, put := range map[string]func(*Store) error{
		"put if match": func(s *Store) error {
			_, err := s.Put(ctx, "k", strings.NewReader("hello"), nil, `"e0"`)
			return err
		},
		"put if absent": func(s *Store) error {
			_, err := s.PutIfAbsent(ctx, "k", strings.NewReader("hello"), nil)
			return err
		},
	} {
		s, puts := conflictServer(t, 3, http.StatusOK)
		if err := put(s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := puts.Load(); n != 4 {
			t.Errorf("%s: %d PUTs, want 4: three conflicts, then the write", name, n)
		}
	}
}

func TestConflictThenLostRace(t *testing.T) {
	s, _ := conflictServer(t, 2, http.StatusPreconditionFailed)
	_, err := s.Put(context.Background(), "k", strings.NewReader("hello"), nil, `"e0"`)
	if !objstore.IsPreconditionFailed(err) {
		t.Fatalf("a conflict, then the other writer won: %v, want a failed precondition", err)
	}
}

func TestConflictGivesUp(t *testing.T) {
	s, puts := conflictServer(t, 1000, http.StatusOK)
	_, err := s.Put(context.Background(), "k", strings.NewReader("hello"), nil, `"e0"`)
	if err == nil {
		t.Fatal("no error after endless conflicts")
	}
	if n := puts.Load(); n != conflictAttempts {
		t.Errorf("%d PUTs, want %d", n, conflictAttempts)
	}
}
