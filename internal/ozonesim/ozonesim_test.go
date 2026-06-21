package ozonesim

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"overwatch/agent/internal/ozoneproto"
)

// dialPrintServer connects, drains the 2-frame connect banner, and returns the conn.
func dialPrintServer(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for i := 0; i < 2; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := ozoneproto.ReadFrame(conn); err != nil {
			t.Fatalf("banner frame %d: %v", i, err)
		}
	}
	return conn
}

func request(t *testing.T, conn net.Conn, cmd map[string]any) map[string]json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(cmd)
	if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	payload, err := ozoneproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode reply %q: %v", payload, err)
	}
	return out
}

func TestPrintServerCommands(t *testing.T) {
	ps := NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	defer ps.Close()

	conn := dialPrintServer(t, ps.Addr())
	defer conn.Close()

	// list
	list := request(t, conn, map[string]any{"command": "list"})
	if _, ok := list["gamelist"]; !ok {
		t.Error("list: missing gamelist")
	}

	// all (game 9 is seeded)
	all := request(t, conn, map[string]any{"command": "all", "gamenumber": 9})
	for _, k := range []string{"game", "players", "events", "scores"} {
		if _, ok := all[k]; !ok {
			t.Errorf("all: missing %q", k)
		}
	}

	// minimal removes events
	min := request(t, conn, map[string]any{"command": "minimal", "gamenumber": 9})
	if _, ok := min["events"]; ok {
		t.Error("minimal: events should be absent")
	}
	if _, ok := min["players"]; !ok {
		t.Error("minimal: players should be present")
	}

	// team subset
	team := request(t, conn, map[string]any{"command": "team", "gamenumber": 9, "ids": []string{"0"}})
	var teams map[string]json.RawMessage
	if err := json.Unmarshal(team["teams"], &teams); err != nil {
		t.Fatalf("team subset decode: %v", err)
	}
	if len(teams) != 1 {
		t.Errorf("team subset: got %d teams, want 1", len(teams))
	}
	if _, ok := teams["0"]; !ok {
		t.Error("team subset: missing requested team 0")
	}

	// player subset
	player := request(t, conn, map[string]any{"command": "player", "gamenumber": 9, "ids": []string{"1"}})
	var players map[string]json.RawMessage
	if err := json.Unmarshal(player["players"], &players); err != nil {
		t.Fatalf("player subset decode: %v", err)
	}
	if _, ok := players["1"]; !ok || len(players) != 1 {
		t.Errorf("player subset: got %v, want only player 1", players)
	}

	// autoprint acks success
	ap := request(t, conn, map[string]any{"autoprint": 1})
	if string(ap["success"]) != "true" {
		t.Errorf("autoprint: got %v", ap)
	}

	// unknown command
	unk := request(t, conn, map[string]any{"command": "frobnicate"})
	if _, ok := unk["error"]; !ok {
		t.Error("unknown command should return error")
	}

	// missing game
	missing := request(t, conn, map[string]any{"command": "all", "gamenumber": 999})
	if _, ok := missing["error"]; !ok {
		t.Error("missing game should return error")
	}

	if ps.Connections() != 1 {
		t.Errorf("connections = %d, want 1", ps.Connections())
	}
	if ps.Requests("all") != 2 { // "all" + the missing-game "all"
		t.Errorf("all requests = %d, want 2", ps.Requests("all"))
	}
}

// The ack message ({"success":true}) must produce no reply.
func TestPrintServerAckIsSilent(t *testing.T) {
	ps := NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	defer ps.Close()

	conn := dialPrintServer(t, ps.Addr())
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{"success": true})
	if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
		t.Fatal(err)
	}
	// Then a real command; the only reply we read must be for the command.
	list := request(t, conn, map[string]any{"command": "list"})
	if _, ok := list["gamelist"]; !ok {
		t.Error("expected gamelist after silent ack")
	}
	if ps.Requests("ack") != 1 {
		t.Errorf("ack count = %d, want 1", ps.Requests("ack"))
	}
}

func TestMessageBusEmit(t *testing.T) {
	mb := NewMessageBus()
	if err := mb.Start(0); err != nil {
		t.Fatal(err)
	}
	defer mb.Close()

	conn, err := net.DialTimeout("tcp", mb.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Wait for the server to register the connection.
	deadline := time.Now().Add(time.Second)
	for mb.ConnectionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	mb.Emit(EventGameStart, "4", "-1")

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if got, want := line, "[1001, 4, -1]\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if len(mb.Sent()) != 1 {
		t.Errorf("sent history = %v", mb.Sent())
	}
}
