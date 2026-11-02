package signal

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// Media carried over the socket is relayed to everyone in the room except
// the one who sent it. Real sockets: the frame is read off the peer's
// client connection, and the sender's client connection stays silent.
func TestSocketMediaIsNeverRelayedToItsSender(t *testing.T) {
	meter := ledger.NewMeter("test")
	g := NewGateway(room.NewRegistry(meter), nil, meter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rm := g.Rooms.GetOrCreate("r")

	connA, cliA := newTestConn(t)
	connB, cliB := newTestConn(t)
	g.mu.Lock()
	g.byID["a"] = connA
	g.byID["b"] = connB
	g.mu.Unlock()
	for _, id := range []string{"a", "b"} {
		if _, err := rm.Join(&room.Participant{ID: id, DisplayName: id, Send: func([]byte) {}}); err != nil {
			t.Fatal(err)
		}
	}

	// "a" sent an audio frame; the gateway announces it to the room minus a.
	g.announce(rm, "a", func(w *wire.Writer) {
		w.WriteHeader(wire.OpMediaFrame, 0, false)
		w.Str("a")
		w.Bits(0, 1)
		w.Bits(1, 1)
		w.Bits(0, 6)
		w.Raw([]byte{1, 2, 3})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := cliB.Read(ctx)
	if err != nil {
		t.Fatalf("b heard nothing: %v", err)
	}
	if typ != websocket.MessageBinary || len(data) == 0 {
		t.Fatalf("b got %v %d bytes", typ, len(data))
	}
	_, rx := wire.NewPreloadedPair()
	_ = rx
	r := wire.NewReader(data, nil)
	h, err := r.ReadHeader()
	if err != nil || h.Op != wire.OpMediaFrame {
		t.Fatalf("b got op %v err %v", h.Op, err)
	}

	quiet, cancelQuiet := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelQuiet()
	if _, back, err := cliA.Read(quiet); err == nil {
		t.Fatalf("the sender was relayed its own media: %d bytes", len(back))
	}
}
