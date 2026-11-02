package messaging

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// The Hub is the persistent-chat delivery path.
//
// Polling would have been simpler and is what most products do, and it is
// exactly the behaviour this whole system exists to argue against: a client
// asking "anything new?" every two seconds spends bandwidth in proportion to
// how long you leave the tab open rather than to how much was said. A
// conversation with nothing happening in it should cost nothing.
//
// So a signed-in client holds one socket, and the server pushes. The frames
// use the same binary protocol as the meeting path, so a delivered message
// costs a few bytes of framing rather than a JSON envelope.

// A socket may fall this far behind before it is closed. A client that
// cannot keep up with its own chat is either gone or hostile, and the right
// answer to both is to stop buffering on its behalf.
const maxPending = 256

type sock struct {
	ws     *websocket.Conn
	userID string

	mu     sync.Mutex
	tx     *wire.Dict
	closed bool
	reason string
	idle   bool   // this device has reported no input for a while
	device string // the tab or app instance, from the socket's ?d=; "" if none

	out chan []byte
}

// OfflineGrace is how long after a person's last socket closes before they
// read as offline. A page reload, a network blip, a laptop changing Wi-Fi:
// the client reconnects in a second or two, and the dots of everyone who
// can see them must not flicker for it. Longer than the client's first
// reconnect back-off, shorter than anyone would call "gone".
const OfflineGrace = 10 * time.Second

// Hub tracks which users are connected and pushes frames to them.
type Hub struct {
	log *slog.Logger
	now func() time.Time // injected in tests to run the grace out

	mu    sync.RWMutex
	conns map[string]map[*sock]struct{}
	// grace is when each recently disconnected person stops reading as
	// online; entries are dropped on reconnect and on expiry.
	grace map[string]time.Time
	// hidden is who chose to appear offline: connected, but reported to
	// peers and the directory as not. Set on connect from the stored
	// status and on every status change, so Online() needs no query.
	hidden map[string]bool
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, now: time.Now, conns: make(map[string]map[*sock]struct{}), hidden: make(map[string]bool), grace: make(map[string]time.Time)}
}

// SetHidden records whether a person appears offline.
func (h *Hub) SetHidden(userID string, hidden bool) {
	h.mu.Lock()
	if hidden {
		h.hidden[userID] = true
	} else {
		delete(h.hidden, userID)
	}
	h.mu.Unlock()
}

// Hidden reports whether a person appears offline by choice.
func (h *Hub) Hidden(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hidden[userID]
}

// SetIdle records one device's idleness. Reports whether the person's
// overall idleness changed, which is what the status loop cares about.
func (h *Hub) SetIdle(s *sock, idle bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.idleLocked(s.userID)
	s.mu.Lock()
	s.idle = idle
	s.mu.Unlock()
	return before != h.idleLocked(s.userID)
}

// Idle: connected on at least one device, and every device idle. The most
// recently active device decides, which is what "every device idle" means.
func (h *Hub) Idle(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.idleLocked(userID)
}

func (h *Hub) idleLocked(userID string) bool {
	set := h.conns[userID]
	if len(set) == 0 {
		return false
	}
	for s := range set {
		s.mu.Lock()
		i := s.idle
		s.mu.Unlock()
		if !i {
			return false
		}
	}
	return true
}

// IdleUsers lists everyone idle on every device. One pass over the live
// sockets; bounded by the connection count.
func (h *Hub) IdleUsers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for id := range h.conns {
		if h.idleLocked(id) {
			out = append(out, id)
		}
	}
	return out
}

func (h *Hub) add(s *sock) {
	h.mu.Lock()
	if h.conns[s.userID] == nil {
		h.conns[s.userID] = make(map[*sock]struct{})
	}
	h.conns[s.userID][s] = struct{}{}
	delete(h.grace, s.userID)
	h.mu.Unlock()
}

func (h *Hub) remove(s *sock) {
	h.mu.Lock()
	if set := h.conns[s.userID]; set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(h.conns, s.userID)
			h.grace[s.userID] = h.now().Add(OfflineGrace)
		}
	}
	h.mu.Unlock()
}

// Connected reports whether a user has at least one live socket right now.
func (h *Hub) Connected(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns[userID]) > 0
}

// Online is what presence says: connected, or disconnected less than
// OfflineGrace ago. Every reader of presence -- the status query, the
// directory, the connect-time snapshot -- goes through here, so they agree
// with the delayed departure announcement.
func (h *Hub) Online(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.conns[userID]) > 0 {
		return true
	}
	until, ok := h.grace[userID]
	return ok && until.After(h.now())
}

// Connections reports the total live socket count, for telemetry.
func (h *Hub) Connections() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.conns {
		n += len(set)
	}
	return n
}

// push encodes once per socket, because each socket owns its own interning
// dictionary. See internal/wire/dict.go for why those cannot be shared.
func (h *Hub) push(userID string, enc func(*wire.Writer)) {
	h.mu.RLock()
	socks := make([]*sock, 0, len(h.conns[userID]))
	for s := range h.conns[userID] {
		socks = append(socks, s)
	}
	h.mu.RUnlock()

	for _, s := range socks {
		if s.send(enc) == sendOverflow {
			// Backed up. Drop the socket rather than the message: a client
			// that reconnects refetches history and is correct again, whereas
			// a silently skipped frame leaves a hole nobody notices.
			h.log.Warn("chat socket backed up, closing", "user", userID)
			s.close("outbound queue overflow")
		}
	}
}

// sendResult says what happened to one frame.
type sendResult int

const (
	sendOK       sendResult = iota
	sendOverflow            // the outbound queue is full; the caller should close
	sendClosed              // the socket was already gone
)

// send encodes one frame and queues it, atomically.
//
// The encode and the queueing MUST happen under a single hold of s.mu, and
// this function exists so that there is exactly one place where that is true.
//
// Encoding a string assigns it the next free index in this socket's transmit
// dictionary and emits the binding; every later frame that uses the same
// string emits only the index. The receiver learns the bindings by reading
// frames in order. So if two frames are encoded in one order and queued in
// the other, the receiver meets an index before the frame that defines it,
// and from then on every interned string on the connection decodes to the
// wrong value. There is no checksum on a string index and no resynchronising
// message: the connection is quietly, permanently wrong.
//
// The earlier version released the lock after encoding and queued afterwards.
// The window between the two was a few nanoseconds, which sounds like nothing
// and was not: measured with 256 concurrent pushes to one socket, 7 rounds in
// 400 produced a corrupted stream. That is the ordinary shape of chat traffic,
// where a message push, a presence change and a typing indicator for the same
// user all arrive from different request goroutines at once.
//
// Queueing inside the lock is safe because the send is non-blocking. Closing
// is deliberately left to the caller, because s.close takes the same mutex.
func (s *sock) send(enc func(*wire.Writer)) sendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sendClosed
	}
	w := wire.NewWriter(s.tx)
	enc(w)
	frame := append([]byte(nil), w.Bytes()...)
	select {
	case s.out <- frame:
		return sendOK
	default:
		return sendOverflow
	}
}

// PushMessage delivers a new or changed message.
func (h *Hub) PushMessage(userID string, m *Message) {
	h.push(userID, func(w *wire.Writer) { encodeConvMessage(w, m) })
}

// PushMessageExcept delivers to every socket of a user but the one device
// named: the sender's other tabs and apps, not the tab that sent.
func (h *Hub) PushMessageExcept(userID, device string, m *Message) {
	h.mu.RLock()
	socks := make([]*sock, 0, len(h.conns[userID]))
	for s := range h.conns[userID] {
		if s.device != device {
			socks = append(socks, s)
		}
	}
	h.mu.RUnlock()
	for _, s := range socks {
		if s.send(func(w *wire.Writer) { encodeConvMessage(w, m) }) == sendOverflow {
			h.log.Warn("chat socket backed up, closing", "user", userID)
			s.close("outbound queue overflow")
		}
	}
}

// encodeConvMessage is one message as the client's thread needs it.
//
// The frame used to stop at the body. A reaction then arrived at the client
// as a bare copy of the message -- no reactions, no quote -- and replaced the
// local one, so the chip somebody had just clicked vanished a few
// milliseconds later. "Reactions do not work" was the exact report. Whatever
// the thread draws from a message has to ride on the push, or the push is a
// regression of the REST view it overwrites.
//
// Bytes: the reply quote is present only on replies (one interned string
// says so); reactions are a count followed by, per emoji, the interned emoji,
// the total, and up to 12 reactor ids (interned, so a regular reactor costs
// one or two bytes after the first time). The viewer works out "mine" from
// the ids, which is what lets one encoding serve every member of the
// conversation instead of one decoration per recipient.
func encodeConvMessage(w *wire.Writer, m *Message) {
	w.WriteHeader(wire.OpConvMessage, 0, false)
	w.Str(m.ConversationID)
	w.Str(m.ID)
	w.Str(m.SenderID)
	w.Str(m.SenderName)
	w.Uvarint(uint64(m.CreatedAt.UnixMilli()))
	w.Bool(m.Deleted)
	w.Bool(m.EditedAt != nil)
	w.Bool(m.Forwarded) // was padding; costs nothing
	w.Bool(m.System)    // likewise
	w.Bits(0, 4)
	w.Blob([]byte(m.Body))

	w.Str(m.ReplyTo)
	if m.ReplyTo != "" {
		q := m.Quoted
		if q == nil {
			q = &Quote{}
		}
		// Whose message is quoted: the id is interned (a regular is one
		// byte) and lets the recipient know "this is a reply to me" without
		// a lookup, which is what the notification needs.
		w.Str(q.SenderID)
		w.Str(q.SenderName)
		w.Bool(q.Deleted)
		w.Bits(0, 7)
		body := q.Body
		if len(body) > 200 {
			body = body[:200]
		}
		w.Blob([]byte(body))
	}

	w.Uvarint(uint64(len(m.Reactions)))
	for _, r := range m.Reactions {
		w.Str(r.Emoji)
		w.Uvarint(uint64(r.Count))
		w.Uvarint(uint64(len(r.Who)))
		for _, who := range r.Who {
			w.Str(who)
		}
	}

	// Files: id, display name, type, size. The id is what the client fetches;
	// the rest is what it draws before deciding to.
	w.Uvarint(uint64(len(m.Attachments)))
	for _, at := range m.Attachments {
		w.Str(at.ID)
		w.Str(at.Name)
		w.Str(at.MIME)
		w.Uvarint(uint64(at.Size))
	}

	// Who the message names: interned ids, so a regular is a byte.
	w.Uvarint(uint64(len(m.Mentions)))
	for _, id := range m.Mentions {
		w.Str(id)
	}

	// Priority (interned; "" costs a byte) and the card payload (a blob;
	// empty costs a byte).
	w.Str(m.Priority)
	w.Blob([]byte(m.Meta))

	// The poll tally: option count (0 for "not a poll", one byte), then per
	// option the count and up to twelve voter ids (none on an anonymous
	// poll). The reader's own choice is not on the wire -- it differs per
	// recipient and the client knows what it clicked; the REST view carries
	// it and the voter list names them when the poll is open.
	if m.Poll == nil {
		w.Uvarint(0)
		return
	}
	w.Uvarint(uint64(len(m.Poll.Counts)))
	for i, n := range m.Poll.Counts {
		w.Uvarint(uint64(n))
		var who []string
		if i < len(m.Poll.Voters) {
			who = m.Poll.Voters[i]
		}
		w.Uvarint(uint64(len(who)))
		for _, id := range who {
			w.Str(id)
		}
	}
	w.Uvarint(uint64(m.Poll.Total))
}

// PushStatus tells one person that another's availability changed.
func (h *Hub) PushStatus(toUserID string, st Status) {
	h.push(toUserID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpStatus, 0, false)
		w.Str(st.UserID)
		w.Str(st.Status)
		w.Str(st.Note)
		var until uint64
		if st.Until != nil {
			until = uint64(st.Until.UnixMilli())
		}
		w.Uvarint(until)
		w.Bool(st.Auto)
		w.Bool(st.Online)
		w.Bits(0, 6)
		w.Uvarint(msOrZero(ptrTime(st.LastSeen)))
	})
}

// PushRead tells one member that another's read cursor moved.
func (h *Hub) PushRead(userID, convID, who string, at time.Time) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpReadCursor, 0, false)
		w.Str(convID)
		w.Str(who)
		w.Uvarint(uint64(at.UnixMilli()))
	})
}

// PushPresence tells one user that another has come or gone. lastSeen is
// when they were last active, sent with a departure so the peer can say
// "last seen just now"; zero when they arrived or when it is not theirs to
// know (the person appears offline).
func (h *Hub) PushPresence(toUserID, whoID string, online bool, lastSeen time.Time) {
	h.push(toUserID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpPresence, 0, false)
		w.Str(whoID)
		w.Bool(online)
		w.Bits(0, 7)
		w.Uvarint(msOrZero(lastSeen))
	})
}

func msOrZero(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixMilli())
}

// PresenceTo tells one socket -- a newly connected one -- that a peer is
// online right now. The connect-time snapshot: without it a client that
// arrives after its peers draws them all offline until they reconnect.
// Delivered to this socket only; the person's other tabs already know.
func (h *Hub) PresenceTo(s *sock, whoID string, online bool) {
	s.send(func(w *wire.Writer) {
		w.WriteHeader(wire.OpPresence, 0, false)
		w.Str(whoID)
		w.Bool(online)
		w.Bits(0, 7)
		w.Uvarint(0)
	})
}

// PushTyping delivers a transient composing indicator.
// PushRing rings one account on every socket it has open. A person signed in
// on a laptop and a phone hears both; whichever answers first wins, and the
// other is told so via PushRingAnswer relayed back through the caller.
func (h *Hub) PushRing(userID, callerID, callerName, room string) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpRing, 0, false)
		w.Str(callerID)
		w.Str(callerName)
		w.Str(room)
	})
}

// PushRingAnswer tells the caller how the callee responded.
func (h *Hub) PushRingAnswer(userID, calleeID, calleeName, room string, accepted bool) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpRingAnswer, 0, false)
		w.Str(calleeID)
		w.Str(calleeName)
		w.Str(room)
		w.Bool(accepted)
		w.Bits(0, 7)
	})
}

// PushTyping: {conv, name, who id, on}. Every string is interned, so after
// the first frame from a person in a conversation the payload is the
// header, three one-byte references and one byte of flag.
func (h *Hub) PushTyping(userID, convID, who, whoID string, on bool) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpTyping, 0, false)
		w.Str(convID)
		w.Str(who)
		w.Str(whoID)
		w.Bool(on)
		w.Bits(0, 7)
	})
}

// close tears the socket down, and never waits for the peer to agree.
//
// The reason it cannot wait is the call graph. Hub.push walks every socket
// belonging to a user and closes any that has fallen behind, and Hub.push runs
// on the HTTP request goroutine of whoever sent the message. A graceful
// websocket close writes a close frame and then BLOCKS until the peer echoes
// one back; a peer that has stopped reading, which is exactly the condition
// that made us close it, never echoes, so the wait runs to the library
// timeout of five seconds.
//
// Measured before this changed: closing one wedged socket blocked its caller
// for 5.0 seconds, and a push to a user with four wedged tabs took 20.0
// seconds on the sender's request goroutine, serially. One laptop that slept
// mid-conversation was enough to do that to everyone who messaged its owner.
//
// So the socket is torn down immediately instead. The cost is that the client
// sees an abnormal closure rather than a status code, which costs nothing
// today because neither client reads the code: both simply reconnect with a
// widening backoff. If a code ever does need to reach the client, the fix is
// to run the graceful close on the socket's OWN goroutine, never on the
// caller's, and not to reintroduce a blocking call here.
//
// reason is retained for the call site to document itself, and for logs.
func (s *sock) close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.reason = reason
	s.mu.Unlock()
	_ = s.ws.CloseNow()
}

// writePump drains the outbound queue.
func (s *sock) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-s.out:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.ws.Write(wctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				s.close("write failed")
				return
			}
		}
	}
}

func ptrTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
