// Package health exposes liveness/readiness endpoints for container healthchecks.
package health

import (
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

type Health struct {
	lastPoll atomic.Int64
	lastPush atomic.Int64
	reason   atomic.Pointer[func() string]
}

// SetReason sets what /readyz says while not ready, when it has something
// more specific to say than "not ready" (e.g. no game server chosen yet). Set
// it before Serve.
func (h *Health) SetReason(fn func() string) { h.reason.Store(&fn) }

func New() *Health { return &Health{} }

func (h *Health) MarkPoll() { h.lastPoll.Store(time.Now().Unix()) }
func (h *Health) MarkPush() { h.lastPush.Store(time.Now().Unix()) }

// Serve starts the health HTTP server. /healthz = process alive,
// /readyz = pushed within the last 30s.
func (h *Health) Serve(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		last := h.lastPush.Load()
		if last > 0 && time.Now().Unix()-last < 30 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		body := "not ready"
		if fn := h.reason.Load(); fn != nil {
			if r := (*fn)(); r != "" {
				body += ": " + r
			}
		}
		_, _ = w.Write([]byte(body))
	})
	// With host networking, another program on the machine may hold the port.
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("[agent] health endpoint not serving on %s: %v", addr, err)
	}
}
