package wire

import (
	"errors"
	"math"
)

var (
	ErrTruncated  = errors.New("wire: truncated frame")
	ErrBadVarint  = errors.New("wire: malformed varint")
	ErrBadHandle  = errors.New("wire: dictionary handle out of range")
	ErrStrTooLong = errors.New("wire: interned string exceeds limit")
)

// MaxInternedString bounds a literal string on the wire. Anything longer is
// a protocol abuse attempt, not a display name.
const MaxInternedString = 512

// Reader is the exact mirror of Writer. It is deliberately allocation-free
// on every path except literal string interning: it slices the input buffer
// rather than copying it, so the caller must not retain returned blobs past
// the lifetime of the frame.
type Reader struct {
	buf  []byte
	pos  int
	acc  uint32
	nbit uint8
	dict *Dict
	err  error
}

func NewReader(b []byte, dict *Dict) *Reader {
	return &Reader{buf: b, dict: dict}
}

// Err returns the first error encountered. The decoder is fail-slow by
// design: every accessor returns a zero value after an error, so message
// decoders can read a whole struct and check once at the end instead of
// checking after every field.
func (r *Reader) Err() error { return r.err }

func (r *Reader) fail(e error) {
	if r.err == nil {
		r.err = e
	}
}

// Remaining reports unread whole bytes.
func (r *Reader) Remaining() int {
	if r.pos > len(r.buf) {
		return 0
	}
	return len(r.buf) - r.pos
}

func (r *Reader) align() {
	r.acc, r.nbit = 0, 0
}

// Bits reads n bits, most significant first.
func (r *Reader) Bits(n uint8) uint32 {
	var out uint32
	for n > 0 {
		if r.nbit == 0 {
			if r.pos >= len(r.buf) {
				r.fail(ErrTruncated)
				return 0
			}
			r.acc = uint32(r.buf[r.pos])
			r.pos++
			r.nbit = 8
		}
		take := r.nbit
		if take > n {
			take = n
		}
		shift := r.nbit - take
		chunk := (r.acc >> shift) & ((1 << take) - 1)
		out = out<<take | chunk
		r.nbit -= take
		n -= take
	}
	return out
}

func (r *Reader) Bool() bool { return r.Bits(1) == 1 }

func (r *Reader) U8() uint8 {
	r.align()
	if r.pos >= len(r.buf) {
		r.fail(ErrTruncated)
		return 0
	}
	v := r.buf[r.pos]
	r.pos++
	return v
}

func (r *Reader) U16() uint16 {
	r.align()
	if r.pos+2 > len(r.buf) {
		r.fail(ErrTruncated)
		return 0
	}
	v := uint16(r.buf[r.pos])<<8 | uint16(r.buf[r.pos+1])
	r.pos += 2
	return v
}

func (r *Reader) U32() uint32 {
	r.align()
	if r.pos+4 > len(r.buf) {
		r.fail(ErrTruncated)
		return 0
	}
	v := uint32(r.buf[r.pos])<<24 | uint32(r.buf[r.pos+1])<<16 |
		uint32(r.buf[r.pos+2])<<8 | uint32(r.buf[r.pos+3])
	r.pos += 4
	return v
}

// Uvarint decodes LEB128, rejecting encodings longer than 10 bytes so a
// hostile peer cannot spin us on a padded varint.
func (r *Reader) Uvarint() uint64 {
	r.align()
	var v uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if r.pos >= len(r.buf) {
			r.fail(ErrTruncated)
			return 0
		}
		b := r.buf[r.pos]
		r.pos++
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v
		}
		shift += 7
	}
	r.fail(ErrBadVarint)
	return 0
}

func (r *Reader) Svarint() int64 { return UnZigZag(r.Uvarint()) }

func (r *Reader) F32() float32 { return math.Float32frombits(r.U32()) }

// Q8 is the inverse of Writer.Q8.
func (r *Reader) Q8() float64 { return float64(r.U8()) / 255.0 }

// Left reports how many bytes remain, never negative.
func (r *Reader) Left() int {
	if r.pos >= len(r.buf) {
		return 0
	}
	return len(r.buf) - r.pos
}

// ReadLen reads a length prefix and validates it BEFORE narrowing it to int.
//
// The order matters and is the whole point of this function. Uvarint returns
// a uint64 that came off the wire, so it can be anything a client chose to
// send; converting first and checking afterwards is unsound in two separate
// ways, both of which were live here:
//
//   - a value above MaxInt64 narrows to a NEGATIVE int, which sails past any
//     "n > limit" guard and reaches make([]T, 0, n) as a negative capacity.
//     That panics with "makeslice: cap out of range".
//   - a merely enormous positive value passes an "n < 0" guard, and then
//     r.pos+n OVERFLOWS to negative, which sails past "r.pos+n > len(buf)"
//     and reaches a slice expression with a negative bound.
//
// Both are reachable by any client that can open a socket, and a panic in Go
// is not contained to one connection: it takes the process down, and every
// meeting on the node with it. Comparing in uint64 space, where there are no
// negatives and no wraparound below the limit, removes the whole class.
func (r *Reader) ReadLen(max int) (int, bool) {
	v := r.Uvarint()
	if r.err != nil {
		return 0, false
	}
	if max < 0 {
		max = 0
	}
	if v > uint64(max) {
		r.fail(ErrTruncated)
		return 0, false
	}
	return int(v), true
}

// Tail returns everything left in the frame -- the payload of a frame whose
// last field needs no length because the frame itself is delimited (by the
// WebSocket message, or by the batch container's per-frame length). No copy;
// valid only until the frame buffer is recycled.
func (r *Reader) Tail() []byte {
	r.align()
	if r.err != nil {
		return nil
	}
	b := r.buf[r.pos:]
	r.pos = len(r.buf)
	return b
}

// Blob returns a sub-slice of the frame buffer -- no copy. Valid only until
// the frame buffer is recycled.
func (r *Reader) Blob() []byte {
	v := r.Uvarint()
	r.align()
	if r.err != nil {
		return nil
	}
	// Bounded by what is actually present, compared as uint64 so a huge
	// length can neither become negative nor overflow the addition.
	if v > uint64(r.Left()) {
		r.fail(ErrTruncated)
		return nil
	}
	n := int(v)
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b
}

// Str mirrors Writer.Str, resolving handles against the shared dictionary
// and interning literals so both sides stay in lockstep.
func (r *Reader) Str() string {
	tag := r.Uvarint()
	if r.err != nil {
		return ""
	}
	if tag&1 == 0 {
		h := uint32(tag >> 1)
		if r.dict == nil {
			r.fail(ErrBadHandle)
			return ""
		}
		s, ok := r.dict.Lookup(h)
		if !ok {
			r.fail(ErrBadHandle)
			return ""
		}
		return s
	}
	n := int(tag >> 1)
	if n > MaxInternedString {
		r.fail(ErrStrTooLong)
		return ""
	}
	r.align()
	if r.pos+n > len(r.buf) {
		r.fail(ErrTruncated)
		return ""
	}
	s := string(r.buf[r.pos : r.pos+n])
	r.pos += n
	if r.dict != nil {
		r.dict.Intern(s)
	}
	return s
}
