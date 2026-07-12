// Package ozoneproto implements the O-Zone Print Server binary framing shared by
// the results client, the cache proxy, and the fake O-Zone server.
//
// Wire frame (both directions): [uint32 little-endian payload length][0x28][JSON].
// The 0x28 token is constant; it is also ASCII '(' (see docs/OZONE_PRINT_SERVER_API.md).
package ozoneproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// TokenByte is the fixed 5th header byte O-Zone uses on every frame.
	TokenByte = 0x28
	// HeaderSize is the 4-byte length prefix plus the 1-byte token.
	HeaderSize = 5
	// MaxPayload caps a single frame at 10 MiB as a safety bound.
	MaxPayload = 10 << 20
)

// Frame wraps a JSON payload in the 5-byte header: little-endian length + 0x28.
// Payloads are bounded by MaxPayload, so the length always fits the uint32 header
// field; an over-size payload (never expected in practice) is capped defensively
// rather than overflowing the length.
func Frame(payload []byte) []byte {
	if len(payload) > MaxPayload {
		payload = payload[:MaxPayload]
	}
	n := len(payload) // 0 <= n <= MaxPayload, so it fits in uint32
	out := make([]byte, HeaderSize+n)
	binary.LittleEndian.PutUint32(out[:4], uint32(n))
	out[4] = TokenByte
	copy(out[HeaderSize:], payload)
	return out
}

// ReadFrame reads exactly one framed message from r and returns the JSON payload.
// It validates the token byte and the length bound.
func ReadFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if header[4] != TokenByte {
		return nil, fmt.Errorf("ozoneproto: unexpected token byte 0x%x", header[4])
	}
	length := int(binary.LittleEndian.Uint32(header[:4]))
	if length <= 0 || length > MaxPayload {
		return nil, fmt.Errorf("ozoneproto: invalid payload length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
