package lasertag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPacksLiveParsesAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "packs_live" {
			t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
		}
		// Note: scrape numbers arrive as strings, and pack 3 has a null charge.
		_, _ = w.Write([]byte(`{
			"ok": true,
			"ole": true,
			"packs": {
				"3": {"team_id":1,"online":true,"playing":true,"state":6,
				      "scraped":{"resets":"2","timeouts":"5","charge":null,"status":"Playing"}},
				"1": {"team_id":0,"online":true,"playing":false,"state":1,
				      "scraped":{"resets":0,"timeouts":0,"charge":88,"status":"Idle"}}
			}
		}`))
	}))
	defer srv.Close()

	packs, err := New(srv.URL).PacksLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 2 {
		t.Fatalf("got %d packs, want 2", len(packs))
	}
	// Sorted by id.
	if packs[0].PackID != 1 || packs[1].PackID != 3 {
		t.Fatalf("packs not sorted by id: %d, %d", packs[0].PackID, packs[1].PackID)
	}
	// String scrape values parsed.
	if packs[1].Scraped.Resets == nil || *packs[1].Scraped.Resets != 2 {
		t.Errorf("pack 3 resets = %v, want 2", packs[1].Scraped.Resets)
	}
	if packs[1].Scraped.Charge != nil {
		t.Errorf("pack 3 charge should be nil (null in JSON)")
	}
	if packs[0].Scraped.Charge == nil || *packs[0].Scraped.Charge != 88 {
		t.Errorf("pack 1 charge = %v, want 88", packs[0].Scraped.Charge)
	}
	if !packs[0].Online || packs[0].Playing {
		t.Errorf("pack 1 online/playing flags wrong")
	}
}

func TestPacksLiveErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"COM bridge unreachable"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).PacksLive(context.Background()); err == nil {
		t.Fatal("expected an error when ok=false")
	}
}

func TestStatusAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/health.php":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Query().Get("action") == "status":
			_, _ = w.Write([]byte(`{"ok":true,"server_mode":6,"game_id":23331,"game_name":"TDM","server_elapsed":42,"duration":720,"players":8,"packs_online":10}`))
		default:
			http.Error(w, "nope", 404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.ServerMode != 6 || s.GameID != 23331 || s.Elapsed != 42 {
		t.Fatalf("status parsed wrong: %+v", s)
	}
}

func TestHealthNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer srv.Close()
	if err := New(srv.URL).Health(context.Background()); err == nil {
		t.Fatal("expected health error when ok=false")
	}
}
