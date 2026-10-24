package health

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReadyzSaysWhyItIsNotReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	h := New()
	h.SetReason(func() string { return "waiting for the game server's announcement on UDP 12106" })
	go h.Serve(addr)

	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/readyz")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.HasPrefix(string(body), "not ready: waiting") {
		t.Fatalf("/readyz = %d %q", resp.StatusCode, body)
	}
}
