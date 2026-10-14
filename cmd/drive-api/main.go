// Command drive-api serves the drive API on its own, for testing and the
// VM runs, with what a host application would supply — authentication, the drive
// registry, actors' profiles and permissions — from a JSON configuration
// file (driveapi.Standalone):
//
//	drive-api -config drive-api.json
//
// Requests carry "Authorization: Bearer <token>" and "X-Drive-Actor:
// <actor>".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gofiber/fiber/v3"

	"github.com/resurgentech/icbfs/pkg/drive/driveapi"
)

func main() {
	configPath := flag.String("config", "drive-api.json", "configuration file")
	listen := flag.String("listen", "", "address to listen on (overrides the configuration's)")
	audit := flag.Bool("audit", false, "write every drive operation to stdout, one JSON line each")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	var st driveapi.Standalone
	if err := json.Unmarshal(data, &st); err != nil {
		log.Fatalf("%s: %v", *configPath, err)
	}
	if *listen != "" {
		st.Listen = *listen
	}
	if st.Listen == "" {
		st.Listen = ":8091"
	}
	if st.Token == "" {
		log.Fatal("the configuration needs a token")
	}
	ctx := context.Background()
	cfg, cleanup, err := st.Config(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	stopEvents := make(chan struct{})
	cfg.Stop = stopEvents
	if *audit {
		enc := json.NewEncoder(os.Stdout)
		var mu sync.Mutex
		cfg.Audit = func(e driveapi.AuditEvent) {
			mu.Lock()
			defer mu.Unlock()
			_ = enc.Encode(e)
		}
	}
	app := fiber.New(fiber.Config{
		BodyLimit:         1 << 40, // the filesystem's max file size decides
		StreamRequestBody: true,
	})
	driveapi.Mount(app, cfg)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		close(stopEvents)
		_ = app.Shutdown()
	}()
	log.Printf("drive-api listening on %s", st.Listen)
	if err := app.Listen(st.Listen, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		log.Fatal(err)
	}
}
