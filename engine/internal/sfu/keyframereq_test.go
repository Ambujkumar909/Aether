package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Keyframe requests on a simulcast track, as a five-person meeting makes
// them. Measured before this test: five people joining at once, every tile
// switching layer; some receivers froze on a stale frame and two never got
// any video, for the whole call, while audio flowed and the server idled at
// 1% CPU. Three faults, each tested here:
//
//   - the request went to one fixed SSRC whatever layer was asked for (the
//     peer's closure captured the first layer's remote track);
//   - the dedup window was per track, not per layer, so two receivers
//     switching to different layers inside 500 ms lost the second request;
//   - a receiver still waiting for a keyframe never asked again.

type pliLog struct {
	mu  sync.Mutex
	got []LayerID
}

func (p *pliLog) add(l LayerID) { p.mu.Lock(); p.got = append(p.got, l); p.mu.Unlock() }
func (p *pliLog) count(l LayerID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, x := range p.got {
		if x == l {
			n++
		}
	}
	return n
}

// A VP8 keyframe and a delta frame payload (payload descriptor + first
// byte of the VP8 header: bit 0 clear = keyframe).
var (
	vp8Key   = []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a}
	vp8Delta = []byte{0x10, 0x01, 0x00, 0x9d, 0x01, 0x2a}
)

func kfTrack(t *testing.T) (*PublishedTrack, *pliLog) {
	t.Helper()
	log := &pliLog{}
	tr := NewPublishedTrack("cam", "pub", webrtc.RTPCodecTypeVideo, webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), log.add)
	return tr, log
}

func kfSub(t *testing.T, tr *PublishedTrack, id string) *Downstream {
	t.Helper()
	local, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "cam", "pub")
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownstream(id, "cam", local, ledger.NewMeter("t"), ledger.DefaultPath(), 0)
	tr.Subscribe(d)
	return d
}

func pkt(seq uint16, payload []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000}, Payload: payload}
}

func TestSwitchingToDifferentLayersAtOnceRequestsEach(t *testing.T) {
	tr, log := kfTrack(t)
	a, b := kfSub(t, tr, "a"), kfSub(t, tr, "b")
	a.Select(tr, LayerQuarter)
	b.Select(tr, LayerQuarter)
	tr.dispatch(LayerQuarter, pkt(1, vp8Key))
	time.Sleep(PLIInterval + 50*time.Millisecond)
	// Two receivers switch, to two different layers, in the same instant.
	a.Select(tr, LayerFull)
	b.Select(tr, LayerHalf)
	if log.count(LayerFull) == 0 {
		t.Fatal("no keyframe requested for the full layer")
	}
	if log.count(LayerHalf) == 0 {
		t.Fatal("the half layer's request was swallowed by the full layer's dedup window")
	}
	// The same layer twice inside the window is still one request.
	c := kfSub(t, tr, "c")
	c.Select(tr, LayerQuarter)
	tr.dispatch(LayerQuarter, pkt(2, vp8Key))
	c.Select(tr, LayerFull)
	if n := log.count(LayerFull); n != 1 {
		t.Fatalf("%d requests for the full layer inside one window", n)
	}
}

func TestAReceiverStillWaitingForAKeyframeAsksAgain(t *testing.T) {
	tr, log := kfTrack(t)
	a := kfSub(t, tr, "a")
	a.Select(tr, LayerQuarter)
	tr.dispatch(LayerQuarter, pkt(1, vp8Key))
	a.Select(tr, LayerFull)
	first := log.count(LayerFull)
	if first != 1 {
		t.Fatalf("the switch requested %d keyframes, want 1", first)
	}
	// The publisher did not answer (the request was lost, or it ignored it):
	// only delta frames arrive on the full layer. The receiver must keep
	// asking, once per window, until a keyframe comes.
	deadline := time.Now().Add(3 * PLIInterval)
	seq := uint16(100)
	for time.Now().Before(deadline) {
		tr.dispatch(LayerFull, pkt(seq, vp8Delta))
		tr.dispatch(LayerQuarter, pkt(seq, vp8Delta))
		seq++
		time.Sleep(20 * time.Millisecond)
	}
	n := log.count(LayerFull)
	if n < 3 {
		t.Fatalf("waited %v on the full layer and asked %d times; a lost request freezes the tile forever", 3*PLIInterval, n)
	}
	if n > 5 {
		t.Fatalf("asked %d times in %v: the retry is not rate-limited", n, 3*PLIInterval)
	}
	// Once the keyframe arrives the asking stops.
	tr.dispatch(LayerFull, pkt(seq, vp8Key))
	if a.Current() != LayerFull {
		t.Fatalf("switch did not complete on the keyframe (current %v)", a.Current())
	}
	before := log.count(LayerFull)
	for range 40 {
		seq++
		tr.dispatch(LayerFull, pkt(seq, vp8Delta))
		time.Sleep(20 * time.Millisecond)
	}
	if after := log.count(LayerFull); after != before {
		t.Fatalf("kept asking after the switch completed (%d more)", after-before)
	}
}

// A receiver that has never been sent a frame (it joined and is waiting for
// its first keyframe) asks again too -- the case of the two who never got
// any video.
func TestAReceiverThatNeverStartedAsksAgain(t *testing.T) {
	tr, log := kfTrack(t)
	a := kfSub(t, tr, "a")
	a.Select(tr, LayerHalf)
	deadline := time.Now().Add(3 * PLIInterval)
	seq := uint16(1)
	for time.Now().Before(deadline) {
		tr.dispatch(LayerHalf, pkt(seq, vp8Delta))
		seq++
		time.Sleep(20 * time.Millisecond)
	}
	if n := log.count(LayerHalf); n < 3 {
		t.Fatalf("a receiver waiting for its first keyframe asked %d times in %v", n, 3*PLIInterval)
	}
}
