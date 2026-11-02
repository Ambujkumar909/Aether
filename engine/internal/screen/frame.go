// Package screen is the remote-control capture path: turning a desktop into
// the fewest bytes that still show it faithfully.
//
// The design is constrained by a lesson learned expensively elsewhere. A
// delta codec was built for this exact job in a sibling project — 16x16
// tiles, a content-addressed cache, keyframes, a lossy refinement layer. On
// paper it was 24x cheaper per frame (~5 KB against ~122 KB). In practice it
// produced banding, permanent desynchronisation whenever a frame was
// dropped, a keyframe trap, a refinement loop that never converged, and
// reconnect storms that spent 13-21 MB to display one screen. Measured on
// the wire at 586 KB/s against a 331 KB/s ceiling.
//
// Every one of those failures had the same root: CROSS-FRAME STATE IN THE
// CODEC. A delta is only valid against a history both ends agree on, so any
// loss, reorder or timeout corrupts everything after it — and the recovery
// is the most expensive operation in the system, which worsens congestion
// exactly when it is already bad.
//
// So here, every frame is a COMPLETE, SELF-CONTAINED JPEG of the rectangle
// it covers. It decodes without reference to any other frame. What this
// package optimises instead is which rectangles are worth sending at all:
//
//  1. an unchanged screen sends nothing whatsoever
//  2. a changed screen sends only the rectangle that changed
//  3. a byte budget drops whole frames under pressure, which is safe
//     precisely because frames are independent
//
// There is exactly one piece of accumulated state — the viewer's canvas,
// since it retains rectangles previously drawn — and its recovery is
// deliberately the cheapest possible: a sequence number per frame, a gap
// detected by the viewer, and a full frame sent in reply. Recovery costs the
// same as an ordinary frame, so loss is bounded and self-healing.
package screen

import (
	"encoding/binary"
	"errors"
)

// Magic marks a remote-control frame. 'F' is chosen because it cannot be
// mistaken for a raw JPEG, whose first byte is always 0xFF.
const Magic = 'F'

// Frame flags.
const (
	// FlagFull marks a frame covering the whole screen, which resynchronises
	// the viewer completely whatever state its canvas was in.
	FlagFull = 1 << 0
	// FlagCursor marks a cursor-only update: position, no pixels.
	FlagCursor = 1 << 1
)

// HeaderLen is magic(1) + flags(1) + seq(2) + x,y,w,h(2 each) = 12 bytes.
//
// Twelve bytes against a JPEG payload measured in kilobytes is under 0.5%,
// so this is deliberately byte-aligned and readable rather than bit-packed.
// Squeezing it to 5.5 bytes with 11-bit coordinates would save roughly 50
// bytes per second at 20 fps and cost every future reader of this file the
// ability to see what a frame contains in a hex dump.
const HeaderLen = 12

// MaxDimension bounds a rectangle. Anything larger is a malformed frame, not
// a large screen.
const MaxDimension = 16384

var (
	ErrShortFrame = errors.New("screen: frame shorter than its header")
	ErrBadMagic   = errors.New("screen: not a remote-control frame")
	ErrBadRect    = errors.New("screen: rectangle out of range")
)

// Header describes one captured rectangle.
type Header struct {
	Flags uint8
	Seq   uint16
	X, Y  uint16
	W, H  uint16
}

// Full reports whether this frame resynchronises the whole screen.
func (h Header) Full() bool { return h.Flags&FlagFull != 0 }

// Encode writes the header into dst, which must be at least HeaderLen.
func (h Header) Encode(dst []byte) {
	_ = dst[HeaderLen-1] // bounds check once
	dst[0] = Magic
	dst[1] = h.Flags
	binary.BigEndian.PutUint16(dst[2:], h.Seq)
	binary.BigEndian.PutUint16(dst[4:], h.X)
	binary.BigEndian.PutUint16(dst[6:], h.Y)
	binary.BigEndian.PutUint16(dst[8:], h.W)
	binary.BigEndian.PutUint16(dst[10:], h.H)
}

// DecodeHeader parses a frame prologue and returns the payload that follows.
func DecodeHeader(b []byte) (Header, []byte, error) {
	if len(b) < HeaderLen {
		return Header{}, nil, ErrShortFrame
	}
	if b[0] != Magic {
		return Header{}, nil, ErrBadMagic
	}
	h := Header{
		Flags: b[1],
		Seq:   binary.BigEndian.Uint16(b[2:]),
		X:     binary.BigEndian.Uint16(b[4:]),
		Y:     binary.BigEndian.Uint16(b[6:]),
		W:     binary.BigEndian.Uint16(b[8:]),
		H:     binary.BigEndian.Uint16(b[10:]),
	}
	if h.W > MaxDimension || h.H > MaxDimension {
		return Header{}, nil, ErrBadRect
	}
	return h, b[HeaderLen:], nil
}

// SeqGap reports how many frames were missed between two sequence numbers,
// correct across the 16-bit wrap.
//
// The viewer calls this on every frame. A non-zero result means its canvas
// now holds a stale rectangle, and the only correct response is to ask for a
// full frame — which costs exactly what an ordinary frame costs, and is the
// whole reason this design survives loss where a delta codec did not.
func SeqGap(prev, next uint16) int {
	return int(next-prev) - 1
}
