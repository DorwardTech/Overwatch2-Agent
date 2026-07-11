// Command proxydemo is a tiny TORN-like client for the O-Zone print-server proxy
// (or the real O-Zone print server). It connects, drains the connect banner, then
// exercises every command and prints the replies — a runnable example of the
// API the cache serves. See docs/DEMOS.md.
//
// Usage:
//
//	go run ./cmd/proxydemo -addr 127.0.0.1:12124
//	go run ./cmd/proxydemo -addr 127.0.0.1:12124 -game 9
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"overwatch/agent/internal/ozoneproto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:12123", "print-server proxy address")
	game := flag.Int("game", 0, "game number for all/minimal/team/player (0 = first valid from list)")
	flag.Parse()

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		log.Fatalf("connect %s: %v", *addr, err)
	}
	defer conn.Close()
	fmt.Printf("connected to %s\n", *addr)

	// Drain the 2-frame connect banner (event types + texts), like TORN does.
	for i := 0; i < 2; i++ {
		if _, err := readFrame(conn); err != nil {
			log.Fatalf("banner frame %d: %v", i, err)
		}
	}
	fmt.Println("drained 2 connect-banner frames")

	// list
	listReply := request(conn, map[string]any{"command": "list"})
	fmt.Printf("\n== list ==\n%s\n", pretty(listReply))

	target := *game
	if target == 0 {
		target = firstValidGame(listReply)
	}
	if target == 0 {
		fmt.Println("\n(no valid game to fetch; cache is empty)")
		return
	}

	for _, cmd := range []map[string]any{
		{"command": "all", "gamenumber": target},
		{"command": "minimal", "gamenumber": target},
		{"command": "team", "gamenumber": target, "ids": []string{"0"}},
		{"command": "player", "gamenumber": target, "ids": []string{"1"}},
	} {
		fmt.Printf("\n== %v ==\n%s\n", cmd, pretty(request(conn, cmd)))
	}
}

func request(conn net.Conn, cmd map[string]any) []byte {
	body, _ := json.Marshal(cmd)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
		log.Fatalf("write: %v", err)
	}
	reply, err := readFrame(conn)
	if err != nil {
		log.Fatalf("read reply: %v", err)
	}
	return reply
}

func readFrame(conn net.Conn) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return ozoneproto.ReadFrame(conn)
}

func firstValidGame(listReply []byte) int {
	var v struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(listReply, &v); err != nil {
		return 0
	}
	for _, g := range v.GameList {
		valid, _ := g["valid"].(float64)
		num, _ := g["gamenum"].(float64)
		if valid > 0 && num > 0 {
			return int(num)
		}
	}
	return 0
}

func pretty(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
