package rc

import (
	"encoding/binary"
	"errors"
	"time"
	"unicode/utf8"
)

// Event kinds.
const (
	KindMouseMove   uint8 = 0
	KindMouseButton uint8 = 1
	KindMouseWheel  uint8 = 2
	KindKey         uint8 = 3
	KindText        uint8 = 4
	KindRelease     uint8 = 5 // release every held button and key
)

// Button identifiers.
type Button uint8

const (
	ButtonLeft Button = iota
	ButtonRight
	ButtonMiddle
	ButtonX1
	ButtonX2
	buttonCount
)

// Coordinates are normalised to the full 16-bit range rather than sent in
// pixels.
//
// Two reasons, both of which have bitten remote control tools. The controller
// sees a scaled, possibly letterboxed copy of the screen, so pixel
// coordinates would have to be translated somewhere, and every translation is
// a chance to click 40 pixels from where the operator was looking. And the
// controlled machine may change resolution mid-session -- a display unplugged,
// a laptop docked -- which silently invalidates every pixel coordinate in
// flight. A normalised coordinate means the same relative point on whatever
// the screen currently is, and it happens to be exactly what the Windows
// injection API wants, so nothing is converted twice.
const CoordMax = 65535

// MaxTextRun bounds one text event. Text arrives from a human typing, so this
// is generous; it exists to bound the decoder, not the user.
const MaxTextRun = 255

var (
	ErrShortEvent   = errors.New("rc: event shorter than its kind requires")
	ErrUnknownKind  = errors.New("rc: unknown event kind")
	ErrBadButton    = errors.New("rc: button out of range")
	ErrTextTooLong  = errors.New("rc: text run exceeds limit")
	ErrInvalidUTF8  = errors.New("rc: text run is not valid utf-8")
	ErrNotPermitted = errors.New("rc: no valid grant for this session")
)

// Event is one input action, complete in itself.
//
// Nothing here is a delta, and that is deliberate. See the package comment:
// screen frames are idempotent and a wrong one is repaired by the next, while
// a click at the wrong coordinates is an irreversible action taken as the
// user. A whole second of dense mouse movement costs about 500 bytes against
// roughly 190 kbit/s for the screen, so certainty about where clicks land is
// bought for about a third of one percent.
type Event struct {
	Kind uint8

	// X and Y are normalised to CoordMax and carried by every mouse event,
	// including buttons and wheel. A click therefore never depends on a
	// preceding move having arrived.
	X, Y uint16

	Button Button
	Down   bool

	// WheelX and WheelY are in notches, signed. Horizontal scroll is
	// included because tables and timelines are exactly the things a support
	// operator needs to scroll sideways.
	WheelX, WheelY int16

	// VK is a virtual key code, and Text is a literal string.
	//
	// Both exist because they solve different problems. VK carries the
	// physical key, which is what shortcuts, modifiers and arrow keys need.
	// Text carries what the user actually meant to type, which is the only
	// thing that survives a different keyboard layout on the far machine, an
	// IME, or a compose sequence. Sending a Danish operator's keystrokes as
	// scancodes to a US-layout machine produces the wrong letters; sending
	// the resulting text does not.
	VK   uint16
	Text string
}

// Encode appends the wire form of an event to dst.
func (e Event) Encode(dst []byte) ([]byte, error) {
	switch e.Kind {
	case KindMouseMove:
		dst = append(dst, KindMouseMove)
		dst = binary.BigEndian.AppendUint16(dst, e.X)
		dst = binary.BigEndian.AppendUint16(dst, e.Y)
	case KindMouseButton:
		if e.Button >= buttonCount {
			return nil, ErrBadButton
		}
		flags := uint8(e.Button)
		if e.Down {
			flags |= 0x80
		}
		dst = append(dst, KindMouseButton, flags)
		dst = binary.BigEndian.AppendUint16(dst, e.X)
		dst = binary.BigEndian.AppendUint16(dst, e.Y)
	case KindMouseWheel:
		dst = append(dst, KindMouseWheel)
		dst = binary.BigEndian.AppendUint16(dst, uint16(e.WheelX))
		dst = binary.BigEndian.AppendUint16(dst, uint16(e.WheelY))
		dst = binary.BigEndian.AppendUint16(dst, e.X)
		dst = binary.BigEndian.AppendUint16(dst, e.Y)
	case KindKey:
		var d uint8
		if e.Down {
			d = 1
		}
		dst = append(dst, KindKey)
		dst = binary.BigEndian.AppendUint16(dst, e.VK)
		dst = append(dst, d)
	case KindText:
		if len(e.Text) > MaxTextRun {
			return nil, ErrTextTooLong
		}
		if !utf8.ValidString(e.Text) {
			return nil, ErrInvalidUTF8
		}
		dst = append(dst, KindText, uint8(len(e.Text)))
		dst = append(dst, e.Text...)
	case KindRelease:
		dst = append(dst, KindRelease)
	default:
		return nil, ErrUnknownKind
	}
	return dst, nil
}

// DecodeEvent reads one event and returns the remaining bytes.
//
// This parses input that arrives from another machine, so it treats every
// length as hostile. A malformed event must produce an error, never a panic
// and never a plausible-looking event: an out-of-range slice here would be a
// crash in the agent that holds the keyboard.
func DecodeEvent(b []byte) (Event, []byte, error) {
	if len(b) < 1 {
		return Event{}, nil, ErrShortEvent
	}
	kind := b[0]
	switch kind {
	case KindMouseMove:
		if len(b) < 5 {
			return Event{}, nil, ErrShortEvent
		}
		return Event{
			Kind: kind,
			X:    binary.BigEndian.Uint16(b[1:]),
			Y:    binary.BigEndian.Uint16(b[3:]),
		}, b[5:], nil

	case KindMouseButton:
		if len(b) < 6 {
			return Event{}, nil, ErrShortEvent
		}
		btn := Button(b[1] & 0x7f)
		if btn >= buttonCount {
			return Event{}, nil, ErrBadButton
		}
		return Event{
			Kind:   kind,
			Button: btn,
			Down:   b[1]&0x80 != 0,
			X:      binary.BigEndian.Uint16(b[2:]),
			Y:      binary.BigEndian.Uint16(b[4:]),
		}, b[6:], nil

	case KindMouseWheel:
		if len(b) < 9 {
			return Event{}, nil, ErrShortEvent
		}
		return Event{
			Kind:   kind,
			WheelX: int16(binary.BigEndian.Uint16(b[1:])),
			WheelY: int16(binary.BigEndian.Uint16(b[3:])),
			X:      binary.BigEndian.Uint16(b[5:]),
			Y:      binary.BigEndian.Uint16(b[7:]),
		}, b[9:], nil

	case KindKey:
		if len(b) < 4 {
			return Event{}, nil, ErrShortEvent
		}
		return Event{
			Kind: kind,
			VK:   binary.BigEndian.Uint16(b[1:]),
			Down: b[3] != 0,
		}, b[4:], nil

	case KindText:
		if len(b) < 2 {
			return Event{}, nil, ErrShortEvent
		}
		n := int(b[1])
		if len(b) < 2+n {
			return Event{}, nil, ErrShortEvent
		}
		s := string(b[2 : 2+n])
		if !utf8.ValidString(s) {
			return Event{}, nil, ErrInvalidUTF8
		}
		return Event{Kind: kind, Text: s}, b[2+n:], nil

	case KindRelease:
		return Event{Kind: kind}, b[1:], nil
	}
	return Event{}, nil, ErrUnknownKind
}

// DecodeAll parses a batch of events.
//
// Input is batched because it arrives in bursts -- a drag is a hundred moves
// in a second -- and one WebSocket frame carrying twenty events costs one set
// of framing rather than twenty.
func DecodeAll(b []byte) ([]Event, error) {
	var out []Event
	for len(b) > 0 {
		e, rest, err := DecodeEvent(b)
		if err != nil {
			return out, err
		}
		out = append(out, e)
		b = rest
	}
	return out, nil
}

// Coalesce collapses redundant mouse movement.
//
// Consecutive moves with nothing between them are redundant: only the last
// position matters, because the cursor is not being sampled in between. This
// matters when a burst arrives after a stall, where replaying every
// intermediate position wastes time moving the cursor through history instead
// of putting it where the operator is now pointing.
//
// The rule is strict about ordering. A move is only dropped if the event
// immediately after it is also a move. Anything else -- a click, a key, a
// wheel notch, a release -- fixes the moves before it in place, because the
// position at the moment of a click is part of what the click means.
func Coalesce(events []Event) []Event {
	if len(events) < 2 {
		return events
	}
	out := events[:0:0]
	for i, e := range events {
		if e.Kind == KindMouseMove && i+1 < len(events) &&
			events[i+1].Kind == KindMouseMove {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Input send rate.
//
// Measured on loopback with tshark, a control session costs almost exactly
// the same number of BYTES PER MESSAGE regardless of what the message
// contains, because a 5-byte mouse move rides 40 bytes of IP, 40 of TCP and
// a WebSocket header: roughly 90 bytes on the wire either way. The cost of
// input is therefore set by how often it is sent, not by how much is in it,
// and the relationship is linear:
//
//	15 ms window   35.3 kbit/s
//	30 ms window   17.6 kbit/s
//	60 ms window    9.1 kbit/s
//
// Sharing the connection with the screen stream was measured too, on the
// theory that an input message could ride a TCP segment already being sent to
// acknowledge incoming screen data. It saved 1.4%: 87,102 bytes against
// 88,304. On loopback the acknowledgement is immediate so there is nothing to
// piggyback on, and while a real network with delayed acknowledgement would do
// better, it is not a lever worth designing around.
//
// So the window is chosen against latency instead, and the two cases have
// genuinely different requirements:
//
//   - While a button is held the operator is dragging, and every millisecond
//     of delay shows up as the dragged object trailing behind the cursor.
//     That is the most latency-sensitive thing in remote control and it gets
//     the tight window.
//
//   - While merely moving, the cursor is the only thing on screen that moves.
//     45 ms is 22 updates a second, which reads as smooth, and costs a third
//     as much.
//
// Typing takes the tight window as well: a character appearing late is more
// noticeable than a cursor arriving late, because the operator is watching
// the text rather than the pointer.
const (
	DragWindow  = 15 * time.Millisecond
	HoverWindow = 45 * time.Millisecond
)

// InputWindow reports how long to accumulate input before sending it.
func InputWindow(dragging bool) time.Duration {
	if dragging {
		return DragWindow
	}
	return HoverWindow
}
