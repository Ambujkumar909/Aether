package wire

import (
	"errors"
	"math"
)

// Writer serialises a single control frame. It supports both byte-oriented
// and bit-oriented writes; bit writes are buffered and flushed on the next
// byte-aligned operation, so a struct of six booleans costs six bits, not
// six bytes.
type Writer struct {
	buf  []byte
	acc  uint32 // pending sub-byte bits, left aligned in the low bits
	nbit uint8  // count of pending bits, always < 8
	dict *Dict
}

// NewWriter returns a Writer sharing the connection's interning dictionary.
// Passing a nil dict disables interning (useful for one-shot frames such as
// the handshake, which must be decodable before a dictionary exists).
func NewWriter(dict *Dict) *Writer {
	return &Writer{buf: make([]byte, 0, 64), dict: dict}
}

// Reset reuses the backing array across frames. The SFU writes tens of
// thousands of control frames per second; this keeps that allocation-free.
func (w *Writer) Reset() {
	w.buf = w.buf[:0]
	w.acc, w.nbit = 0, 0
}

// Bytes returns the finished frame, flushing any partial bit accumulator.
func (w *Writer) Bytes() []byte {
	w.align()
	return w.buf
}

// Len is the current encoded size in bytes, counting a partial bit
// accumulator as a full byte. Used by the ledger for exact accounting.
func (w *Writer) Len() int {
	n := len(w.buf)
	if w.nbit > 0 {
		n++
	}
	return n
}

// align flushes pending bits, zero padding to the byte boundary.
func (w *Writer) align() {
	if w.nbit > 0 {
		w.buf = append(w.buf, byte(w.acc<<(8-w.nbit)))
		w.acc, w.nbit = 0, 0
	}
}

// Bits writes the low n bits of v, most significant first. n must be <= 32.
func (w *Writer) Bits(v uint32, n uint8) {
	for n > 0 {
		take := 8 - w.nbit
		if take > n {
			take = n
		}
		chunk := (v >> (n - take)) & ((1 << take) - 1)
		w.acc = w.acc<<take | chunk
		w.nbit += take
		n -= take
		if w.nbit == 8 {
			w.buf = append(w.buf, byte(w.acc))
			w.acc, w.nbit = 0, 0
		}
	}
}

// Bool writes exactly one bit.
func (w *Writer) Bool(b bool) {
	if b {
		w.Bits(1, 1)
	} else {
		w.Bits(0, 1)
	}
}

func (w *Writer) U8(v uint8) {
	w.align()
	w.buf = append(w.buf, v)
}

func (w *Writer) U16(v uint16) {
	w.align()
	w.buf = append(w.buf, byte(v>>8), byte(v))
}

func (w *Writer) U32(v uint32) {
	w.align()
	w.buf = append(w.buf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Uvarint writes an LEB128 unsigned integer.
func (w *Writer) Uvarint(v uint64) {
	w.align()
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

// Svarint writes a zigzag-then-LEB128 signed integer. This is the encoding
// for every delta in the protocol: bitrate deltas, timestamp deltas,
// sequence deltas.
func (w *Writer) Svarint(v int64) { w.Uvarint(ZigZag(v)) }

// F32 writes an IEEE-754 float. Used sparingly -- most "float" quantities in
// this protocol are quantised to integers with a documented scale factor
// instead, because a quantised audio level in 1 byte beats a float in 4.
func (w *Writer) F32(v float32) { w.U32(math.Float32bits(v)) }

// Q8 quantises a 0.0..1.0 ratio into a single byte. Audio levels, packet
// loss fractions, and quality scores all use this: 1/255 resolution is well
// below perceptual and control-loop relevance, and it saves 3 bytes each.
func (w *Writer) Q8(v float64) {
	switch {
	case v <= 0:
		w.U8(0)
	case v >= 1:
		w.U8(255)
	default:
		w.U8(uint8(v*255 + 0.5))
	}
}

// Raw writes bytes with no length prefix: the last field of a frame, whose
// end is the frame's end. See Reader.Tail.
func (w *Writer) Raw(b []byte) {
	w.align()
	w.buf = append(w.buf, b...)
}

// Blob writes a length-prefixed byte slice.
func (w *Writer) Blob(b []byte) {
	w.Uvarint(uint64(len(b)))
	w.align()
	w.buf = append(w.buf, b...)
}

// Str writes a dictionary-interned string.
//
// Encoding: one uvarint tag. Low bit 0 means "handle", and the remaining
// bits are the dictionary index. Low bit 1 means "literal", and the
// remaining bits are the byte length, followed by the raw bytes; both sides
// then append it to their dictionaries at the next index.
//
// A room with 50 participants exchanging state updates references each
// participant ID thousands of times. Interning turns 36 bytes into 1.
func (w *Writer) Str(s string) {
	if w.dict == nil {
		w.Uvarint(uint64(len(s))<<1 | 1)
		w.align()
		w.buf = append(w.buf, s...)
		return
	}
	if h, ok := w.dict.Handle(s); ok {
		w.Uvarint(uint64(h) << 1)
		return
	}
	w.Uvarint(uint64(len(s))<<1 | 1)
	w.align()
	w.buf = append(w.buf, s...)
	w.dict.Intern(s)
}

var ErrFrameTooLarge = errors.New("wire: frame exceeds maximum control frame size")

// MaxFrame caps a single control frame. Control traffic that wants more than
// this is a design error -- bulk payloads belong on a media or data channel,
// not on the signalling path where head-of-line blocking hurts.
const MaxFrame = 1 << 20
