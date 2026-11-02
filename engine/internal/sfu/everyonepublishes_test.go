package sfu

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// Everybody can publish everything, at the same time.
//
// The reported worry was that participants "are not able to share anything",
// so this pins down that there is no cap, no first-come-first-served slot, and
// no interference between publishers: every participant sends a camera AND a
// screen simultaneously, and every one of those tracks is registered and
// fanned out.
//
// Deliberately more than a smoke test of two peers. The failure modes that
// matter here only appear with many publishers renegotiating at once, which is
// what a room full of people turning their cameras on looks like.
func TestEveryParticipantCanPublishCameraAndScreen(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	const n = 12 // 24 tracks, 12*11*2 = 264 potential feeds

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("everyone", ledger.NewMeter("everyone"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	clients := make([]*client, n)
	for i := range clients {
		clients[i] = newClient(t, r, fmt.Sprintf("p%02d", i))
	}

	// Everyone publishes a camera, then everyone adds a screen, which is the
	// order a real meeting does it in.
	pubs := make([]*controllablePublisher, 0, n*2)
	for i, c := range clients {
		pubs = append(pubs, c.publishControllable(fmt.Sprintf("cam-%02d", i)))
	}
	for i, c := range clients {
		pubs = append(pubs, c.publishControllable(fmt.Sprintf("screen-%02d", i)))
	}
	defer func() {
		for _, p := range pubs {
			p.close()
		}
	}()

	want := n * 2
	deadline := time.After(120 * time.Second)
	for {
		if got := r.Stats().Tracks; got == want {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d of %d tracks were registered; some participants "+
				"could not publish", r.Stats().Tracks, want)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("%d participants published %d tracks (camera + screen each)", n, want)

	// Every participant must be receiving from somebody.
	deadline = time.After(90 * time.Second)
	for {
		receiving := 0
		for _, c := range clients {
			if c.received.Load() > 0 {
				receiving++
			}
		}
		if receiving == n {
			break
		}
		select {
		case <-deadline:
			receiving = 0
			for _, c := range clients {
				if c.received.Load() > 0 {
					receiving++
				}
			}
			t.Fatalf("only %d of %d participants received any media", receiving, n)
		case <-time.After(200 * time.Millisecond):
		}
	}

	feeds := len(r.Stats().Feeds)
	t.Logf("all %d participants receiving; %d active forwards", n, feeds)
	if feeds == 0 {
		t.Fatal("no forwards registered")
	}

	// Nobody is sent their own media back, however many tracks they publish.
	for i, c := range clients {
		for _, f := range r.Stats().Feeds {
			if f.Subscriber == fmt.Sprintf("p%02d", i) &&
				f.Publisher == fmt.Sprintf("p%02d", i) {
				t.Fatalf("participant %d is subscribed to their own track %s", i, f.Publisher)
			}
		}
		_ = c
	}
}

// A screen declared by its publisher must be judged as a screen by the
// allocator, and the declaration must be revocable.
func TestScreenDeclarationReachesTheAllocator(t *testing.T) {
	r := NewRouter("decl", ledger.NewMeter("decl"), quiet(), nil, nil, nil)
	r.Start()
	defer r.Stop()

	cam := NewPublishedTrack("cam-1", "alice", webrtc.RTPCodecTypeVideo,
		webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), func(LayerID) {})
	screen := NewPublishedTrack("screen-1", "alice", webrtc.RTPCodecTypeVideo,
		webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), func(LayerID) {})

	r.mu.Lock()
	r.tracks["cam-1"] = cam
	r.tracks["screen-1"] = screen
	r.mu.Unlock()

	if cam.IsScreenShare() || screen.IsScreenShare() {
		t.Fatal("a browser track was guessed to be a screen before anyone said so")
	}

	r.MarkScreenTrack("alice", "screen-1")
	if !screen.IsScreenShare() {
		t.Fatal("the declared screen track was not marked")
	}
	if cam.IsScreenShare() {
		t.Fatal("the camera was marked as a screen")
	}

	// Stopping the share clears it, or the camera inherits screen weighting
	// for the rest of the meeting.
	r.MarkScreenTrack("alice", "")
	if screen.IsScreenShare() {
		t.Fatal("stopping the share left the track marked as a screen")
	}

	// Another participant's declaration must not touch alice's tracks.
	bob := NewPublishedTrack("cam-2", "bob", webrtc.RTPCodecTypeVideo,
		webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), func(LayerID) {})
	r.mu.Lock()
	r.tracks["cam-2"] = bob
	r.mu.Unlock()
	r.MarkScreenTrack("bob", "cam-2")
	if !bob.IsScreenShare() {
		t.Fatal("bob's declaration did not apply")
	}
	if cam.IsScreenShare() || screen.IsScreenShare() {
		t.Fatal("bob's declaration leaked onto alice's tracks")
	}
}
