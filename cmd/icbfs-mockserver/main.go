// Command icbfs-mockserver is a mock logging server for icbfs: daemons
// (icbfs-logd) connect to it over a websocket; it hands the configured
// store credentials to mounts of allowed filesystems and appends every
// logged filesystem event to a JSON-lines file. See internal/logserver.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/logserver"
)

func main() {
	addr := flag.String("addr", ":8090", "address to listen on")
	secret := flag.String("secret", os.Getenv("ICBFS_LOGD_SECRET"), "shared secret daemons must present (default $ICBFS_LOGD_SECRET)")
	logPath := flag.String("log", "icbfs-events.jsonl", "JSON-lines file events are appended to")
	allow := flag.String("allow", "", "comma-separated filesystem names allowed to mount; empty allows any")
	endpoint := flag.String("endpoint", "", "S3-API endpoint handed to mounts")
	bucket := flag.String("bucket", "icbfs", "bucket handed to mounts")
	region := flag.String("region", "us-east-1", "region handed to mounts")
	accessKey := flag.String("access-key", "", "access key handed to mounts")
	secretKey := flag.String("secret-key", "", "secret key handed to mounts")
	flag.Parse()

	var allowed []string
	if *allow != "" {
		allowed = strings.Split(*allow, ",")
	}
	s, err := logserver.New(logserver.Config{
		Secret: *secret,
		Log:    *logPath,
		Creds:  fslog.Creds{Endpoint: *endpoint, Bucket: *bucket, Region: *region, AccessKey: *accessKey, SecretKey: *secretKey},
		Allow:  allowed,
	})
	if err != nil {
		log.Fatalf("icbfs-mockserver: %v", err)
	}
	defer s.Close()
	log.Printf("icbfs-mockserver: listening on %s, logging to %s", *addr, *logPath)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
