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

// frames drains a client socket into a channel for the life of the test.
// (A Read with a context that expires closes a coder/websocket connection,
// so "nothing arrives within N ms" must be checked on a channel, not by a
// timed Read.)
func frames(t *testing.T, cli *websocket.Conn) <-chan []byte {
	ch := make(chan []byte, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			_, d, err := cli.Read(ctx)
			if err != nil {
				return
			}
			ch <- d
		}
	}()
	return ch
}

func sameDeviceFrame(t *testing.T, d []byte) uint64 {
	t.Helper()
	r := wire.NewReader(d, nil)
	h, err := r.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if h.Op != wire.OpSameDevice {
		t.Fatalf("op %v, want SameDevice", h.Op)
	}
	return r.Uvarint()
}

func expectCount(t *testing.T, who string, ch <-chan []byte, want uint64) {
	t.Helper()
	select {
	case d := <-ch:
		if n := sameDeviceFrame(t, d); n != want {
			t.Fatalf("%s: told %d others, want %d", who, n, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: told nothing, want %d others", who, want)
	}
}

func expectNothing(t *testing.T, who string, ch <-chan []byte) {
	t.Helper()
	select {
	case d := <-ch:
		t.Fatalf("%s: told something (%d bytes), want nothing", who, len(d))
	case <-time.After(300 * time.Millisecond):
	}
}

// Two participants from one address: both are told, with the count. A
// third from another address is told nothing, and its arrival tells the
// pair nothing new. A third from the shared address updates everyone on
// it. Real sockets.
func TestSameDeviceAdvisoryReachesExactlyThoseWhoShareAnAddress(t *testing.T) {
	meter := ledger.NewMeter("test")
	g := NewGateway(room.NewRegistry(meter), nil, meter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rm := g.Rooms.GetOrCreate("r")
	conns := map[string]*Conn{}
	chans := map[string]<-chan []byte{}
	for _, id := range []string{"a", "b", "c", "d"} {
		c, cli := newTestConn(t)
		conns[id] = c
		chans[id] = frames(t, cli)
	}
	g.mu.Lock()
	for id, c := range conns {
		g.byID[id] = c
	}
	g.mu.Unlock()
	join := func(id, addr string) {
		p := &room.Participant{ID: id, DisplayName: id, Send: func([]byte) {}, Addr: addr}
		if _, err := rm.Join(p); err != nil {
			t.Fatal(err)
		}
		g.adviseSameDevice(rm, p)
	}

	join("a", "192.168.1.20")
	expectNothing(t, "a alone", chans["a"])
	join("b", "192.168.1.20")
	expectCount(t, "a", chans["a"], 1)
	expectCount(t, "b", chans["b"], 1)
	join("c", "192.168.1.77")
	expectNothing(t, "c, another address", chans["c"])
	expectNothing(t, "a on c's arrival", chans["a"])
	join("d", "192.168.1.20")
	expectCount(t, "d", chans["d"], 2)
	expectCount(t, "a on d's arrival", chans["a"], 2)
	expectCount(t, "b on d's arrival", chans["b"], 2)
	expectNothing(t, "c on d's arrival", chans["c"])
	// No address at all (a test double, a proxy that hid it): never advised.
	if n := rm.SameAddr("", "a"); n != 0 {
		t.Fatalf("an empty address matched %d", n)
	}
}
