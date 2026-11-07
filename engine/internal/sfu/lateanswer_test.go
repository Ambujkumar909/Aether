package sfu

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// Candidates that arrive before their description must still be used. The
// harness used to drop them, and on a loaded CI runner -- where they always
// came first -- media tests timed out with no remote candidate on either end.
func TestCandidatesBeforeTheirDescriptionStillConnect(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: brings up real ICE and DTLS")
	}
	r := NewRouter("late", ledger.NewMeter("late"), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	r.Start()
	defer r.Stop()
	slow := func(c *client) { c.sdpDelay = 200 * time.Millisecond }
	alice := newClientWith(t, r, "alice", webrtc.Configuration{}, slow)
	bob := newClientWith(t, r, "bob", webrtc.Configuration{}, slow)
	defer alice.publish("alice-cam")()
	select {
	case <-bob.gotTrack:
	case <-time.After(20 * time.Second):
		t.Fatalf("no track | alice: %s | bob: %s", alice.diag(), bob.diag())
	}
}
