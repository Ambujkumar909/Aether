package signal

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// A hundred people on the socket media path, measured, not modelled: real
// sockets, real frames, the real per-connection encoder and coalescer.
//
// Two shapes of the same room:
//   - three people talking (the rest muted, and a muted sender sends
//     nothing since the client change that goes with this test): 3 x 50
//     audio frames a second, each relayed to 99 others;
//   - everyone unmuted and "talking" (the worst case, and what the room cost
//     before muted senders went silent): 100 x 50 frames a second.
//
// Accuracy first: every recipient must receive every frame that was meant
// for it, in order. Then the cost: wall time per second of audio, and the
// bytes each recipient had to take. Run with -v to see the figures.
func TestHundredPersonRoomRelaysEveryAudioFrameToEveryoneElse(t *testing.T) {
	if testing.Short() {
		t.Skip("100 real sockets")
	}
	const N = 100
	const fps = 50 // 20 ms Opus chunks
	meter := ledger.NewMeter("test")
	g := NewGateway(room.NewRegistry(meter), nil, meter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rm := g.Rooms.GetOrCreate("big")

	type client struct {
		id    string
		conn  *Conn
		cli   *websocket.Conn
		got   atomic.Int64 // media frames received
		bytes atomic.Int64
	}
	clients := make([]*client, N)
	var readers sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Per recipient, per speaker: the last timestamp seen, to prove order.
	order := make([]map[string]uint64, N)
	var orderMu sync.Mutex
	disorder := atomic.Int64{}
	for i := 0; i < N; i++ {
		c, cli := newTestConn(t)
		id := fmt.Sprintf("p%03d", i)
		clients[i] = &client{id: id, conn: c, cli: cli}
		order[i] = map[string]uint64{}
		g.mu.Lock()
		g.byID[id] = c
		g.mu.Unlock()
		if _, err := rm.Join(&room.Participant{ID: id, DisplayName: id, Send: func([]byte) {}, Addr: fmt.Sprintf("10.0.%d.%d", i/250, i%250)}); err != nil {
			t.Fatal(err)
		}
		readers.Add(1)
		go func(i int, cl *client) {
			defer readers.Done()
			_, rx := wire.NewPreloadedPair()
			for {
				_, data, err := cl.cli.Read(ctx)
				if err != nil {
					return
				}
				cl.bytes.Add(int64(len(data)))
				// A message may batch several frames (the coalescer).
				each := func(fn func([]byte) error) {
					r := wire.NewReader(data, rx)
					h, err := r.ReadHeader()
					if err != nil {
						return
					}
					if h.Op == wire.OpBatch {
						// SplitBatch walks the body: the reader is already past the header.
						_ = wire.SplitBatch(r, fn)
						return
					}
					_ = fn(data)
				}
				each(func(frame []byte) error {
					fr := wire.NewReader(frame, rx)
					h, err := fr.ReadHeader()
					if err != nil || h.Op != wire.OpMediaFrame {
						return nil
					}
					who := fr.Str()
					kind := fr.Bits(1)
					fr.Bits(1)
					fr.Bits(6)
					if kind == 1 {
						fr.Uvarint()
					}
					body := fr.Tail()
					if len(body) < 8 {
						return nil
					}
					ts := binary.BigEndian.Uint64(body)
					cl.got.Add(1)
					orderMu.Lock()
					if prev, ok := order[i][who]; ok && ts <= prev {
						disorder.Add(1)
					}
					order[i][who] = ts
					orderMu.Unlock()
					return nil
				})
			}
		}(i, clients[i])
	}

	relay := func(speakers int, seconds int) (time.Duration, time.Duration, int64) {
		for _, cl := range clients {
			cl.got.Store(0)
			cl.bytes.Store(0)
		}
		orderMu.Lock()
		for i := range order {
			order[i] = map[string]uint64{}
		}
		orderMu.Unlock()
		payload := make([]byte, 120) // a 48 kbit/s x 20 ms Opus frame; the first 8 bytes carry the sequence
		start := time.Now()
		frames := fps * seconds
		tick := time.NewTicker(time.Second / fps)
		defer tick.Stop()
		var busy time.Duration // time the relay itself took, out of the wall time
		for f := 0; f < frames; f++ {
			<-tick.C
			t0 := time.Now()
			for s := 0; s < speakers; s++ {
				sp := clients[s]
				binary.BigEndian.PutUint64(payload, uint64(f+1))
				g.announce(rm, sp.id, func(w *wire.Writer) {
					w.WriteHeader(wire.OpMediaFrame, 0, false)
					w.Str(sp.id)
					w.Bits(0, 1)
					w.Bits(1, 1)
					w.Bits(0, 6)
					w.Raw(payload)
				})
			}
			busy += time.Since(t0)
		}
		elapsed := time.Since(start)
		t.Logf("  %d speakers: relay work %v of %v wall (%.1f%% of one core), %.0f µs per frame fanned out to %d",
			speakers, busy.Round(time.Millisecond), elapsed.Round(time.Millisecond), 100*busy.Seconds()/elapsed.Seconds(),
			float64(busy.Microseconds())/float64(frames*speakers), N-1)
		// Let the coalescers flush and the readers drain.
		want := int64(frames) * int64(speakers)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			all := true
			for i, cl := range clients {
				w := want
				if i < speakers {
					w = want - int64(frames) // a speaker does not get its own
				}
				if cl.got.Load() < w {
					all = false
					break
				}
			}
			if all {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		var total int64
		for i, cl := range clients {
			w := want
			if i < speakers {
				w = want - int64(frames)
			}
			if got := cl.got.Load(); got != w {
				t.Fatalf("%d speakers: recipient %s got %d frames, want %d", speakers, cl.id, got, w)
			}
			total += cl.bytes.Load()
		}
		if d := disorder.Load(); d != 0 {
			t.Fatalf("%d speakers: %d frames arrived out of order", speakers, d)
		}
		return elapsed, busy, total
	}

	// Three talking, ninety-seven muted (and silent on the wire).
	el3, busy3, bytes3 := relay(3, 2)
	perRecipient3 := bytes3 / N / 2 // per second
	t.Logf("3 speakers x 2 s: relayed %d frames to 99 recipients each in %v (%.1f%% of real time); %d B/s per recipient",
		3*fps*2, el3.Round(time.Millisecond), 100*el3.Seconds()/2, perRecipient3)
	// Real time means the relay's own work for 2 s of audio fits in 2 s (with
	// headroom). Wall time is the wrong measure: the frames are paced by a
	// ticker, and on a loaded machine (the whole suite under -race) the ticker
	// itself falls behind while the relay sits idle -- that once read 3.4 s of
	// wall for 1.5 s of work and failed a relay that was keeping up.
	if busy3 > 2500*time.Millisecond {
		t.Fatalf("three speakers could not be relayed in real time: %v of relay work for 2 s of audio (%v wall)", busy3, el3)
	}
	// Everyone unmuted and sending: the ceiling.
	el100, _, bytes100 := relay(N, 1)
	perRecipient100 := bytes100 / N
	t.Logf("100 speakers x 1 s: relayed %d frames (%d deliveries) in %v (%.0f%% of real time); %d B/s per recipient",
		N*fps, N*fps*(N-1), el100.Round(time.Millisecond), 100*el100.Seconds(), perRecipient100)

	cancel()
	for _, cl := range clients {
		_ = cl.cli.CloseNow()
	}
	readers.Wait()
	_ = meter
}
