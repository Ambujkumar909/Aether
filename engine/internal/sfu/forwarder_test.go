package sfu

import (
	"testing"

	"github.com/pion/rtp"
)

// A synthetic publisher for one layer, producing a well-formed RTP stream.
type source struct {
	seq  uint16
	ts   uint32
	ssrc uint32
}

func newSource(seed uint16, ssrc uint32) *source {
	return &source{seq: seed, ts: uint32(seed) * 1000, ssrc: ssrc}
}

// frame emits the packets of one frame. RTP marks the last packet of a frame,
// and all packets in a frame share a timestamp.
func (s *source) frame(pkts int) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, pkts)
	for i := 0; i < pkts; i++ {
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SSRC:           s.ssrc,
				SequenceNumber: s.seq,
				Timestamp:      s.ts,
				Marker:         i == pkts-1,
			},
			Payload: []byte{byte(i)},
		}
		out = append(out, p)
		s.seq++
	}
	s.ts += 3000 // one frame at 30 fps on a 90 kHz clock
	return out
}

// skip simulates upstream loss.
func (s *source) skip(n uint16) { s.seq += n }

// A receiver's view of what arrived, which is the only thing that matters.
type sink struct {
	seqs []uint16
	tss  []uint32
}

func (k *sink) add(p *rtp.Packet) {
	k.seqs = append(k.seqs, p.SequenceNumber)
	k.tss = append(k.tss, p.Timestamp)
}

// gaps reports every discontinuity in the received sequence numbers.
func (k *sink) gaps() []int {
	var g []int
	for i := 1; i < len(k.seqs); i++ {
		d := int(k.seqs[i] - k.seqs[i-1])
		if d != 1 {
			g = append(g, d)
		}
	}
	return g
}

func TestNeverStartsOnADeltaFrame(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(1000, 111)

	// Delta frames before any keyframe must be discarded: the decoder has no
	// reference and would produce garbage.
	for _, p := range src.frame(3) {
		if d := f.WriteRTP(LayerFull, p, false); d != Drop {
			t.Fatal("forwarded a delta frame before any keyframe")
		}
	}
	for _, p := range src.frame(3) {
		if d := f.WriteRTP(LayerFull, p, true); d != Forward {
			t.Fatal("dropped the first keyframe")
		}
	}
}

// With a clean inbound stream the receiver must see a perfectly contiguous
// sequence space.
func TestContiguousOutputForCleanInput(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(60000, 111) // near the 16-bit wrap, deliberately
	k := &sink{}

	for i := 0; i < 40; i++ {
		for _, p := range src.frame(4) {
			if f.WriteRTP(LayerFull, p, i == 0) == Forward {
				k.add(p)
			}
		}
	}
	if g := k.gaps(); len(g) != 0 {
		t.Fatalf("receiver saw %d discontinuities in a clean stream: %v", len(g), g)
	}
	if k.seqs[0] != 0 {
		t.Fatalf("outbound space started at %d, not 0", k.seqs[0])
	}
	// Timestamps must be non-decreasing and advance one frame per frame.
	for i := 1; i < len(k.tss); i++ {
		if k.tss[i] < k.tss[i-1] {
			t.Fatalf("timestamp went backwards at %d: %d -> %d", i, k.tss[i-1], k.tss[i])
		}
	}
}

// Genuine upstream loss must remain visible downstream, because the
// receiver's concealment and NACK logic depends on seeing the gap.
func TestUpstreamLossStaysVisible(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(500, 111)
	k := &sink{}

	for _, p := range src.frame(3) {
		if f.WriteRTP(LayerFull, p, true) == Forward {
			k.add(p)
		}
	}
	src.skip(5) // five packets lost upstream
	for _, p := range src.frame(3) {
		if f.WriteRTP(LayerFull, p, false) == Forward {
			k.add(p)
		}
	}

	g := k.gaps()
	if len(g) != 1 || g[0] != 6 {
		t.Fatalf("expected exactly one gap of 6, got %v", g)
	}
}

// The headline property: a layer switch must be invisible in the sequence
// space. This is the test that a naive forwarder fails catastrophically.
func TestLayerSwitchIsInvisibleToTheReceiver(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)

	// The two layers are independent encodes with unrelated sequence and
	// timestamp origins -- which is exactly the real situation, and exactly
	// what makes naive relaying produce a multi-thousand-packet apparent gap.
	full := newSource(100, 111)
	half := newSource(55000, 222)
	k := &sink{}

	push := func(layer LayerID, pkts []*rtp.Packet, kf bool) {
		for _, p := range pkts {
			if f.WriteRTP(layer, p, kf) == Forward {
				k.add(p)
			}
		}
	}

	// Run on the full layer while the half layer also publishes.
	push(LayerFull, full.frame(4), true)
	for i := 0; i < 10; i++ {
		push(LayerFull, full.frame(4), false)
		push(LayerHalf, half.frame(2), false)
	}
	beforeSwitch := len(k.seqs)

	// Ask for the lower layer. Nothing should change until its keyframe.
	needKF := f.Select(LayerHalf)
	if !needKF {
		t.Fatal("a layer switch must request a keyframe")
	}
	for i := 0; i < 5; i++ {
		push(LayerFull, full.frame(4), false)
		push(LayerHalf, half.frame(2), false) // delta frames: not yet
	}
	if f.Current() != LayerFull {
		t.Fatal("switched before the target layer produced a keyframe")
	}
	if len(k.seqs) == beforeSwitch {
		t.Fatal("stopped forwarding while waiting for a keyframe; the receiver would freeze")
	}

	// The keyframe arrives and the switch takes effect.
	push(LayerHalf, half.frame(2), true)
	if f.Current() != LayerHalf {
		t.Fatal("did not switch on the target keyframe")
	}
	for i := 0; i < 10; i++ {
		push(LayerFull, full.frame(4), false) // must now be dropped
		push(LayerHalf, half.frame(2), false)
	}

	if g := k.gaps(); len(g) != 0 {
		t.Fatalf("the switch was visible to the receiver as %v; "+
			"this is the freeze every naive SFU produces", g)
	}
	for i := 1; i < len(k.tss); i++ {
		if k.tss[i] < k.tss[i-1] {
			t.Fatalf("timestamp regressed across the switch at %d: %d -> %d",
				i, k.tss[i-1], k.tss[i])
		}
	}

	_, switches, _ := f.Stats()
	if switches != 1 {
		t.Fatalf("recorded %d switches, expected 1", switches)
	}
}

// Repeated switching must not accumulate error.
func TestManySwitchesStayContiguous(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	srcs := map[LayerID]*source{
		LayerFull:    newSource(100, 111),
		LayerHalf:    newSource(40000, 222),
		LayerQuarter: newSource(9000, 333),
	}
	k := &sink{}
	order := []LayerID{LayerFull, LayerHalf, LayerQuarter, LayerHalf, LayerFull, LayerQuarter}

	first := true
	for _, want := range order {
		f.Select(want)
		// Every layer keeps publishing; a keyframe on the target completes
		// the switch.
		for round := 0; round < 6; round++ {
			for _, l := range []LayerID{LayerFull, LayerHalf, LayerQuarter} {
				kf := (l == want && round == 1) || (first && l == LayerFull && round == 0)
				for _, p := range srcs[l].frame(3) {
					if f.WriteRTP(l, p, kf) == Forward {
						k.add(p)
					}
				}
			}
		}
		first = false
		if f.Current() != want {
			t.Fatalf("failed to reach layer %v, stuck on %v", want, f.Current())
		}
	}

	if g := k.gaps(); len(g) != 0 {
		t.Fatalf("six switches produced %d discontinuities: %v", len(g), g)
	}
	for i := 1; i < len(k.tss); i++ {
		if k.tss[i] < k.tss[i-1] {
			t.Fatalf("timestamp regressed at %d across repeated switches", i)
		}
	}
	t.Logf("6 switches, %d packets, zero discontinuities", len(k.seqs))
}

// Unsubscribing forwards nothing at all -- not a low layer, nothing.
func TestSelectNoneForwardsNothing(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(1, 111)
	for _, p := range src.frame(3) {
		f.WriteRTP(LayerFull, p, true)
	}
	f.Select(LayerNone)
	for _, p := range src.frame(3) {
		if f.WriteRTP(LayerFull, p, true) != Drop {
			t.Fatal("forwarded a packet after unsubscribing")
		}
	}
}

// Packets from layers we are not forwarding must never reach the receiver.
func TestOtherLayersAreDropped(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerHalf)
	half := newSource(10, 222)
	full := newSource(9000, 111)

	for _, p := range half.frame(2) {
		if f.WriteRTP(LayerHalf, p, true) != Forward {
			t.Fatal("dropped the selected layer")
		}
	}
	for _, p := range full.frame(2) {
		if f.WriteRTP(LayerFull, p, true) != Drop {
			t.Fatal("forwarded an unselected layer")
		}
	}
}

// Reordered packets must land at the position they belong to, not at the
// head, or the receiver sees a spurious gap followed by a duplicate.
func TestReorderedPacketsKeepTheirPlace(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(200, 111)

	var got []uint16
	pkts := src.frame(4)
	for _, p := range pkts {
		if f.WriteRTP(LayerFull, p, true) == Forward {
			got = append(got, p.SequenceNumber)
		}
	}

	next := src.frame(4)
	// Deliver the third packet before the second.
	swapped := []*rtp.Packet{next[0], next[2], next[1], next[3]}
	for _, p := range swapped {
		if f.WriteRTP(LayerFull, p, false) == Forward {
			got = append(got, p.SequenceNumber)
		}
	}

	// Sorted, the outbound numbers must still be contiguous 0..7.
	seen := map[uint16]bool{}
	for _, s := range got {
		if seen[s] {
			t.Fatalf("duplicate outbound sequence %d in %v", s, got)
		}
		seen[s] = true
	}
	for i := uint16(0); i < 8; i++ {
		if !seen[i] {
			t.Fatalf("outbound sequence %d never sent; got %v", i, got)
		}
	}
}
