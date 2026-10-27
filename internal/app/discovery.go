package app

import (
	"context"
	"fmt"
	"log"
	"net/netip"
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

// awaitOzone listens for announcements until one game server has been the
// only one heard for libWindow, and libWindow has passed since it started
// listening (see ozonelib.Service.Wait), then makes it this process's server.
// It returns false if ctx ends first. Nothing can reach the print server
// meanwhile: there is no address to dial and no server-state reading.
//
// Once the window has passed, it logs what it has heard, in the words /readyz
// uses: a selector that matches none of the servers heard, announcements it
// will not trust, one server at two addresses, several servers, or nothing at
// all (only that gets the networking advice). Each text is logged when it
// changes, and again every five minutes while it stands.
func (a *App) awaitOzone(ctx context.Context) bool {
	if err := a.lib.Start(ctx); err != nil {
		log.Printf("[agent] O-Zone discovery: cannot listen on UDP %d: %v — set OZONE_WS_HOST to the game server's IP", a.lib.Port(), err)
	}
	var logged string
	var loggedAt time.Time
	c, ok := a.lib.Wait(ctx, a.libWindow, func(r ozonelib.Result, windowDone bool) {
		// A lone server is being confirmed: it is chosen, or the reason
		// changes, within a window.
		if !windowDone || r.Outcome == ozonelib.Selected {
			return
		}
		text := a.lib.NotReadyReason()
		if text == "" || (text == logged && time.Since(loggedAt) < 5*time.Minute) {
			return
		}
		logged, loggedAt = text, time.Now()
		log.Printf("[agent] O-Zone discovery: %s", text)
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
	// Both measured on the monotonic clock, so a step of the wall clock can
	// neither make our address look silent nor hide that it is.
	silent := time.Since(a.pinnedAt)
	if age, ok := a.serverModeAge(); ok && age < silent {
		silent = age
	}
	if silent < a.followAfter {
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

// identityChanged reports whether the machine at our address has become a
// different game server (a DHCP lease handed on, say): it announces, usably
// and at least twice, a site or arena other than the one chosen, and the
// chosen one has not been heard there within an entry's lifetime (see
// ozonelib.Table.IdentityLost). Only auto mode asks: with a fixed host it is
// false at once and reads nothing, so a fixed-host agent behaves as before.
//
// It reads the chosen server, which awaitOzone sets before the message bus
// starts and nothing changes after, so the bus goroutine may ask too.
func (a *App) identityChanged() (reason string, lost bool) {
	if a.lib == nil || !a.cfg.OzoneDiscover || a.pinnedAt.IsZero() {
		return "", false
	}
	current, err := netip.ParseAddr(a.ozoneAddr())
	if err != nil || !a.lib.IdentityLost(a.pin.Key, current) {
		return "", false
	}
	return fmt.Sprintf("O-Zone at %s is no longer site %s arena %d", current, a.pin.SIDRaw, a.pin.Arena), true
}

// identityLost restarts the process when the machine at our address has
// become a different game server, which maybeFollow cannot see because that
// machine still answers. collect asks on every poll, as soon as it has the
// server state and before anything uses it or starts on the strength of it.
// It closes the print-server gate first: work that an earlier poll started
// re-checks the gate before each fetch, and the print server at our address
// is now another server's. Run goroutine only.
func (a *App) identityLost() bool {
	reason, lost := a.identityChanged()
	if !lost {
		return false
	}
	a.pollUnsettled.Store(true)
	log.Printf("[agent] %s; restarting to choose again", reason)
	a.restartFor(reason)
	return true
}

// restartFor ends the run so the process restarts, as reboot_agent does. The
// reason is logged by the caller and not reported to central. maybeFollow
// restarts only after our address has stopped answering, so no poll, and no
// self-report, has run for a while. identityLost runs inside collect, which
// drops the poll that notices: it is not pushed, and starts no print-server
// work and no commands. The polls before it are pushed as usual: until the
// chosen server's last announcement lapses (up to 30 s after it stops) there
// is nothing to tell the two machines apart by.
func (a *App) restartFor(reason string) {
	a.restartReason = reason
	if a.stopRun != nil {
		a.stopRun()
	}
}
