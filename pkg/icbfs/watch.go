package icbfs

import (
	"context"
	"strings"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	core "github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/notify/miniosrc"
)

// WatchMinIO streams a wake-up whenever something in filesystem name
// changes, whoever changed it (a mount, the API, another machine), from
// MinIO's bucket notifications. A wake-up means "go look"; a burst of
// writes (one save touches several objects) arrives as one. It ends when
// ctx does. icbfsdrive.Drive.Notify takes it, for the drive API's event
// stream.
func WatchMinIO(ctx context.Context, c S3, name string) (<-chan struct{}, error) {
	fsys := core.New(OpenS3(c), name)
	if err := fsys.Attach(ctx); err != nil {
		return nil, err
	}
	client, err := NewMinIOClient(c.Endpoint, c.AccessKey, c.SecretKey)
	if err != nil {
		return nil, err
	}
	src := miniosrc.New(ctx, client, c.Bucket, fsys.Prefix())
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		defer src.Close()
		for range src.Signals() {
			select {
			case out <- struct{}{}:
			default: // one is already waiting to be read
			}
		}
	}()
	return out, nil
}

// NewMinIOClient builds the minio-go client MinIO's notifications need
// (they aren't part of the S3 API). endpoint has a scheme, as S3.Endpoint
// does; minio.New wants the bare host and a Secure flag.
func NewMinIOClient(endpoint, accessKey, secretKey string) (*minio.Client, error) {
	secure := strings.HasPrefix(endpoint, "https://")
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	return minio.New(host, &minio.Options{
		Creds:  miniocreds.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
}
