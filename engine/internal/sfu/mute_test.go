package sfu

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
)

// Turning the camera off must stop the bytes, not merely stop the picture.
//
// This is the server half of a bug found on the client. The obvious way to
// mute is `track.enabled = false`, and it does not stop the encoder: per spec
// a disabled MediaStreamTrack emits black frames for video and digital
// silence for audio, so RTP keeps flowing. The SFU has no way to tell those
// frames from real ones and forwards them to every subscriber, which in a
// forty person meeting means one participant with their camera "off" still
// costs thirty-nine outbound streams of nothing.
//
// The client now calls replaceTrack(null), which genuinely stops sending
// without renegotiating. What this test pins down is the property the server
// must have for that to work: when a publisher stops sending, forwarding
// stops too, promptly and completely, and resumes when they do.

// controllablePublisher publishes RTP that can be paused and resumed, which is
// what replaceTrack(null) looks like from the far side of the wire.
type controllablePublisher struct {
	track  rtpWriter
	pause  chan bool
	stop   chan struct{}
	seq    uint16
	tstamp uint32
}

func (c *client) publishControllable(trackID string) *controllablePublisher {
	c.t.Helper()
	p := &controllablePublisher{
		track: c.mustTrack(trackID),
		pause: make(chan bool, 4),
		stop:  make(chan struct{}),
	}
	go func() {
		payload := make([]byte, 800)
		copy(payload, []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a})
		sending := true
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case sending = <-p.pause:
			case <-tick.C:
				if !sending {
					continue
				}
				_ = p.track.WriteRTP(&rtp.Packet{
					Header: rtp.Header{
						Version: 2, PayloadType: 96,
						SequenceNumber: p.seq, Timestamp: p.tstamp, Marker: true,
					},
					Payload: payload,
				})
				p.seq++
				p.tstamp += 3000
			}
		}
	}()
	return p
}

func (p *controllablePublisher) off()   { p.pause <- false }
func (p *controllablePublisher) on()    { p.pause <- true }
func (p *controllablePublisher) close() { close(p.stop) }

func TestCameraOffStopsTheBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	meter := ledger.NewMeter("mute")
	r := NewRouter("mute", meter, log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	pub := alice.publishControllable("alice-cam")
	defer pub.close()

	// Wait until media is genuinely flowing, or the rest of the test proves
	// nothing: a feed that never started is trivially "stopped".
	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatal("bob never received a track")
	}
	deadline := time.After(30 * time.Second)
	for bob.received.Load() < 20 {
		select {
		case <-deadline:
			t.Fatalf("only %d packets arrived before the camera was touched", bob.received.Load())
		case <-time.After(100 * time.Millisecond):
		}
	}

	// --- measure while the camera is on ------------------------------------
	onStart := meter.Snapshot().Total()
	onPackets := bob.received.Load()
	time.Sleep(2 * time.Second)
	onBytes := meter.Snapshot().Total() - onStart
	onDelivered := bob.received.Load() - onPackets

	if onDelivered == 0 {
		t.Fatal("no packets were delivered while the camera was on")
	}

	// --- turn the camera off ------------------------------------------------
	pub.off()
	// Give whatever is already queued in the pipeline time to drain, so the
	// quiet window measures the steady state rather than the tail.
	time.Sleep(500 * time.Millisecond)

	offStart := meter.Snapshot().Total()
	offPackets := bob.received.Load()
	time.Sleep(2 * time.Second)
	offBytes := meter.Snapshot().Total() - offStart
	offDelivered := bob.received.Load() - offPackets

	// --- turn it back on ----------------------------------------------------
	pub.on()
	time.Sleep(200 * time.Millisecond)
	backStart := meter.Snapshot().Total()
	backPackets := bob.received.Load()
	time.Sleep(2 * time.Second)
	backBytes := meter.Snapshot().Total() - backStart
	backDelivered := bob.received.Load() - backPackets

	t.Log("")
	t.Log("  CAMERA ON / OFF / ON, two seconds each")
	t.Log("  " + hyphens(56))
	t.Logf("  on    %9d B forwarded   %5d packets delivered", onBytes, onDelivered)
	t.Logf("  OFF   %9d B forwarded   %5d packets delivered", offBytes, offDelivered)
	t.Logf("  on    %9d B forwarded   %5d packets delivered", backBytes, backDelivered)
	t.Log("")
	if onBytes > 0 {
		t.Logf("  camera off costs %.2f%% of camera on",
			100*float64(offBytes)/float64(onBytes))
	}
	t.Log("")

	// Off must be genuinely off. A small residue is tolerated for RTCP, which
	// continues by design and is what keeps the connection alive, but it must
	// be a rounding error against the media stream rather than a fraction of
	// it.
	if offBytes > onBytes/20 {
		t.Fatalf("camera off still forwarded %d B against %d B on (%.1f%%); "+
			"muting is not stopping the stream", offBytes, onBytes,
			100*float64(offBytes)/float64(onBytes))
	}
	if offDelivered > onDelivered/20 {
		t.Fatalf("camera off still delivered %d packets against %d on; "+
			"black frames are being forwarded", offDelivered, onDelivered)
	}
	if backDelivered == 0 {
		t.Fatal("turning the camera back on delivered nothing; the feed did not recover")
	}
}

// A participant leaving must stop their feed for everybody else, and must not
// leave the forward registered.
func TestLeavingStopsEveryFeed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("leave", ledger.NewMeter("leave"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	pub := alice.publishControllable("alice-cam")
	defer pub.close()

	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatal("bob never received a track")
	}
	for bob.received.Load() < 20 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(r.Stats().Feeds) == 0 {
		t.Fatal("no feed registered while media was flowing")
	}

	// Alice leaves.
	r.RemovePeer("alice")
	time.Sleep(500 * time.Millisecond)

	before := bob.received.Load()
	time.Sleep(1500 * time.Millisecond)
	after := bob.received.Load()

	t.Logf("after alice left, bob received %d further packets", after-before)

	if after-before > 5 {
		t.Fatalf("bob received %d packets from a participant who had left", after-before)
	}
	st := r.Stats()
	if len(st.Feeds) != 0 {
		t.Fatalf("%d feeds still registered after the publisher left", len(st.Feeds))
	}
	if st.Tracks != 0 {
		t.Fatalf("%d tracks still registered after the publisher left", st.Tracks)
	}
}
