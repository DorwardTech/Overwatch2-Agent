package ozonelib

import (
	"net/netip"
	"strings"
	"testing"
)

// announcement builds a packet the way the game server lays one out:
// indented, with an XML declaration, a server block and a clock.
func announcement(ip, arena, sid string) string {
	return `
   <?xml version="1.0" encoding="utf-8"?>
   <lib>
       <server>
            <ip>` + ip + `</ip>
            <arena>` + arena + `</arena>
            <sid>` + sid + `</sid>
       </server>
       <time>
            <year>2024</year>
            <month>8</month>
            <day>6</day>
            <weekday>1</weekday>
            <hour>12</hour>
            <min>24</min>
            <sec>59</sec>
            <usec>129158</usec>
       </time>
    </lib>
`
}

func TestParseReadsEveryField(t *testing.T) {
	p, err := Parse([]byte(announcement("192.0.2.10", "1", "4242")))
	if err != nil {
		t.Fatal(err)
	}
	if p.IP != netip.MustParseAddr("192.0.2.10") || p.SID != 4242 || p.SIDRaw != "4242" || p.Arena != 1 {
		t.Fatalf("Parse() = %+v", p)
	}
	want := WallClock{Year: 2024, Month: 8, Day: 6, Weekday: 1, Hour: 12, Min: 24, Sec: 59, Usec: 129158}
	if !p.HasTime || p.Time != want {
		t.Fatalf("clock = %+v (HasTime %v), want %+v", p.Time, p.HasTime, want)
	}
	if got := p.Time.Text(); got != "2024-08-06T12:24:59.129158" {
		t.Fatalf("Text() = %q", got)
	}
	if p.Key() != (Key{SID: 4242, Arena: 1}) {
		t.Fatalf("Key() = %+v", p.Key())
	}
}

func TestParseKeepsSiteIDDigits(t *testing.T) {
	p, err := Parse([]byte(announcement("192.0.2.10", "0", "0068")))
	if err != nil {
		t.Fatal(err)
	}
	if p.SID != 68 || p.SIDRaw != "0068" || p.Arena != 0 {
		t.Fatalf("Parse() = %+v, want SID 68 raw 0068 arena 0", p)
	}
}

func TestParseAcceptsACompactPacketAndUnknownElements(t *testing.T) {
	b := `<lib><server><ip>192.0.2.10</ip><arena>2</arena><sid>4242</sid><port>12113</port></server><extra/></lib>`
	p, err := Parse([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if p.HasTime || p.Arena != 2 {
		t.Fatalf("Parse() = %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	good := announcement("192.0.2.10", "1", "4242")
	cases := map[string]string{
		"empty":                "",
		"too long":             good + strings.Repeat(" ", MaxPacket),
		"doctype":              `<!DOCTYPE lib>` + good,
		"entity":               `<!doctype lib [<!ENTITY x "y">]><lib/>`,
		"latin-1 declaration":  strings.Replace(good, "utf-8", "iso-8859-1", 1),
		"us-ascii declaration": strings.Replace(good, "utf-8", "us-ascii", 1),
		"wrong root":           strings.ReplaceAll(good, "lib>", "lob>"),
		"trailing element":     good + "<lib/>",
		"trailing text":        good + "junk",
		"not xml":              "hello",
		"missing ip":           announcement("", "1", "4242"),
		"missing sid":          announcement("192.0.2.10", "1", ""),
		"missing arena":        announcement("192.0.2.10", "", "4242"),
		"unspecified ip":       announcement("0.0.0.0", "1", "4242"),
		"broadcast ip":         announcement("255.255.255.255", "1", "4242"),
		"multicast ip":         announcement("224.0.0.1", "1", "4242"),
		"ipv6 ip":              announcement("::1", "1", "4242"),
		"short ip":             announcement("1.2.3", "1", "4242"),
		"arena 256":            announcement("192.0.2.10", "256", "4242"),
		"negative arena":       announcement("192.0.2.10", "-1", "4242"),
		"sid too big":          announcement("192.0.2.10", "1", "4294967296"),
		"sid 11 digits":        announcement("192.0.2.10", "1", "00000000001"),
		"sid not digits":       announcement("192.0.2.10", "1", "42a"),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := Parse([]byte(b)); err == nil {
				t.Fatalf("Parse() accepted %+v", p)
			}
		})
	}
}

func TestParseAcceptsLoopbackAndMappedAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::ffff:192.0.2.10"} {
		p, err := Parse([]byte(announcement(ip, "1", "4242")))
		if err != nil {
			t.Fatalf("%s: %v", ip, err)
		}
		if !p.IP.Is4() {
			t.Fatalf("%s parsed to %v, want IPv4", ip, p.IP)
		}
	}
}

func TestParseToleratesABadClock(t *testing.T) {
	b := strings.Replace(announcement("192.0.2.10", "1", "4242"), "<month>8</month>", "<month>13</month>", 1)
	p, err := Parse([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if p.HasTime || p.Time != (WallClock{}) {
		t.Fatalf("a month of 13 gave HasTime=%v, Time=%+v", p.HasTime, p.Time)
	}
}

func TestParseSIDAndArena(t *testing.T) {
	if n, raw, err := ParseSID(" 4294967295 "); err != nil || n != 4294967295 || raw != "4294967295" {
		t.Fatalf("ParseSID(max) = %d, %q, %v", n, raw, err)
	}
	if n, err := ParseArena("255"); err != nil || n != 255 {
		t.Fatalf("ParseArena(255) = %d, %v", n, err)
	}
}

// Whatever arrives, Parse never panics, and what it accepts holds up.
func FuzzParse(f *testing.F) {
	f.Add([]byte(announcement("192.0.2.10", "1", "4242")))
	f.Add([]byte(`<lib><server><ip>127.0.0.1</ip><arena>0</arena><sid>0</sid></server></lib>`))
	f.Add([]byte(`<lib/>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Parse(b)
		if err != nil {
			return
		}
		if !p.IP.Is4() || p.IP.IsUnspecified() || p.IP.IsMulticast() {
			t.Fatalf("accepted address %v", p.IP)
		}
		if p.Arena < 0 || p.Arena > 255 || p.SIDRaw == "" {
			t.Fatalf("accepted %+v", p)
		}
		if len(b) > MaxPacket {
			t.Fatalf("accepted %d bytes", len(b))
		}
	})
}
