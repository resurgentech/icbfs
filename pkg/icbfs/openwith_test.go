package icbfs

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// refusing is a transport that sends nothing and counts what it was asked.
type refusing struct{ asked atomic.Int64 }

var errRefused = errors.New("refused by the host's client")

func (r *refusing) RoundTrip(*http.Request) (*http.Response, error) {
	r.asked.Add(1)
	return nil, errRefused
}

// OpenWith's HTTPClient sends every request of a store of either kind: a
// client that refuses is the only thing the store ever reaches.
func TestOpenWith_HTTPClientSendsEveryRequest(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("an invented account key"))
	for name, c := range map[string]StoreConfig{
		"s3":    {Kind: KindS3, S3: S3{Endpoint: "https://store.invalid", Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		"azure": {Kind: KindAzure, Azure: Azure{Account: "inventedaccount", Key: key, Container: "c"}},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &refusing{}
			store, err := OpenWith(c, Options{HTTPClient: &http.Client{Transport: rt}})
			if err != nil {
				t.Fatalf("OpenWith: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := store.Head(ctx, "a-key"); err == nil {
				t.Fatal("Head answered through a client that refuses every request")
			}
			if rt.asked.Load() == 0 {
				t.Fatal("the store did not send its request through Options.HTTPClient")
			}
		})
	}
}

// Open is OpenWith with no options: it still opens both kinds.
func TestOpen_NoOptions(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("an invented account key"))
	for _, c := range []StoreConfig{
		{S3: S3{Endpoint: "https://store.invalid", Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		{Kind: KindAzure, Azure: Azure{Account: "inventedaccount", Key: key, Container: "c"}},
	} {
		if _, err := Open(c); err != nil {
			t.Fatalf("Open(%q): %v", c.Kind, err)
		}
	}
	if _, err := OpenWith(StoreConfig{Kind: "ftp"}, Options{}); err == nil {
		t.Fatal("OpenWith opened a store of an unknown kind")
	}
}
