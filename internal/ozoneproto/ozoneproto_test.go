package ozoneproto

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	payload := []byte(`{"command":"list"}`)
	framed := Frame(payload)

	if len(framed) != HeaderSize+len(payload) {
		t.Fatalf("framed length = %d, want %d", len(framed), HeaderSize+len(payload))
	}
	if framed[4] != TokenByte {
		t.Fatalf("token byte = 0x%x, want 0x28", framed[4])
	}
	// length prefix is little-endian len(payload)
	if got := int(framed[0]) | int(framed[1])<<8 | int(framed[2])<<16 | int(framed[3])<<24; got != len(payload) {
		t.Fatalf("length prefix = %d, want %d", got, len(payload))
	}

	out, err := ReadFrame(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("round-trip mismatch: got %q want %q", out, payload)
	}
}

// TORN's request format ([4-byte len]['(']+json) must decode identically,
// because '(' is 0x28. This pins the cross-implementation framing agreement.
func TestTornStyleFrameDecodes(t *testing.T) {
	json := []byte(`{"command":"list"}`)
	// Build the frame exactly as Torn5/OZone.cs does: 4-byte length then "(" + json.
	tornWire := append([]byte{byte(len(json)), 0, 0, 0, '('}, json...)
	out, err := ReadFrame(bytes.NewReader(tornWire))
	if err != nil {
		t.Fatalf("ReadFrame on TORN-style wire: %v", err)
	}
	if !bytes.Equal(out, json) {
		t.Fatalf("TORN-style decode mismatch: got %q want %q", out, json)
	}
}

func TestReadFrameRejectsBadToken(t *testing.T) {
	bad := []byte{2, 0, 0, 0, 0x29, '{', '}'}
	if _, err := ReadFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error on bad token byte")
	}
}
