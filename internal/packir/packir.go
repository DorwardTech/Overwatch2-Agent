// Package packir receives IR emitter-strength readings from an on-prem bench
// node over the venue LAN and forwards them to Overwatch central, buffering
// across outages exactly like the telemetry path. It is deliberately
// independent of the O-Zone WebSocket and print-server code paths and runs in
// both ozone and legacy agent modes.
//
// The listener is unauthenticated and LAN-scoped (like the print-server proxy):
// bind it to a private interface and never port-forward it. Pack identity is a
// node-side concern — pack_ozone_id is carried explicitly in the payload.
package packir

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"overwatch/agent/internal/buffer"
	"overwatch/agent/internal/push"
)

// Forwarder delivers a batched pack-IR payload to central. *push.Pusher
// satisfies it via PushPackIR.
type Forwarder interface {
	PushPackIR(payload []byte, idempotencyKey string) error
}

// Reading is one ESP8266 IR measurement received over the LAN. Optionals are
// pointers so an omitted field forwards as absent (central stores null) rather
// than a misleading zero; pack_ozone_id is required.
type Reading struct {
	PackOzoneID *int     `json:"pack_ozone_id"`
	IRPeakRaw   *int     `json:"ir_peak_raw,omitempty"`
	IRPeakMV    *int     `json:"ir_peak_mv,omitempty"`
	TempC       *float64 `json:"temp_c,omitempty"`
	DistanceMM  *int     `json:"distance_mm,omitempty"`
	SampleCount *int     `json:"sample_count,omitempty"`
	SensorID    string   `json:"sensor_id,omitempty"`
	MeasuredAt  string   `json:"measured_at,omitempty"`
	Source      string   `json:"source,omitempty"`
}

const (
	maxBodyBytes  = 1 << 16 // 64 KiB — a single reading is tiny.
	retryInterval = 30 * time.Second
)

// Server is the LAN listener plus the buffered forwarder to central.
type Server struct {
	addr         string
	agentVersion string
	fwd          Forwarder
	buf          *buffer.Buffer
	bufferFile   string
	srv          *http.Server
	trigger      chan struct{}

	seq       atomic.Int64
	received  atomic.Int64
	forwarded atomic.Int64
}

// New builds a listener bound to addr that forwards via fwd. bufferMax caps the
// in-memory retry queue; bufferFile ("" disables) spills it across restarts so
// an outage that overlaps a restart doesn't drop readings.
func New(addr, agentVersion string, fwd Forwarder, bufferMax int, bufferFile string) *Server {
	s := &Server{
		addr:         addr,
		agentVersion: agentVersion,
		fwd:          fwd,
		buf:          buffer.New(bufferMax),
		bufferFile:   bufferFile,
		trigger:      make(chan struct{}, 1),
	}
	// Seed push_seq from the clock so idempotency keys don't repeat across
	// restarts (readings are seconds apart, so seconds outrun the counter).
	s.seq.Store(time.Now().Unix())

	if bufferFile != "" {
		if n, err := s.buf.Load(bufferFile); err != nil {
			log.Printf("[packir] buffer restore failed: %v", err)
		} else if n > 0 {
			log.Printf("[packir] restored %d unsent reading(s) from %s", n, bufferFile)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /local/pack-ir", s.handle)
	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Start serves the listener and runs the retry-drain loop until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	log.Printf("[packir] pack-IR listener on %s (LAN-only, unauthenticated)", s.addr)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[packir] listener stopped: %v", err)
		}
	}()
	go s.drainLoop(ctx)
	return nil
}

// Close spills any unsent readings and shuts the listener down.
func (s *Server) Close() error {
	if s.bufferFile != "" {
		if err := s.buf.Save(s.bufferFile); err != nil {
			log.Printf("[packir] buffer save failed: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// Status is a snapshot for the admin overview.
func (s *Server) Status() map[string]any {
	return map[string]any{
		"received":  s.received.Load(),
		"forwarded": s.forwarded.Load(),
		"buffered":  s.buf.Len(),
	}
}

// handle accepts one ESP reading, wraps it in a central batch, and queues it.
// Malformed input is rejected 400 at the edge so a bad reading can never poison
// the forward queue (central would 422 it and the drain would retry forever).
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read failed"})
		return
	}
	var reading Reading
	if err := json.Unmarshal(body, &reading); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON"})
		return
	}
	if reading.PackOzoneID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "pack_ozone_id required"})
		return
	}

	seq := s.seq.Add(1)
	payload, err := json.Marshal(map[string]any{
		"push_seq": seq,
		"readings": []Reading{reading},
		"agent":    map[string]any{"version": s.agentVersion},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "encode failed"})
		return
	}

	s.buf.Push(strconv.FormatInt(seq, 10), payload)
	s.received.Add(1)
	s.signal()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "buffered": s.buf.Len()})
}

// drainLoop forwards buffered readings on a trigger (a fresh reading arrived)
// and on a periodic tick (so readings buffered during an outage are retried
// even when no new reading arrives to trigger them).
func (s *Server) drainLoop(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			s.drain()
		case <-ticker.C:
			s.drain()
		}
	}
}

// drain sends buffered readings oldest-first, stopping on the first failure so
// they stay queued for the next attempt (the Pusher itself has no retry).
func (s *Server) drain() {
	for {
		e, ok := s.buf.Peek()
		if !ok {
			return
		}
		if err := s.fwd.PushPackIR(e.Data, e.Key); err != nil {
			if push.Unsendable(err) {
				// Central will reject this batch identically every time, and
				// the queue is FIFO — leaving it at the head would block every
				// later reading behind it. Drop it and keep draining.
				log.Printf("[packir] forward rejected permanently — dropping batch %s: %v", e.Key, err)
				s.buf.PopFront()
				continue
			}
			log.Printf("[packir] forward failed, %d reading(s) buffered: %v", s.buf.Len(), err)
			return
		}
		s.buf.PopFront()
		s.forwarded.Add(1)
	}
}

func (s *Server) signal() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
