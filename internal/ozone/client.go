// Package ozone is a minimal WebSocket client for the O-Zone read-only API.
package ozone

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// maxFrame caps a single websocket message. The agent runs in a 128 MB
// container and gorilla reads a frame into memory before returning it, so
// without a limit one oversized message from a compromised (or impersonated)
// O-Zone host is enough to OOM-kill the agent — which skips the graceful spill
// and drops whatever telemetry was buffered. Real responses are well under a
// megabyte; 10 MiB matches the bound the print-server framing already applies.
const maxFrame = 10 << 20

// maxForeignFrames bounds how many frames answering other commands one call
// discards before giving up. The reply timeout bounds the wait as well; this
// bounds the work if something floods the connection inside it.
const maxForeignFrames = 64

// frameQueue and frameQueueBytes bound what the reader holds for calls to
// take. Past either bound the new frame is dropped and counted lost, so memory
// stays bounded although one frame may be maxFrame bytes. The reader never
// waits for room. A reader that stopped would leave O-Zone blocked writing to
// this client and its pings unanswered; worse, it would stamp what piled up in
// the socket meanwhile only when it got to it, so a frame sent before a request
// could pass for its reply. The queue is deep enough that a call taking frames
// as they come does not fall behind; a frame dropped while a caller is judging
// its calls shows up as lost in TakeDiscarded, and the caller fails closed.
const (
	frameQueue      = 64
	frameQueueBytes = 8 << 20
)

// replyTimeout is how long Command waits for its reply. A variable so tests
// need not wait it out.
var replyTimeout = 10 * time.Second

// maxDiscarded bounds how many discarded frames are kept for TakeDiscarded.
const maxDiscarded = 32

type frame struct {
	data []byte
	at   time.Time
}

type Client struct {
	conn    *websocket.Conn
	frames  chan frame
	done    chan struct{} // closed by Close
	closing sync.Once
	queued  atomic.Int64 // bytes held in frames

	// dead is closed when the reader stops; deadErr, written before, says why.
	// Every call after that fails at once, as a websocket's read error does.
	dead    chan struct{}
	deadErr error

	// foreign counts frames that arrived after a request but answered another
	// command; stale, frames that arrived before the request they could have
	// been taken for; overflow, frames the reader dropped with the queue full.
	foreign  atomic.Int64
	stale    atomic.Int64
	overflow atomic.Int64

	// discarded holds the frames calls have dropped since TakeDiscarded,
	// decoded; lost counts those dropped without being kept. Only calls and
	// TakeDiscarded touch discarded, from the one goroutine making them; the
	// reader adds to lost.
	discarded []map[string]any
	lost      atomic.Int64

	// observer, if set, is shown every frame as it arrives; see Observe.
	observer atomic.Pointer[func(map[string]any)]
}

// Dial connects to the O-Zone WebSocket server.
func Dial(host, port string) (*Client, error) {
	u := url.URL{Scheme: "ws", Host: host + ":" + port, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxFrame)
	c := &Client{
		conn:   conn,
		frames: make(chan frame, frameQueue),
		done:   make(chan struct{}),
		dead:   make(chan struct{}),
	}
	go c.read()
	return c, nil
}

// read is the connection's only reader for its whole life. It stamps every
// frame with the moment it arrived, which is what lets Command tell its reply
// from a frame that was already waiting when the request went out; it never
// stops reading, so the stamp is when the frame came in. Reading continuously
// also answers O-Zone's pings.
func (c *Client) read() {
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			c.deadErr = err
			close(c.dead)
			return
		}
		f := frame{data: msg, at: time.Now()}
		if fn := c.observer.Load(); fn != nil {
			var resp map[string]any
			if json.Unmarshal(msg, &resp) == nil {
				(*fn)(resp)
			}
		}

		size := int64(len(msg))
		if !c.admit(size) {
			c.overflow.Add(1)
			c.lost.Add(1)
			continue
		}
		c.queued.Add(size)
		select {
		case c.frames <- f:
		case <-c.done:
			return
		}
	}
}

// Observe has the reader show fn every frame that decodes, as it arrives and
// before anything else is done with it: a frame a call takes as its reply, one
// a call drops, one dropped with the queue full, and one no call ever takes,
// alike. A caller that must act on some frames wherever they turn up (a server
// state saying a game is on) need not wait for a call to reach them. fn runs on
// the reader's goroutine, alongside the caller's calls: it must be quick, and
// safe to run concurrently with them. It gets its own decoded copy.
func (c *Client) Observe(fn func(map[string]any)) { c.observer.Store(&fn) }

// admit reports whether the reader may queue a frame of size bytes. There is
// always room in an empty queue, so a reply bigger than the byte bound still
// gets through. Only the reader sends, so room found here is still there at
// the send.
func (c *Client) admit(size int64) bool {
	n := len(c.frames)
	return n == 0 || (n < cap(c.frames) && c.queued.Load()+size <= frameQueueBytes)
}

func (c *Client) take(f frame) frame {
	c.queued.Add(-int64(len(f.data)))
	return f
}

// errNoFrame is what next returns when its timer fires first.
var errNoFrame = errors.New("no frame")

// next returns the next queued frame. A frame already queued is returned even
// if the connection has since closed, so a reply that beat the close is kept.
func (c *Client) next(timer <-chan time.Time) (frame, error) {
	select {
	case f := <-c.frames:
		return c.take(f), nil
	default:
	}
	select {
	case f := <-c.frames:
		return c.take(f), nil
	case <-c.dead:
		select {
		case f := <-c.frames:
			return c.take(f), nil
		default:
		}
		return frame{}, c.deadErr
	case <-timer:
		return frame{}, errNoFrame
	}
}

// keep records a dropped frame for TakeDiscarded.
func (c *Client) keep(data []byte) {
	var resp map[string]any
	if len(c.discarded) >= maxDiscarded || json.Unmarshal(data, &resp) != nil {
		c.lost.Add(1)
		return
	}
	c.discarded = append(c.discarded, resp)
}

// Command sends a read-only command and returns O-Zone's reply to it.
//
// A reply has to arrive after the request is written and echo the command as
// CMD; a frame without a CMD is accepted as before. Earlier versions took the
// next frame off the socket, whatever it answered and whenever it had come.
// With several clients on one O-Zone, frames arrive that answer other commands
// or that were sent before the request, and taking one shifted every reply
// after it: at one venue about half the pushes went out carrying a
// GETACTIVEPACKS, GETGAMELIST or GETTEAMINFO reply as the server state, and no
// packs. Matching the echo alone was not enough. A GETSERVERSTATE frame left
// waiting since the last poll matches too, and a stale "idle" taken as current
// could clear an active game and open the print-server gate.
//
// Frames dropped on the way are kept for TakeDiscarded.
//
// Not safe for concurrent use: one call at a time, as the poll loop makes them.
func (c *Client) Command(cmd string) (map[string]any, error) {
	// Everything received so far came before this request, so none of it is
	// the reply.
drain:
	for {
		select {
		case f := <-c.frames:
			c.take(f)
			c.stale.Add(1)
			c.keep(f.data)
		default:
			break drain
		}
	}
	select {
	case <-c.dead:
		return nil, c.deadErr
	default:
	}

	sent := time.Now()
	_ = c.conn.SetWriteDeadline(sent.Add(5 * time.Second))
	if err := c.conn.WriteJSON(map[string]string{"CMD": cmd}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(replyTimeout)
	defer timer.Stop()
	for foreign := 0; ; {
		f, err := c.next(timer.C)
		if errors.Is(err, errNoFrame) {
			return nil, fmt.Errorf("%s: no reply within %s", cmd, replyTimeout)
		}
		if err != nil {
			return nil, err
		}
		if f.at.Before(sent) {
			c.stale.Add(1)
			c.keep(f.data)
			continue
		}

		var resp map[string]any
		if err := json.Unmarshal(f.data, &resp); err != nil {
			return nil, fmt.Errorf("decode %s: %w", cmd, err)
		}
		if Answers(resp, cmd) {
			return resp, nil
		}

		c.foreign.Add(1)
		if len(c.discarded) < maxDiscarded {
			c.discarded = append(c.discarded, resp)
		} else {
			c.lost.Add(1)
		}
		if foreign++; foreign >= maxForeignFrames {
			return nil, fmt.Errorf("%s: no reply among %d frames answering other commands", cmd, foreign)
		}
	}
}

// TakeDiscarded returns the frames calls have dropped since the last call to
// it (stale or answering another command), decoded, and how many more were
// dropped without being kept (the queue was full, or they did not decode).
// It starts the record afresh. The poll loop uses it to judge whether a poll's
// replies were in step; it must be called from the goroutine making calls.
func (c *Client) TakeDiscarded() ([]map[string]any, int64) {
	frames := c.discarded
	c.discarded = nil
	return frames, c.lost.Swap(0)
}

// Flush forgets everything received so far and starts the TakeDiscarded record
// afresh: what came before a caller's next request says nothing about it. The
// poll loop flushes before each poll, so a backlog from between polls cannot
// count against the poll as frames it lost. Like TakeDiscarded, it must be
// called from the goroutine making calls.
func (c *Client) Flush() {
	for {
		select {
		case f := <-c.frames:
			c.take(f)
			c.stale.Add(1)
		default:
			c.discarded = nil
			c.lost.Store(0)
			return
		}
	}
}

// DropDiscarded forgets the frames calls have dropped so far but keeps the
// count of those lost unseen: a frame kept so far can be judged now and set
// aside, but a lost one could have been anything, so the loss still counts
// at the next TakeDiscarded. Like TakeDiscarded, it must be called from the
// goroutine making calls.
func (c *Client) DropDiscarded() { c.discarded = nil }

// ForeignFrames reports how many frames this connection has discarded because
// they arrived after a request but answered a different command.
func (c *Client) ForeignFrames() int64 { return c.foreign.Load() }

// StaleFrames reports how many frames this connection has discarded because
// they arrived before the request they might have been taken for.
func (c *Client) StaleFrames() int64 { return c.stale.Load() }

// OverflowFrames reports how many frames the reader has dropped because the
// queue was full.
func (c *Client) OverflowFrames() int64 { return c.overflow.Load() }

// Answers reports whether resp is a reply to cmd, judged by the CMD O-Zone
// echoes back: the same command in any case, or no echo at all.
func Answers(resp map[string]any, cmd string) bool {
	echo, ok := resp["CMD"]
	if !ok || echo == nil {
		return true
	}
	s, ok := echo.(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), cmd)
}

func (c *Client) Close() {
	if c.done != nil {
		c.closing.Do(func() { close(c.done) })
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
