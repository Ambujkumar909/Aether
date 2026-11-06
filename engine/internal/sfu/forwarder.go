package sfu

import (
	"sync"

	"github.com/pion/rtp"
)

// The forwarder is where an SFU is actually won or lost.
//
// Its job sounds trivial -- take packets from one of several inbound
// encodings of the same camera and hand them to one outbound track -- and it
// is not, because the receiver on the far side is running a jitter buffer and
// a decoder that were never told a switch happened. From their point of view
// there is exactly one stream with one monotonic sequence number space and
// one monotonic timestamp clock. Every inbound layer has its own.
//
// So a naive forwarder that simply relays whichever layer is currently
// selected produces, on the very first quality change: a sequence number that
// jumps backwards or forwards by thousands, a jitter buffer that concludes it
// has lost an enormous burst, a decoder fed a P-frame referencing a frame it
// has never seen, and a receiver that freezes until the next keyframe. Users
// experience this as "it stutters whenever the network changes", which is to
// say, constantly.
//
// This forwarder maintains its own outbound sequence and timestamp space and
// translates every packet into it. Switching layers is then invisible to the
// receiver: the numbers keep counting, and the only requirement is that the
// switch happens on a keyframe boundary so the decoder has something it can
// start from without reference to the layer we just left.

// LayerID identifies one inbound encoding of a track.
type LayerID uint8

const (
	LayerQuarter LayerID = 0
	LayerHalf    LayerID = 1
	LayerFull    LayerID = 2
	LayerNone    LayerID = 255
)

func (l LayerID) String() string {
	switch l {
	case LayerQuarter:
		return "q"
	case LayerHalf:
		return "h"
	case LayerFull:
		return "f"
	default:
		return "none"
	}
}

// Forwarder translates packets from a selected inbound layer into a single
// continuous outbound stream.
//
// One forwarder exists per (published track, subscriber) pair. It is written
// to be cheap: the hot path takes one mutex, does integer arithmetic, and
// mutates two header fields.
type Forwarder struct {
	mu sync.Mutex

	current LayerID // layer whose packets we are relaying now
	target  LayerID // layer we want to be relaying

	// Outbound space. These count what the receiver sees, not what any
	// publisher sent.
	outSeq uint16
	outTS  uint32

	// Inbound reference for the current layer, captured at the moment we
	// locked onto it. Offsets are derived from these.
	refSeq uint16
	refTS  uint32

	started bool

	// keyframed is true for streams that HAVE keyframes -- video. The start
	// gate and the layer-switch gate both wait for one, which is correct for
	// video and fatal for anything else: audio has no keyframes, so every
	// audio packet ever offered to this forwarder was refused at the start
	// gate and no participant ever heard another. Signalling, ICE and DTLS
	// all succeeded, which is what made it look like a network problem.
	keyframed bool

	// awaitingKeyframe is set while a switch is pending. Until a keyframe
	// arrives on the target layer we keep relaying the current one, because
	// a receiver watching a slightly stale layer is strictly better than a
	// receiver watching a frozen one.
	awaitingKeyframe bool

	// Dropped inbound packets must not create outbound gaps, and forwarded
	// ones must not collide. These track the translation.
	lastInSeq    uint16
	haveLastIn   bool
	packetsOut   uint64
	switches     uint64
	keyframeWait uint64

	// resuming is set when forwarding restarts after a pause (LayerNone and
	// back). The next packet (for video, the next keyframe) re-anchors the
	// translation so the pause leaves no gap in the outbound sequence.
	resuming bool
}

// NewForwarder starts with nothing selected. The first Select decides.
func NewForwarder() *Forwarder {
	return &Forwarder{current: LayerNone, target: LayerNone, keyframed: true}
}

// NewAudioForwarder is a forwarder for a stream with no keyframes. It starts
// on the first packet and switches layers immediately.
func NewAudioForwarder() *Forwarder {
	return &Forwarder{current: LayerNone, target: LayerNone, keyframed: false}
}

// Select requests a layer. The change takes effect at the next keyframe on
// that layer, except when nothing is being forwarded yet, in which case it is
// immediate -- there is no continuity to preserve.
//
// Returns true if a keyframe should be requested from the publisher.
func (f *Forwarder) Select(target LayerID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if target == f.target {
		return false
	}
	f.target = target

	if target == LayerNone {
		// Unsubscribing needs no keyframe and no ceremony.
		f.current = LayerNone
		f.awaitingKeyframe = false
		f.resuming = false
		return false
	}
	if !f.started {
		f.current = target
		f.awaitingKeyframe = false
		return true
	}
	if f.current == LayerNone {
		// Resuming after a pause (the tile went off screen and came back).
		// The publisher numbered on through the pause, so carrying on from
		// the last packet sent would show the receiver the whole pause as
		// one gap -- reported as loss, which drives its bandwidth estimate
		// down until its tiles starve (ARCHITECTURE 6bu). Re-anchor on the
		// next keyframe instead, exactly as a layer switch does.
		f.current = target
		f.awaitingKeyframe = false
		f.resuming = true
		return f.keyframed
	}
	if target == f.current {
		f.awaitingKeyframe = false
		return false
	}

	f.awaitingKeyframe = true
	f.keyframeWait++
	return true
}

// Waiting reports the layer this forwarder is waiting on a keyframe from,
// if it is: a switch in progress (waiting on the target), or a video stream
// that has not started (waiting on its first keyframe). The track asks the
// publisher again while this holds -- a request that was lost or ignored
// must not leave a tile frozen for the rest of the call.
func (f *Forwarder) Waiting() (LayerID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.target == LayerNone {
		return LayerNone, false
	}
	if f.awaitingKeyframe {
		return f.target, true
	}
	if f.keyframed && f.resuming {
		return f.current, true
	}
	if f.keyframed && !f.started && f.current != LayerNone {
		return f.current, true
	}
	return LayerNone, false
}

// Current reports the layer actually being relayed.
func (f *Forwarder) Current() LayerID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// Target reports the layer we are trying to reach.
func (f *Forwarder) Target() LayerID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

// Stats exposes counters for telemetry.
func (f *Forwarder) Stats() (packets, switches, keyframeWaits uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.packetsOut, f.switches, f.keyframeWait
}

// Decision is what WriteRTP tells the caller to do with a packet.
type Decision uint8

const (
	// Drop means this packet belongs to a layer we are not forwarding.
	Drop Decision = iota
	// Forward means the packet header has been rewritten and should be sent.
	Forward
)

// WriteRTP considers one inbound packet from one layer.
//
// The packet header is mutated in place when the decision is Forward, so the
// caller must pass a packet it owns. isKeyframe must be accurate: it is the
// only thing that makes a layer switch safe, and a false positive produces
// exactly the corruption this file exists to prevent.
func (f *Forwarder) WriteRTP(layer LayerID, pkt *rtp.Packet, isKeyframe bool) Decision {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.target == LayerNone {
		return Drop
	}

	// A pending switch completes on the first keyframe of the target layer.
	//
	// This returns immediately rather than falling through. The switch has
	// already rewritten the header into the outbound space, and letting the
	// normal path run afterwards would compute its inbound delta from a
	// sequence number that is no longer an inbound sequence number.
	if f.awaitingKeyframe && layer == f.target && isKeyframe {
		f.completeSwitchLocked(pkt)
		return Forward
	}

	if layer != f.current {
		return Drop
	}

	if f.resuming {
		if f.keyframed && !isKeyframe {
			return Drop
		}
		f.resuming = false
		f.reanchorLocked(pkt)
		return Forward
	}

	if !f.started {
		if f.keyframed && !isKeyframe {
			// Never begin a video stream on a delta frame. The decoder would
			// have no reference and would emit garbage or nothing until the
			// next keyframe arrived anyway. Audio has no such thing; every
			// packet is independently decodable and the stream starts now.
			return Drop
		}
		f.started = true
		f.refSeq = pkt.SequenceNumber
		f.refTS = pkt.Timestamp
		// Start the outbound space at zero-ish rather than mirroring the
		// publisher, so nothing about our output leaks the sender's state.
		f.outSeq = 0
		f.outTS = 0
		f.lastInSeq = pkt.SequenceNumber
		f.haveLastIn = true

		pkt.SequenceNumber = f.outSeq
		pkt.Timestamp = f.outTS
		f.packetsOut++
		return Forward
	}

	// Advance the outbound sequence by however much the inbound advanced,
	// so that a genuine loss upstream stays visible as a gap downstream --
	// the receiver's NACK and concealment logic depends on seeing it.
	if f.haveLastIn {
		delta := pkt.SequenceNumber - f.lastInSeq
		// Reordered or duplicate packets within the current layer: forward
		// them at the position they belong to rather than at the head.
		if delta > 0x8000 {
			back := f.lastInSeq - pkt.SequenceNumber
			pkt.SequenceNumber = f.outSeq - back
			pkt.Timestamp = f.outTS - (f.refTS - pkt.Timestamp)
			f.packetsOut++
			return Forward
		}
		f.outSeq += delta
	} else {
		f.outSeq++
	}

	f.outTS += pkt.Timestamp - f.refTS
	f.refTS = pkt.Timestamp
	f.lastInSeq = pkt.SequenceNumber
	f.haveLastIn = true

	pkt.SequenceNumber = f.outSeq
	pkt.Timestamp = f.outTS
	f.packetsOut++
	return Forward
}

// completeSwitchLocked re-anchors the translation onto the target layer.
//
// The outbound sequence advances by exactly one across a switch. It must not
// jump: a gap would be read downstream as loss and trigger NACKs for packets
// that were never sent and never will be. It must not repeat either, which is
// why this is +1 and not +0.
//
// The timestamp advances by one frame interval rather than continuing the old
// layer's clock, because the two layers are separate encodes of the same wall
// time and their RTP clocks have no relationship worth preserving.
func (f *Forwarder) completeSwitchLocked(pkt *rtp.Packet) {
	f.current = f.target
	f.awaitingKeyframe = false
	f.switches++
	f.reanchorLocked(pkt)
}

// reanchorLocked continues the outbound space by one onto a new inbound
// stream position: a layer switch, or a resume after a pause.
func (f *Forwarder) reanchorLocked(pkt *rtp.Packet) {
	f.outSeq++
	f.outTS += frameTickGuess
	f.refSeq = pkt.SequenceNumber
	f.refTS = pkt.Timestamp
	f.lastInSeq = pkt.SequenceNumber
	f.haveLastIn = true

	pkt.SequenceNumber = f.outSeq
	pkt.Timestamp = f.outTS
	f.packetsOut++
}

// frameTickGuess is one frame at 30 fps on a 90 kHz video clock.
//
// A guess is acceptable here and an exact value is not available: we are
// bridging two independent encoder clocks and there is no correct answer,
// only a plausible one. Being slightly wrong costs a few milliseconds of
// presentation jitter on the single frame at the switch boundary. Being
// absent -- reusing the old timestamp -- would give two frames the same
// presentation time and make the decoder drop one.
const frameTickGuess = 90000 / 30

// Reset returns the forwarder to its initial state, for a track that has gone
// away and come back.
func (f *Forwarder) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
	f.haveLastIn = false
	f.awaitingKeyframe = false
	f.current = LayerNone
	f.target = LayerNone
}
