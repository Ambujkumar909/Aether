package wire

// LEB128 unsigned varint. 1 byte covers 0..127, which is where the
// overwhelming majority of our field values live: dictionary handles,
// layer indices, participant counts, opcode arguments.

// UvarintSize reports the encoded width of v without allocating.
func UvarintSize(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// ZigZag maps signed values onto unsigned so that small magnitudes -- which
// is what deltas are -- stay in the one-byte range regardless of sign.
func ZigZag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func UnZigZag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }
