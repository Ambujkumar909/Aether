package sfu

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// publishAudio adds an Opus track to the publisher connection, negotiates,
// and sends a 20 ms packet stream until stop is closed.
func (c *client) publishAudio(trackID string, stop <-chan struct{}) {
	c.publishAudioWith(trackID, stop, false)
}

// publishAudioWith optionally tags every packet with a header extension, as
// a browser does (MID, audio level), so a test can see whether the tag is
// forwarded.
func (c *client) publishAudioWith(trackID string, stop <-chan struct{}, tagged bool) {
	c.t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		trackID, "stream-"+c.peer.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	sender, err := c.pub.AddTrack(track)
	if err != nil {
		c.t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	c.renegotiate()
	go func() {
		seq, ts := uint16(0), uint32(0)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				pkt := &rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts},
					Payload: []byte{0xfc, 0xff, 0xfe, 0x01, 0x02, 0x03, 0x04, 0x05},
				}
				if tagged {
					// One-byte-header extension id 1, value "0": the shape of a
					// browser's sdes:mid tag for its first section.
					_ = pkt.Header.SetExtension(1, []byte("0"))
				}
				_ = track.WriteRTP(pkt)
				seq++
				ts += 960
			}
		}
	}()
}

// renegotiate offers the publisher connection's current state to the server.
func (c *client) renegotiate() {
	c.t.Helper()
	offer, err := c.pub.CreateOffer(nil)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.pub.SetLocalDescription(offer); err != nil {
		c.t.Fatal(err)
	}
	if err := c.peer.HandleOffer(offer.SDP); err != nil {
		c.t.Fatalf("server rejected publisher offer: %v", err)
	}
}

// waitGrowth waits until trackID's count on c has grown by at least n.
func waitGrowth(c *client, trackID string, n int64, within time.Duration) (grew int64, ok bool) {
	start := c.packetsOf(trackID)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if g := c.packetsOf(trackID) - start; g >= n {
			return g, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return c.packetsOf(trackID) - start, false
}

// A person talking turns the camera on. Their browser renegotiates the
// publisher connection to add the video, and their voice must keep reaching
// everyone -- measured in Chrome before this test existed: the voice stopped
// at the server the moment the camera came on, for the rest of the call,
// while the browser kept sending it. Every earlier test published only
// video, so none could see it.
func TestAudioSurvivesAddingTheCamera(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("renego", ledger.NewMeter("renego"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")
	stop := make(chan struct{})
	defer close(stop)

	alice.publishAudio("alice-mic", stop)
	if g, ok := waitGrowth(bob, "alice-mic", 25, 30*time.Second); !ok {
		t.Fatalf("bob never heard alice: %d audio packets", g)
	}

	// The camera comes on: a second track, a second publisher offer.
	cam := alice.publishControllable("alice-cam")
	defer cam.close()
	if g, ok := waitGrowth(bob, "alice-cam", 20, 30*time.Second); !ok {
		t.Fatalf("bob never got alice's camera: %d packets", g)
	}
	if g, ok := waitGrowth(bob, "alice-mic", 50, 5*time.Second); !ok {
		t.Fatalf("alice's voice stopped reaching bob when her camera came on: %d audio packets in 5 s after it (expected ~250)", g)
	}

	// And off again (the track is stopped, the transceiver stays): still heard.
	cam.off()
	time.Sleep(300 * time.Millisecond)
	if g, ok := waitGrowth(bob, "alice-mic", 50, 5*time.Second); !ok {
		t.Fatalf("alice's voice stopped when her camera went off: %d audio packets", g)
	}
}

// The publisher's header extensions do not reach subscribers: they are the
// publisher's connection's tags (see forwardCopy for what forwarding them
// did in Chrome). Real ICE, DTLS and SRTP; the tag is on every packet sent.
func TestPublisherExtensionsAreNotForwarded(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("ext", ledger.NewMeter("ext"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()
	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")
	stop := make(chan struct{})
	defer close(stop)
	alice.publishAudioWith("alice-mic", stop, true)
	if g, ok := waitGrowth(bob, "alice-mic", 50, 30*time.Second); !ok {
		t.Fatalf("bob never heard alice: %d packets", g)
	}
	if n := bob.extSeen.Load(); n != 0 {
		t.Fatalf("%d of %d packets reached bob still carrying the publisher's header extension", n, bob.packetsOf("alice-mic"))
	}
}
