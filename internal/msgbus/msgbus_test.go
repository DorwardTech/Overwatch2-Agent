package msgbus

import (
	"context"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"overwatch/agent/internal/ozonesim"
)

func TestParse(t *testing.T) {
	cases := []struct {
		line string
		want Event
		ok   bool
	}{
		{"[1000]", Event{Code: 1000, Args: []string{}, Raw: "[1000]"}, true},
		{"[1001, 4, -1]", Event{Code: 1001, Args: []string{"4", "-1"}, Raw: "[1001, 4, -1]"}, true},
		{"[1006, 0, RED, 1, #FF0000]", Event{Code: 1006, Args: []string{"0", "RED", "1", "#FF0000"}, Raw: "[1006, 0, RED, 1, #FF0000]"}, true},
		{"  [1003]  ", Event{Code: 1003, Args: []string{}, Raw: "[1003]"}, true},
		{"garbage", Event{}, false},
		{"[]", Event{}, false},
		{"[notanumber, 1]", Event{}, false},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.line)
		if ok != tc.ok {
			t.Errorf("Parse(%q) ok=%v want %v", tc.line, ok, tc.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.line, got, tc.want)
		}
	}
}

func TestGameNumber(t *testing.T) {
	ev, _ := Parse("[1001, 7, -1]")
	if n, ok := ev.GameNumber(); !ok || n != 7 {
		t.Errorf("GameNumber = %d, %v; want 7, true", n, ok)
	}
	idle, _ := Parse("[1000]")
	if _, ok := idle.GameNumber(); ok {
		t.Error("idle event should not yield a game number")
	}
}

// End-to-end against the fake message bus: events emitted are delivered, parsed,
// and handed to the handler in order.
func TestClientConsumesFromFakeBus(t *testing.T) {
	bus := ozonesim.NewMessageBus()
	if err := bus.Start(0); err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	var mu sync.Mutex
	var got []int
	host, port, err := net.SplitHostPort(bus.Addr())
	if err != nil {
		t.Fatal(err)
	}
	client := New(host, port, func(e Event) {
		mu.Lock()
		got = append(got, e.Code)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	// Wait for the client to connect.
	waitFor(t, func() bool { return bus.ConnectionCount() == 1 })

	bus.Emit(EventIdle)
	bus.Emit(EventGameStart, "9", "-1")
	bus.Emit(EventGameFinish)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	want := []int{EventIdle, EventGameStart, EventGameFinish}
	if !reflect.DeepEqual(got[:3], want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
