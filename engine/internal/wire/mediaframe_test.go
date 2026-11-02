package wire

import (
	"bytes"
	"testing"
)

// The media-over-WebSocket frame, round-tripped through the real encoder and
// decoder. The layout has to match web/src/wire.js bit for bit; the opcode
// parity test guards the opcode, this guards the body. A one-bit slip here is
// every fallback frame decoding as garbage with no error anywhere.
//
// Layout (server to client; client to server has no publisher):
//
//	header | publisher (interned) | kind:1 key:1 spare:6 | [ts uvarint, video only] | payload to the end
//
// Audio has no timestamp (chunks are contiguous; the receiver counts them)
// and no payload length (the frame's end is the payload's end), and the
// opcode is a one-byte one. On a 120 B Opus frame that is 5 bytes of 127,
// every 20 ms, in both directions.
func TestMediaFrameRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab, 0xcd}, 700) // a plausible P-frame

	cases := []struct {
		name string
		kind uint32
		key  uint32
		ts   uint64
	}{
		{"video key", 1, 1, 1_000_000},
		{"video delta", 1, 0, 1_033_333},
		{"audio", 0, 1, 0},
		{"zero ts", 1, 1, 0},
		{"large ts", 1, 0, 1 << 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := NewWriter(nil)
			w.WriteHeader(OpMediaFrame, 0, false)
			w.Str("publisher-42")
			w.Bits(c.kind, 1)
			w.Bits(c.key, 1)
			w.Bits(0, 6)
			if c.kind == 1 {
				w.Uvarint(c.ts)
			}
			w.Raw(payload)

			r := NewReader(w.Bytes(), nil)
			h, err := r.ReadHeader()
			if err != nil || h.Op != OpMediaFrame {
				t.Fatalf("header: %v op=%v", err, h.Op)
			}
			if got := r.Str(); got != "publisher-42" {
				t.Fatalf("publisher = %q", got)
			}
			if got := r.Bits(1); got != c.kind {
				t.Fatalf("kind = %d, want %d", got, c.kind)
			}
			if got := r.Bits(1); got != c.key {
				t.Fatalf("key = %d, want %d", got, c.key)
			}
			r.Bits(6)
			if c.kind == 1 {
				if got := r.Uvarint(); got != c.ts {
					t.Fatalf("ts = %d, want %d", got, c.ts)
				}
			}
			if got := r.Tail(); !bytes.Equal(got, payload) {
				t.Fatalf("payload mismatch: %d bytes, want %d", len(got), len(payload))
			}
			if err := r.Err(); err != nil {
				t.Fatalf("reader error: %v", err)
			}
			if r.Left() != 0 {
				t.Fatalf("%d bytes left over", r.Left())
			}
		})
	}
}

// An audio frame client to server is the one-byte header, one byte of flags
// and the Opus bytes: nothing else. Counted, so a field cannot creep back in.
func TestAudioFrameIsHeaderFlagsAndPayloadOnly(t *testing.T) {
	opus := bytes.Repeat([]byte{0x5a}, 120) // 48 kbit/s x 20 ms
	w := NewWriter(nil)
	w.WriteHeader(OpMediaFrame, 0, false)
	w.Bits(0, 1)
	w.Bits(1, 1)
	w.Bits(0, 6)
	w.Raw(opus)
	if n := len(w.Bytes()); n != len(opus)+2 {
		t.Fatalf("audio frame is %d bytes, want payload+2 (%d)", n, len(opus)+2)
	}
	// And a video frame carries its timestamp, and only that more.
	w = NewWriter(nil)
	w.WriteHeader(OpMediaFrame, 0, false)
	w.Bits(1, 1)
	w.Bits(1, 1)
	w.Bits(0, 6)
	w.Uvarint(1_000_000) // 3 bytes
	w.Raw(opus)
	if n := len(w.Bytes()); n != len(opus)+2+3 {
		t.Fatalf("video frame is %d bytes, want payload+5 (%d)", n, len(opus)+5)
	}
}

func TestMediaFrameCarriesAKeyframe(t *testing.T) {
	big := bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, 30_000) // 120 KB
	w := NewWriter(nil)
	w.WriteHeader(OpMediaFrame, 0, false)
	w.Str("p")
	w.Bits(1, 1)
	w.Bits(1, 1)
	w.Bits(0, 6)
	w.Uvarint(42)
	w.Raw(big)
	r := NewReader(w.Bytes(), nil)
	if _, err := r.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	r.Str()
	r.Bits(8)
	r.Uvarint()
	if got := r.Tail(); !bytes.Equal(got, big) || r.Err() != nil {
		t.Fatalf("a 120 KB keyframe did not survive: %d bytes, err %v", len(got), r.Err())
	}
}
