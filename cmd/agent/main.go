// Overwatch site agent: connects to the local O-Zone WebSocket API, batches
// telemetry, and pushes it to the central server. Buffers on outage; reconnects
// with backoff; shuts down gracefully.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"overwatch/agent/internal/app"
	"overwatch/agent/internal/config"
)

func main() {
	// `agent healthcheck` is used by the container healthcheck (distroless has no shell).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck()
		return
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("[agent] Overwatch agent starting")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[agent] configuration error: %v", err)
	}
	log.Printf("[agent] O-Zone %s:%s -> central %s", cfg.OzoneHost, cfg.OzonePort, cfg.CentralURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("[agent] shutdown signal received, draining…")
	}()

	app.New(cfg).Run(ctx)
	log.Println("[agent] stopped")
	os.Exit(0)
}

func healthcheck() {
	addr := os.Getenv("HEALTH_ADDR")
	if addr == "" {
		addr = ":8088"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	_ = resp.Body.Close()
	os.Exit(0)
}
