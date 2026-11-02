package sfu

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Audio through the forwarder.
//
// The forwarder refused to begin any stream on a packet that was not a
// keyframe. For video that rule is what stops a decoder being fed a delta
// frame with no reference. Audio has no keyframes, so the rule refused every
// audio packet there ever was, and no participant heard another -- with
// signalling, ICE and DTLS all reporting success, because they had. This is
// the test that would have said so.

func audioPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
		Payload: []byte{0xfc, 0xff, 0xfe}, // an Opus frame; there is no such thing as a keyframe
	}
}

// The regression. Against the old code the first call returns Drop, and so
// does every call after it: the stream never starts.
func TestAudioStartsOnItsFirstPacket(t *testing.T) {
	f := NewAudioForwarder()
	f.Select(LayerFull)

	// dispatch() computes isKeyframe as (video && IsKeyframe), so for audio
	// it is always false. That is the input the forwarder actually receives.
	if got := f.WriteRTP(LayerFull, audioPacket(1000), false); got != Forward {
		t.Fatalf("first audio packet: got %v, want Forward", got)
	}
	for i := uint16(1); i < 50; i++ {
		if got := f.WriteRTP(LayerFull, audioPacket(1000+i), false); got != Forward {
			t.Fatalf("audio packet %d: got %v, want Forward", i, got)
		}
	}
	if n, _, _ := f.Stats(); n != 50 {
		t.Fatalf("forwarded %d audio packets, want 50", n)
	}
}

// The video rule is unchanged: a video forwarder still refuses to start on a
// delta frame. Fixing audio must not have loosened the thing that protects
// video decoders.
func TestVideoStillWaitsForAKeyframe(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	if got := f.WriteRTP(LayerFull, audioPacket(1), false); got != Drop {
		t.Fatalf("video started on a delta frame: got %v, want Drop", got)
	}
	if got := f.WriteRTP(LayerFull, audioPacket(2), true); got != Forward {
		t.Fatalf("video refused a keyframe: got %v, want Forward", got)
	}
}

// Audio output is contiguous, like video: the outbound sequence space starts
// at zero and advances with the inbound, so the receiver's jitter buffer sees
// a clean stream rather than the publisher's numbering.
func TestAudioOutputIsContiguous(t *testing.T) {
	f := NewAudioForwarder()
	f.Select(LayerFull)
	var prev uint16
	for i := uint16(0); i < 20; i++ {
		p := audioPacket(7000 + i)
		if f.WriteRTP(LayerFull, p, false) != Forward {
			t.Fatalf("packet %d dropped", i)
		}
		if i > 0 && p.SequenceNumber != prev+1 {
			t.Fatalf("packet %d: out seq %d, want %d", i, p.SequenceNumber, prev+1)
		}
		prev = p.SequenceNumber
	}
	if prev != 19 {
		t.Fatalf("outbound sequence ended at %d, want 19 (started at 0)", prev)
	}
}

// Downstreams are built with the right forwarder for their kind, so the fix
// reaches the code path that actually runs rather than only the constructor
// the test calls directly.
func TestDownstreamPicksForwarderByKind(t *testing.T) {
	if f := newForwarderFor(webrtc.RTPCodecTypeAudio); f.keyframed {
		t.Fatal("an audio downstream was given a keyframed forwarder")
	}
	if f := newForwarderFor(webrtc.RTPCodecTypeVideo); !f.keyframed {
		t.Fatal("a video downstream was given an audio forwarder")
	}
}
