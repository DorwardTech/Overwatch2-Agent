// Command ozonesim runs a fake O-Zone: the Print Server (TCP 12123) and the
// External Message Bus (TCP 12111), serving golden fixtures. It lets the agent's
// cache, proxy, and idle-gating be demoed end-to-end without hardware.
//
// Env:
//
//	PRINT_SERVER_PORT  default 12123
//	MSG_BUS_PORT       default 12111
//	GAME_CYCLE         "1" to loop an idle→start→finish→idle game lifecycle on
//	                   the message bus (default off; the bus stays idle)
//	CYCLE_PERIOD       seconds between lifecycle phases when GAME_CYCLE=1 (default 20)
package main

import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"overwatch/agent/internal/ozonesim"
)

func main() {
	psPort := envInt("PRINT_SERVER_PORT", 12123)
	mbPort := envInt("MSG_BUS_PORT", 12111)

	ps := ozonesim.NewPrintServer()
	if err := ps.Start(psPort); err != nil {
		log.Fatalf("ozonesim: print server: %v", err)
	}
	log.Printf("[ozonesim] print server listening on %s", ps.Addr())

	mb := ozonesim.NewMessageBus()
	if err := mb.Start(mbPort); err != nil {
		log.Fatalf("ozonesim: message bus: %v", err)
	}
	log.Printf("[ozonesim] message bus listening on %s", mb.Addr())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	if os.Getenv("GAME_CYCLE") == "1" {
		go runGameCycle(mb, envInt("CYCLE_PERIOD", 20))
	} else {
		mb.Emit(ozonesim.EventIdle)
	}

	<-stop
	log.Print("[ozonesim] shutting down")
	_ = ps.Close()
	_ = mb.Close()
}

// runGameCycle loops a realistic lifecycle so the agent's state machine and
// idle-gating can be observed: idle → name → start → (time ticks) → finish → idle.
func runGameCycle(mb *ozonesim.MessageBus, periodSec int) {
	period := time.Duration(periodSec) * time.Second
	game := 9
	for {
		mb.Emit(ozonesim.EventIdle)
		time.Sleep(period)

		mb.Emit(ozonesim.EventGameName, "Competition Team Elimination")
		mb.Emit(ozonesim.EventPreGame, "0")
		mb.Emit(ozonesim.EventGameStart, strconv.Itoa(game), "-1")
		log.Printf("[ozonesim] game %d started (print server should NOT be hit now)", game)

		for pct := 75; pct >= 0; pct -= 25 {
			time.Sleep(period / 4)
			mb.Emit(ozonesim.EventTimeRemaining, strconv.Itoa(pct))
		}

		mb.Emit(ozonesim.EventGameFinish)
		mb.Emit(ozonesim.EventPostGame, "0")
		log.Printf("[ozonesim] game %d finished (print server fetch allowed after delay)", game)
		time.Sleep(period)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
