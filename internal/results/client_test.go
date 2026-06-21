package results

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestFrameHeader(t *testing.T) {
	body := []byte(`{"command":"list"}`)
	pkt := frame(body)

	if len(pkt) != 5+len(body) {
		t.Fatalf("packet length = %d, want %d", len(pkt), 5+len(body))
	}
	if got := int(binary.LittleEndian.Uint32(pkt[:4])); got != len(body) {
		t.Fatalf("header length = %d, want %d", got, len(body))
	}
	if pkt[4] != tokenByte {
		t.Fatalf("token byte = 0x%x, want 0x28", pkt[4])
	}
}

func TestGameDataRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		hdr := make([]byte, 5)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32(hdr[:4]))
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["command"] != "all" {
			return
		}

		resp, _ := json.Marshal(map[string]any{
			"game":    map[string]any{"gamenum": 3, "gamename": "TDM"},
			"players": map[string]any{},
		})
		_, _ = conn.Write(frame(resp))
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := Dial(host, port, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data, err := c.GameData(3, 2*time.Second)
	if err != nil {
		t.Fatalf("GameData: %v", err)
	}
	game, ok := data["game"].(map[string]any)
	if !ok || game["gamename"] != "TDM" {
		t.Fatalf("unexpected payload: %v", data)
	}
}

func TestGameListRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32(hdr[:4]))
		_, _ = io.ReadFull(conn, make([]byte, n))
		resp, _ := json.Marshal(map[string]any{
			"gamelist": []any{
				map[string]any{"gamenum": 1, "valid": 1, "gamename": "TDM"},
				map[string]any{"gamenum": 2, "valid": 0, "gamename": "Invalid"},
			},
		})
		_, _ = conn.Write(frame(resp))
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := Dial(host, port, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	list, err := c.GameList(2 * time.Second)
	if err != nil {
		t.Fatalf("GameList: %v", err)
	}
	games, ok := list["gamelist"].([]any)
	if !ok || len(games) != 2 {
		t.Fatalf("expected 2 games, got: %v", list)
	}
}

// TestDrainThenGameData mimics O-Zone's real handshake: two pushed messages on
// connect, then the game data. Without draining the two, GameData would return
// the first handshake frame (no players) instead of the game.
func TestDrainThenGameData(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Handshake: texts + event_types.
		_, _ = conn.Write(frame([]byte(`{"texts":{}}`)))
		_, _ = conn.Write(frame([]byte(`{"event_types":{}}`)))

		// Then answer the command with real game data.
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32(hdr[:4]))
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"game":    map[string]any{"gamename": "Domination"},
			"players": map[string]any{"1": map[string]any{"alias": "Ava"}},
		})
		_, _ = conn.Write(frame(resp))
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := Dial(host, port, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Drain(2, 2*time.Second)

	data, err := c.GameData(7, 2*time.Second)
	if err != nil {
		t.Fatalf("GameData: %v", err)
	}
	game, ok := data["game"].(map[string]any)
	if !ok || game["gamename"] != "Domination" {
		t.Fatalf("expected game data after drain, got: %v", data)
	}
	if _, ok := data["players"].(map[string]any); !ok {
		t.Fatalf("expected players in payload, got: %v", data)
	}
}
