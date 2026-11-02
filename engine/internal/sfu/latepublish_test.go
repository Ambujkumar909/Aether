package sfu

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

// Publishing a track after the session is already running.
//
// This is the exact path a screen share takes when the sharer has no working
// camera: they joined publishing nothing, and now a video track has to appear
// on a connection that is already negotiated and carrying traffic. It is also
// the path taken by anyone who joins muted and later turns their camera on,
// which is most people.
//
// Nothing covered it before. Every media test in this package published its
// track up front, during setup, which is the one case that does NOT require
// renegotiating a live connection.

func TestPublishingAfterJoinReachesSubscribers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("late", ledger.NewMeter("late"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	// Both are connected and idle. Nobody has published anything, which is
	// what a room of people who joined with their cameras off looks like.
	time.Sleep(1500 * time.Millisecond)
	if n := r.Stats().Tracks; n != 0 {
		t.Fatalf("%d tracks published before anyone shared anything", n)
	}
	if got := bob.received.Load(); got != 0 {
		t.Fatalf("bob received %d packets from an empty room", got)
	}

	// Alice now shares her screen: a brand new track on an already
	// established, already negotiated connection.
	pub := alice.publishControllable("alice-screen")
	defer pub.close()

	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatal("bob never received the track alice published after joining; " +
			"screen sharing without a camera would appear to do nothing")
	}

	deadline := time.After(30 * time.Second)
	for bob.received.Load() < 20 {
		select {
		case <-deadline:
			t.Fatalf("bob received only %d packets from a late-published track",
				bob.received.Load())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Logf("a track published mid-session reached the subscriber: %d packets",
		bob.received.Load())

	if n := r.Stats().Tracks; n != 1 {
		t.Fatalf("router reports %d tracks after one late publish", n)
	}
	if alice.received.Load() != 0 {
		t.Fatal("alice was sent her own screen share back")
	}
}

// Somebody already publishing a camera who then starts sharing a screen adds a
// SECOND track. Both must reach subscribers: a client that replaces the camera
// with the screen sends one track, but a client that shares alongside it sends
// two, and the SFU must not lose either.
func TestTwoTracksFromOnePublisherBothArrive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("two", ledger.NewMeter("two"), log, nil, nil, nil)
	r.Start()
	defer r.Stop()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	cam := alice.publishControllable("alice-cam")
	defer cam.close()

	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatal("bob never received the camera")
	}
	for bob.received.Load() < 10 {
		time.Sleep(100 * time.Millisecond)
	}

	// Now the screen, alongside the camera.
	screen := alice.publishControllable("alice-screen")
	defer screen.close()

	deadline := time.After(30 * time.Second)
	for {
		if r.Stats().Tracks == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("router reports %d tracks; a second track from the same "+
				"publisher was lost", r.Stats().Tracks)
		case <-time.After(100 * time.Millisecond):
		}
	}

	// And bob must be subscribed to both, not just the first.
	feeds := r.Stats().Feeds
	if len(feeds) != 2 {
		t.Fatalf("bob has %d feeds from a publisher sending two tracks", len(feeds))
	}
	t.Logf("both tracks from one publisher are forwarded: %d feeds", len(feeds))
}

// A publisher who joins, publishes, stops, and publishes again must work every
// time. Rapid toggling is the reported bug, and the server half of it is that
// nothing must accumulate or wedge across cycles.
func TestRepeatedPublishCyclesStayClean(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	before := baseline()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter("cycle", ledger.NewMeter("cycle"), log, nil, nil, nil)
	r.Start()

	alice := newClient(t, r, "alice")
	bob := newClient(t, r, "bob")

	pub := alice.publishControllable("alice-cam")
	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatal("bob never received the track")
	}

	// Ten on/off cycles as fast as the pipeline allows.
	for i := 0; i < 10; i++ {
		pub.off()
		time.Sleep(60 * time.Millisecond)
		pub.on()
		time.Sleep(60 * time.Millisecond)
	}

	// After all that, media must still be flowing.
	settled := bob.received.Load()
	time.Sleep(1500 * time.Millisecond)
	delivered := bob.received.Load() - settled

	t.Logf("after 10 on/off cycles, %d packets still flowing", delivered)
	if delivered == 0 {
		t.Fatal("media stopped after repeated toggling; the pipeline wedged")
	}
	if n := r.Stats().Tracks; n != 1 {
		t.Fatalf("%d tracks registered after toggling one track", n)
	}

	// Tear down both ends before asserting. The client connections belong to
	// this test, not to the server, and leaving them open would measure the
	// harness rather than the product.
	pub.close()
	alice.close()
	bob.close()
	r.Stop()
	if n, ok := settleTo(before, 10*time.Second); !ok {
		t.Fatalf("toggling leaked goroutines: %d -> %d", before, n)
	}
}
