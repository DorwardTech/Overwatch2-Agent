// Package health exposes liveness/readiness endpoints for container healthchecks.
package health

import (
	"net/http"
	"sync/atomic"
	"time"
)

type Health struct {
	lastPoll atomic.Int64
	lastPush atomic.Int64
}

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
		_, _ = w.Write([]byte("not ready"))
	})
	_ = http.ListenAndServe(addr, mux)
}
