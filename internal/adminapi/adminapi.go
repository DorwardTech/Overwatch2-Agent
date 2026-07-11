// Package adminapi is the agent's token-protected control/observability HTTP API,
// plus a tiny browser control panel so venue staff can drive it without curl.
// Unlike the print-server proxy (which must stay unauthenticated for TORN), every
// /api route requires a bearer token, since it can mutate the cache.
//
// Routes:
//
//	GET  /                  static control-panel page (no secret; JS sends the token)
//	GET  /api/overview      cache + game-state summary
//	GET  /api/games         cached game metadata
//	GET  /api/games/{n}     verbatim "all" payload for one game
//	POST /api/resync        trigger an idle-gated cache refresh
//	POST /api/purge         drop all cached games
//	POST /api/collect       pull games from central (Overwatch) into the cache
package adminapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"overwatch/agent/internal/store"
)

// uiHTML is the self-contained control panel served at "/". It holds no secret:
// the operator types the admin token, it's verified against /api/overview, and
// it's sent as the bearer header on every action (the only browser-safe way to
// gate a served page — a plain navigation can't set an Authorization header).
//
//go:embed ui.html
var uiHTML []byte

// Backend is the subset of the agent the admin API needs.
type Backend interface {
	Games() []store.GameMeta
	GameRaw(gameNumber int) ([]byte, bool)
	Overview() map[string]any
	Resync()
	Purge() (int, error)
	// Collect pulls games from central (Overwatch) into the cache for the window
	// [from, to] (zero bounds are open-ended), skipping already-cached games.
	Collect(from, to time.Time) (map[string]any, error)
}

// Links are the public documentation URLs the control panel links to (hosted on
// Overwatch). Empty fields hide their button.
type Links struct {
	Changelog string
	APIDocs   string
}

// Server hosts the admin API.
type Server struct {
	backend Backend
	token   string
	addr    string
	links   Links
	srv     *http.Server
}

// New creates an admin API for backend, bound to addr, requiring token. links
// are surfaced as buttons in the control panel.
func New(backend Backend, addr, token string, links Links) *Server {
	s := &Server{backend: backend, token: token, addr: addr, links: links}
	s.srv = &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Handler exposes the routed, auth-wrapped handler (used in tests).
func (s *Server) Handler() http.Handler { return s.routes() }

// Start serves in the background.
func (s *Server) Start() error {
	log.Printf("[agent] admin API listening on %s", s.addr)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[agent] admin API stopped: %v", err)
		}
	}()
	return nil
}

// Close shuts the server down.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// The JSON API stays bearer-gated. The UI shell at "/" is served without auth
	// (it contains no secret) so a browser can load it; its buttons then call the
	// gated /api/* routes with the token the operator typed in.
	mux.Handle("/api/", s.auth(s.apiRoutes()))
	mux.HandleFunc("GET /{$}", s.handleUI)
	return mux
}

func (s *Server) apiRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/games", s.handleGames)
	mux.HandleFunc("GET /api/games/{n}", s.handleGame)
	mux.HandleFunc("POST /api/resync", s.handleResync)
	mux.HandleFunc("POST /api/purge", s.handlePurge)
	mux.HandleFunc("POST /api/collect", s.handleCollect)
	return mux
}

// handleUI serves the static control panel. No auth: it embeds no secret and
// only calls the (authenticated) /api routes with a token the user provides.
func (s *Server) handleUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Everything is inline and same-origin; forbid any external fetch/script.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.page())
}

// page injects the public documentation URLs into the control-panel shell.
func (s *Server) page() []byte {
	docs := map[string]string{}
	if s.links.Changelog != "" {
		docs["changelog"] = s.links.Changelog
	}
	if s.links.APIDocs != "" {
		docs["api"] = s.links.APIDocs
	}
	b, err := json.Marshal(docs)
	if err != nil {
		b = []byte("null")
	}
	return bytes.Replace(uiHTML, []byte("__OW_DOCS__"), b, 1)
}

// auth enforces a bearer token on every route in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	expect := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expect) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Overview())
}

func (s *Server) handleGames(w http.ResponseWriter, _ *http.Request) {
	games := s.backend.Games()
	if games == nil {
		games = []store.GameMeta{}
	}
	writeJSON(w, http.StatusOK, games)
}

func (s *Server) handleGame(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid game number"})
		return
	}
	raw, ok := s.backend.GameRaw(n)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "game not found"})
		return
	}
	// Serve the verbatim O-Zone payload as-is.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) handleResync(w http.ResponseWriter, _ *http.Request) {
	s.backend.Resync()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued"})
}

func (s *Server) handlePurge(w http.ResponseWriter, _ *http.Request) {
	n, err := s.backend.Purge()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged": n})
}

// handleCollect pulls games from central into the cache. The window is given by
// optional `from`/`to` query params (RFC3339, "2006-01-02T15:04", O-Zone's
// "2006-01-02 15:04:05", or a bare "2006-01-02"); omit a bound to leave it open.
func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	from, err := parseTimeParam(r.URL.Query().Get("from"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid 'from': " + err.Error()})
		return
	}
	to, err := parseTimeParam(r.URL.Query().Get("to"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid 'to': " + err.Error()})
		return
	}
	res, err := s.backend.Collect(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// parseTimeParam parses an optional timeframe bound. Empty yields the zero time
// (an open bound). It accepts several common layouts so the browser's
// datetime-local value works as-is.
func parseTimeParam(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
