// Package results is a TCP client for O-Zone's binary Print Server / results API
// (port 12123): a 5-byte header — 4-byte little-endian payload length followed by
// the 0x28 token byte — then a JSON payload. Used to fetch completed game data
// (per-pack hit zones + fitness) after a game finishes.
package results

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	tokenByte  = 0x28
	maxPayload = 10 << 20 // 10 MiB safety cap
)

type Client struct {
	conn net.Conn
}

// Dial opens a TCP connection to the O-Zone results API.
func Dial(host, port string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// Drain reads and discards `frames` server-pushed messages. On connect O-Zone
// sends a fixed handshake (texts + event_types) before it will answer commands;
// these must be consumed first or the next receive() returns the handshake
// instead of the requested game data. A read error stops draining (best-effort).
func (c *Client) Drain(frames int, timeout time.Duration) {
	for i := 0; i < frames; i++ {
		if _, err := c.readFrame(timeout); err != nil {
			return
		}
	}
}

// GameData requests the full data (incl. tagsonl/tagsbyl detail) for one game.
func (c *Client) GameData(gameNumber int, timeout time.Duration) (map[string]any, error) {
	if err := c.send(map[string]any{"gamenumber": gameNumber, "command": "all"}, timeout); err != nil {
		return nil, err
	}
	return c.receive(timeout)
}

// GameList requests the summary list of all games stored in O-Zone.
func (c *Client) GameList(timeout time.Duration) (map[string]any, error) {
	if err := c.send(map[string]any{"command": "list"}, timeout); err != nil {
		return nil, err
	}
	return c.receive(timeout)
}

// GameDataRaw requests one game's full data and returns the verbatim JSON payload
// bytes (no decode/re-encode). Used to populate the local cache 1:1 so the proxy
// can replay exactly what O-Zone returned.
func (c *Client) GameDataRaw(gameNumber int, timeout time.Duration) ([]byte, error) {
	if err := c.send(map[string]any{"gamenumber": gameNumber, "command": "all"}, timeout); err != nil {
		return nil, err
	}
	return c.readFrame(timeout)
}

// GameListRaw requests the game list and returns the verbatim JSON payload bytes.
func (c *Client) GameListRaw(timeout time.Duration) ([]byte, error) {
	if err := c.send(map[string]any{"command": "list"}, timeout); err != nil {
		return nil, err
	}
	return c.readFrame(timeout)
}

// Acknowledge tells O-Zone the data was received.
func (c *Client) Acknowledge(timeout time.Duration) error {
	return c.send(map[string]any{"success": true}, timeout)
}

func (c *Client) send(cmd map[string]any, timeout time.Duration) error {
	body, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err = c.conn.Write(frame(body))
	return err
}

func (c *Client) receive(timeout time.Duration) (map[string]any, error) {
	body, err := c.readFrame(timeout)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode results payload: %w", err)
	}
	return out, nil
}

// readFrame reads one length-prefixed, 0x28-tokened message and returns its body.
func (c *Client) readFrame(timeout time.Duration) ([]byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))

	header := make([]byte, 5)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		return nil, err
	}
	if header[4] != tokenByte {
		return nil, fmt.Errorf("unexpected token byte 0x%x", header[4])
	}
	length := int(binary.LittleEndian.Uint32(header[:4]))
	if length <= 0 || length > maxPayload {
		return nil, fmt.Errorf("invalid payload length %d", length)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return nil, err
	}
	return body, nil
}

// frame builds the 5-byte header (little-endian length + 0x28 token) + JSON body.
// Bodies are bounded by maxPayload, so the length fits the uint32 header field;
// an over-size body (never expected) is capped defensively rather than overflowing.
func frame(body []byte) []byte {
	if len(body) > maxPayload {
		body = body[:maxPayload]
	}
	n := len(body) // 0 <= n <= maxPayload, so it fits in uint32
	packet := make([]byte, 5+n)
	binary.LittleEndian.PutUint32(packet[:4], uint32(n))
	packet[4] = tokenByte
	copy(packet[5:], body)
	return packet
}
