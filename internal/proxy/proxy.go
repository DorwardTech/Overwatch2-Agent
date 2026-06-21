// Package proxy is the transparent O-Zone Print Server proxy. Scoring software
// (TORN) and printers connect here instead of to O-Zone; the proxy answers from
// the local verbatim cache, so the real O-Zone print server is never touched
// during a live game. It must be byte-for-byte indistinguishable from O-Zone —
// see docs/OZONE_PRINT_SERVER_API.md for the contract.
//
// Security posture (per design): the TCP listener is unauthenticated because TORN
// speaks plain TCP and cannot be changed. Bind it to the trusted venue LAN only;
// never port-forward it to the public internet.
package proxy

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"sync/atomic"

	"overwatch/agent/internal/cache"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozoneproto"
)

// Server is the print-server proxy.
type Server struct {
	cache  *cache.Cache
	addr   string
	banner [][]byte

	ln     net.Listener
	mu     sync.Mutex
	conns  atomic.Int64 // currently-open connections
	served atomic.Int64 // total requests answered
	closed atomic.Bool
}

// New creates a proxy serving from c, binding to addr (e.g. "0.0.0.0:12123").
func New(c *cache.Cache, addr string) *Server {
	return &Server{
		cache: c,
		addr:  addr,
		// Connect banner: event types then scoresheet texts, exactly two frames,
		// matching what O-Zone pushes (and what TORN/the agent drain on connect).
		banner: [][]byte{
			ozonefix.Compact(ozonefix.EventTypesBannerJSON()),
			ozonefix.Compact(ozonefix.TextsBannerJSON()),
		},
	}
}

// Start binds the listener and begins accepting connections in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	log.Printf("[agent] print-server proxy listening on %s", ln.Addr())
	go s.acceptLoop(ln)
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Connections returns the number of currently-open client connections.
func (s *Server) Connections() int64 { return s.conns.Load() }

// Served returns the total number of requests answered.
func (s *Server) Served() int64 { return s.served.Load() }

// Close stops the listener.
func (s *Server) Close() error {
	s.closed.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	s.conns.Add(1)
	defer func() {
		s.conns.Add(-1)
		_ = conn.Close()
	}()

	// Push the 2-frame connect banner before answering any command.
	for _, frame := range s.banner {
		if _, err := conn.Write(ozoneproto.Frame(frame)); err != nil {
			return
		}
	}

	for {
		payload, err := ozoneproto.ReadFrame(conn)
		if err != nil {
			return // client closed or framing broke; TORN will reconnect
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			continue // ignore malformed request, keep the connection
		}
		reply, silent := s.respond(req)
		if silent {
			continue
		}
		s.served.Add(1)
		if _, err := conn.Write(ozoneproto.Frame(reply)); err != nil {
			return
		}
	}
}

// respond maps one request to its O-Zone reply (see the command table in
// docs/OZONE_PRINT_SERVER_API.md). silent is true for the data-ack message.
func (s *Server) respond(req map[string]any) (reply []byte, silent bool) {
	// Data-acknowledged ({"success":true}) gets no reply.
	if _, ok := req["success"]; ok {
		return nil, true
	}
	// Auto-print on/off is acknowledged.
	if _, ok := req["autoprint"]; ok {
		return []byte(`{"success":true}`), false
	}

	command, _ := req["command"].(string)
	switch command {
	case "list":
		return s.cache.BuildListResponse(), false
	case "all":
		return s.gameReply(req, func(n int) ([]byte, bool) { return s.cache.GameRaw(n) }), false
	case "minimal":
		return s.gameReply(req, func(n int) ([]byte, bool) { return s.cache.MinimalRaw(n) }), false
	case "team":
		return s.subsetReply(req, "teams"), false
	case "player":
		return s.subsetReply(req, "players"), false
	default:
		return []byte(`{"error":"Unknown command"}`), false
	}
}

func (s *Server) gameReply(req map[string]any, lookup func(int) ([]byte, bool)) []byte {
	num, ok := gameNumber(req)
	if !ok {
		return []byte(`{"error":"Missing gamenumber"}`)
	}
	raw, found := lookup(num)
	if !found {
		return []byte(`{"error":"Game not found"}`)
	}
	return raw
}

func (s *Server) subsetReply(req map[string]any, section string) []byte {
	num, ok := gameNumber(req)
	if !ok {
		return []byte(`{"error":"Missing gamenumber"}`)
	}
	raw, found := s.cache.Subset(num, section, stringIDs(req["ids"]))
	if !found {
		return []byte(`{"error":"Game not found"}`)
	}
	return raw
}

func gameNumber(req map[string]any) (int, bool) {
	v, ok := req["gamenumber"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func stringIDs(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			ids = append(ids, s)
		}
	}
	return ids
}
