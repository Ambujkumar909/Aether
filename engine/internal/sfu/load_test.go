package sfu

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// A real meeting, at real sizes, measured rather than modelled.
//
// Every bandwidth figure quoted for a full room until now came from arithmetic
// over the simulcast ladder. Arithmetic is not a measurement, and the build
// rules say so. This brings up N genuine pion clients against the real Router
// -- real ICE, real DTLS, real SRTP -- publishes real RTP, and reads what the
// ledger actually counted.
//
// It measures the WORST case on purpose: every participant subscribes to every
// other, with no viewport gating, because there is no browser here to report
// what it painted. That is the ceiling, not the expectation. In a real 40
// person meeting the client reports the painted size of each tile and the
// allocator sends nothing for the ones off screen, which is what turns 39
// inbound streams into roughly 9. Both numbers matter: this one bounds what
// the server must survive if the gating ever fails.

// loadPacketBytes is a realistic RTP payload. Video packets run close to the
// path MTU; using tiny payloads would flatter the per-packet overhead.
const loadPacketBytes = 1100

// loadPacketsPerSecond per publisher. Deliberately modest: the point is to
// measure the fan-out multiplication and the per-packet cost, not to saturate
// a laptop's loopback with forty simultaneous 1.5 Mbps encodes.
const loadPacketsPerSecond = 50

// publishLoad sends a steady stream of realistically sized packets.
func (c *client) publishLoad(trackID string, stop <-chan struct{}) {
	c.t.Helper()
	track := c.mustTrack(trackID)

	go func() {
		seq := uint16(0)
		ts := uint32(0)
		payload := make([]byte, loadPacketBytes)
		// A VP8 keyframe descriptor at the front so the forwarder will start
		// on it; without one, nothing is ever forwarded and the test measures
		// an empty room.
		copy(payload, []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a})

		tick := time.NewTicker(time.Second / loadPacketsPerSecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = track.WriteRTP(&rtp.Packet{
					Header: rtp.Header{
						Version: 2, PayloadType: 96,
						SequenceNumber: seq, Timestamp: ts, Marker: true,
					},
					Payload: payload,
				})
				seq++
				ts += 3000
			}
		}
	}()
}

// The big sizes are opt-in, and that is a correction rather than a
// convenience.
//
// Running every size by default made `go test ./... -race` unusable: under the
// race detector a forty-person room is a hundred real peer connections doing
// real DTLS, and the subtest alone took 11m54s -- past Go's fifteen minute
// per-test alarm, so the whole package failed on a timeout that had nothing to
// do with the code under test. A suite that cannot be run is not a safety net,
// and the natural response to one that takes a quarter of an hour is to stop
// running it.
//
// So the default sweep is small enough to sit inside an ordinary run, and the
// full ladder is asked for explicitly:
//
//	AETHER_LOAD=1 go test ./internal/sfu/ -run TestMeetingLoad -v
//
// The measurements in docs/ARCHITECTURE.md come from the full ladder.
func loadSizes() []int {
	if os.Getenv("AETHER_LOAD") != "" {
		return []int{5, 10, 20, 40, 50}
	}
	return []int{5, 10}
}

func TestMeetingLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test: brings up real ICE and DTLS for every participant")
	}

	for _, n := range loadSizes() {
		t.Run(fmt.Sprintf("%dpeople", n), func(t *testing.T) {
			runMeetingLoad(t, n)
		})
	}
}

func runMeetingLoad(t *testing.T, n int) {
	t.Helper()
	before := baseline()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	meter := ledger.NewMeter("load")
	r := NewRouter("load", meter, log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	// --- join -------------------------------------------------------------
	joinStart := time.Now()
	clients := make([]*client, n)
	for i := range clients {
		clients[i] = newClient(t, r, fmt.Sprintf("p%02d", i))
	}
	joinSetup := time.Since(joinStart)

	stop := make(chan struct{})
	pubStart := time.Now()
	for i, c := range clients {
		c.publishLoad(fmt.Sprintf("cam-%02d", i), stop)
	}

	// Every participant must actually receive from someone before the
	// measurement means anything. Without this the test can report a
	// beautifully cheap room that never connected.
	connected := 0
	deadline := time.After(90 * time.Second)
	for connected < n {
		connected = 0
		for _, c := range clients {
			if c.received.Load() > 0 {
				connected++
			}
		}
		if connected >= n {
			break
		}
		select {
		case <-deadline:
			close(stop)
			t.Fatalf("only %d of %d participants ever received media", connected, n)
		case <-time.After(200 * time.Millisecond):
		}
	}
	timeToMedia := time.Since(pubStart)

	// --- steady state -----------------------------------------------------
	settle := meter.Snapshot()
	settleAt := time.Now()
	const measure = 5 * time.Second
	time.Sleep(measure)

	after := meter.Snapshot()
	elapsed := time.Since(settleAt)
	peakGoroutines := liveGoroutines()

	close(stop)

	// --- account ----------------------------------------------------------
	delta := func(c ledger.Class) int64 { return after.Bytes[c] - settle.Bytes[c] }
	deltaPkts := func(c ledger.Class) int64 { return after.Packets[c] - settle.Packets[c] }

	payload := delta(ledger.ClassMediaPayload)
	rtpHdr := delta(ledger.ClassRTPHeader)
	srtp := delta(ledger.ClassSRTPTag)
	transport := delta(ledger.ClassTransport)
	ip := delta(ledger.ClassIP)
	eth := delta(ledger.ClassEthernet)
	total := after.Total() - settle.Total()
	pkts := deltaPkts(ledger.ClassMediaPayload)

	secs := elapsed.Seconds()
	mbps := func(b int64) float64 { return float64(b) * 8 / secs / 1e6 }

	st := r.Stats()
	feeds := len(st.Feeds)

	t.Log("")
	t.Logf("  %d-PERSON MEETING, MEASURED (worst case: everyone sees everyone)", n)
	t.Log("  " + hyphens(66))
	t.Logf("  peers                      %6d", st.Peers)
	t.Logf("  published tracks           %6d", st.Tracks)
	t.Logf("  active forwards (feeds)    %6d   (n*(n-1) = %d)", feeds, n*(n-1))
	t.Logf("  peer connections on server %6d   (2 per participant)", st.Peers*2)
	t.Log("")
	t.Logf("  time to create clients     %6.2f s", joinSetup.Seconds())
	t.Logf("  time to first media, all   %6.2f s", timeToMedia.Seconds())
	t.Logf("  goroutines (peak / base)   %6d / %d", peakGoroutines, before)
	t.Log("")
	t.Logf("  over %.1f s of steady state:", secs)
	t.Logf("    media payload            %10d B   %7.2f Mbit/s", payload, mbps(payload))
	t.Logf("    RTP headers              %10d B   %7.2f Mbit/s", rtpHdr, mbps(rtpHdr))
	t.Logf("    SRTP auth tags           %10d B   %7.2f Mbit/s", srtp, mbps(srtp))
	t.Logf("    UDP                      %10d B   %7.2f Mbit/s", transport, mbps(transport))
	t.Logf("    IP                       %10d B   %7.2f Mbit/s", ip, mbps(ip))
	t.Logf("    Ethernet                 %10d B   %7.2f Mbit/s", eth, mbps(eth))
	t.Logf("    TOTAL EGRESS             %10d B   %7.2f Mbit/s", total, mbps(total))
	t.Log("")
	if pkts > 0 {
		t.Logf("    packets forwarded        %10d   (%.0f/s)", pkts, float64(pkts)/secs)
		t.Logf("    bytes per packet, wire   %10.1f B", float64(total)/float64(pkts))
		t.Logf("    payload efficiency       %10.1f%%", 100*float64(payload)/float64(total))
	}
	if n > 0 {
		t.Logf("    per participant, egress  %10.2f Mbit/s", mbps(total)/float64(n))
	}
	t.Log("")

	if total == 0 {
		t.Fatal("a room of real clients forwarded nothing; the measurement is empty")
	}
	if feeds == 0 {
		t.Fatal("no active forwards; nobody was subscribed to anybody")
	}
}

func hyphens(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

// mustTrack adds a video track to the publisher connection and completes the
// offer/answer, returning the track to write on.
func (c *client) mustTrack(trackID string) rtpWriter {
	c.t.Helper()
	track, err := newLocalTrack(trackID)
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
	return track
}

type rtpWriter interface {
	WriteRTP(*rtp.Packet) error
}

func newLocalTrack(trackID string) (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		trackID, "stream-"+trackID)
}
