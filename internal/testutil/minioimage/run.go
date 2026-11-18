package minioimage

import (
	"context"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/minio"
)

// Run starts a MinIO container of Image(), trying again when Docker could
// not publish its port. The container's port 9000 is published on a host
// port Docker picks when the container starts (testcontainers asks for
// any free one); on a busy machine that port can be taken between Docker's
// choice and its bind, by a test's own outgoing connection from the same
// range of ports, and the start fails with "address already in use" (or
// "port is already allocated"). Such a start left nothing running; a new
// one picks another port.
func Run(ctx context.Context) (*minio.MinioContainer, error) {
	var err error
	for try := 1; ; try++ {
		var c *minio.MinioContainer
		c, err = minio.Run(ctx, Image())
		if err == nil || try == 5 || !portTaken(err) {
			return c, err
		}
		if c != nil {
			_ = c.Terminate(context.Background())
		}
		time.Sleep(time.Duration(try) * 200 * time.Millisecond)
	}
}

func portTaken(err error) bool {
	s := err.Error()
	return strings.Contains(s, "address already in use") || strings.Contains(s, "port is already allocated")
}
