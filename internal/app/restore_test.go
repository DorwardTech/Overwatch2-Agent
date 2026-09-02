package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"overwatch/agent/internal/config"
)

// newRestoreApp wires an agent to a fake central that serves the failover
// store: the metadata list, and one verbatim payload per game number.
func newRestoreApp(t *testing.T, games []int, raws map[int]string) *App {
	t.Helper()
	const base = "/api/agent/ozone-games"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == base:
			metas := make([]map[string]any, 0, len(games))
			for _, n := range games {
				metas = append(metas, map[string]any{
					"game_number": n, "game_name": "g" + strconv.Itoa(n), "game_type": 1,
					"duration": 601, "start_time": "2026-09-02 10:00:00", "end_time": "2026-09-02 10:10:00",
					"player_count": 2, "valid": 1,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"games": metas})
		case strings.HasPrefix(r.URL.Path, base+"/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, base+"/"))
			raw, ok := raws[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return New(config.Config{
		CentralURL:   srv.URL + "/api/agent/ingest",
		Token:        "test",
		CacheEnabled: true,
		CacheDir:     t.TempDir(),
	})
}

// Central's failover store is filled by agents, and an agent whose print-server
// connection desynchronised could file one game's payload under another game's
// number. Restoring that verbatim would carry the corruption back into a rebuilt
// venue cache — and permanently: HasRaw would report the game as present, so
// refreshCache would never fetch the real one and the proxy would serve the
// wrong game's scores to TORN under the right number.
func TestCollectRejectsAPayloadForAnotherGame(t *testing.T) {
	a := newRestoreApp(t, []int{9}, map[int]string{9: string(gamePayload(7))})

	result, err := a.Collect(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if result["collected"] != 0 || result["failed"] != 1 {
		t.Fatalf("result = %v, want collected=0 failed=1", result)
	}
	if a.store.HasRaw(9) {
		t.Error("a payload naming game #7 was cached as game #9")
	}
	// The metadata is still worth keeping: with HasRaw false, refreshCache can
	// go and fetch the real payload from the print server.
	if _, ok := a.store.Meta(9); !ok {
		t.Error("the list entry was discarded along with the payload")
	}
}

// Central answers a game it does not hold with an error reply, and an older
// central answers 404. Neither is game data, and neither may be cached.
func TestCollectRejectsAReplyThatIsNotGameData(t *testing.T) {
	a := newRestoreApp(t, []int{9}, map[int]string{9: `{"error":"Game not found"}`})

	result, err := a.Collect(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if result["collected"] != 0 || result["failed"] != 1 {
		t.Fatalf("result = %v, want collected=0 failed=1", result)
	}
	if a.store.HasRaw(9) {
		t.Error("an error reply was cached as game #9")
	}
}

// The check must not reject the ordinary case, and what it stores must be the
// bytes central sent, byte for byte — the cache is the proxy's source of truth.
func TestCollectStoresAMatchingPayloadVerbatim(t *testing.T) {
	want := string(gamePayload(9))
	a := newRestoreApp(t, []int{9}, map[int]string{9: want})

	result, err := a.Collect(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if result["collected"] != 1 || result["failed"] != 0 {
		t.Fatalf("result = %v, want collected=1 failed=0", result)
	}
	raw, ok, err := a.store.GameRaw(9)
	if err != nil || !ok {
		t.Fatalf("game #9 not cached (ok=%t, err=%v)", ok, err)
	}
	if string(raw) != want {
		t.Errorf("cached payload = %s, want %s", raw, want)
	}
}

// A payload with a game section but no number of its own is not a mismatch —
// only a reply that positively identifies as a DIFFERENT game is rejected.
func TestCollectAcceptsAPayloadThatNamesNoGameNumber(t *testing.T) {
	a := newRestoreApp(t, []int{9}, map[int]string{9: `{"game":{"gamename":"g9"},"players":{}}`})

	result, err := a.Collect(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if result["collected"] != 1 {
		t.Fatalf("result = %v, want collected=1", result)
	}
}

// The startup restore is the other way central's store reaches the cache, and
// it runs unattended on every boot of a rebuilt box.
func TestRestoreFromCentralRejectsAPayloadForAnotherGame(t *testing.T) {
	a := newRestoreApp(t, []int{9, 10}, map[int]string{
		9:  string(gamePayload(7)), // mis-keyed at central
		10: string(gamePayload(10)),
	})

	a.restoreFromCentral(context.Background())

	if a.store.HasRaw(9) {
		t.Error("a payload naming game #7 was restored as game #9")
	}
	if !a.store.HasRaw(10) {
		t.Error("the intact game was not restored")
	}
}
