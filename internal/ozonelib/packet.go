// Package ozonelib listens for the game server's LAN announcement: a small
// XML document it broadcasts over UDP every few seconds, naming its IPv4
// address, its site ID and its arena, with its own wall clock. The agent uses
// it to find the server without a configured address, and to report the
// server's identity and clock to central.
//
// Nothing here dials anything. A packet is only ever read, checked and
// recorded; the caller decides what, if anything, to connect to.
package ozonelib

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// DefaultPort is the UDP port the announcement is broadcast to.
const DefaultPort = 12106

// MaxPacket bounds an announcement. Real ones are a few hundred bytes; a
// larger datagram is rejected unread rather than parsed.
const MaxPacket = 2048

// WallClock is the game server's own clock as it announced it. It carries no
// time zone, so it is reported as given and never converted.
type WallClock struct {
	Year, Month, Day, Weekday, Hour, Min, Sec, Usec int
}

// Text renders the clock as a zone-less ISO 8601 timestamp,
// e.g. 2024-08-06T12:24:59.129158.
func (w WallClock) Text() string {
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%06d", w.Year, w.Month, w.Day, w.Hour, w.Min, w.Sec, w.Usec)
}

// Packet is one parsed announcement.
type Packet struct {
	IP      netip.Addr // the address the server says it serves on (IPv4)
	SID     uint32     // site ID, compared numerically
	SIDRaw  string     // site ID as sent, for reports (leading zeros kept)
	Arena   int        // arena number, compared as sent (no 0/1-based shift)
	Time    WallClock  // the server's clock, valid only if HasTime
	HasTime bool
}

// Key identifies one game server: a site and an arena.
type Key struct {
	SID   uint32
	Arena int
}

// Key returns the packet's identity.
func (p Packet) Key() Key { return Key{SID: p.SID, Arena: p.Arena} }

// The decode targets carry explicit lower-case tags: an untagged field would
// silently decode to its zero value instead of failing.
type xmlTime struct {
	Year    string `xml:"year"`
	Month   string `xml:"month"`
	Day     string `xml:"day"`
	Weekday string `xml:"weekday"`
	Hour    string `xml:"hour"`
	Min     string `xml:"min"`
	Sec     string `xml:"sec"`
	Usec    string `xml:"usec"`
}

type xmlLib struct {
	XMLName xml.Name `xml:"lib"`
	Server  struct {
		IP    string `xml:"ip"`
		Arena string `xml:"arena"`
		SID   string `xml:"sid"`
	} `xml:"server"`
	Time *xmlTime `xml:"time"`
}

var errNoDTD = errors.New("document type declarations are not accepted")

// Parse decodes and checks one announcement. A packet that fails any check is
// rejected whole; only a malformed clock is tolerated, as HasTime=false.
func Parse(b []byte) (Packet, error) {
	if len(b) == 0 {
		return Packet{}, errors.New("empty packet")
	}
	if len(b) > MaxPacket {
		return Packet{}, fmt.Errorf("packet of %d bytes exceeds %d", len(b), MaxPacket)
	}
	b = bytes.TrimSpace(b)
	lower := bytes.ToLower(b)
	if bytes.Contains(lower, []byte("<!doctype")) || bytes.Contains(lower, []byte("<!entity")) {
		return Packet{}, errNoDTD
	}

	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true // and no CharsetReader: anything but UTF-8 is an error
	var doc xmlLib
	if err := d.Decode(&doc); err != nil {
		return Packet{}, fmt.Errorf("decode: %w", err)
	}
	// After the document, only whitespace, comments and processing
	// instructions may follow.
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Packet{}, fmt.Errorf("after document: %w", err)
		}
		switch t := tok.(type) {
		case xml.Comment, xml.ProcInst:
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return Packet{}, errors.New("text after the document")
			}
		default:
			return Packet{}, errors.New("content after the document")
		}
	}

	var p Packet
	var err error
	if p.IP, err = parseIP(doc.Server.IP); err != nil {
		return Packet{}, err
	}
	if p.SID, p.SIDRaw, err = ParseSID(doc.Server.SID); err != nil {
		return Packet{}, err
	}
	if p.Arena, err = ParseArena(doc.Server.Arena); err != nil {
		return Packet{}, err
	}
	if doc.Time != nil {
		p.Time, p.HasTime = parseClock(doc.Time)
	}
	return p, nil
}

func parseIP(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("ip %q: %w", s, err)
	}
	a = a.Unmap()
	switch {
	case !a.Is4():
		return netip.Addr{}, fmt.Errorf("ip %q is not IPv4", s)
	case a.IsUnspecified(), a.IsMulticast(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return netip.Addr{}, fmt.Errorf("ip %q is not a host address", s)
	}
	return a, nil
}

// ParseSID reads a site ID: 1 to 10 digits, at most 4294967295. It returns the
// number, for comparing ("0068" equals 68), and the digits as given.
func ParseSID(s string) (uint32, string, error) {
	raw := strings.TrimSpace(s)
	if raw == "" || len(raw) > 10 || strings.Trim(raw, "0123456789") != "" {
		return 0, "", fmt.Errorf("site ID %q is not 1-10 digits", s)
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, "", fmt.Errorf("site ID %q is out of range", s)
	}
	return uint32(n), raw, nil
}

// ParseArena reads an arena number, 0 to 255.
func ParseArena(s string) (int, error) {
	raw := strings.TrimSpace(s)
	if raw == "" || len(raw) > 3 || strings.Trim(raw, "0123456789") != "" {
		return 0, fmt.Errorf("arena %q is not 0-255", s)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n > 255 {
		return 0, fmt.Errorf("arena %q is not 0-255", s)
	}
	return n, nil
}

// parseClock reads the clock fields; any missing or out-of-range field makes
// the whole clock invalid.
func parseClock(t *xmlTime) (WallClock, bool) {
	var w WallClock
	ok := true
	field := func(s string, lo, hi int, dst *int) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < lo || n > hi {
			ok = false
			return
		}
		*dst = n
	}
	field(t.Year, 1, 9999, &w.Year)
	field(t.Month, 1, 12, &w.Month)
	field(t.Day, 1, 31, &w.Day)
	field(t.Weekday, 0, 7, &w.Weekday)
	field(t.Hour, 0, 23, &w.Hour)
	field(t.Min, 0, 59, &w.Min)
	field(t.Sec, 0, 60, &w.Sec)
	field(t.Usec, 0, 999999, &w.Usec)
	if !ok {
		return WallClock{}, false
	}
	return w, true
}
