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
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"overwatch/agent/internal/cache"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozoneproto"
)

// bannerGap separates the two connect-banner frames. TORN's O-Zone connector
// (Torn5/OZone.cs ReadFromOzone) does NOT delimit pushed frames by length — it
// reads until a chunk ends with '}' and then stops only if no more data is
// immediately available (a ~1ms DataAvailable poll). If the two banner frames
// arrive coalesced, TORN's first read swallows BOTH and its second read blocks
// forever, hanging the client on "connecting". Real O-Zone emits them with a
// natural gap; this reproduces it. 50ms is imperceptible (once per connection)
// and safely exceeds TORN's poll.
const bannerGap = 50 * time.Millisecond

// The proxy listens on the venue LAN with no authentication — TORN speaks plain
// TCP and cannot be changed — so anything on that network can open connections
// to it. The agent has 128 MB. These three bounds keep an unauthenticated peer,
// malicious or merely broken, from exhausting it:
const (
	// maxConns caps concurrent clients. A venue runs TORN and perhaps a printer;
	// 64 is far past legitimate use and still bounds the memory the listener can
	// be made to hold. Being refused is recoverable — TORN reconnects — whereas
	// an OOM kill takes the agent down and skips the graceful buffer spill,
	// losing unsent telemetry with it.
	maxConns = 64

	// maxRequestFrame caps an INBOUND frame. Requests are a few dozen bytes
	// ({"gamenumber":9,"command":"all"}); the 10 MiB protocol maximum exists for
	// the responses we send. Without a tighter cap here a client could declare
	// 10 MiB in the header and then stall, and the proxy would allocate the
	// whole buffer up front and hold it: about a dozen such connections is the
	// container.
	maxRequestFrame = 64 << 10

	// bodyTimeout bounds the wait for a request body once its header has
	// arrived. It deliberately does NOT apply to the header: a client that has
	// sent nothing is just idle between games, which is normal and must not be
	// disconnected. A request body is a few dozen bytes on a LAN, so five
	// seconds is already orders of magnitude more than one needs.
	bodyTimeout = 5 * time.Second
)

// writeTimeout bounds a single write. A peer that has stopped reading — TORN
// crashed mid-game, or a half-open connection the OS has not yet reaped — fills
// the socket buffer, and an unbounded Write then blocks this goroutine (holding
// the connection slot) for as long as the peer stays up. Reads deliberately
// have NO deadline: TORN holds its connection open and idle between games, so
// cutting a quiet reader would be a fault, not a fix.
const writeTimeout = 30 * time.Second

// writeFrame sends one framed message under the write deadline.
func writeFrame(conn net.Conn, body []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := conn.Write(ozoneproto.Frame(body))
	return err
}

// Server is the print-server proxy.
type Server struct {
	cache  *cache.Cache
	addr   string
	banner [][]byte

	ln      net.Listener
	mu      sync.Mutex
	conns   atomic.Int64 // currently-open connections
	served  atomic.Int64 // total requests answered
	refused atomic.Int64 // connections turned away at maxConns
	closed  atomic.Bool
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

// Refused returns how many connections were turned away at the concurrency cap.
// A non-zero value means something on the venue LAN is opening far more
// connections than a scoring client does.
func (s *Server) Refused() int64 { return s.refused.Load() }

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
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept error (EMFILE, resets): the proxy is TORN's
			// game server — log and keep accepting rather than dying silently.
			log.Printf("[agent] proxy: accept failed: %v (retrying)", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Refuse rather than accept-and-hold once the cap is reached: an
		// accepted connection costs a goroutine and a read buffer, and this
		// listener is reachable by anything on the venue LAN.
		if s.conns.Load() >= maxConns {
			s.refused.Add(1)
			_ = conn.Close()
			continue
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

	// TORN keeps its connection open between games, so an idle read deadline
	// would cut a healthy client; TCP keepalive reaps only dead peers, freeing
	// goroutines pinned by half-open connections.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(time.Minute)
	}

	// Push the 2-frame connect banner before answering any command. The frames
	// must be spaced apart (bannerGap): TORN delimits them by a no-more-data gap,
	// not by length, so coalesced frames hang its connect (see bannerGap).
	for i, frame := range s.banner {
		if i > 0 {
			time.Sleep(bannerGap)
		}
		if err := writeFrame(conn, frame); err != nil {
			return
		}
	}

	for {
		payload, err := readRequest(conn)
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
		if err := writeFrame(conn, reply); err != nil {
			return
		}
	}
}

// readRequest reads one client request, bounded in size and — once its header
// has arrived — in time. No deadline covers the wait for the header itself:
// TORN holds its connection open and idle between games.
func readRequest(conn net.Conn) ([]byte, error) {
	length, err := ozoneproto.ReadHeader(conn)
	if err != nil {
		return nil, err
	}
	if length > maxRequestFrame {
		return nil, fmt.Errorf("proxy: request frame of %d bytes is too large", length)
	}

	_ = conn.SetReadDeadline(time.Now().Add(bodyTimeout))
	body, err := ozoneproto.ReadBody(conn, length)
	_ = conn.SetReadDeadline(time.Time{}) // idle again: no deadline
	if err != nil {
		return nil, err
	}

	return body, nil
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
