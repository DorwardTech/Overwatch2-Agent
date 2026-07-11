package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"overwatch/agent/internal/store"
)

type fakeBackend struct {
	games     []store.GameMeta
	raw       map[int][]byte
	resynced  int
	purged    int
	collected int
	lastFrom  time.Time
	lastTo    time.Time
}

func (f *fakeBackend) Games() []store.GameMeta { return f.games }
func (f *fakeBackend) GameRaw(n int) ([]byte, bool) {
	b, ok := f.raw[n]
	return b, ok
}
func (f *fakeBackend) Overview() map[string]any {
	return map[string]any{"games": len(f.games), "state": "idle"}
}
func (f *fakeBackend) Resync()             { f.resynced++ }
func (f *fakeBackend) Purge() (int, error) { f.purged++; return len(f.games), nil }
func (f *fakeBackend) Collect(from, to time.Time) (map[string]any, error) {
	f.collected++
	f.lastFrom, f.lastTo = from, to
	return map[string]any{"collected": 1, "already_cached": 0}, nil
}

func newServer() (*Server, *fakeBackend) {
	b := &fakeBackend{
		games: []store.GameMeta{{GameNumber: 9, GameName: "TDM", Valid: 1}},
		raw:   map[int][]byte{9: []byte(`{"game":{"gamenum":9}}`)},
	}
	return New(b, ":0", "secret"), b
}

func do(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	s, _ := newServer()
	h := s.Handler()

	if rec := do(t, h, "GET", "/api/overview", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/overview", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/overview", "secret"); rec.Code != http.StatusOK {
		t.Errorf("good token: status = %d, want 200", rec.Code)
	}
}

func TestGamesAndGame(t *testing.T) {
	s, _ := newServer()
	h := s.Handler()

	rec := do(t, h, "GET", "/api/games", "secret")
	var games []store.GameMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &games); err != nil {
		t.Fatal(err)
	}
	if len(games) != 1 || games[0].GameNumber != 9 {
		t.Fatalf("games = %+v", games)
	}

	rec = do(t, h, "GET", "/api/games/9", "secret")
	if !bytes.Equal(rec.Body.Bytes(), []byte(`{"game":{"gamenum":9}}`)) {
		t.Errorf("game 9 not served verbatim: %s", rec.Body.Bytes())
	}

	if rec := do(t, h, "GET", "/api/games/404", "secret"); rec.Code != http.StatusNotFound {
		t.Errorf("missing game: status = %d, want 404", rec.Code)
	}
}

func TestResyncAndPurge(t *testing.T) {
	s, b := newServer()
	h := s.Handler()

	if rec := do(t, h, "POST", "/api/resync", "secret"); rec.Code != http.StatusAccepted {
		t.Errorf("resync status = %d, want 202", rec.Code)
	}
	if b.resynced != 1 {
		t.Errorf("resync not invoked")
	}

	rec := do(t, h, "POST", "/api/purge", "secret")
	if rec.Code != http.StatusOK {
		t.Errorf("purge status = %d", rec.Code)
	}
	if b.purged != 1 {
		t.Errorf("purge not invoked")
	}
}

func TestControlPanelServedWithoutAuth(t *testing.T) {
	s, _ := newServer()
	h := s.Handler()

	// The page must load with NO token (a browser navigation can't set a bearer
	// header) — the gating happens client-side once the operator types the token.
	rec := do(t, h, "GET", "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "<!doctype html>") {
		t.Error("GET / did not return the control-panel HTML")
	}
	// Critical: the served shell must never embed the admin token.
	if strings.Contains(body, "secret") {
		t.Error("control-panel HTML leaks the admin token")
	}
}

func TestAPIStillGatedAfterUIAdded(t *testing.T) {
	s, _ := newServer()
	h := s.Handler()

	if rec := do(t, h, "GET", "/api/overview", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("api without token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/overview", "secret"); rec.Code != http.StatusOK {
		t.Errorf("api with token = %d, want 200", rec.Code)
	}
}

func TestCollect(t *testing.T) {
	s, b := newServer()
	h := s.Handler()

	// A dated window is parsed and passed to the backend.
	rec := do(t, h, "POST", "/api/collect?from=2026-01-01&to=2026-02-01", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("collect status = %d, want 200", rec.Code)
	}
	if b.collected != 1 {
		t.Error("collect not invoked")
	}
	if b.lastFrom.IsZero() || b.lastTo.IsZero() {
		t.Errorf("from/to not parsed: %v..%v", b.lastFrom, b.lastTo)
	}

	// Open-ended (no params) is allowed.
	if rec := do(t, h, "POST", "/api/collect", "secret"); rec.Code != http.StatusOK {
		t.Errorf("open collect = %d, want 200", rec.Code)
	}

	// A malformed time is a 400, not a silent open bound.
	if rec := do(t, h, "POST", "/api/collect?from=whenever", "secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad from = %d, want 400", rec.Code)
	}

	// Still bearer-gated.
	if rec := do(t, h, "POST", "/api/collect", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("collect without token = %d, want 401", rec.Code)
	}
}

// The interface is satisfied by *app.App at compile time elsewhere; here we keep
// the fake honest.
var _ Backend = (*fakeBackend)(nil)
