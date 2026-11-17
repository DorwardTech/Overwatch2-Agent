// Package ozonesim is a fake O-Zone for tests and local demos. It speaks the
// real Print Server TCP framing (port 12123) and the External Message Bus line
// protocol (port 12111), serving golden fixtures so the agent's cache, proxy,
// and idle-gating can be exercised without laser-tag hardware.
//
// It is the test/demo counterpart to docs/OZONE_PRINT_SERVER_API.md. The fake
// records connection and per-command counts so tests can assert, for example,
// that the agent never contacts the print server during an active game.
package ozonesim

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozoneproto"
)

// PrintServer is a fake O-Zone Print Server (TCP, 0x28-framed JSON).
type PrintServer struct {
	mu        sync.Mutex
	ln        net.Listener
	listJSON  []byte               // served for {"command":"list"}
	games     map[int][]byte       // gamenumber -> full "all" payload (verbatim)
	banner    [][]byte             // frames pushed on connect (default: event_types, texts)
	conns     int                  // total connections accepted
	open      map[net.Conn]bool    // connections still open
	wmu       sync.Mutex           // one write at a time on any connection
	requests  map[string]int       // command -> count
	onRequest func(command string) // test hook, invoked per command (may be nil)
	ackReply  []byte               // sent back to {"success":true} when set (see AnswerAcks)
	ackDelay  time.Duration        // how long after the acknowledgement
	closed    bool
}

// OnRequest registers a hook invoked (synchronously) for each command received.
// Tests use it to change agent state mid-batch, e.g. starting a game between
// two fetches to assert the agent aborts.
func (p *PrintServer) OnRequest(fn func(command string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onRequest = fn
}

// AnswerAcks makes the server answer each {"success":true} with body, after
// delay, as a game server that mistakes one for a request might. By default
// an acknowledgement gets no reply.
func (p *PrintServer) AnswerAcks(body []byte, delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ackReply, p.ackDelay = append([]byte(nil), body...), delay
}

// NewPrintServer seeds a coherent default: game #9 from the golden, a one-entry
// game list for it, and the standard 2-frame connect banner (event types, texts).
func NewPrintServer() *PrintServer {
	defaultList := []byte(`{"gamelist":[{"gamenum":9,"gamename":"Competition Team Elimination","duration":601,"starttime":"2020-02-18 16:06:05","endtime":"2020-02-18 16:16:06","playercount":2,"valid":1}]}`)
	return &PrintServer{
		listJSON: defaultList,
		games:    map[int][]byte{9: ozonefix.Compact(ozonefix.AllResponseJSON())},
		banner: [][]byte{
			ozonefix.Compact(ozonefix.EventTypesBannerJSON()),
			ozonefix.Compact(ozonefix.TextsBannerJSON()),
		},
		requests: map[string]int{},
		open:     map[net.Conn]bool{},
	}
}

// OpenConnections returns how many connections are open now.
func (p *PrintServer) OpenConnections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.open)
}

// PushGame sends a game's "all" payload, unasked, to every open connection,
// as real O-Zone does when a game ends. It reports how many it was sent to.
func (p *PrintServer) PushGame(gameNumber int) int {
	p.mu.Lock()
	raw, ok := p.games[gameNumber]
	p.mu.Unlock()
	if !ok {
		return 0
	}
	return p.SendToAll(raw)
}

// SendToAll sends one frame, unasked, to every open connection (a print
// command, the auto-print setting, a game's data). It reports how many it
// was sent to.
func (p *PrintServer) SendToAll(body []byte) int {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.open))
	for c := range p.open {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	sent := 0
	for _, c := range conns {
		if p.write(c, body) == nil {
			sent++
		}
	}
	return sent
}

// DropConnections closes every open connection from the server's side.
func (p *PrintServer) DropConnections() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.open))
	for c := range p.open {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (p *PrintServer) write(c net.Conn, body []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := c.Write(ozoneproto.Frame(body))
	return err
}

// SetList overrides the verbatim response to {"command":"list"}.
func (p *PrintServer) SetList(listJSON []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listJSON = append([]byte(nil), listJSON...)
}

// AddGame registers (or replaces) the full "all" payload for a game number.
func (p *PrintServer) AddGame(gameNumber int, allJSON []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.games[gameNumber] = ozonefix.Compact(allJSON)
}

// Start begins listening on 127.0.0.1:<port>. Use port 0 for an ephemeral port.
func (p *PrintServer) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go p.acceptLoop(ln)
	return nil
}

// Addr returns the bound address (host:port).
func (p *PrintServer) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// Connections returns the number of connections accepted so far.
func (p *PrintServer) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

// Requests returns how many times a given command was received.
func (p *PrintServer) Requests(command string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[command]
}

// Close stops the listener and closes the connections still open.
func (p *PrintServer) Close() error {
	p.mu.Lock()
	p.closed = true
	ln := p.ln
	p.mu.Unlock()
	p.DropConnections()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

func (p *PrintServer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns++
		p.open[conn] = true
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *PrintServer) handle(conn net.Conn) {
	defer func() {
		p.mu.Lock()
		delete(p.open, conn)
		p.mu.Unlock()
		_ = conn.Close()
	}()

	// Push the connect banner first (2 frames), like real O-Zone.
	p.mu.Lock()
	banner := p.banner
	p.mu.Unlock()
	for _, frame := range banner {
		if err := p.write(conn, frame); err != nil {
			return
		}
	}

	for {
		payload, err := ozoneproto.ReadFrame(conn)
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}
		resp, silent := p.respond(req)
		if silent {
			continue
		}
		if err := p.write(conn, resp); err != nil {
			return
		}
	}
}

// respond returns the framed JSON reply and whether the request is silent (ack).
func (p *PrintServer) respond(req map[string]any) (reply []byte, silent bool) {
	// Data-acknowledged messages get no reply, unless AnswerAcks says so.
	if _, ok := req["success"]; ok {
		p.count("ack")
		p.mu.Lock()
		reply, delay := p.ackReply, p.ackDelay
		p.mu.Unlock()
		if reply == nil {
			return nil, true
		}
		time.Sleep(delay)
		return reply, false
	}
	if _, ok := req["autoprint"]; ok {
		p.count("autoprint")
		return []byte(`{"success":true}`), false
	}

	command, _ := req["command"].(string)
	p.count(command)

	switch command {
	case "list":
		p.mu.Lock()
		defer p.mu.Unlock()
		return append([]byte(nil), p.listJSON...), false
	case "all":
		return p.gameResponse(req, false), false
	case "minimal":
		return p.gameResponse(req, true), false
	case "team":
		return p.subsetResponse(req, "teams"), false
	case "player":
		return p.subsetResponse(req, "players"), false
	default:
		return []byte(`{"error":"Unknown command"}`), false
	}
}

func (p *PrintServer) count(command string) {
	p.mu.Lock()
	p.requests[command]++
	hook := p.onRequest
	p.mu.Unlock()
	if hook != nil {
		hook(command)
	}
}

func (p *PrintServer) gameResponse(req map[string]any, minimal bool) []byte {
	num, ok := gameNumber(req)
	if !ok {
		return []byte(`{"error":"Missing gamenumber"}`)
	}
	p.mu.Lock()
	raw, found := p.games[num]
	p.mu.Unlock()
	if !found {
		return []byte(`{"error":"Game not found"}`)
	}
	if !minimal {
		return append([]byte(nil), raw...)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		return append([]byte(nil), raw...)
	}
	delete(data, "events")
	out, err := json.Marshal(data)
	if err != nil {
		return append([]byte(nil), raw...)
	}
	return out
}

// subsetResponse returns game + the named ids from the "teams"/"players" map.
func (p *PrintServer) subsetResponse(req map[string]any, section string) []byte {
	num, ok := gameNumber(req)
	if !ok {
		return []byte(`{"error":"Missing gamenumber"}`)
	}
	p.mu.Lock()
	raw, found := p.games[num]
	p.mu.Unlock()
	if !found {
		return []byte(`{"error":"Game not found"}`)
	}

	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		return []byte(`{"error":"Invalid cached data"}`)
	}
	ids := stringIDs(req["ids"])
	out := map[string]json.RawMessage{}
	if g, ok := full["game"]; ok {
		out["game"] = g
	}
	if sectionRaw, ok := full[section]; ok {
		var all map[string]json.RawMessage
		if err := json.Unmarshal(sectionRaw, &all); err == nil {
			picked := map[string]json.RawMessage{}
			if len(ids) == 0 {
				picked = all
			} else {
				for _, id := range ids {
					if v, ok := all[id]; ok {
						picked[id] = v
					}
				}
			}
			if b, err := json.Marshal(picked); err == nil {
				out[section] = b
			}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"error":"Invalid cached data"}`)
	}
	return b
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
		switch s := item.(type) {
		case string:
			ids = append(ids, s)
		case float64:
			ids = append(ids, fmt.Sprintf("%d", int(s)))
		}
	}
	return ids
}
