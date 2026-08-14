package packir

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"overwatch/agent/internal/push"
)

// fakeForwarder is a controllable Forwarder: flip fail to simulate central down.
type fakeForwarder struct {
	mu    sync.Mutex
	fail  bool
	calls []struct {
		payload []byte
		key     string
	}
}

func (f *fakeForwarder) PushPackIR(payload []byte, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("central down")
	}
	f.calls = append(f.calls, struct {
		payload []byte
		key     string
	}{append([]byte(nil), payload...), key})
	return nil
}

func (f *fakeForwarder) setFail(b bool) { f.mu.Lock(); f.fail = b; f.mu.Unlock() }
func (f *fakeForwarder) count() int     { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func postReading(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/local/pack-ir", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec
}

func TestForwardsReading(t *testing.T) {
	f := &fakeForwarder{}
	s := New("127.0.0.1:0", "1.1.0", f, 100, "")

	rec := postReading(s, `{"pack_ozone_id":7,"ir_peak_raw":812,"ir_peak_mv":806,"temp_c":41.2,"measured_at":"2026-07-14T09:15:03Z"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	s.drain()

	if f.count() != 1 {
		t.Fatalf("forwarded %d, want 1", f.count())
	}
	var env struct {
		PushSeq  int64 `json:"push_seq"`
		Readings []struct {
			PackOzoneID int     `json:"pack_ozone_id"`
			IRPeakRaw   int     `json:"ir_peak_raw"`
			TempC       float64 `json:"temp_c"`
		} `json:"readings"`
		Agent struct {
			Version string `json:"version"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(f.calls[0].payload, &env); err != nil {
		t.Fatalf("unmarshal forwarded payload: %v", err)
	}
	if len(env.Readings) != 1 || env.Readings[0].PackOzoneID != 7 || env.Readings[0].IRPeakRaw != 812 || env.Readings[0].TempC != 41.2 {
		t.Fatalf("bad forwarded readings: %+v", env.Readings)
	}
	if env.Agent.Version != "1.1.0" {
		t.Errorf("agent version = %q, want 1.1.0", env.Agent.Version)
	}
	if f.calls[0].key != strconv.FormatInt(env.PushSeq, 10) {
		t.Errorf("idempotency key %q != push_seq %d", f.calls[0].key, env.PushSeq)
	}
	if s.forwarded.Load() != 1 || s.buf.Len() != 0 {
		t.Errorf("forwarded=%d buffered=%d, want 1/0", s.forwarded.Load(), s.buf.Len())
	}
}

func TestBuffersWhenCentralDownThenReplays(t *testing.T) {
	f := &fakeForwarder{fail: true}
	s := New("127.0.0.1:0", "1.1.0", f, 100, "")

	postReading(s, `{"pack_ozone_id":1,"ir_peak_raw":500}`)
	s.drain() // central down: nothing sent, reading stays buffered
	if s.buf.Len() != 1 || s.forwarded.Load() != 0 || f.count() != 0 {
		t.Fatalf("during outage: buffered=%d forwarded=%d calls=%d, want 1/0/0", s.buf.Len(), s.forwarded.Load(), f.count())
	}

	f.setFail(false)
	s.drain() // recovery: the buffered reading replays
	if s.buf.Len() != 0 || s.forwarded.Load() != 1 || f.count() != 1 {
		t.Fatalf("after recovery: buffered=%d forwarded=%d calls=%d, want 0/1/1", s.buf.Len(), s.forwarded.Load(), f.count())
	}
}

func TestRejectsMissingPackID(t *testing.T) {
	f := &fakeForwarder{}
	s := New("127.0.0.1:0", "1.1.0", f, 100, "")

	rec := postReading(s, `{"ir_peak_raw":500}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if s.buf.Len() != 0 || s.received.Load() != 0 {
		t.Errorf("a reading without pack_ozone_id must not be buffered: buffered=%d received=%d", s.buf.Len(), s.received.Load())
	}
}

func TestRejectsInvalidJSON(t *testing.T) {
	s := New("127.0.0.1:0", "1.1.0", &fakeForwarder{}, 100, "")
	rec := postReading(s, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if s.buf.Len() != 0 {
		t.Errorf("invalid JSON must not buffer")
	}
}

// End-to-end through the real Pusher: the reading must land on /api/agent/pack-ir
// with the site token and an idempotency key, wrapped in a readings batch.
func TestForwardsToPackIrEndpoint(t *testing.T) {
	var gotPath, gotToken, gotIdem string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Agent-Token")
		gotIdem = r.Header.Get("Idempotency-Key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New("127.0.0.1:0", "1.1.0", push.New(srv.URL+"/api/agent/ingest", "tok123"), 100, "")
	postReading(s, `{"pack_ozone_id":9}`)
	s.drain()

	if gotPath != "/api/agent/pack-ir" {
		t.Errorf("path = %q, want /api/agent/pack-ir", gotPath)
	}
	if gotToken != "tok123" {
		t.Errorf("token = %q, want tok123", gotToken)
	}
	if gotIdem == "" {
		t.Error("missing Idempotency-Key header")
	}
	if readings, _ := gotBody["readings"].([]any); len(readings) != 1 {
		t.Fatalf("readings len = %d, want 1", len(readings))
	}
}
