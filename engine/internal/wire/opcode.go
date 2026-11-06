package wire

import "errors"

// Version occupies the top two bits of every frame's control byte. Two bits
// is enough: a wire format that needs a fourth incompatible revision has
// failed at design, and the EXT opcode space gives us room to add messages
// without ever bumping this.
const Version = 0b01

// Frame control byte layout:
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|VER|R|E| OPCODE|
//	+-+-+-+-+-+-+-+-+
//
//	VER    (2) protocol version, always 0b01
//	R      (1) request-id present; an LEB128 id follows the control byte
//	E      (1) extended opcode; one more byte follows, opcode = nib<<8|next
//	OPCODE (4) opcode, or the high nibble of a 12-bit extended opcode
//
// A heartbeat is therefore exactly one byte of payload. Carried in an
// unmasked server-to-client WebSocket frame that is 3 bytes on the wire.
// The JSON keepalive every competing product ships is 40-60 bytes plus the
// same frame header, and it runs every few seconds for the entire call.
const (
	flagReqID = 1 << 5
	flagExt   = 1 << 4
	maskOp    = 0x0f
	maskVer   = 0xc0
)

// Hot-path opcodes. These sixteen are the frames that run at call frequency,
// and they are the only ones that fit in a single control byte. Membership
// here is a bandwidth decision, not a taxonomy decision -- if a message does
// not run at least once per second per participant, it belongs in the
// extended space where the extra byte does not matter.
type Op uint16

const (
	OpPing        Op = 0  // 1 byte. Liveness + RTT probe.
	OpPong        Op = 1  // 1 byte, or +delta when echoing a timestamp.
	OpBatch       Op = 2  // Container: [uvarint len][frame] repeated.
	OpStateDelta  Op = 3  // Packed participant state bits. ~3 bytes.
	OpSpeakers    Op = 4  // Ranked active-speaker list w/ quantised energy.
	OpLayerReq    Op = 5  // Subscriber requests a spatial/temporal layer.
	OpLayerGrant  Op = 6  // SFU confirms what it will actually send.
	OpBweReport   Op = 7  // Receiver-side estimate + loss + jitter.
	OpSubscribe   Op = 8  // Begin receiving a track.
	OpUnsubscribe Op = 9  // Stop receiving a track. Biggest single saving.
	OpKeyframeReq Op = 10 // PLI/FIR equivalent, deduplicated by the SFU.
	OpSdp         Op = 11 // Offer/answer, direction in the body.
	OpIce         Op = 12 // Trickle candidate.
	OpRoster      Op = 13 // Join/leave/roster mutation.
	OpMediaFrame  Op = 14 // Media over the socket: the most frequent frame there is, so it has the one-byte header. See below.
	OpError       Op = 15 // Protocol or application error.
)

// Extended opcodes, 0x100 and up. Two bytes of control instead of one.
// Everything here is cold path: it happens at join, at leave, or when a
// human clicks something unusual.
const (
	OpHello       Op = 0x100 // Client capabilities, codec support, dict seed.
	OpWelcome     Op = 0x101 // Server assigns identity, epoch, room config.
	OpRecording   Op = 0x102
	OpScreenShare Op = 0x103 // Server to all: the presenter changed. {stopped str, by str}.
	OpBreakout    Op = 0x104
	OpQualityCap  Op = 0x105 // Renegotiate a hard capability change.
	OpStatsDump   Op = 0x106 // On-demand full ledger snapshot.
	OpGoodbye     Op = 0x107
	OpViewport    Op = 0x108 // Painted size and visibility of every tile.
	OpConvMessage Op = 0x109 // Persistent chat: a new or changed message.
	OpTyping      Op = 0x10a // Persistent chat: somebody is composing.
	OpConvRead    Op = 0x10b // Persistent chat: a read cursor moved.
	OpPresence    Op = 0x10c // Somebody came online or went away.

	// Remote control. OpControlInput is the only high-rate one; the rest are
	// lifecycle and arrive a handful of times per session.
	OpControlState Op = 0x10d // A control session changed state.
	OpControlInput Op = 0x10e // A batch of input events for the controlled machine.
	OpControlFrame Op = 0x10f // A screen frame from the controlled machine.

	// The waiting room. A knocker receives OpLobbyStatus and nothing else:
	// until they are admitted they are not in the meeting, so no roster, no
	// chat and no media frame is addressed to them.
	OpLobbyStatus   Op = 0x110 // Server to a waiter: where their knock stands.
	OpLobbyList     Op = 0x111 // Server to hosts: who is waiting.
	OpLobbyDecide   Op = 0x112 // Host to server: admit or deny one waiter.
	OpMeetingPolicy Op = 0x113 // Admission policy and lock state.

	// What attendees may do, decided per meeting by its host. Enforced on the
	// server; the client's copy only greys out buttons.
	OpPermissions Op = 0x114 // Host to server, and server to everybody.
	OpMuteAll     Op = 0x115 // Host to server: mute every non-host now.

	// Media over the signalling socket itself. The fallback for a network
	// that blocks UDP so thoroughly that even a TURN relay cannot form a pair
	// -- which a campus network did, three tests running. A WebSocket rides
	// the HTTPS connection that demonstrably works (chat never once failed
	// there), so media on it cannot be blocked without blocking the site.
	// Zoom's web client shipped exactly this. WebRTC remains the fast path;
	// this is what carries the call when it cannot.
	//
	// One frame per encoded chunk: [publisher str][kind+key bits]
	// [timestamp uvarint, video only][payload to the end of the frame]. The
	// server rewrites the publisher and fans out to everyone else in the
	// room; it never decodes. Its value is 14 -- one of the sixteen one-byte
	// opcodes -- because at fifty frames a second per speaker the second
	// header byte was the largest avoidable cost on the whole socket
	// (measured: 1 of 172 bytes on the wire per audio frame). The in-meeting
	// chat op it displaced is a few frames a minute.
	OpChat Op = 0x116 // Text + reactions in a meeting. Moved from 14 to make room for media.

	// Calling somebody, the way Teams does: a ring on their persistent chat
	// socket, wherever they are signed in, with a room to join if they pick
	// up. Client to server: [target user str][room str]. Server to the
	// target: [caller id str][caller name str][room str]. The answer goes the
	// other way with an accepted bit, so the caller's screen can say
	// "declined" rather than wait forever.
	OpRing       Op = 0x117
	OpRingAnswer Op = 0x118

	// A line of live caption, recognised on the speaker's own machine.
	//
	// Client to server: [text str][final bit][7 reserved]. Server to everyone
	// else: [speaker id str][text str][final bit][7 reserved].
	//
	// The whole current sentence rides in every frame; it is never a delta.
	// A caption assembled from fragments would be permanently wrong if one
	// were dropped, and repairing it would cost more than resending -- the
	// cross-frame-state rule, applied to text. The client throttles interim
	// results so this costs a few frames a second while somebody is talking
	// and nothing at all when they are not.
	OpCaption Op = 0x119

	// A reaction: one emoji, thrown at the room and gone.
	//
	// Client to server: [emoji str]. Server to everyone: [sender id str]
	// [emoji str]. Deliberately not stored anywhere. A reaction is a gesture
	// during a moment, and a log of them is neither wanted nor kept.
	OpReaction Op = 0x11a
	// OpReadCursor: persistent chat, server to members -- somebody's read
	// cursor moved. What makes "seen by" true rather than eventual. Only sent
	// for conversations small enough for a seen-by row to mean anything.
	OpReadCursor Op = 0x11b

	// OpStatus: server to client. Somebody you share a conversation with
	// changed their availability. {who str, status str, note str, until ms}.
	OpStatus Op = 0x11c
	// OpIdle: client to server. This device has had no input for a while
	// (true) or has input again (false). A person is idle when every device
	// of theirs is; idle reads as Away, automatically. {idle bool, 7 pad}.
	OpIdle Op = 0x11d
	// OpSameDevice: server to client, at join. Somebody else in this room
	// connected from the same address as you -- almost always a second
	// client on the same machine. Their loudspeaker is not a reference
	// your echo canceller can subtract, so each of you hears yourself back;
	// the client says so once. {others uvarint}.
	OpSameDevice Op = 0x11e

	// OpSpotlight: who the host has put on everybody's stage (room/spotlight.go).
	// Client to server: {id str, on bool, 7 spare bits}; id "" with on false
	// is "Stop all spotlights". Server to everybody, only when the list
	// changed, and at join only when it is not empty: {n uvarint, n × id str},
	// always the whole list, so one lost frame is repaired by the next. The
	// ids are already in the dictionary from the roster, so each costs a byte.
	OpSpotlight Op = 0x11f

	// OpEndMeeting: host to server, empty: end the meeting for everyone.
	// Server to each participant, empty, flushed just before their connection
	// is closed, so the client can say "This meeting has ended" rather than
	// showing a dropped link.
	OpEndMeeting Op = 0x120

	// OpSetRole: a meeting role (room/roles.go). Host to server: "Make a
	// presenter", "Make an attendee", co-organizer. Server to everybody
	// whenever a role changes, including the host handing over on leaving,
	// which nothing announced before: the new host's controls stayed grey.
	// {id str, role 2 bits, 6 spare bits} both ways.
	OpSetRole Op = 0x121

	// OpRemove: host to server {id str}, "Remove from meeting". Server to the
	// person removed, empty, flushed before their connection is closed.
	OpRemove Op = 0x122

	// OpLowerHands: host to server, empty, "Lower all hands". Server to each
	// person whose hand was up, empty: "Your hand was lowered".
	OpLowerHands Op = 0x123

	// OpBlock: "Disable mic" / "Disable camera" for one attendee (room/roles.go
	// SetBlocked). Host to server, and server to everybody on a change and to
	// an arrival for each person disabled: {id str, mic bool, cam bool, 6 spare}.
	OpBlock Op = 0x124

	// OpQnA: server to the people in a meeting, one Q&A change as JSON
	// ({"question": ...} or {"settings": ...}): whole, so the panel draws it
	// without asking. Questions not yet published go only to the hosts and
	// their author. Rare and human-paced; measured in ARCHITECTURE.
	OpQnA Op = 0x125

	// OpNotes: server to the people in a meeting, one meeting-notes change as
	// JSON ({"created": true} or {"item": ...}), whole.
	OpNotes Op = 0x126

	// OpWhiteboard: the board (room/whiteboard.go, signal/whiteboard.go):
	// open, strokes, live previews, erase, sticky notes, clear, close.
	OpWhiteboard Op = 0x127

	// OpTogether: Together mode (room/together.go, signal/together.go): on or
	// off for everyone and the scene, and each person's 64 x 36 outline.
	OpTogether Op = 0x128

	// OpActivity: the activity feed on the chat socket (messaging/activity.go):
	// a new item, ids that became read, an item taken back.
	OpActivity Op = 0x129

	// OpSlots: server to one receiver, what each of its WebRTC media slots is
	// carrying (sfu/slots.go). Sent whenever a slot is re-pointed.
	OpSlots Op = 0x12a
)

var ErrBadVersion = errors.New("wire: unsupported protocol version")
var ErrNestedBatch = errors.New("wire: batch frames may not nest")

// Header is a decoded frame prologue.
type Header struct {
	Op    Op
	ReqID uint64
	HasID bool
}

// WriteHeader emits the control byte plus optional request id and extended
// opcode byte. Returns the number of bytes the header occupied.
func (w *Writer) WriteHeader(op Op, reqID uint64, hasID bool) int {
	start := len(w.buf)
	ctrl := byte(Version<<6) | byte(op&maskOp)
	if op > 0x0f {
		ctrl = byte(Version<<6) | flagExt | byte((op>>8)&maskOp)
	}
	if hasID {
		ctrl |= flagReqID
	}
	w.U8(ctrl)
	if hasID {
		w.Uvarint(reqID)
	}
	if op > 0x0f {
		w.U8(byte(op & 0xff))
	}
	return len(w.buf) - start
}

// ReadHeader decodes a frame prologue.
func (r *Reader) ReadHeader() (Header, error) {
	ctrl := r.U8()
	if r.err != nil {
		return Header{}, r.err
	}
	if ctrl&maskVer != Version<<6 {
		return Header{}, ErrBadVersion
	}
	var h Header
	if ctrl&flagReqID != 0 {
		h.ReqID = r.Uvarint()
		h.HasID = true
	}
	if ctrl&flagExt != 0 {
		hi := Op(ctrl&maskOp) << 8
		h.Op = hi | Op(r.U8())
	} else {
		h.Op = Op(ctrl & maskOp)
	}
	return h, r.err
}

// EncodeBatch packs several already-encoded frames into one OpBatch frame.
//
// This exists to amortise the transport header, not the protocol header.
// A client-to-server WebSocket frame costs 6 bytes minimum because RFC 6455
// mandates a 4-byte mask on every client frame; server-to-client costs 2.
// Sending three 4-byte state deltas as three messages costs 3*(6+4)=30 bytes
// upstream. Batched it is 6+1+3*(1+4)=22. At scale, on a coalescing timer of
// 15-20ms, this reclaims a double-digit percentage of signalling bytes for
// free -- and coalescing also cuts syscalls and wakeups, which is where the
// server-side CPU actually goes.
func EncodeBatch(dst *Writer, frames [][]byte) error {
	return EncodeBatchParts(dst, frames, nil)
}

// EncodeBatchParts is EncodeBatch for frames in two parts: frame i is
// heads[i] followed by tails[i] (tails may be nil, or hold nil entries). A
// media frame's payload is the same bytes for every receiver; only the
// header before it differs (it names the publisher in each receiver's own
// dictionary). Kept apart, one payload serves the whole room and is copied
// once per receiver, into that connection's reusable write buffer, instead
// of once per receiver into a frame of its own (ARCHITECTURE 6bs).
func EncodeBatchParts(dst *Writer, heads, tails [][]byte) error {
	tail := func(i int) []byte {
		if i < len(tails) {
			return tails[i]
		}
		return nil
	}
	if len(heads) == 1 {
		dst.buf = append(dst.buf, heads[0]...)
		dst.buf = append(dst.buf, tail(0)...)
		return nil
	}
	dst.WriteHeader(OpBatch, 0, false)
	for i, f := range heads {
		if len(f) > 0 && f[0]&maskVer == Version<<6 &&
			f[0]&flagExt == 0 && Op(f[0]&maskOp) == OpBatch {
			return ErrNestedBatch
		}
		t := tail(i)
		dst.Uvarint(uint64(len(f) + len(t)))
		dst.align()
		dst.buf = append(dst.buf, f...)
		dst.buf = append(dst.buf, t...)
	}
	return nil
}

// SplitBatch walks an OpBatch body, yielding each contained frame. The
// callback receives sub-slices of the input; it must not retain them.
func SplitBatch(r *Reader, fn func([]byte) error) error {
	for r.Remaining() > 0 {
		v := r.Uvarint()
		if r.err != nil {
			return r.err
		}
		r.align()
		// Checked as uint64 against what remains: converting first would let a
		// huge length wrap r.pos+n negative and slip past the bound.
		if v == 0 || v > uint64(r.Left()) {
			return ErrTruncated
		}
		n := int(v)
		f := r.buf[r.pos : r.pos+n]
		r.pos += n
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}
