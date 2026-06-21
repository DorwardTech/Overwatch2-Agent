package ozonesim

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// Message Bus event codes (External Message Bus, ExternalMessageBusAPI.MD).
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

// MessageBus is a fake O-Zone External Message Bus (TCP, newline-delimited
// "[code, args...]" strings). Connected clients receive every Emit.
type MessageBus struct {
	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	sent  []string // history of emitted messages
}

// NewMessageBus creates an idle message bus.
func NewMessageBus() *MessageBus {
	return &MessageBus{conns: map[net.Conn]struct{}{}}
}

// Start listens on 127.0.0.1:<port>. Use port 0 for an ephemeral port.
func (m *MessageBus) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ln = ln
	m.mu.Unlock()
	go m.acceptLoop(ln)
	return nil
}

// Addr returns the bound address (host:port).
func (m *MessageBus) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Emit broadcasts a "[code, args...]\n" message to all connected clients.
func (m *MessageBus) Emit(code int, args ...string) {
	parts := append([]string{fmt.Sprintf("%d", code)}, args...)
	line := "[" + strings.Join(parts, ", ") + "]\n"

	m.mu.Lock()
	m.sent = append(m.sent, strings.TrimSpace(line))
	conns := make([]net.Conn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()

	for _, c := range conns {
		_, _ = c.Write([]byte(line))
	}
}

// Sent returns the history of emitted messages (for assertions).
func (m *MessageBus) Sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sent...)
}

// ConnectionCount returns the number of currently connected clients.
func (m *MessageBus) ConnectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

// Close stops the listener and drops clients.
func (m *MessageBus) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for c := range m.conns {
		_ = c.Close()
	}
	if m.ln != nil {
		return m.ln.Close()
	}
	return nil
}

func (m *MessageBus) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.conns[conn] = struct{}{}
		m.mu.Unlock()
		go m.drain(conn)
	}
}

// drain reads and discards anything the client sends (the bus is transmit-only)
// and removes the connection when it closes.
func (m *MessageBus) drain(conn net.Conn) {
	buf := make([]byte, 256)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
	m.mu.Lock()
	delete(m.conns, conn)
	m.mu.Unlock()
	_ = conn.Close()
}
