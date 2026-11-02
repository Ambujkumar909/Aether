package messaging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// ---------------------------------------------------------------------------
// Test scaffolding
// ---------------------------------------------------------------------------

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wsPair returns a genuinely connected pair of websockets.
//
// These tests deliberately use real connections rather than a fake, because
// what is being tested IS the close behaviour: which side sends the close
// frame, whether a second close panics, whether a write racing a close is
// safe. A fake that cannot get those wrong proves nothing about the code that
// can.
func wsPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		accepted <- c
		<-release // the handler must outlive the connection
	}))

	cli, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		close(release)
		ts.Close()
		t.Fatalf("dial: %v", err)
	}

	var srv *websocket.Conn
	select {
	case srv = <-accepted:
	case <-time.After(5 * time.Second):
		close(release)
		ts.Close()
		t.Fatal("server never accepted the websocket")
	}

	t.Cleanup(func() {
		_ = cli.CloseNow()
		_ = srv.CloseNow()
		close(release)
		ts.Close()
	})
	return srv, cli
}

// newSock builds a hub-attached socket over a real connection.
func newSock(t *testing.T, h *Hub, userID string, buffer int) (*sock, *websocket.Conn, *wire.Dict) {
	t.Helper()
	srv, cli := wsPair(t)
	tx, rx := wire.NewPreloadedPair()
	s := &sock{ws: srv, userID: userID, tx: tx, out: make(chan []byte, buffer)}
	h.add(s)
	return s, cli, rx
}

// goroutines returns the current count, after giving anything in the middle of
// exiting a chance to finish.
func goroutines() int {
	for i := 0; i < 50; i++ {
		runtime.Gosched()
	}
	return runtime.NumGoroutine()
}

// noLeaks fails the test if goroutines outlive it.
//
// Hand-rolled rather than pulling in a leak detector, because the whole point
// of this file is that teardown is verified rather than assumed, and that
// argument is weaker if the verification itself is an opaque dependency.
func noLeaks(t *testing.T) {
	t.Helper()
	before := goroutines()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			after := goroutines()
			if after <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutine leak: %d before, %d after\n%s", before, after, buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// ---------------------------------------------------------------------------
// The ordering invariant the interning dictionary depends on
// ---------------------------------------------------------------------------

// Frames must reach the wire in the order they were encoded.
//
// This is not a style preference, it is the precondition of the whole
// interning scheme. Encoding a string assigns it the next index in the
// socket's transmit dictionary and emits that index; the receiver learns the
// binding by seeing frames in the same order. If two frames are encoded in
// one order and queued in the other, the receiver binds index 0 to the
// SECOND string and index 1 to the first, and from that moment every
// interned string on the connection decodes to the wrong value. Silently,
// plausibly, and permanently: there is no checksum on a string index, and
// the only repair is a full reconnect that nothing knows to trigger.
//
// internal/wire/doc.go states the requirement. This test is what actually
// holds the Hub to it, because the Hub is the one place that encodes under a
// lock and then queues after releasing it.
func TestPushEncodeAndQueueAreAtomic(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	const total = 600
	s, cli, rx := newSock(t, h, "u1", total+16)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.writePump(ctx)

	// Many goroutines contend for the socket, which is exactly the shape of
	// real traffic: a message push, a presence change and a typing indicator
	// for the same user all originate from different request goroutines.
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h.PushMessage("u1", &Message{
				ConversationID: fmt.Sprintf("conv-%d", n),
				ID:             fmt.Sprintf("msg-%d", n),
				SenderID:       fmt.Sprintf("user-%d", n),
				SenderName:     fmt.Sprintf("name-%d", n),
				CreatedAt:      time.UnixMilli(int64(n)),
				Body:           fmt.Sprintf("body-%d", n),
			})
		}(i)
	}
	wg.Wait()

	// Decode every frame the client actually receives, in the order it
	// receives them, using the paired receive dictionary.
	readCtx, readCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer readCancel()

	seen := 0
	in := &frames{cli: cli}
	for seen < total {
		data, err := in.next(readCtx)
		if err != nil {
			t.Fatalf("after %d frames: read: %v", seen, err)
		}
		r := wire.NewReader(data, rx)
		hdr, err := r.ReadHeader()
		if err != nil {
			t.Fatalf("frame %d header: %v", seen, err)
		}
		if hdr.Op != wire.OpConvMessage {
			t.Fatalf("frame %d opcode %v", seen, hdr.Op)
		}
		conv, id, sender, name := r.Str(), r.Str(), r.Str(), r.Str()
		ms := r.Uvarint()
		_, _ = r.Bool(), r.Bool()
		r.Bits(6)
		body := string(r.Blob())
		if err := r.Err(); err != nil {
			t.Fatalf("frame %d body: %v", seen, err)
		}

		// Every field of one message carries the same ordinal. A dictionary
		// that has slipped produces a frame whose fields disagree, which is
		// precisely the corruption that is otherwise invisible.
		want := strings.TrimPrefix(conv, "conv-")
		for label, got := range map[string]string{
			"id": strings.TrimPrefix(id, "msg-"), "sender": strings.TrimPrefix(sender, "user-"),
			"name": strings.TrimPrefix(name, "name-"), "body": strings.TrimPrefix(body, "body-"),
		} {
			if got != want {
				t.Fatalf("frame %d: dictionary desync: conv=%q but %s=%q\n"+
					"the transmit dictionary was mutated in one order and the frames queued in another",
					seen, conv, label, got)
			}
		}
		if fmt.Sprint(ms) != want {
			t.Fatalf("frame %d: conv=%q but timestamp=%d", seen, conv, ms)
		}
		seen++
	}
	t.Logf("%d concurrent pushes decoded with the dictionary intact", seen)
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

// Closing twice must be safe. Both the read loop and the write pump can
// notice a failure at the same moment and both call close.
func TestDoubleCloseIsSafe(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, _, _ := newSock(t, h, "u1", 4)

	s.close("first")
	s.close("second")
	s.close("third")

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("socket does not report itself closed")
	}
}

// Many goroutines closing at once must produce exactly one close, and must
// not deadlock. Run this under -race.
func TestConcurrentCloseIsSafe(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, _, _ := newSock(t, h, "u1", 4)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.close("") }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent close deadlocked")
	}
}

// A push racing a close must never panic, deadlock, or write to a closed
// connection. This is the ordinary case of a user closing a tab while a
// message is being delivered.
func TestPushRacingCloseIsSafe(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, _, _ := newSock(t, h, "u1", 64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.writePump(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			h.PushTyping("u1", "c1", "someone", "who-id", true)
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		s.close("")
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("push racing close deadlocked")
	}
}

// A socket that cannot keep up is closed rather than buffered without bound.
// Unbounded buffering on behalf of a client that has stopped reading is how
// a chat server runs out of memory because of one bad connection.
func TestBackedUpSocketIsClosedNotBuffered(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	// A tiny queue and no write pump: nothing drains, so it fills at once.
	s, _, _ := newSock(t, h, "u1", 2)

	for i := 0; i < 50; i++ {
		h.PushTyping("u1", "c1", "who", "who-id", true)
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("a socket that stopped draining was buffered instead of closed")
	}
	if len(s.out) > cap(s.out) {
		t.Fatalf("queue exceeded its capacity: %d > %d", len(s.out), cap(s.out))
	}
	t.Logf("queue capped at %d frames, socket closed", cap(s.out))
}

// ---------------------------------------------------------------------------
// The write pump
// ---------------------------------------------------------------------------

// The write pump must exit when its context is cancelled, or every closed
// tab leaves a goroutine behind for the life of the process.
func TestWritePumpExitsOnContextCancel(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, _, _ := newSock(t, h, "u1", 4)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.writePump(ctx); close(stopped) }()

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("write pump ignored context cancellation")
	}
}

// The write pump must also exit when the connection dies under it, rather
// than spinning on a broken socket.
func TestWritePumpExitsWhenConnectionDies(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, cli, _ := newSock(t, h, "u1", 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { s.writePump(ctx); close(stopped) }()

	_ = cli.CloseNow() // the far end vanishes
	for i := 0; i < 200; i++ {
		h.PushTyping("u1", "c1", "who", "who-id", true)
	}

	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("write pump did not notice a dead connection")
	}
}

// ---------------------------------------------------------------------------
// Hub bookkeeping
// ---------------------------------------------------------------------------

// A removed socket must receive nothing further, and removal must be
// reflected in the online count immediately.
func TestRemovedSocketReceivesNothing(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, _, _ := newSock(t, h, "u1", 16)

	if !h.Online("u1") {
		t.Fatal("user is not online after adding a socket")
	}
	h.remove(s)
	if h.Connected("u1") {
		t.Fatal("user still connected after removing their only socket")
	}
	// Presence waits out the grace before it says offline: a reconnect in
	// that window must not have flickered anybody's dot.
	if !h.Online("u1") {
		t.Fatal("inside the grace the user must still read online")
	}
	h.now = func() time.Time { return time.Now().Add(OfflineGrace + time.Second) }
	if h.Online("u1") {
		t.Fatal("user still online after the grace")
	}
	if n := h.Connections(); n != 0 {
		t.Fatalf("connection count is %d after removal", n)
	}

	for i := 0; i < 10; i++ {
		h.PushTyping("u1", "c1", "who", "who-id", true)
	}
	if len(s.out) != 0 {
		t.Fatalf("a removed socket received %d frames", len(s.out))
	}
}

// Two tabs are one user. Removing one must not report the user offline, and
// the bookkeeping must survive both being removed in either order.
func TestMultipleSocketsPerUser(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	a, _, _ := newSock(t, h, "u1", 16)
	b, _, _ := newSock(t, h, "u1", 16)

	if n := h.Connections(); n != 2 {
		t.Fatalf("expected 2 connections, got %d", n)
	}
	h.remove(a)
	if !h.Online("u1") {
		t.Fatal("closing one of two tabs reported the user offline")
	}
	h.remove(b)
	if h.Connected("u1") {
		t.Fatal("user still connected after both sockets removed")
	}
	h.now = func() time.Time { return time.Now().Add(OfflineGrace + time.Second) }
	if h.Online("u1") {
		t.Fatal("user still online after both sockets removed and the grace")
	}
	h.remove(b) // idempotent
	if n := h.Connections(); n != 0 {
		t.Fatalf("connection count is %d after removing everything", n)
	}
}

// Add and remove from many goroutines must not corrupt the map or leak
// entries. Run under -race.
func TestHubBookkeepingUnderConcurrency(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d", n%4)
			s := &sock{userID: user, tx: wire.NewDict(), out: make(chan []byte, 4)}
			for j := 0; j < 50; j++ {
				h.add(s)
				h.Online(user)
				h.Connections()
				h.remove(s)
			}
		}(i)
	}
	wg.Wait()

	if n := h.Connections(); n != 0 {
		t.Fatalf("%d connections left behind", n)
	}
	h.mu.RLock()
	left := len(h.conns)
	h.mu.RUnlock()
	if left != 0 {
		t.Fatalf("%d user entries left in the map; empty sets are not being pruned", left)
	}
}

// frames reads what a client receives one frame at a time, unpacking the
// writer's batches (writePump coalesces) in order.
type frames struct {
	cli     *websocket.Conn
	pending [][]byte
	batches int
}

func (f *frames) next(ctx context.Context) ([]byte, error) {
	for len(f.pending) == 0 {
		typ, data, err := f.cli.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ != websocket.MessageBinary {
			return nil, fmt.Errorf("a %v message", typ)
		}
		r := wire.NewReader(data, nil)
		hdr, err := r.ReadHeader()
		if err != nil {
			return nil, err
		}
		if hdr.Op != wire.OpBatch {
			f.pending = append(f.pending, data)
			continue
		}
		f.batches++
		if err := wire.SplitBatch(r, func(b []byte) error {
			f.pending = append(f.pending, append([]byte(nil), b...))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := f.pending[0]
	f.pending = f.pending[1:]
	return out, nil
}

// The writer coalesces: frames pushed together leave as one batch, in order;
// a frame alone leaves alone, unwrapped; and closing while a frame is held
// neither hangs nor leaks the writer.
func TestWriterCoalesces(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, cli, rx := newSock(t, h, "u1", 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.writePump(ctx)

	// Spaced inside the window, as a message and its activity row are (the
	// writer has already woken for the first when the next arrives).
	for i := range 3 {
		h.PushTyping("u1", fmt.Sprintf("conv-%d", i), "Ada", "u-ada", true)
		time.Sleep(coalesceWindow / 5)
	}
	rc, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	_, data, err := cli.Read(rc)
	if err != nil {
		t.Fatal(err)
	}
	r := wire.NewReader(data, nil)
	if hdr, _ := r.ReadHeader(); hdr.Op != wire.OpBatch {
		t.Fatalf("three frames pushed together left as op %v, not one batch", hdr.Op)
	}
	var got []string
	if err := wire.SplitBatch(r, func(b []byte) error {
		fr := wire.NewReader(b, rx)
		if hdr, err := fr.ReadHeader(); err != nil || hdr.Op != wire.OpTyping {
			return fmt.Errorf("inner op %v: %v", hdr.Op, err)
		}
		got = append(got, fr.Str())
		return fr.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "conv-0,conv-1,conv-2" {
		t.Fatalf("batch order: %v", got)
	}

	// Alone, later: unwrapped.
	time.Sleep(4 * coalesceWindow)
	h.PushTyping("u1", "conv-9", "Ada", "u-ada", false)
	_, data, err = cli.Read(rc)
	if err != nil {
		t.Fatal(err)
	}
	if hdr, _ := wire.NewReader(data, nil).ReadHeader(); hdr.Op != wire.OpTyping {
		t.Fatalf("a lone frame left as op %v", hdr.Op)
	}

	// Closed while holding one.
	h.PushTyping("u1", "conv-10", "Ada", "u-ada", true)
	s.close("test")
	done := make(chan struct{})
	go func() { _, _, _ = cli.Read(rc); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the client never saw the socket end")
	}
}
