package signal

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// presenterFixture: three participants on real sockets, and a way to feed a
// client frame through the real dispatcher as that participant sent it.
type presenterFixture struct {
	t     *testing.T
	g     *Gateway
	rm    *room.Room
	parts map[string]*room.Participant
	conns map[string]*Conn
	rx    map[string]<-chan []byte
	dicts map[string]*wire.Dict // each client's receive dictionary, as a real client keeps
}

func newPresenterFixture(t *testing.T, ids ...string) *presenterFixture {
	meter := ledger.NewMeter("test")
	f := &presenterFixture{t: t, g: NewGateway(room.NewRegistry(meter), nil, meter, slog.New(slog.NewTextHandler(io.Discard, nil))),
		parts: map[string]*room.Participant{}, conns: map[string]*Conn{}, rx: map[string]<-chan []byte{}, dicts: map[string]*wire.Dict{}}
	f.rm = f.g.Rooms.GetOrCreate("r")
	for _, id := range ids {
		c, cli := newTestConn(t)
		f.conns[id] = c
		f.rx[id] = frames(t, cli)
		_, f.dicts[id] = wire.NewPreloadedPair()
		f.g.mu.Lock()
		f.g.byID[id] = c
		f.g.mu.Unlock()
		p := &room.Participant{ID: id, DisplayName: id, Send: func([]byte) {}}
		if _, err := f.rm.Join(p); err != nil {
			t.Fatal(err)
		}
		f.parts[id] = p
	}
	return f
}

func (f *presenterFixture) send(id string, enc func(w *wire.Writer)) {
	f.t.Helper()
	w := wire.NewWriter(nil)
	enc(w)
	r := wire.NewReader(w.Bytes(), nil)
	h, err := r.ReadHeader()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.dispatch(f.rm, nil, nil, f.parts[id], f.conns[id], h, r); err != nil {
		f.t.Fatalf("dispatch from %s: %v", id, err)
	}
}

func (f *presenterFixture) share(id string, on bool) {
	f.send(id, func(w *wire.Writer) {
		s := wire.State{Participant: id, Sharing: on}
		if on {
			s.ScreenTrack = id + "-screen"
		}
		s.Encode(w)
	})
}

// screenFrame: a socket-path video frame with the screen flag, as a client sends it.
func (f *presenterFixture) screenFrame(id string) {
	f.send(id, func(w *wire.Writer) {
		w.WriteHeader(wire.OpMediaFrame, 0, false)
		w.Bits(1, 1) // video
		w.Bits(1, 1) // key
		w.Bits(1, 6) // screen
		w.Uvarint(1)
		w.Raw([]byte{9, 9, 9})
	})
}

// drain collects every frame that arrives at id within d, decoded to
// (op, first string) pairs.
type seen struct {
	op   wire.Op
	a, b string
}

func (f *presenterFixture) drain(id string, d time.Duration) []seen {
	var out []seen
	deadline := time.After(d)
	for {
		select {
		case data := <-f.rx[id]:
			// The server coalesces frames into batches; walk them.
			var one func([]byte) error
			one = func(frame []byte) error {
				r := wire.NewReader(frame, f.dicts[id])
				h, err := r.ReadHeader()
				if err != nil {
					return nil
				}
				switch h.Op {
				case wire.OpBatch:
					return wire.SplitBatch(r, one)
				case wire.OpScreenShare:
					a := r.Str()
					b := r.Str()
					out = append(out, seen{h.Op, a, b})
				case wire.OpMediaFrame:
					out = append(out, seen{h.Op, r.Str(), ""})
				case wire.OpSpotlight:
					// The whole list, joined, as a: "" when empty.
					n := r.Uvarint()
					var ids []string
					for range n {
						ids = append(ids, r.Str())
					}
					out = append(out, seen{h.Op, strings.Join(ids, ","), ""})
				case wire.OpTogether:
					// a: "state" or the sender; b: on/scene or the mask length.
					if r.Bits(3) == togState {
						on := r.Bool()
						r.Bits(4)
						if on {
							r.Str()
							out = append(out, seen{h.Op, "state", fmt.Sprint("on ", r.Uvarint())})
						} else {
							out = append(out, seen{h.Op, "state", "off"})
						}
					} else {
						r.Bits(5)
						from := r.Str()
						out = append(out, seen{h.Op, from, fmt.Sprint(len(r.Blob()))})
					}
				case wire.OpRecording:
					// a: "state" or the speaker; b: the flags or the text.
					kind := r.Bits(3)
					if kind == recRefused {
						what, reason := r.Bits(2), r.Bits(1)
						out = append(out, seen{h.Op, "refused", fmt.Sprint(what, reason)})
						break
					}
					if kind == recState {
						rec, tr, capt := r.Bool(), r.Bool(), r.Bool()
						r.Bits(2)
						if rec {
							r.Str()
							r.Uvarint()
						}
						if tr {
							r.Str()
							r.Uvarint()
						}
						out = append(out, seen{h.Op, "state", fmt.Sprint(rec, tr, capt)})
					} else {
						final := r.Bool()
						r.Bits(4)
						sp := r.Str()
						r.Uvarint()
						out = append(out, seen{h.Op, sp, fmt.Sprint(final, ":", string(r.Blob()))})
					}
				case wire.OpReaction:
					who := r.Str()
					out = append(out, seen{h.Op, who, r.Str()})
				case wire.OpStateDelta:
					// Decoded in full: its strings teach the dictionary, and a
					// client that skips them reads later references as blanks.
					var st wire.State
					_ = st.Decode(r)
					out = append(out, seen{h.Op, st.Participant, ""})
				default:
					out = append(out, seen{op: h.Op})
				}
				return nil
			}
			_ = one(data)
		case <-deadline:
			return out
		}
	}
}

func has(ss []seen, op wire.Op, a, b string) bool {
	for _, s := range ss {
		if s.op == op && s.a == a && (b == "*" || s.b == b) {
			return true
		}
	}
	return false
}

// a presents; b starts presenting: everybody -- a, b and c -- is told a
// stopped and b took over, a's screen frames stop reaching c at once, b's
// reach c. a's late "I stopped" does not take the stage from b; b stopping
// frees it. Through the real dispatcher, real sockets.
func TestStartingAShareTakesOverTheStage(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")

	f.share("a", true)
	first := f.drain("c", 400*time.Millisecond)
	if has(first, wire.OpScreenShare, "*", "*") || func() bool {
		for _, s := range first {
			if s.op == wire.OpScreenShare {
				return true
			}
		}
		return false
	}() {
		t.Fatalf("the first presenter caused a takeover notice: %+v", first)
	}
	f.screenFrame("a")
	if got := f.drain("c", 400*time.Millisecond); !has(got, wire.OpMediaFrame, "a", "") {
		t.Fatalf("c did not get the first presenter's screen: %+v (presenter %q, may %v)", got, f.rm.Presenter(), f.rm.May("a", "share"))
	}

	f.share("b", true)
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 600*time.Millisecond); !has(got, wire.OpScreenShare, "a", "b") {
			t.Fatalf("%s was not told a stopped and b took over: %+v", id, got)
		}
	}
	if f.rm.Presenter() != "b" {
		t.Fatalf("presenter %q, want b", f.rm.Presenter())
	}

	// a's client has not stopped yet and keeps sending its screen: refused.
	f.screenFrame("a")
	f.screenFrame("b")
	got := f.drain("c", 500*time.Millisecond)
	if has(got, wire.OpMediaFrame, "a", "") {
		t.Fatal("a's screen still reached c after b took over")
	}
	if !has(got, wire.OpMediaFrame, "b", "") {
		t.Fatal("b's screen did not reach c")
	}

	// a's client stops (late): b keeps the stage.
	f.share("a", false)
	if f.rm.Presenter() != "b" {
		t.Fatalf("a's late stop took the stage from b (presenter %q)", f.rm.Presenter())
	}
	// b stops: nobody presents, and anybody may start.
	f.share("b", false)
	if f.rm.Presenter() != "" || !f.rm.May("a", "share") {
		t.Fatalf("the stage was not freed (presenter %q)", f.rm.Presenter())
	}
	f.drain("c", 200*time.Millisecond)
	f.share("c", true)
	if got := f.drain("a", 400*time.Millisecond); func() bool {
		for _, s := range got {
			if s.op == wire.OpScreenShare {
				return true
			}
		}
		return false
	}() {
		t.Fatal("starting on a free stage announced a takeover")
	}
}
