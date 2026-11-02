package signal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// ---------------------------------------------------------------------------
// Scaffolding
// ---------------------------------------------------------------------------

// wsPair returns a genuinely connected pair. Real connections, because what
// is under test is close behaviour, and a fake cannot get close wrong.
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
		<-release
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
		t.Fatal("no accept")
	}
	t.Cleanup(func() {
		_ = cli.CloseNow()
		_ = srv.CloseNow()
		close(release)
		ts.Close()
	})
	return srv, cli
}

func newTestConn(t *testing.T) (*Conn, *websocket.Conn) {
	t.Helper()
	srv, cli := wsPair(t)
	return NewConn(srv, ledger.NewMeter("test")), cli
}

func goroutines() int {
	for i := 0; i < 50; i++ {
		runtime.Gosched()
	}
	return runtime.NumGoroutine()
}

func noLeaks(t *testing.T) {
	t.Helper()
	before := goroutines()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if goroutines() <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutine leak: %d -> %d\n%s", before, goroutines(), buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// ---------------------------------------------------------------------------
// Close must never block the caller
// ---------------------------------------------------------------------------

// Closing a control connection must not wait for the peer.
//
// The call graph makes this a liveness property for the whole room, not just
// for one participant. Gateway.announce and Room.Broadcast walk every
// participant and call Send; Send closes a connection whose outbound queue has
// overflowed; and a graceful websocket close BLOCKS until the peer echoes a
// close frame. A peer whose queue overflowed is by definition not reading, so
// it never echoes and the wait runs to the library timeout.
//
// The result would be that one wedged participant stalls roster updates,
// speaker changes and SDP for everybody else in the meeting, serially, for
// seconds at a time.
func TestCloseDoesNotBlockTheCaller(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t) // client end connected but never reading

	start := time.Now()
	c.CloseWithReason(websocket.StatusPolicyViolation, "outbound queue overflow")
	elapsed := time.Since(start)

	t.Logf("close returned in %v", elapsed)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("close blocked its caller for %v; this runs inside the "+
			"broadcast loop, once per wedged participant", elapsed)
	}
}

// The same property through the path that actually reaches it: an overflowing
// queue closes the connection from inside Send.
func TestSendOverflowClosesWithoutStalling(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)

	start := time.Now()
	// Overflow the queue. Nothing drains it, because nothing reads the far
	// end and the coalescing flush cannot complete.
	for i := 0; i < MaxQueuedFrames+64; i++ {
		c.Send([]byte{0x01, 0x02})
	}
	elapsed := time.Since(start)

	t.Logf("%d sends including the overflow close took %v", MaxQueuedFrames+64, elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("overflowing a queue took %v on the sender's goroutine", elapsed)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		t.Fatal("the connection overflowed but was not closed")
	}
}

// A room-sized fan-out where several participants are wedged must not
// degrade with the number of wedged participants.
func TestBroadcastToWedgedParticipantsIsNotSerial(t *testing.T) {
	noLeaks(t)
	const n = 4
	conns := make([]*Conn, n)
	for i := range conns {
		conns[i], _ = newTestConn(t)
	}

	start := time.Now()
	for _, c := range conns {
		for i := 0; i < MaxQueuedFrames+8; i++ {
			c.Send([]byte{0x01})
		}
	}
	elapsed := time.Since(start)

	t.Logf("fan-out to %d wedged participants took %v", n, elapsed)
	if elapsed > 3*time.Second {
		t.Fatalf("a broadcast touching %d wedged participants took %v", n, elapsed)
	}
}

// ---------------------------------------------------------------------------
// Close idempotence and teardown
// ---------------------------------------------------------------------------

func TestDoubleCloseIsSafe(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)
	c.Close()
	c.Close()
	c.CloseWithReason(websocket.StatusInternalError, "again")

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		t.Fatal("connection does not report itself closed")
	}
}

func TestConcurrentCloseIsSafe(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Close() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent close deadlocked")
	}
}

// Closing must stop the coalescing timer. A live timer holds a reference to
// the connection and fires a flush against a closed socket, so leaving it
// running leaks both a timer and the connection it captures, per disconnect.
func TestCloseStopsTheCoalescingTimer(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)

	c.Send([]byte{0x01}) // starts the timer
	c.mu.Lock()
	running := c.timer != nil
	c.mu.Unlock()
	if !running {
		t.Fatal("sending did not start the coalescing timer")
	}

	c.Close()
	c.mu.Lock()
	stopped := c.timer == nil
	c.mu.Unlock()
	if !stopped {
		t.Fatal("closing left the coalescing timer running")
	}

	// And a flush after close must be inert rather than panicking or writing.
	c.flush()
}

// Sending after close must be silently dropped, not queued forever.
func TestSendAfterCloseIsDropped(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)
	c.Close()

	for i := 0; i < 100; i++ {
		c.Send([]byte{0x01})
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d frames queued on a closed connection", n)
	}
}

// A send racing a close must be safe under -race.
func TestSendRacingCloseIsSafe(t *testing.T) {
	noLeaks(t)
	c, _ := newTestConn(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c.Send([]byte{0x01})
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		c.Close()
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("send racing close deadlocked")
	}
}

// ---------------------------------------------------------------------------
// The interning dictionary, again
// ---------------------------------------------------------------------------

// Frames must reach the outbound queue in the order their strings were
// interned.
//
// Conn has the same hazard the chat Hub had, and a wider one: Encode takes
// writeMu and mutates the transmit dictionary, Send takes mu and appends to
// the queue, and callers do c.Send(c.Encode(...)) as two separate steps under
// two different locks. Two goroutines announcing at once -- two participants
// joining, a roster update racing a speaker update -- can therefore intern in
// one order and queue in the other, which permanently corrupts every interned
// string on that connection.
//
// Every frame here carries the SAME string, so the first one encoded defines
// the index and all the rest reference it. One reorder is immediately visible.
func TestEncodeAndQueueAreOrdered(t *testing.T) {
	noLeaks(t)
	const rounds, per = 100, 128

	for round := 0; round < rounds; round++ {
		c, _ := newTestConn(t)
		_, rx := wire.NewPreloadedPair()
		// Pair the reader with this connection's transmit dictionary.
		c.txDict, rx = wire.NewPreloadedPair()
		// Hold the coalescer: under load, 128 goroutines can outlast its
		// 15 ms window and the flush would drain the queue being inspected.
		c.mu.Lock()
		c.timer = time.AfterFunc(time.Hour, func() {})
		c.mu.Unlock()

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < per; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c.Emit(func(w *wire.Writer) {
					w.WriteHeader(wire.OpRoster, 0, false)
					w.Str("participant-alpha")
					w.Str("display-beta")
				})
			}()
		}
		close(start)
		wg.Wait()

		c.mu.Lock()
		frames := c.pending
		c.pending = nil
		c.mu.Unlock()

		if len(frames) != per {
			t.Fatalf("round %d: queued %d frames, want %d", round, len(frames), per)
		}
		for i, f := range frames {
			r := wire.NewReader(f, rx)
			if _, err := r.ReadHeader(); err != nil {
				t.Fatalf("round %d frame %d: header: %v", round, i, err)
			}
			id, name := r.Str(), r.Str()
			if err := r.Err(); err != nil {
				t.Fatalf("round %d frame %d: %v -- a frame referenced a dictionary "+
					"index no earlier frame defined, so frames were queued in a "+
					"different order than they were interned", round, i, err)
			}
			if id != "participant-alpha" || name != "display-beta" {
				t.Fatalf("round %d frame %d: decoded %q / %q; dictionary desynced",
					round, i, id, name)
			}
		}
	}
	t.Logf("%d rounds x %d concurrent emits: dictionary intact", rounds, per)
}

// Batches must reach the wire in the order they left the queue. flush takes
// the queue under mu and writes outside it, so two flushes at once -- the
// coalescing timer and an EmitNow, or two timers behind a slow write -- could
// write in the opposite order. That reorders frames, and worse, a later batch
// can reference a string interned in an earlier one the peer has not seen.
// Each frame carries a counter stamped under mu, i.e. its queue position.
func TestFlushesReachTheWireInQueueOrder(t *testing.T) {
	c, cli := newTestConn(t)
	defer c.Close()
	_, rx := wire.NewPreloadedPair()
	c.txDict, rx = wire.NewPreloadedPair()

	const writers, per = 8, 1500
	var seq uint64
	done := make(chan error, 1)
	go func() {
		var last uint64
		got := 0
		for got < writers*per {
			_, data, err := cli.Read(context.Background())
			if err != nil {
				done <- err
				return
			}
			r := wire.NewReader(data, rx)
			h, err := r.ReadHeader()
			if err != nil {
				done <- err
				return
			}
			var bad error
			each := func(f []byte) error {
				fr := wire.NewReader(f, rx)
				fr.ReadHeader()
				n := fr.Uvarint()
				if n != last+1 && bad == nil {
					bad = fmt.Errorf("frame %d arrived after %d", n, last)
				}
				last = n
				got++
				return nil
			}
			if h.Op == wire.OpBatch {
				_ = wire.SplitBatch(r, each)
			} else {
				_ = each(data) // a lone frame is sent unwrapped
			}
			if bad != nil {
				done <- bad
				return
			}
		}
		done <- nil
	}()
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				emit := c.Emit
				if j%3 == 0 {
					emit = c.EmitNow
				}
				emit(func(w *wire.Writer) {
					w.WriteHeader(wire.OpRoster, 0, false)
					seq++ // under c.mu: this is the queue position
					w.Uvarint(seq)
				})
			}
		}()
	}
	wg.Wait()
	c.flush()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("frames never all arrived")
	}
}
