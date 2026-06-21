package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"overwatch/agent/internal/store"
)

type fakeBackend struct {
	games    []store.GameMeta
	raw      map[int][]byte
	resynced int
	purged   int
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

// The interface is satisfied by *app.App at compile time elsewhere; here we keep
// the fake honest.
var _ Backend = (*fakeBackend)(nil)
