package ozonesim

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// LIBAnnouncement is what a fake game server announces about itself on the
// LAN discovery port.
type LIBAnnouncement struct {
	IP    string // the address it says it serves on
	Arena int
	SID   string // site ID, as sent (leading zeros kept)
	Time  time.Time
}

// LIBPacket renders a as the XML document a game server broadcasts.
func LIBPacket(a LIBAnnouncement) []byte {
	t := a.Time
	if t.IsZero() {
		t = time.Date(2024, 8, 6, 12, 24, 59, 129158000, time.UTC)
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<lib>
  <server>
    <ip>%s</ip>
    <arena>%d</arena>
    <sid>%s</sid>
  </server>
  <time>
    <year>%d</year>
    <month>%d</month>
    <day>%d</day>
    <weekday>%d</weekday>
    <hour>%d</hour>
    <min>%d</min>
    <sec>%d</sec>
    <usec>%d</usec>
  </time>
</lib>
`, a.IP, a.Arena, a.SID, t.Year(), int(t.Month()), t.Day(), int(t.Weekday()), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1000))
}

// LIBSender sends announcements by unicast to Target ("host:port") from the
// local address Source (e.g. 127.0.0.1, or 127.0.0.2 to play a server that
// has moved). Real servers broadcast; unicast reaches a test listener the
// same way without needing a broadcast-capable network.
type LIBSender struct {
	Target string
	Source string

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// Send sends one announcement.
func (s *LIBSender) Send(a LIBAnnouncement) error {
	return s.SendRaw(LIBPacket(a))
}

// SendRaw sends b as one datagram.
func (s *LIBSender) SendRaw(b []byte) error {
	laddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(s.Source, "0"))
	if err != nil {
		return err
	}
	raddr, err := net.ResolveUDPAddr("udp4", s.Target)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp4", laddr, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(b)
	return err
}

// Start sends a every interval until Stop, replacing any earlier Start.
func (s *LIBSender) Start(a LIBAnnouncement, interval time.Duration) {
	s.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	stop, done := make(chan struct{}), make(chan struct{})
	s.stop, s.done = stop, done
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			_ = s.Send(a)
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends a Start. It is safe to call when nothing is running.
func (s *LIBSender) Stop() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
