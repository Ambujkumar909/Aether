package signal

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// A receiver that is behind loses media, not the meeting: video from a
// publisher is dropped until that publisher's next key frame (asking for one
// once), audio frame by frame, and the connection stays up.
func TestMediaDropsWholeFramesUntilAKeyFrame(t *testing.T) {
	c, _ := newTestConn(t)
	t.Cleanup(func() { c.Close() })
	// Hold the flush: this test fills the queue by hand.
	c.mu.Lock()
	c.timer = time.AfterFunc(time.Hour, func() {})
	c.mu.Unlock()
	frame := func(w *wire.Writer) { w.WriteHeader(wire.OpMediaFrame, 0, false); w.Raw(make([]byte, 64)) }
	behind := func(on bool) {
		c.mu.Lock()
		if on {
			c.pendingB = MaxQueuedMediaBytes
		} else {
			c.pendingB = 0
		}
		c.mu.Unlock()
	}
	queued := func() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pending) }

	if c.EmitMedia("p", true, false, 100, frame) || queued() != 1 {
		t.Fatal("a delta with room for it is queued")
	}
	behind(true)
	if !c.EmitMedia("p", true, false, 100, frame) {
		t.Fatal("the first video drop asks for a key frame")
	}
	if c.EmitMedia("p", true, false, 100, frame) {
		t.Fatal("asked twice for one gap")
	}
	if c.EmitMedia("p", false, false, 100, frame) || queued() != 1 {
		t.Fatal("audio over the budget is dropped, and asks for nothing")
	}
	// A key frame that arrives while still behind is lost too: ask again.
	if !c.EmitMedia("p", true, true, 100, frame) {
		t.Fatal("a dropped key frame does not ask for another")
	}
	behind(false)
	// Room again, but p's deltas stay dropped until its key frame.
	c.EmitMedia("p", true, false, 100, frame)
	if queued() != 1 {
		t.Fatal("a delta after a gap was sent before the key frame")
	}
	// Another publisher is not affected.
	c.EmitMedia("q", true, false, 100, frame)
	if queued() != 2 {
		t.Fatal("q was held for p's gap")
	}
	c.EmitMedia("p", true, true, 100, frame)
	c.EmitMedia("p", true, false, 100, frame)
	if queued() != 4 {
		t.Fatalf("after the key frame, p flows again: %d queued", queued())
	}
	if c.MediaDrops() != 5 {
		t.Fatalf("drops counted: %d, want 5", c.MediaDrops())
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		t.Fatal("dropping media closed the connection")
	}
}
