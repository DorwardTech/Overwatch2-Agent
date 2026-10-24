package app

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"time"

	"overwatch/agent/internal/ozonelib"
)

// Discovery: with OZONE_WS_HOST=auto the agent chooses the game server's
// address from its LAN announcement, once, before it first connects, and
// keeps it for the life of the process. Every server-state reading and
// message-bus event in a process therefore comes from the address the print
// server is dialled at; the gate needs to know nothing about addresses.
// Following a server to a new address, or noticing that the machine at ours
// has become a different server, restarts the process instead, which starts
// with the gate shut.

// setOzoneHost sets the game server's address for this process. It can be
// set once only: a second call is a programming error and is refused.
func (a *App) setOzoneHost(h string) bool {
	if !a.ozoneHost.CompareAndSwap(nil, &h) {
		log.Printf("[agent] internal error: the game server's address is already set to %s; refusing %s", a.ozoneAddr(), h)
		return false
	}
	return true
}

// ozoneAddr is the game server's address, or "" until auto mode chooses one.
func (a *App) ozoneAddr() string {
	if p := a.ozoneHost.Load(); p != nil {
		return *p
	}
	return ""
}

// awaitOzone listens for announcements until exactly one game server is heard
// and libWindow has passed, then makes it this process's server. It returns
// false if ctx ends first. Nothing can reach the print server meanwhile:
// there is no address to dial and no server-state reading.
func (a *App) awaitOzone(ctx context.Context) bool {
	if err := a.lib.Start(ctx); err != nil {
		log.Printf("[agent] O-Zone discovery: cannot listen on UDP %d: %v — set OZONE_WS_HOST to the game server's IP", a.lib.Port(), err)
	}
	var loggedNone time.Time
	var loggedSet string
	c, ok := a.lib.Wait(ctx, a.libWindow, func(r ozonelib.Result, windowDone bool) {
		if !windowDone {
			return
		}
		port := a.lib.Port()
		switch r.Outcome {
		case ozonelib.None:
			if loggedNone.IsZero() || time.Since(loggedNone) >= 5*time.Minute {
				loggedNone = time.Now()
				log.Printf("[agent] O-Zone discovery: no game server announcement heard on UDP %d — in Docker this needs host networking; on Windows allow inbound UDP %d; or set OZONE_WS_HOST to the game server's IP", port, port)
			}
		case ozonelib.Ambiguous, ozonelib.Conflict:
			names := make([]string, 0, len(r.Candidates))
			for _, c := range r.Candidates {
				names = append(names, ozonelib.Describe(c))
			}
			set := strings.Join(names, "; ")
			if set != loggedSet {
				loggedSet = set
				log.Printf("[agent] O-Zone discovery: %s, not choosing between %s — set OZONE_SID and OZONE_ARENA", r.Outcome, set)
			}
		}
	})
	if !ok {
		return false
	}
	if !a.setOzoneHost(c.IP.String()) {
		return false
	}
	log.Printf("[agent] O-Zone found via LIB: %s", ozonelib.Describe(c))
	if a.cfg.LIB.SID == nil || a.cfg.LIB.Arena == nil {
		log.Printf("[agent] O-Zone discovery: pinned to site %s arena %d for this run; set OZONE_SID and OZONE_ARENA to make it permanent", c.SIDRaw, c.Arena)
	}
	a.pin = c
	a.pinnedAt = time.Now()
	a.lib.SetSelected(c)
	if a.bus != nil {
		a.bus.SetHost(c.IP.String())
		go a.bus.Run(ctx)
	}
	return true
}

// maybeFollow restarts the process when the chosen server has plainly moved
// to a new address: our address has not answered for followAfter (a server
// that still answers is never left), the server is announced from exactly
// one other address, heard at least twice, and that address answers a
// server-state request. The probe's reading is discarded: the new process
// reads the server state for itself, with the gate shut until it does.
func (a *App) maybeFollow(ctx context.Context) bool {
	if a.lib == nil || !a.cfg.OzoneDiscover || a.pinnedAt.IsZero() {
		return false
	}
	last := a.pinnedAt
	if at := a.serverModeAt.Load(); at != 0 && time.Unix(0, at).After(last) {
		last = time.Unix(0, at)
	}
	if time.Since(last) < a.followAfter {
		return false
	}
	current, err := netip.ParseAddr(a.ozoneAddr())
	if err != nil {
		return false
	}
	moved, ok := a.lib.Moved(a.pin.Key, current)
	if !ok {
		return false
	}
	if _, err := a.probe(ctx, moved.String(), a.cfg.OzonePort, 5*time.Second); err != nil {
		return false
	}
	reason := fmt.Sprintf("O-Zone site %s arena %d moved %s -> %s", a.pin.SIDRaw, a.pin.Arena, current, moved)
	log.Printf("[agent] %s; restarting to reconnect (print server stays closed until the new address reports idle)", reason)
	a.restartFor(reason)
	return true
}

// identityLost restarts the process when the machine at our address has
// become a different game server (a DHCP lease handed on, say), which
// maybeFollow cannot see because that machine still answers.
func (a *App) identityLost() bool {
	if a.lib == nil || !a.cfg.OzoneDiscover || a.pinnedAt.IsZero() {
		return false
	}
	current, err := netip.ParseAddr(a.ozoneAddr())
	if err != nil || !a.lib.IdentityLost(a.pin.Key, current) {
		return false
	}
	reason := fmt.Sprintf("O-Zone at %s is no longer site %s arena %d", current, a.pin.SIDRaw, a.pin.Arena)
	log.Printf("[agent] %s; restarting to choose again", reason)
	a.restartFor(reason)
	return true
}

// restartFor ends the run so the process restarts, as reboot_agent does. The
// reason is logged by the caller; no self-report follows (none is sent while
// the server is unreachable, and a poll that finds our address has become
// another server is dropped), so it is not reported.
func (a *App) restartFor(reason string) {
	a.restartReason = reason
	if a.stopRun != nil {
		a.stopRun()
	}
}
