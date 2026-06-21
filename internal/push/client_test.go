package push

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveURL(t *testing.T) {
	cases := []struct {
		in, endpoint, want string
	}{
		{"https://ow2.example/api/agent/ingest", "game-results", "https://ow2.example/api/agent/game-results"},
		{"http://localhost:8080/api/agent/ingest", "game-results", "http://localhost:8080/api/agent/game-results"},
		{"https://ow2.example/api/agent/ingest", "commands", "https://ow2.example/api/agent/commands"},
	}
	for _, c := range cases {
		if got := deriveURL(c.in, c.endpoint); got != c.want {
			t.Errorf("deriveURL(%q, %q) = %q, want %q", c.in, c.endpoint, got, c.want)
		}
	}
}

func TestFetchCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/commands" {
			t.Errorf("path = %q, want /api/agent/commands", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"commands":[{"id":3,"type":"refetch_game","payload":{"game_number":42}}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")
	cmds, err := p.FetchCommands()
	if err != nil {
		t.Fatalf("FetchCommands: %v", err)
	}
	if len(cmds) != 1 || cmds[0].ID != 3 || cmds[0].Type != "refetch_game" {
		t.Fatalf("unexpected commands: %+v", cmds)
	}
	if n, _ := cmds[0].Payload["game_number"].(float64); int(n) != 42 {
		t.Errorf("game_number = %v, want 42", cmds[0].Payload["game_number"])
	}
}

func TestFetchCommandsToleratesArrayPayload(t *testing.T) {
	// central may serialise an empty payload as [] (JSON array); the agent must
	// not choke on it and must still parse the rest of the command.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"commands":[{"id":5,"type":"reboot_agent","payload":[]}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")
	cmds, err := p.FetchCommands()
	if err != nil {
		t.Fatalf("FetchCommands should tolerate array payload: %v", err)
	}
	if len(cmds) != 1 || cmds[0].Type != "reboot_agent" {
		t.Fatalf("unexpected commands: %+v", cmds)
	}
	if cmds[0].Payload == nil || len(cmds[0].Payload) != 0 {
		t.Fatalf("array payload should decode to an empty map, got %+v", cmds[0].Payload)
	}
}

func TestFetchCommandsNotFoundIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")
	cmds, err := p.FetchCommands()
	if err != nil {
		t.Fatalf("FetchCommands on 404 should not error: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("expected no commands, got %+v", cmds)
	}
}

func TestAckCommand(t *testing.T) {
	var gotPath, gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		gotStatus, _ = body["status"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")
	if err := p.AckCommand(9, "acked", map[string]any{"game_number": 42}); err != nil {
		t.Fatalf("AckCommand: %v", err)
	}
	if gotPath != "/api/agent/commands/9/ack" {
		t.Errorf("path = %q, want /api/agent/commands/9/ack", gotPath)
	}
	if gotStatus != "acked" {
		t.Errorf("status = %q, want acked", gotStatus)
	}
}

// PushOzoneGame must send raw as a JSON string and central must receive the exact
// bytes; FetchOzoneGameRaw must return the body verbatim — the failover round-trip.
func TestOzoneFailoverRoundTrip(t *testing.T) {
	raw := []byte(`{"game":{"gamenum":9},"players":{"1":{"alias":"Ava","tagsby":2}}}`)

	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent/ozone-games":
			b, _ := io.ReadAll(r.Body)
			var body struct {
				GameNumber int    `json:"game_number"`
				RawJSON    string `json:"raw_json"`
			}
			_ = json.Unmarshal(b, &body)
			stored = []byte(body.RawJSON)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent/ozone-games":
			_, _ = w.Write([]byte(`{"games":[{"game_number":9,"game_name":"TDM","valid":1}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent/ozone-games/9":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(stored)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")

	if err := p.PushOzoneGame(OzoneGameMeta{GameNumber: 9, GameName: "TDM", Valid: 1}, raw); err != nil {
		t.Fatalf("PushOzoneGame: %v", err)
	}
	if !bytes.Equal(stored, raw) {
		t.Fatalf("central stored non-verbatim bytes:\n got %s\nwant %s", stored, raw)
	}

	metas, err := p.FetchOzoneGameMeta()
	if err != nil || len(metas) != 1 || metas[0].GameNumber != 9 {
		t.Fatalf("FetchOzoneGameMeta = %+v, err=%v", metas, err)
	}

	got, ok, err := p.FetchOzoneGameRaw(9)
	if err != nil || !ok {
		t.Fatalf("FetchOzoneGameRaw: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("restored bytes not verbatim:\n got %s\nwant %s", got, raw)
	}
}

func TestFetchOzoneGameRawNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok")
	_, ok, err := p.FetchOzoneGameRaw(123)
	if err != nil {
		t.Fatalf("404 should not error: %v", err)
	}
	if ok {
		t.Fatal("missing game should report not found")
	}
}

func TestPushGameResults(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Agent-Token")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/agent/ingest", "tok123")
	if err := p.PushGameResults(7, map[string]any{"players": map[string]any{}}); err != nil {
		t.Fatalf("PushGameResults: %v", err)
	}

	if gotPath != "/api/agent/game-results" {
		t.Errorf("path = %q, want /api/agent/game-results", gotPath)
	}
	if gotToken != "tok123" {
		t.Errorf("token = %q", gotToken)
	}
	if n, _ := gotBody["game_number"].(float64); int(n) != 7 {
		t.Errorf("game_number = %v, want 7", gotBody["game_number"])
	}
}
