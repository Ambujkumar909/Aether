package wire

import (
	"math/rand"
	"testing"
)

// Coverage-guided fuzzing of the frame decoder.
//
// These frames arrive from clients, which means from anybody who can reach the
// server. A decoder that panics on malformed input is a remote crash: Go does
// not contain a panic to one connection, it takes the process down and every
// meeting on it.
//
// TestDecoderSurvivesGarbage already existed and is worth keeping, but random
// bytes almost never form a valid header, so the overwhelming majority of its
// iterations bail out at the first check and never reach a message decoder at
// all. The two tests below fix that from both ends: the fuzzer explores by
// coverage rather than by chance, and the structured test builds a VALID
// header over a random body so the decoders are reached every single time.

// decodeEverything runs one frame through every decoder that could claim it.
// Errors are expected and fine; panics are not.
func decodeEverything(t *testing.T, b []byte) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic decoding % x: %v", b, p)
		}
	}()

	_, rx := NewPreloadedPair()
	r := NewReader(b, rx)
	h, err := r.ReadHeader()
	if err != nil {
		return
	}

	switch h.Op {
	case OpStateDelta:
		var m State
		_ = m.Decode(r)
	case OpSpeakers:
		var m Speakers
		_ = m.Decode(r)
	case OpLayerReq:
		var m LayerReq
		_ = m.Decode(r)
	case OpBweReport:
		var m BweReport
		_ = m.Decode(r)
	case OpSdp:
		var m SDP
		_ = m.Decode(r)
	case OpIce:
		var m ICE
		_ = m.Decode(r)
	case OpViewport:
		var m Viewport
		_ = m.Decode(r)
	case OpBatch:
		// Nested batches are rejected by design; the callback must still
		// never see a slice it cannot hold.
		_ = SplitBatch(r, func(f []byte) error {
			if len(f) > len(b) {
				t.Fatalf("batch produced a frame longer than its container")
			}
			return nil
		})
	default:
		// Every other opcode is read as opaque fields. Exercise the primitive
		// readers directly, since that is what those handlers do.
		_ = r.Str()
		_ = r.Str()
		_ = r.Uvarint()
		_ = r.Blob()
		_ = r.Bits(6)
		_ = r.U16()
		_ = r.Svarint()
		_ = r.Q8()
	}
	// Reading past the end must set the error flag rather than run off the
	// slice, so this is safe and is the point.
	for i := 0; i < 8; i++ {
		_ = r.U32()
		_ = r.Str()
	}
}

func FuzzDecodeFrame(f *testing.F) {
	// Seeds: real encodings, so the fuzzer starts from reachable states rather
	// than spending its budget rediscovering the header format.
	seed := func(fn func(w *Writer)) []byte {
		tx, _ := NewPreloadedPair()
		w := NewWriter(tx)
		fn(w)
		return append([]byte(nil), w.Bytes()...)
	}

	f.Add(seed(func(w *Writer) { w.WriteHeader(OpPing, 0, false) }))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpStateDelta, 0, false)
		(&State{Participant: "abc", Role: RoleMember}).Encode(w)
	}))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpSpeakers, 0, false)
		(&Speakers{List: []Speaker{{Participant: "a", Energy: 0.5}}}).Encode(w)
	}))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpSdp, 0, false)
		(&SDP{Dir: 0, IsOffer: true, Body: "v=0\r\n"}).Encode(w)
	}))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpIce, 0, false)
		(&ICE{Dir: 0, Candidate: "candidate:1 1 udp"}).Encode(w)
	}))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpViewport, 0, false)
		(&Viewport{Tiles: []Tile{{Track: "t", Width: 640, Height: 360, Visible: true}}}).Encode(w)
	}))
	f.Add(seed(func(w *Writer) {
		w.WriteHeader(OpChat, 0, false)
		w.Str("someone")
		w.Blob([]byte("hello"))
	}))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		decodeEverything(t, data)
	})
}

// A valid header over a random body, so a decoder is reached on every
// iteration rather than one in a few thousand.
func TestDecodersSurviveRandomBodies(t *testing.T) {
	ops := []Op{
		OpPing, OpPong, OpStateDelta, OpSpeakers, OpLayerReq, OpBweReport,
		OpSubscribe, OpUnsubscribe, OpKeyframeReq, OpSdp, OpIce, OpRoster,
		OpChat, OpError, OpBatch,
		OpHello, OpWelcome, OpViewport, OpConvMessage, OpTyping, OpPresence,
		OpControlState, OpControlInput, OpControlFrame,
	}

	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 60000; i++ {
		op := ops[rng.Intn(len(ops))]

		tx, _ := NewPreloadedPair()
		w := NewWriter(tx)
		w.WriteHeader(op, uint64(rng.Intn(1<<20)), rng.Intn(2) == 0)
		header := append([]byte(nil), w.Bytes()...)

		body := make([]byte, rng.Intn(64))
		rng.Read(body)

		decodeEverything(t, append(header, body...))
	}
}

// Length prefixes are the classic remote crash. A frame claiming a huge string
// or blob must be rejected against the bytes actually present, not trusted.
func TestOversizedLengthPrefixesAreRejected(t *testing.T) {
	cases := [][]byte{
		// Uvarint length far beyond the buffer, for a string.
		{0x0d, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f},
		// The same for a blob.
		{0x0e, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f},
		// A varint that never terminates.
		{0x0d, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Claims a dictionary handle nothing defined.
		{0x0d, 0x00, 0x7f},
	}
	for i, b := range cases {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("case %d: panic on % x: %v", i, b, p)
				}
			}()
			_, rx := NewPreloadedPair()
			r := NewReader(b, rx)
			if _, err := r.ReadHeader(); err != nil {
				return
			}
			s := r.Str()
			blob := r.Blob()
			// Whatever comes back, it must not be longer than the input could
			// possibly contain, and the error flag must be set.
			if len(s) > len(b) || len(blob) > len(b) {
				t.Fatalf("case %d: decoder produced %d bytes of string and %d of blob "+
					"from a %d byte frame", i, len(s), len(blob), len(b))
			}
			if r.Err() == nil && (len(s) > 0 || len(blob) > 0) {
				t.Fatalf("case %d: an oversized length prefix was accepted silently", i)
			}
		}()
	}
}

// The screen track identifier rides the state frame, but only while sharing.
//
// Gating it on Sharing is what keeps the common case at three bytes, which is
// a measured property of this protocol rather than a coincidence. A change
// that made the field unconditional would cost a byte on every state delta in
// every meeting, forever, to carry an empty string.
func TestScreenTrackRidesOnlyWhileSharing(t *testing.T) {
	roundTrip := func(in State) (State, int) {
		tx, rx := NewPreloadedPair()
		// Both identifiers are interned first, as they are in a live session:
		// the participant was named at join, and the screen track was named
		// the moment the share started. Three bytes is the steady-state size,
		// not the size of the very first frame.
		tx.Intern(in.Participant)
		rx.Intern(in.Participant)
		if in.ScreenTrack != "" {
			tx.Intern(in.ScreenTrack)
			rx.Intern(in.ScreenTrack)
		}
		w := NewWriter(tx)
		in.Encode(w)
		n := w.Len()
		r := NewReader(append([]byte(nil), w.Bytes()...), rx)
		if _, err := r.ReadHeader(); err != nil {
			t.Fatal(err)
		}
		var out State
		if err := out.Decode(r); err != nil {
			t.Fatal(err)
		}
		return out, n
	}

	quiet, quietBytes := roundTrip(State{Participant: "p1", Role: RoleMember})
	if quiet.ScreenTrack != "" {
		t.Fatalf("a participant who is not sharing decoded a screen track %q", quiet.ScreenTrack)
	}
	if quietBytes != 3 {
		t.Fatalf("a state delta is %d bytes, not 3; the field is no longer gated", quietBytes)
	}

	sharing, sharingBytes := roundTrip(State{
		Participant: "p1", Sharing: true, ScreenTrack: "track-abc", Role: RoleMember,
	})
	if sharing.ScreenTrack != "track-abc" {
		t.Fatalf("screen track round-tripped as %q", sharing.ScreenTrack)
	}
	if !sharing.Sharing {
		t.Fatal("sharing flag lost")
	}
	t.Logf("state delta: %d bytes idle, %d bytes while presenting", quietBytes, sharingBytes)
}
