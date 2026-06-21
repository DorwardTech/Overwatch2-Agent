// Package ozone is a minimal WebSocket client for the O-Zone read-only API.
package ozone

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn *websocket.Conn
}

// Dial connects to the O-Zone WebSocket server.
func Dial(host, port string) (*Client, error) {
	u := url.URL{Scheme: "ws", Host: host + ":" + port, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

// Command sends a read-only command and returns the decoded JSON response.
func (c *Client) Command(cmd string) (map[string]any, error) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteJSON(map[string]string{"CMD": cmd}); err != nil {
		return nil, err
	}

	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	var resp map[string]any
	if err := json.Unmarshal(msg, &resp); err != nil {
		return nil, fmt.Errorf("decode %s: %w", cmd, err)
	}
	return resp, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
