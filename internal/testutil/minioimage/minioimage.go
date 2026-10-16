// Package minioimage names the MinIO container image the tests run.
package minioimage

import "os"

// Default is the image the tests run unless ICBFS_MINIO_IMAGE names
// another. MinIO no longer publishes minio/minio (on Docker Hub or quay.io)
// for free use; Chainguard's build is free, as its latest tag only.
const Default = "cgr.dev/chainguard/minio:latest"

// Image is $ICBFS_MINIO_IMAGE, or Default.
func Image() string {
	if img := os.Getenv("ICBFS_MINIO_IMAGE"); img != "" {
		return img
	}
	return Default
}
