// Package msgbus is a client for O-Zone's External Message Bus (TCP 12111). The
// bus is transmit-only: O-Zone pushes newline-delimited "[code, arg, ...]"
// strings. Game lifecycle events (idle/start/abort/finish) drive the agent's
// game-state machine, which is the primary signal for idle-gating print-server
// access (with WS SERVERMODE as a backstop).
//
// See Project-Lattice/Init-Resources/ExternalMessageBusAPI.MD for the full event
// table; the codes below are the lifecycle subset the agent acts on.
package msgbus

import (
	"bufio"
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Lifecycle event codes (ExternalMessageBusAPI.MD §"Game Events").
const (
	EventIdle          = 1000
	EventGameStart     = 1001
	EventGameAbort     = 1002
	EventGameFinish    = 1003
	EventGameName      = 1004
	EventTimeRemaining = 1005
	EventTeamAdded     = 1006
	EventTeamWinning   = 1007
	EventPreGame       = 1012
	EventPostGame      = 1013
)

// Event is one parsed message-bus line.
type Event struct {
	Code int
	Args []string
	Raw  string
}

// GameNumber returns the game number argument of a GAME_START event, if present.
func (e Event) GameNumber() (int, bool) {
	if e.Code != EventGameStart || len(e.Args) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(e.Args[0]))
	if err != nil {
		return 0, false
	}
	return n, true
}

// Parse parses one bus line, e.g. "[1001, 4, -1]" -> {Code:1001, Args:["4","-1"]}.
func Parse(line string) (Event, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return Event{}, false
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	if inner == "" {
		return Event{}, false
	}
	parts := strings.Split(inner, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return Event{}, false
	}
	return Event{Code: code, Args: parts[1:], Raw: line}, true
}

// Client is a reconnecting Message Bus consumer.
type Client struct {
	addr      string
	handler   func(Event)
	connected atomic.Bool
}

// New creates a client that calls handler for each parsed event.
func New(host, port string, handler func(Event)) *Client {
	return &Client{addr: net.JoinHostPort(host, port), handler: handler}
}

// Connected reports whether the bus is currently connected.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run connects and consumes events until ctx is cancelled, reconnecting with
// capped exponential backoff. The bus is best-effort: failures never stop the
// agent, they just fall back to SERVERMODE-based gating.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", c.addr)
		if err != nil {
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		log.Printf("[agent] message bus connected (%s)", c.addr)
		c.connected.Store(true)
		backoff = time.Second
		c.consume(ctx, conn)
		c.connected.Store(false)
		_ = conn.Close()
	}
}

func (c *Client) consume(ctx context.Context, conn net.Conn) {
	// Close the connection when the context ends so the blocking scan returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if ev, ok := Parse(line); ok {
			c.handler(ev)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}
