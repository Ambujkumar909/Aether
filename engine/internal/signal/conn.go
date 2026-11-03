// Package signal is the control-plane transport: a persistent, binary,
// coalescing WebSocket carrying the wire protocol.
package signal

import (
	"context"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// CoalesceWindow is how long an outbound frame waits for company.
//
// Every millisecond here is a millisecond of added control-plane latency, so
// the number is chosen against a specific worst case: a mute toggle must
// feel instantaneous, and human perception of "instant" for a UI state
// change sits around 100 ms. Fifteen milliseconds is invisible against that
// budget while still capturing the bursts that actually matter -- a
// participant joining fires roster, state, and track frames within
// microseconds of each other, and batching those three into one WebSocket
// message saves two frame headers, two TLS records, and two syscalls.
//
// Media never passes through here. This window applies only to control
// frames, where 15 ms costs nothing and buys a great deal.
const CoalesceWindow = 15 * time.Millisecond

// MaxQueuedFrames bounds the outbound queue. A client that cannot drain
// this many control frames is not a client having a bad network moment; it
// is a client that has gone away without saying so, or one deliberately
// refusing to read in order to make us buffer on its behalf. Either way the
// correct response is to disconnect it rather than to grow memory.
const MaxQueuedFrames = 1024

// MaxQueuedMediaBytes bounds the queue in bytes for media. A count of frames
// alone let 1,024 screen-sized frames (60 KB each) sit queued for one slow
// receiver, and in the 50-person stress run one sender's 24 MB burst took the
// server to 2 GB (ARCHITECTURE 6bs). Media past this is dropped, whole, for
// that receiver only; the connection stays up. 2 MB is seconds of any real
// meeting's media and holds the largest key frame several times over.
const MaxQueuedMediaBytes = 2 << 20

// Conn is one participant's control channel.
type Conn struct {
	ws    *websocket.Conn
	Meter *ledger.Meter

	// Two dictionaries, one per direction. See wire.Dict for why sharing a
	// single table between directions corrupts sessions.
	txDict *wire.Dict
	rxDict *wire.Dict

	mu          sync.Mutex
	pending     [][]byte
	pendingB    int             // bytes in pending
	behind      map[string]bool // publishers whose video is dropped until a key frame
	mediaDrops  int             // media frames dropped for this receiver
	timer       *time.Timer
	closed      bool
	closeReason string

	writeMu sync.Mutex
	scratch *wire.Writer

	ParticipantID string
	RoomID        string
	// Addr is the host part of the peer's remote address, for the
	// same-device advisory. Not trusted for anything else.
	Addr string

	// UserID is the authenticated account behind this connection, empty for
	// a guest. It is set from the verified session, never from anything the
	// client sent, because it decides who may bypass the waiting room.
	UserID    string
	UserEmail string

	// Ticket is a breakout ticket the client presented on connecting (the
	// "bt" query parameter): entry to a breakout room, or back into the main
	// meeting past its lobby. Spent by the gateway's admission, never trusted
	// by itself (signal/breakout.go).
	Ticket string
}

func NewConn(ws *websocket.Conn, meter *ledger.Meter) *Conn {
	tx, rx := wire.NewPreloadedPair()
	return &Conn{
		ws: ws, Meter: meter,
		txDict: tx, rxDict: rx,
		scratch: wire.NewWriter(nil),
	}
}

// Emit runs fn against a writer bound to this connection's transmit
// dictionary and queues the resulting frame, as one atomic step.
//
// Broadcasting therefore encodes once per recipient rather than once per
// room. That sounds backwards for efficiency and is not: the encode is a few
// hundred nanoseconds of appending to a reused buffer, while the alternative
// -- one shared encode with interning disabled -- puts a full 36 byte
// identifier on the wire for every recipient of every frame. We spend CPU,
// which we have, to save bytes, which the user pays for.
//
// The atomicity is the part that matters, and it replaced a genuine bug.
// This used to be a public Encode returning a frame, which callers passed to
// Send as a second step: c.Send(c.Encode(...)). Encode took writeMu and
// mutated the transmit dictionary; Send took mu and appended to the queue.
// Two goroutines announcing at once -- two participants joining, a roster
// update racing a speaker update -- could therefore intern in one order and
// queue in the other.
//
// That is not a cosmetic reordering. Interning a string assigns it the next
// free index and emits the binding once; every later frame emits only the
// index. A receiver that meets an index before the frame defining it has no
// way to recover and no way to notice: from then on every interned string on
// that connection decodes to the wrong value, or fails outright. Measured
// against the two-step version, 128 concurrent announcements on one
// connection corrupted the stream within 43 rounds.
//
// So there is now exactly one path that touches txDict, and it holds mu
// across both the interning and the queueing. Send remains for frames that
// were encoded WITHOUT interning and are shared between recipients, which is
// what Room.Broadcast does; those carry no dictionary state and so have no
// ordering requirement.
func (c *Conn) Emit(fn func(w *wire.Writer)) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	// Checked before encoding, not after: interning a string into txDict for
	// a frame that is then discarded would itself desync the connection,
	// because the sender would believe the receiver had been told a binding
	// it never saw.
	if len(c.pending) >= MaxQueuedFrames {
		c.mu.Unlock()
		c.CloseWithReason(websocket.StatusPolicyViolation, "outbound queue overflow")
		return
	}
	w := wire.NewWriter(c.txDict)
	fn(w)
	b := w.Bytes()
	frame := make([]byte, len(b))
	copy(frame, b)

	c.pending = append(c.pending, frame)
	c.pendingB += len(frame)
	if c.timer == nil {
		c.timer = time.AfterFunc(CoalesceWindow, c.flush)
	}
	c.mu.Unlock()
}

// EmitMedia queues a media frame unless this receiver is behind, in which
// case the frame is dropped whole, before it is encoded (so the dictionary is
// untouched). Video from a publisher, once dropped, stays dropped until that
// publisher's next key frame: a decoder never sees a delta whose reference it
// missed, so the picture freezes briefly rather than smearing. needKey says
// a gap just started (or its key frame was lost too), so the publisher should
// be asked for a key frame. Audio is dropped frame by frame; the decoder conceals a gap.
func (c *Conn) EmitMedia(publisher string, video, key bool, size int, fn func(w *wire.Writer)) (needKey bool) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	if video && c.behind[publisher] && !key {
		c.mediaDrops++
		c.mu.Unlock()
		return false
	}
	if len(c.pending) >= MaxQueuedFrames || c.pendingB+size > MaxQueuedMediaBytes {
		c.mediaDrops++
		// A video drop -- the first delta of a gap, or a key frame that
		// would have ended one -- asks for a key frame. (Deltas inside a gap
		// returned above and ask for nothing.)
		if video {
			if c.behind == nil {
				c.behind = map[string]bool{}
			}
			c.behind[publisher] = true
			needKey = true
		}
		c.mu.Unlock()
		return needKey
	}
	if video && key {
		delete(c.behind, publisher)
	}
	w := wire.NewWriter(c.txDict)
	fn(w)
	frame := append([]byte(nil), w.Bytes()...)
	c.pending = append(c.pending, frame)
	c.pendingB += len(frame)
	if c.timer == nil {
		c.timer = time.AfterFunc(CoalesceWindow, c.flush)
	}
	c.mu.Unlock()
	return false
}

// MediaDrops is how many media frames this receiver has had dropped.
func (c *Conn) MediaDrops() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mediaDrops
}

// EmitNow encodes, queues and flushes immediately, bypassing coalescing.
func (c *Conn) EmitNow(fn func(w *wire.Writer)) {
	c.Emit(fn)
	c.flush()
}

// Send queues a frame, starting the coalescing timer if it is not running.
func (c *Conn) Send(frame []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if len(c.pending) >= MaxQueuedFrames {
		c.mu.Unlock()
		c.CloseWithReason(websocket.StatusPolicyViolation, "outbound queue overflow")
		return
	}
	c.pending = append(c.pending, frame)
	c.pendingB += len(frame)
	if c.timer == nil {
		c.timer = time.AfterFunc(CoalesceWindow, c.flush)
	}
	c.mu.Unlock()
}

// SendNow queues a frame and flushes immediately, bypassing coalescing.
// Reserved for frames where 15 ms is genuinely too long: keyframe requests,
// which sit directly in the path of a visible black rectangle.
func (c *Conn) SendNow(frame []byte) {
	c.Send(frame)
	c.flush()
}

func (c *Conn) flush() {
	// writeMu is held from taking the queue to the end of the write, so
	// batches reach the wire in the order they left the queue. Releasing it
	// before the write let two flushes (the timer and an EmitNow, or two
	// timers behind a slow write) cross: frames reordered, and a batch could
	// reference a string interned in one the peer had not yet been sent.
	// TestFlushesReachTheWireInQueueOrder failed every run before this.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.closed || len(c.pending) == 0 {
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.mu.Unlock()
		return
	}
	frames := c.pending
	c.pending = nil
	c.pendingB = 0
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()

	c.scratch.Reset()
	if err := wire.EncodeBatch(c.scratch, frames); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, c.scratch.Bytes()); err != nil {
		c.CloseWithReason(websocket.StatusInternalError, "write failed")
		return
	}
	// Server to client frames are unmasked, hence masked=false.
	c.Meter.CountSignal(c.scratch.Len(), false, len(frames))
}

// Read receives one WebSocket message and dispatches every frame it
// contains, batched or not.
func (c *Conn) Read(ctx context.Context, handle func(wire.Header, *wire.Reader) error) error {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageBinary {
		// Text frames are not part of this protocol. Accepting them would
		// mean maintaining a second parser and a second attack surface for
		// no benefit.
		c.CloseWithReason(websocket.StatusUnsupportedData, "binary frames only")
		return nil
	}
	// Client to server frames carry the mandatory 4 byte mask.
	c.Meter.CountSignal(len(data), true, 1)

	r := wire.NewReader(data, c.rxDict)
	h, err := r.ReadHeader()
	if err != nil {
		return err
	}
	if h.Op == wire.OpBatch {
		return wire.SplitBatch(r, func(f []byte) error {
			fr := wire.NewReader(f, c.rxDict)
			fh, err := fr.ReadHeader()
			if err != nil {
				return err
			}
			return handle(fh, fr)
		})
	}
	return handle(h, r)
}

// CloseWithReason shuts the connection down once, and never waits for the
// peer to agree.
//
// The call graph is what makes the non-blocking part mandatory. Gateway
// announcements and Room.Broadcast walk every participant and queue a frame
// for each; queueing closes any connection whose outbound queue has
// overflowed; and a graceful websocket close writes a close frame and then
// BLOCKS until the peer echoes one. A peer whose queue overflowed is by
// definition not reading, so it never echoes and the wait runs to the library
// timeout of five seconds.
//
// Measured before this changed: closing one wedged connection blocked its
// caller for 5.0 seconds, and a fan-out touching four wedged participants
// took 20.0 seconds, serially, on the goroutine of whoever triggered the
// announcement. Roster updates, speaker changes and SDP for the entire
// meeting would stall behind a single participant whose laptop had slept.
//
// The cost of tearing down immediately is that the peer sees an abnormal
// closure rather than a status code, which costs nothing today because the
// client does not read close codes; it reconnects with a widening backoff
// whatever it is told. If a code ever must reach the client, run the graceful
// close on the connection's OWN goroutine rather than the caller's, and do
// not reintroduce a blocking call here.
//
// reason is retained so call sites document themselves and so it can be
// logged.
func (c *Conn) CloseWithReason(code websocket.StatusCode, reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeReason = reason
	// The coalescing timer holds a reference to this connection and would
	// otherwise fire a flush against a closed socket, leaking a timer and the
	// connection it captures on every disconnect.
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	_ = c.ws.CloseNow()
}

// CloseReason reports why this connection was closed, for logs and tests.
func (c *Conn) CloseReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

// Close shuts the connection down normally.
func (c *Conn) Close() { c.CloseWithReason(websocket.StatusNormalClosure, "") }

// DictSize reports interning table occupancy, for telemetry.
func (c *Conn) DictSize() (tx, rx int) { return c.txDict.Len(), c.rxDict.Len() }
