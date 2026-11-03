package signal

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

func mediaFrom(ss []seen, who string) int {
	n := 0
	for _, s := range ss {
		if s.op == wire.OpMediaFrame && s.a == who {
			n++
		}
	}
	return n
}

// Over the socket, a camera goes only to those who have it on screen; one
// that comes into view is asked for a key frame; audio and a screen go to
// everyone regardless.
func TestSocketCamerasFollowTheViewport(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 200*time.Millisecond)
	}
	media := func(from string, kind, spare uint32) {
		f.send(from, func(w *wire.Writer) {
			w.WriteHeader(wire.OpMediaFrame, 0, false)
			w.Bits(kind, 1)
			w.Bits(0, 1)
			w.Bits(spare, 6)
			if kind == 1 {
				w.Uvarint(1)
			}
			w.Raw([]byte{1, 2, 3, 4})
		})
	}
	view := func(from string, visible map[string]bool) {
		f.send(from, func(w *wire.Writer) {
			v := wire.Viewport{}
			for id, on := range visible {
				v.Tiles = append(v.Tiles, wire.Tile{Track: id, Width: 320, Height: 180, Visible: on})
			}
			v.Encode(w)
		})
	}

	// Nobody has reported: c's camera reaches a and b.
	media("c", 1, 0)
	if mediaFrom(f.drain("a", 300*time.Millisecond), "c") != 1 || mediaFrom(f.drain("b", 300*time.Millisecond), "c") != 1 {
		t.Fatal("before any report, a camera goes to everyone")
	}

	// b shows a, not c.
	view("b", map[string]bool{"a": true, "c": false})
	f.drain("a", 300*time.Millisecond) // a was asked for a key frame
	media("c", 1, 0)
	if n := mediaFrom(f.drain("b", 300*time.Millisecond), "c"); n != 0 {
		t.Fatalf("b was sent a camera it is not showing (%d)", n)
	}
	if n := mediaFrom(f.drain("a", 300*time.Millisecond), "c"); n != 1 {
		t.Fatalf("a, who never reported, missed it (%d)", n)
	}
	// Audio and a screen still reach b.
	media("c", 0, 0)
	media("c", 1, 1)
	if n := mediaFrom(f.drain("b", 300*time.Millisecond), "c"); n != 2 {
		t.Fatalf("audio and screen to b: %d, want 2", n)
	}

	// c comes into b's view: c is asked for a key frame, and b is sent c's camera.
	view("b", map[string]bool{"a": true, "c": true})
	gotReq := false
	for _, s := range f.drain("c", 400*time.Millisecond) {
		if s.op == wire.OpKeyframeReq {
			gotReq = true
		}
	}
	if !gotReq {
		t.Fatal("c was not asked for a key frame when it came into view")
	}
	media("c", 1, 0)
	if n := mediaFrom(f.drain("b", 300*time.Millisecond), "c"); n != 1 {
		t.Fatalf("b, now showing c, got %d", n)
	}
}

// One sender's media is held to a byte allowance before the fan-out: far
// above any real encoder, but a flood of junk reaches the others as the
// burst, not as the flood.
func TestSocketMediaIsHeldToAByteAllowance(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 200*time.Millisecond)
	}
	const size = 100 << 10
	payload := make([]byte, size)
	start := time.Now()
	for range 120 { // 12 MB, against a 6 MB burst
		f.send("c", func(w *wire.Writer) {
			w.WriteHeader(wire.OpMediaFrame, 0, false)
			w.Bits(0, 1) // audio: no viewport, no key frames, to everyone
			w.Bits(0, 1)
			w.Bits(0, 6)
			w.Raw(payload)
		})
		time.Sleep(time.Millisecond)
	}
	took := time.Since(start).Seconds()
	n := mediaFrom(f.drain("b", 800*time.Millisecond), "c")
	allowed := (6e6 + took*25e6/8) / size
	if float64(n) > allowed+2 || n < 40 {
		t.Fatalf("12 MB sent in %.2f s: %d frames relayed, the allowance is about %.0f", took, n, allowed)
	}
}
