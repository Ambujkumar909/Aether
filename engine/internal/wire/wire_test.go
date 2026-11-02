package wire

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"testing"
)

// The size tests are the point of this package. They are written as exact
// equalities, not upper bounds, so that any future change which silently
// inflates a hot-path frame fails the build instead of quietly costing every
// user bandwidth for the rest of the product's life.

const uuid = "7f3a9c21-4b6e-4d8a-9c1f-2e5b8a7d3f60"

func TestStateDeltaIsThreeBytes(t *testing.T) {
	tx, rx := NewPreloadedPair()
	// Simulate the participant having been named once already, at join.
	tx.Intern(uuid)
	rx.Intern(uuid)

	w := NewWriter(tx)
	s := State{Participant: uuid, MicMuted: true, Role: RoleMember}
	s.Encode(w)
	got := w.Bytes()

	if len(got) != 3 {
		t.Fatalf("state delta is %d bytes, want exactly 3: % x", len(got), got)
	}

	// The JSON this replaces, for the record.
	j, _ := json.Marshal(map[string]any{
		"type": "participant_update", "participantId": uuid,
		"audioMuted": true, "videoMuted": false, "handRaised": false,
		"sharing": false, "role": "member",
	})
	t.Logf("aether=%d bytes  json=%d bytes  ratio=%.1fx",
		len(got), len(j), float64(len(j))/float64(len(got)))

	var back State
	r := NewReader(got, rx)
	if _, err := r.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	if err := back.Decode(r); err != nil {
		t.Fatal(err)
	}
	if back != s {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, s)
	}
}

func TestPingIsOneByte(t *testing.T) {
	w := NewWriter(nil)
	EncodePing(w)
	if n := len(w.Bytes()); n != 1 {
		t.Fatalf("ping is %d bytes, want exactly 1", n)
	}
}

func TestLayerReqIsThreeBytes(t *testing.T) {
	tx, _ := NewPreloadedPair()
	tx.Intern("track-cam-7f3a")
	w := NewWriter(tx)
	l := LayerReq{Track: "track-cam-7f3a", Spatial: 2, Temporal: 1, Reason: ReasonViewport}
	l.Encode(w)
	if n := len(w.Bytes()); n != 3 {
		t.Fatalf("layer request is %d bytes, want exactly 3: % x", n, w.Bytes())
	}
}

func TestSpeakersFourEntriesIsTenBytes(t *testing.T) {
	tx, _ := NewPreloadedPair()
	ids := []string{"p-alpha", "p-bravo", "p-charlie", "p-delta"}
	for _, id := range ids {
		tx.Intern(id)
	}
	w := NewWriter(tx)
	s := Speakers{}
	for i, id := range ids {
		s.List = append(s.List, Speaker{Participant: id, Energy: float64(i) / 4})
	}
	s.Encode(w)
	if n := len(w.Bytes()); n != 10 {
		t.Fatalf("4-speaker report is %d bytes, want exactly 10: % x", n, w.Bytes())
	}
}

// Interning must actually pay off, not just be present.
func TestInterningCollapsesRepeatedIdentifiers(t *testing.T) {
	tx, _ := NewPreloadedPair()
	w := NewWriter(tx)
	for i := 0; i < 100; i++ {
		s := State{Participant: uuid, MicMuted: i%2 == 0}
		s.Encode(w)
	}
	got := len(w.Bytes())
	// First frame pays the 36-byte literal plus a length byte; the other 99
	// pay one byte for the handle. 1+1+36+1 + 99*3 = 336.
	if want := 39 + 99*3; got != want {
		t.Fatalf("100 state deltas = %d bytes, want %d", got, want)
	}
	t.Logf("100 deltas: %d bytes (%.2f bytes/delta amortised)", got, float64(got)/100)
}

// The dictionary desync hazard documented in dict.go: two independent
// directions must never share a table. This test encodes the invariant.
func TestDirectionalDictionariesDoNotDesync(t *testing.T) {
	// Client's view.
	cTx, cRx := NewPreloadedPair()
	// Server's view: its Rx mirrors the client's Tx, and vice versa.
	sTx, sRx := NewPreloadedPair()

	// Both sides transmit a distinct new string "simultaneously".
	cw := NewWriter(cTx)
	(&State{Participant: "client-side-id"}).Encode(cw)
	sw := NewWriter(sTx)
	(&State{Participant: "server-side-id"}).Encode(sw)

	// Each decodes the other's frame into its own Rx table.
	cr := NewReader(sw.Bytes(), cRx)
	if _, err := cr.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	var a State
	if err := a.Decode(cr); err != nil {
		t.Fatal(err)
	}
	sr := NewReader(cw.Bytes(), sRx)
	if _, err := sr.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	var b State
	if err := b.Decode(sr); err != nil {
		t.Fatal(err)
	}

	if a.Participant != "server-side-id" || b.Participant != "client-side-id" {
		t.Fatalf("crossed frames desynced: got %q and %q", a.Participant, b.Participant)
	}
	// Handles for the two strings collide numerically -- both are the next
	// index in their respective tables -- which is exactly why the tables
	// must be separate.
	ch, _ := cTx.Handle("client-side-id")
	sh, _ := sTx.Handle("server-side-id")
	if ch != sh {
		t.Fatalf("expected colliding handles %d/%d, which is the hazard being guarded", ch, sh)
	}
}

func TestBatchRoundTrip(t *testing.T) {
	tx, rx := NewPreloadedPair()
	tx.Intern(uuid)
	rx.Intern(uuid)

	var frames [][]byte
	for i := 0; i < 3; i++ {
		w := NewWriter(tx)
		(&State{Participant: uuid, MicMuted: i%2 == 0}).Encode(w)
		frames = append(frames, append([]byte(nil), w.Bytes()...))
	}

	out := NewWriter(nil)
	if err := EncodeBatch(out, frames); err != nil {
		t.Fatal(err)
	}
	// 1 control byte + 3 * (1 length byte + 3 frame bytes) = 13.
	if n := len(out.Bytes()); n != 13 {
		t.Fatalf("batch of 3 deltas is %d bytes, want 13", n)
	}

	r := NewReader(out.Bytes(), rx)
	h, err := r.ReadHeader()
	if err != nil || h.Op != OpBatch {
		t.Fatalf("bad batch header: %v %v", h, err)
	}
	n := 0
	err = SplitBatch(r, func(f []byte) error {
		n++
		fr := NewReader(f, rx)
		if _, err := fr.ReadHeader(); err != nil {
			return err
		}
		var s State
		return s.Decode(fr)
	})
	if err != nil || n != 3 {
		t.Fatalf("split yielded %d frames, err=%v", n, err)
	}
}

func TestBatchRejectsNesting(t *testing.T) {
	inner := NewWriter(nil)
	inner.WriteHeader(OpBatch, 0, false)
	out := NewWriter(nil)
	err := EncodeBatch(out, [][]byte{inner.Bytes(), inner.Bytes()})
	if err != ErrNestedBatch {
		t.Fatalf("nested batch accepted, got err=%v", err)
	}
}

// A decoder must never panic on arbitrary input. This is the entire attack
// surface of an unauthenticated signalling endpoint.
func TestDecoderSurvivesGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(48))
		rng.Read(b)
		_, rx := NewPreloadedPair()
		r := NewReader(b, rx)
		h, err := r.ReadHeader()
		if err != nil {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic decoding % x: %v", b, p)
				}
			}()
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
			case OpBatch:
				_ = SplitBatch(r, func([]byte) error { return nil })
			}
		}()
	}
}

func TestVarintRoundTrip(t *testing.T) {
	vals := []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 40, ^uint64(0)}
	for _, v := range vals {
		w := NewWriter(nil)
		w.Uvarint(v)
		if got := UvarintSize(v); got != len(w.Bytes()) {
			t.Fatalf("UvarintSize(%d)=%d but encoded %d", v, got, len(w.Bytes()))
		}
		r := NewReader(w.Bytes(), nil)
		if back := r.Uvarint(); back != v {
			t.Fatalf("uvarint %d round tripped to %d", v, back)
		}
	}
	for _, v := range []int64{0, -1, 1, -300, 300, 1 << 40, -(1 << 40)} {
		if back := UnZigZag(ZigZag(v)); back != v {
			t.Fatalf("zigzag %d -> %d", v, back)
		}
	}
}

func TestBitPackingIsExact(t *testing.T) {
	w := NewWriter(nil)
	for i := 0; i < 8; i++ {
		w.Bool(i%2 == 0)
	}
	if got := w.Bytes(); !bytes.Equal(got, []byte{0b10101010}) {
		t.Fatalf("8 bools packed to % x, want aa", got)
	}
	r := NewReader([]byte{0b10101010}, nil)
	for i := 0; i < 8; i++ {
		if r.Bool() != (i%2 == 0) {
			t.Fatalf("bit %d mismatch", i)
		}
	}
}
