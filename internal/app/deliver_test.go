package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"overwatch/agent/internal/config"
)

// newDeliverApp wires an agent to a central whose reply status is chosen per
// request body.
func newDeliverApp(t *testing.T, status func(body string) int) (*App, *atomic.Int64) {
	t.Helper()
	var received atomic.Int64
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Add(1)
		w.WriteHeader(status(string(body)))
	}))
	t.Cleanup(central.Close)

	return New(config.Config{
		CentralURL: central.URL + "/api/agent/ingest",
		Token:      "test",
		BufferMax:  100,
	}), &received
}

// The buffer is strictly FIFO, so a batch central will never accept blocks every
// newer batch behind it until capacity finally evicts it — hours of telemetry
// lost to one malformed payload. A rejection that names the payload as the
// problem drops that batch and the drain carries on.
func TestDeliverDropsABatchCentralWillNeverAccept(t *testing.T) {
	a, received := newDeliverApp(t, func(body string) int {
		if strings.Contains(body, "poison") {
			return http.StatusUnprocessableEntity
		}
		return http.StatusOK
	})

	a.deliver("k1", []byte(`{"push_seq":1,"note":"poison"}`))
	if n := a.buf.Len(); n != 0 {
		t.Fatalf("buffered = %d, want the rejected batch dropped", n)
	}

	a.deliver("k2", []byte(`{"push_seq":2}`))
	if n := a.buf.Len(); n != 0 {
		t.Fatalf("buffered = %d, want the next batch delivered", n)
	}
	if got := received.Load(); got != 2 {
		t.Fatalf("central saw %d pushes, want 2", got)
	}
}

// The converse: an outage, an overloaded central, or a rejection the operator
// can fix must keep the telemetry queued. Dropping on 401/403/404 would discard
// a venue's data over a rotated token or a mis-set URL — exactly while someone
// is fixing it.
func TestDeliverKeepsBufferingRecoverableFailures(t *testing.T) {
	for name, code := range map[string]int{
		"server error": http.StatusInternalServerError,
		"unauthorised": http.StatusUnauthorized,
		"forbidden":    http.StatusForbidden,
		"not found":    http.StatusNotFound,
		"rate limited": http.StatusTooManyRequests,
		"timeout":      http.StatusRequestTimeout,
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := newDeliverApp(t, func(string) int { return code })

			a.deliver("k1", []byte(`{"push_seq":1}`))

			if n := a.buf.Len(); n != 1 {
				t.Fatalf("buffered = %d, want the batch kept for retry after HTTP %d", n, code)
			}
		})
	}
}

// A queue that already holds a poison entry must not stay wedged: the drain
// drops it and delivers everything behind it in the same pass.
func TestDeliverUnblocksTheQueueBehindAPoisonBatch(t *testing.T) {
	a, _ := newDeliverApp(t, func(body string) int {
		if strings.Contains(body, "poison") {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})

	// Queue a poison batch and two good ones behind it, as an outage would.
	a.buf.Push("k1", []byte(`{"push_seq":1,"note":"poison"}`))
	a.buf.Push("k2", []byte(`{"push_seq":2}`))

	a.deliver("k3", []byte(`{"push_seq":3}`))

	if n := a.buf.Len(); n != 0 {
		t.Fatalf("buffered = %d, want the queue fully drained", n)
	}
}
