package signal

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// A keyframe request naming a socket publisher reaches that publisher once
// per window however many watchers ask; a publisher not in the room, or the
// asker themselves, gets nothing. Real sockets.
func TestKeyframeRequestReachesSocketPublisherOnce(t *testing.T) {
	meter := ledger.NewMeter("test")
	g := NewGateway(room.NewRegistry(meter), nil, meter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rm := g.Rooms.GetOrCreate("r")
	pubConn, pubCli := newTestConn(t)
	g.mu.Lock()
	g.byID["pub"] = pubConn
	g.mu.Unlock()
	for _, id := range []string{"pub", "w1", "w2", "w3"} {
		if _, err := rm.Join(&room.Participant{ID: id, DisplayName: id, Send: func([]byte) {}}); err != nil {
			t.Fatal(err)
		}
	}
	got := frames(t, pubCli)
	// Three watchers stumble at once.
	g.forwardKeyframeReq(rm, "w1", "pub")
	g.forwardKeyframeReq(rm, "w2", "pub")
	g.forwardKeyframeReq(rm, "w3", "pub")
	select {
	case d := <-got:
		r := wire.NewReader(d, nil)
		h, err := r.ReadHeader()
		if err != nil || h.Op != wire.OpKeyframeReq {
			t.Fatalf("op %v err %v", h.Op, err)
		}
		if who := r.Str(); who != "pub" {
			t.Fatalf("names %q", who)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the publisher was not asked")
	}
	select {
	case d := <-got:
		t.Fatalf("asked twice inside the window (%d bytes)", len(d))
	case <-time.After(400 * time.Millisecond):
	}
	// After the window, a new request goes through.
	g.forwardKeyframeReq(rm, "w1", "pub")
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("not asked again after the window")
	}
	// Unknown publisher, or oneself: nothing.
	g.forwardKeyframeReq(rm, "w1", "nobody")
	g.forwardKeyframeReq(rm, "pub", "pub")
	select {
	case d := <-got:
		t.Fatalf("a request for nobody or oneself produced %d bytes", len(d))
	case <-time.After(400 * time.Millisecond):
	}
}
