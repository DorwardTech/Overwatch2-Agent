package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozonelib"
)

const (
	// defaultDiscoverSeconds is as long as auto mode listens before it
	// chooses: long enough to hear every server at least twice.
	defaultDiscoverSeconds = 11
	maxDiscoverSeconds     = 60
)

// runDiscover is the `discover` command: it listens for the game server's
// LAN announcement and lists every server heard, with the one the agent
// would choose. It only listens; it connects to nothing, the print server
// least of all, so it is safe during a game and beside a running agent (the
// port is shared). It runs as `docker exec <agent> /agent discover` and from
// a Windows prompt alike.
func runDiscover(opts options) int {
	// Best-effort, as for healthcheck: the file supplies OZONE_LIB_PORT and
	// the selectors if it sets them, and the defaults stand in otherwise.
	if _, _, err := applyEnvFile(opts); err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v — carrying on without it\n", err)
	}
	lib, err := config.LoadLIBSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return discover(ctx, os.Stdout, lib, os.Getenv("OZONE_WS_HOST"), opts.seconds, ozonelib.Scan)
}

// scanFunc is ozonelib.Scan, replaced in tests.
type scanFunc func(ctx context.Context, port int, d time.Duration) ([]ozonelib.Candidate, ozonelib.Counters, error)

// discover listens for seconds (less if ctx ends first), writes what it heard
// to w and returns the exit code. host is OZONE_WS_HOST as set.
func discover(ctx context.Context, w io.Writer, lib config.LIBSettings, host string, seconds int, scan scanFunc) int {
	fmt.Fprintf(w, "Listening on UDP %d for %d s...\n", lib.Port, seconds)
	cs, n, err := scan(ctx, lib.Port, time.Duration(seconds)*time.Second)
	local := map[netip.Addr]bool{}
	for _, c := range cs {
		local[c.IP] = ozonelib.IsLocal(c.IP)
	}
	return writeDiscover(w, discoverReport{
		LIB:         lib,
		Seconds:     seconds,
		Host:        strings.TrimSpace(host),
		OS:          runtime.GOOS,
		BindErr:     err,
		Interrupted: ctx.Err() != nil,
		Candidates:  cs,
		Counters:    n,
		Local:       local,
	})
}

// discoverReport is everything discover heard and knows, gathered first so
// that writing it out is a pure function of it.
type discoverReport struct {
	LIB         config.LIBSettings // the port listened on, and the selectors
	Seconds     int
	Host        string // OZONE_WS_HOST as set, trimmed ("" means the default)
	OS          string // runtime.GOOS: which advice applies
	BindErr     error
	Interrupted bool
	Candidates  []ozonelib.Candidate
	Counters    ozonelib.Counters
	Local       map[netip.Addr]bool // announced addresses that are this machine's own
}

// writeDiscover writes the report: one row per server heard, the counters,
// what the agent would do with the configured selectors and, when nothing it
// could use was heard, what to check. It returns the exit code: 0 when at
// least one usable server was heard, 1 otherwise.
func writeDiscover(w io.Writer, r discoverReport) int {
	port := r.LIB.Port
	if r.BindErr != nil {
		fmt.Fprintf(w, "Cannot listen on UDP %d: %v\n", port, r.BindErr)
		fmt.Fprintln(w, "Another program may hold the port exclusively (the agent itself shares it). Close that")
		fmt.Fprintln(w, "program, or set OZONE_WS_HOST to the game server's IP instead of discovering it.")
		return exitFailure
	}
	if r.Interrupted {
		fmt.Fprintln(w, "Interrupted: this is what was heard so far.")
	}

	usable := 0
	n := r.Counters
	// Datagrams reached the socket, but none could be used: the network path
	// and the firewall are fine, and the advice says what is wrong instead.
	dropped := len(r.Candidates) == 0 && (n.Offlink > 0 || n.Rejected > 0)
	fmt.Fprintln(w)
	switch {
	case dropped:
		fmt.Fprintf(w, "Announcements arrived, but none could be used: %d from outside this machine's networks, %d that could not be read.\n", n.Offlink, n.Rejected)
	case len(r.Candidates) == 0:
		fmt.Fprintln(w, "No game server announcement was heard.")
	default:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SID\tARENA\tIP\tSENDER\tLOCAL\tPACKETS\tO-ZONE TIME\tRECEIVED (UTC)\tSTATUS")
		for _, c := range r.Candidates {
			local, clock, status := "no", "-", "usable"
			if r.Local[c.IP] {
				local = "yes"
			}
			if c.Last.HasTime {
				clock = c.Last.Time.Text()
			}
			switch {
			case c.Usable:
				usable++
			case c.Reason != "":
				status = c.Reason
			default:
				status = "sent from " + c.Sender.String()
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", c.SIDRaw, c.Arena, c.IP, c.Sender, local,
				c.Packets, clock, c.LastSeen.UTC().Format("2006-01-02 15:04:05"), status)
		}
		_ = tw.Flush()
	}
	fmt.Fprintf(w, "\nPackets: %d recorded, %d rejected, %d off-link dropped, %d sender mismatch\n",
		n.Packets, n.Rejected, n.Offlink, n.SenderMismatch)
	fmt.Fprintln(w)
	for _, line := range discoverOutcome(r) {
		fmt.Fprintln(w, line)
	}
	if usable > 0 {
		return exitOK
	}

	fmt.Fprintln(w, "\nNo usable game server was heard. Check that:")
	switch {
	case dropped:
		if n.Offlink > 0 {
			fmt.Fprintln(w, "  - this machine's address and subnet mask put it on the game server's network: the")
			fmt.Fprintln(w, "    announcements came from an address outside every network it is connected to, and")
			fmt.Fprintln(w, "    the agent ignores those.")
		}
		if n.Rejected > 0 {
			fmt.Fprintln(w, "  - what is sending to this port is the game server's announcement: some datagrams were")
			fmt.Fprintln(w, "    too large or not an announcement the agent can read.")
		}
	case r.OS == "windows":
		fmt.Fprintf(w, "  - Windows Firewall allows inbound UDP %d for this program. Installing the service adds\n", port)
		fmt.Fprintln(w, "    that rule for the installed executable, so run discover from that copy; and check that")
		fmt.Fprintln(w, "    the network's profile is not set to block all incoming connections.")
		if port != firewallPort {
			fmt.Fprintf(w, "    The installed rule opens UDP %d only; OZONE_LIB_PORT=%d needs a rule of its own.\n", firewallPort, port)
		}
	default:
		fmt.Fprintln(w, "  - in a container, the agent uses host networking (network_mode: host): a bridge network")
		fmt.Fprintf(w, "    never receives the broadcast. A host firewall must allow inbound UDP %d too.\n", port)
	}
	fmt.Fprintln(w, "  - this machine is on the game server's subnet or VLAN: the announcement is a broadcast,")
	fmt.Fprintln(w, "    and a broadcast does not cross a router.")
	fmt.Fprintln(w, "  - the game server is running: it announces itself every few seconds.")
	if len(r.Candidates) > 0 {
		fmt.Fprintln(w, "Every server heard announced an address other than the one it sent from, and the agent")
		fmt.Fprintln(w, "trusts no such announcement. Set OZONE_WS_HOST to the game server's address instead.")
	}
	if r.Interrupted {
		fmt.Fprintf(w, "Discover was stopped early: give a server at least %d s to be heard.\n", defaultDiscoverSeconds)
	}
	return exitFailure
}

// discoverOutcome says in plain words what the agent would do with what was
// heard: with a fixed OZONE_WS_HOST, whether that address was heard; and the
// choice auto mode would make with the configured OZONE_SID and OZONE_ARENA.
func discoverOutcome(r discoverReport) []string {
	var out []string
	lead := "With " + selectorText(r.LIB)
	if !strings.EqualFold(r.Host, "auto") {
		host, note := r.Host, ""
		if host == "" {
			host, note = "127.0.0.1", " (the default)"
		}
		out = append(out, fmt.Sprintf("OZONE_WS_HOST is %s%s: the agent connects to that address whatever is announced.", host, note))
		if ip, err := netip.ParseAddr(host); err == nil {
			if heard := heardAt(r, ip.Unmap()); len(heard) > 0 {
				out = append(out, "That address was heard announcing "+strings.Join(heard, ", ")+".")
			} else {
				out = append(out, "No server heard announces that address.")
			}
		}
		lead = "In auto mode (OZONE_WS_HOST=auto), with " + selectorText(r.LIB)
	}

	res := ozonelib.SelectFrom(r.Candidates, ozonelib.Selector{SID: r.LIB.SID, Arena: r.LIB.Arena})
	switch res.Outcome {
	case ozonelib.Selected:
		c := res.Selected
		out = append(out, lead+" the agent would choose "+ozonelib.Describe(c)+".")
		if r.LIB.SID == nil || r.LIB.Arena == nil {
			out = append(out, fmt.Sprintf("It would keep to that server for one run only; set OZONE_SID=%s and OZONE_ARENA=%d to make that permanent.", c.SIDRaw, c.Arena))
		}
	case ozonelib.Ambiguous:
		var servers []string
		seen := map[ozonelib.Key]bool{}
		for _, c := range res.Candidates {
			if !seen[c.Key] {
				seen[c.Key] = true
				servers = append(servers, serverText(c))
			}
		}
		out = append(out, fmt.Sprintf("%s the agent would wait: %d servers heard (%s) — set OZONE_SID and OZONE_ARENA to choose one.",
			lead, len(servers), strings.Join(servers, ", ")))
	case ozonelib.Conflict:
		var ips []string
		for _, c := range res.Candidates {
			if s := c.IP.String(); !slices.Contains(ips, s) {
				ips = append(ips, s)
			}
		}
		out = append(out, fmt.Sprintf("%s the agent would wait: %s is announced from %d addresses (%s), and it will not guess which is real — set OZONE_WS_HOST to the right one.",
			lead, serverText(res.Candidates[0]), len(ips), strings.Join(ips, ", ")))
	default:
		if slices.ContainsFunc(r.Candidates, func(c ozonelib.Candidate) bool { return c.Usable }) {
			out = append(out, lead+" the agent would wait: none of the servers heard matches.")
		} else {
			out = append(out, lead+" the agent would wait: no usable server was heard.")
		}
	}
	return out
}

// heardAt lists the servers usably announcing ip, the configured host. A
// loopback host also matches this machine's own addresses, as the agent's
// report-only listener has it.
func heardAt(r discoverReport, ip netip.Addr) []string {
	var out []string
	for _, c := range r.Candidates {
		if c.Usable && (c.IP == ip || (ip.IsLoopback() && r.Local[c.IP])) {
			out = append(out, serverText(c))
		}
	}
	return out
}

// selectorText names the configured selectors, as discover's outcome uses them.
func selectorText(l config.LIBSettings) string {
	sid, arena := "", ""
	if l.SID != nil {
		sid = "OZONE_SID=" + l.SIDRaw
	}
	if l.Arena != nil {
		arena = "OZONE_ARENA=" + strconv.Itoa(*l.Arena)
	}
	switch {
	case sid != "" && arena != "":
		return sid + " " + arena
	case sid != "":
		return sid + " (OZONE_ARENA unset)"
	case arena != "":
		return arena + " (OZONE_SID unset)"
	default:
		return "OZONE_SID and OZONE_ARENA unset"
	}
}

func serverText(c ozonelib.Candidate) string {
	return "site " + c.SIDRaw + " arena " + strconv.Itoa(c.Arena)
}
