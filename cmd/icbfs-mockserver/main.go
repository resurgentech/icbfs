// Command icbfs-mockserver is a mock of the server end of the
// daemon chain: daemons (icbfs-logd) connect to it over a websocket; it tells
// mounts how to mount the drives in its registry (store credentials,
// filesystem, root, read-only), appends every logged filesystem event to
// a JSON-lines file, and runs icbfs-cli's side commands sent through
// daemons. See internal/logserver.
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
	drives := flag.String("drives", "", `the drive registry, comma-separated: id=filesystem[:/root][:ro] ("docs=docs,team-a=docs:/team-a:ro")`)
	allowCommands := flag.String("allow-commands", "", `comma-separated side commands daemons may run ("names create,trash list"); empty allows every one`)
	endpoint := flag.String("endpoint", "", "S3-API endpoint handed to mounts")
	storeEndpoint := flag.String("store-endpoint", "", "the S3-API endpoint as this server reaches it, for side commands (default -endpoint)")
	bucket := flag.String("bucket", "icbfs", "bucket handed to mounts")
	region := flag.String("region", "us-east-1", "region handed to mounts")
	accessKey := flag.String("access-key", "", "access key handed to mounts")
	secretKey := flag.String("secret-key", "", "secret key handed to mounts")
	flag.Parse()

	registry, err := parseDrives(*drives)
	if err != nil {
		log.Fatalf("icbfs-mockserver: -drives: %v", err)
	}
	var commands []string
	if *allowCommands != "" {
		commands = strings.Split(*allowCommands, ",")
	}
	s, err := logserver.New(logserver.Config{
		AllowCommands: commands,
		StoreEndpoint: *storeEndpoint,
		Secret:        *secret,
		Log:           *logPath,
		Creds:         fslog.Creds{Endpoint: *endpoint, Bucket: *bucket, Region: *region, AccessKey: *accessKey, SecretKey: *secretKey},
		Drives:        registry,
	})
	if err != nil {
		log.Fatalf("icbfs-mockserver: %v", err)
	}
	defer s.Close()
	log.Printf("icbfs-mockserver: listening on %s, logging to %s", *addr, *logPath)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}

// parseDrives reads id=filesystem[:/root][:ro],...
func parseDrives(spec string) (map[string]fslog.Mount, error) {
	out := map[string]fslog.Mount{}
	if spec == "" {
		return out, nil
	}
	for _, d := range strings.Split(spec, ",") {
		id, rest, ok := strings.Cut(d, "=")
		if !ok || id == "" || rest == "" {
			return nil, errBadDrive(d)
		}
		parts := strings.Split(rest, ":")
		m := fslog.Mount{Filesystem: parts[0]}
		for _, p := range parts[1:] {
			switch {
			case p == "ro":
				m.ReadOnly = true
			case strings.HasPrefix(p, "/"):
				m.Root = p
			default:
				return nil, errBadDrive(d)
			}
		}
		out[id] = m
	}
	return out, nil
}

type errBadDrive string

func (e errBadDrive) Error() string {
	return "bad drive " + string(e) + `: want id=filesystem[:/root][:ro]`
}
