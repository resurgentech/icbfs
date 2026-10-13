// Command icbfs-logd is the local logging daemon: icbfs mounts on this
// machine connect to its Unix socket to get their store credentials and
// log what happens on them; it relays both over one websocket to the
// server. See internal/logd.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/resurgentech/icbfs/internal/logd"
)

func main() {
	socket := flag.String("socket", "/run/icbfs-logd.sock", "Unix socket icbfs mounts connect to (icbfs mount -log-socket)")
	server := flag.String("server", "ws://127.0.0.1:8090/ws/daemon", "the server's websocket URL")
	secret := flag.String("secret", os.Getenv("ICBFS_LOGD_SECRET"), "shared secret the server expects (default $ICBFS_LOGD_SECRET)")
	flag.Parse()

	d, err := logd.New(logd.Config{Socket: *socket, Server: *server, Secret: *secret})
	if err != nil {
		log.Fatalf("icbfs-logd: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("icbfs-logd: serving mounts on %s, relaying to %s", *socket, *server)
	if err := d.Run(ctx); err != nil {
		log.Fatalf("icbfs-logd: %v", err)
	}
}
