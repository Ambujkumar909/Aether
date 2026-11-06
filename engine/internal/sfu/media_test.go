package sfu

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// End to end through the real media plane: two pion clients, the real Router,
// real ICE, real DTLS, real SRTP. Unit tests on the forwarder prove the
// translation arithmetic; only this proves the whole path is connected.

// client is a browser stand-in: the two peer connections a participant has,
// wired to a server Peer through the same signalling callbacks the WebSocket
// gateway uses.
type client struct {
	t    *testing.T
	pub  *webrtc.PeerConnection
	sub  *webrtc.PeerConnection
	peer *Peer

	received atomic.Int64
	gotTrack chan struct{}
	// perTrack counts packets by the forwarded track's id, so a test can
	// tell "the room still gets packets" from "THIS feed still flows".
	perTrack sync.Map // string -> *atomic.Int64
	// extSeen counts received packets that still carried a header extension.
	extSeen atomic.Int64
	// slotTrack is the slot map the server sent: slot id -> published track
	// id, so packets are counted against what the slot carries now.
	slotTrack sync.Map
}

// packetsOf is how many packets of trackID this client has received.
func (c *client) packetsOf(trackID string) int64 {
	if v, ok := c.perTrack.Load(trackID); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func newClient(t *testing.T, r *Router, id string) *client {
	return newClientWith(t, r, id, webrtc.Configuration{})
}

// newClientWith is newClient with an explicit RTCPeerConnection configuration,
// so a test can hand the client ICE servers and a transport policy -- which is
// how the relay path is exercised: a client forced to relay-only, against a
// real TURN server, must still connect to the SFU.
func newClientWith(t *testing.T, r *Router, id string, cfg webrtc.Configuration) *client {
	t.Helper()

	// No ICE servers: everything is on loopback, and reaching for STUN in a
	// test would make it depend on the network.
	//
	// Loopback candidates are enabled on the CLIENT too. pion excludes them
	// by default, so a client with the defaults only offers the machine's
	// LAN address -- and a server bound to 127.0.0.1 can never pair with
	// that. The earlier version of this harness passed only because the
	// server was bound to 0.0.0.0 and so reachable at the LAN address by
	// accident; it was testing the network, not loopback.
	cse := webrtc.SettingEngine{}
	cse.SetIncludeLoopbackCandidate(true)
	capi := webrtc.NewAPI(webrtc.WithSettingEngine(cse))
	pub, err := capi.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := capi.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}

	c := &client{t: t, pub: pub, sub: sub, gotTrack: make(chan struct{}, 8)}

	// Server to client signalling, handed to AddPeer so it is attached before
	// the first subscription can fire a renegotiation. Setting it afterwards is
	// the race the gateway had; the harness must not quietly have it too.
	sendSDP := func(dir Direction, isOffer bool, body string) {
		if dir == Publisher {
			if err := pub.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer, SDP: body,
			}); err != nil {
				t.Logf("client %s: publisher answer: %v", id, err)
			}
			return
		}
		// Subscriber: the server offers, we answer.
		if err := sub.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer, SDP: body,
		}); err != nil {
			t.Logf("client %s: subscriber offer: %v", id, err)
			return
		}
		ans, err := sub.CreateAnswer(nil)
		if err != nil {
			t.Logf("client %s: create answer: %v", id, err)
			return
		}
		if err := sub.SetLocalDescription(ans); err != nil {
			t.Logf("client %s: set local answer: %v", id, err)
			return
		}
		if err := c.peer.HandleAnswer(ans.SDP); err != nil {
			t.Logf("client %s: server rejected answer: %v", id, err)
		}
	}
	sendICE := func(dir Direction, cand string) {
		pc := pub
		if dir == Subscriber {
			pc = sub
		}
		var init webrtc.ICECandidateInit
		if err := jsonUnmarshal(cand, &init); err == nil {
			_ = pc.AddICECandidate(init)
		}
	}

	sendSlots := func(bs []SlotBinding) {
		for _, b := range bs {
			c.slotTrack.Store(b.Slot, b.Track)
		}
	}
	peer, err := r.AddPeer(id, Signalling{SendSDP: sendSDP, SendICE: sendICE, SendSlots: sendSlots})
	if err != nil {
		t.Fatalf("AddPeer(%s): %v", id, err)
	}
	c.peer = peer

	// Client to server signalling.
	pub.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		_ = peer.HandleICE(Publisher, mustJSON(ic.ToJSON()))
	})
	sub.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		_ = peer.HandleICE(Subscriber, mustJSON(ic.ToJSON()))
	})

	sub.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case c.gotTrack <- struct{}{}:
		default:
		}
		slotID := tr.ID()
		go func() {
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				if pkt.Header.Extension || len(pkt.Header.Extensions) > 0 {
					c.extSeen.Add(1)
				}
				c.received.Add(1)
				key := slotID
				if v, ok := c.slotTrack.Load(slotID); ok && v.(string) != "" {
					key = v.(string)
				}
				v, _ := c.perTrack.LoadOrStore(key, new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
			}
		}()
	})

	// The subscriber connection must be able to receive before the server
	// offers anything.
	if _, err := sub.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}

	// Close the client's own connections when the test ends.
	//
	// Without this every test in this package leaked two real pion
	// PeerConnections, each carrying a substantial number of goroutines, for
	// the lifetime of the test binary. That is not merely untidy: it makes
	// every goroutine-leak assertion in the package unreliable, because the
	// baseline they measure against is polluted by the harness itself. A leak
	// detector that cannot trust its own baseline reports nothing useful.
	t.Cleanup(c.close)
	return c
}

// close tears down the client side. Safe to call more than once.
func (c *client) close() {
	if c.pub != nil {
		_ = c.pub.Close()
	}
	if c.sub != nil {
		_ = c.sub.Close()
	}
}

// publish sends a synthetic VP8 stream and returns a stop function.
func (c *client) publish(trackID string) func() {
	c.t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		trackID, "stream-"+trackID)
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

	stop := make(chan struct{})
	go func() {
		seq := uint16(0)
		ts := uint32(0)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				// A VP8 keyframe descriptor: S set, PID 0, payload header P=0.
				payload := []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a}
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: seq,
						Timestamp:      ts,
						Marker:         true,
					},
					Payload: payload,
				}
				_ = track.WriteRTP(pkt)
				seq++
				ts += 3000
			}
		}
	}()
	return func() { close(stop) }
}

func TestTwoPeersExchangeMediaThroughTheRouter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("itest", ledger.NewMeter("test"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	stop := alice.publish("alice-cam")
	defer stop()

	// Bob must receive a track and then actual packets.
	select {
	case <-bob.gotTrack:
	case <-time.After(20 * time.Second):
		t.Fatal("bob never received a track from alice")
	}

	deadline := time.After(20 * time.Second)
	for {
		if bob.received.Load() > 10 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("bob received only %d packets", bob.received.Load())
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Logf("bob received %d forwarded packets", bob.received.Load())

	// Alice must not be sent her own camera back.
	if alice.received.Load() > 0 {
		t.Fatalf("alice was sent %d packets of her own feed", alice.received.Load())
	}

	// The router must know what it is doing.
	st := r.Stats()
	if st.Peers != 2 {
		t.Fatalf("router reports %d peers", st.Peers)
	}
	if st.Tracks != 1 {
		t.Fatalf("router reports %d tracks", st.Tracks)
	}
	if len(st.Feeds) != 1 {
		t.Fatalf("router reports %d feeds, expected 1", len(st.Feeds))
	}
	f := st.Feeds[0]
	if f.Subscriber != "bob" || f.Publisher != "alice" {
		t.Fatalf("feed is %s -> %s", f.Publisher, f.Subscriber)
	}
	t.Logf("feed %s -> %s on layer %q, %d packets",
		f.Publisher, f.Subscriber, f.Layer, f.Packets)

	// The ledger must have booked the forwarded bytes with their full stack
	// overhead, not just the payload. This is the point at which the numbers
	// in the telemetry panel stop being a model and start being measurements.
	snap := r.Meter.Snapshot()
	if snap.Bytes[ledger.ClassMediaPayload] == 0 {
		t.Fatal("ledger recorded no media payload for a session that forwarded packets")
	}
	if snap.Bytes[ledger.ClassIP] == 0 || snap.Bytes[ledger.ClassRTPHeader] == 0 {
		t.Fatal("ledger booked payload but no per-packet overhead")
	}
	t.Logf("ledger: %s total, %.1f%% payload efficiency",
		ledger.Human(snap.Total()), 100*snap.Efficiency())
}
