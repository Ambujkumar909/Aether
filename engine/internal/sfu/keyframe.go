package sfu

import "strings"

// Keyframe detection.
//
// The forwarder will only switch layers on a keyframe, so everything it
// promises rests on this file being right. A false negative costs a few
// hundred milliseconds of delay before a switch completes. A false positive
// re-anchors the outbound stream onto a delta frame the receiver cannot
// decode, which produces exactly the corruption the forwarder exists to
// prevent -- and it produces it silently, as a green or smeared picture
// rather than an error.
//
// So each detector reads the actual payload format rather than guessing from
// packet size or the marker bit, and each returns false when it cannot be
// sure. Refusing to switch is always recoverable; switching wrongly is not.

// IsKeyframe reports whether an RTP payload begins a decodable keyframe.
//
// mimeType is the negotiated codec, e.g. "video/VP8". Only the first packet
// of a frame carries the information, so this returns false for continuation
// packets, which is correct: the forwarder anchors on the frame start.
func IsKeyframe(mimeType string, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch {
	case strings.EqualFold(mimeType, "video/VP8"):
		return vp8Keyframe(payload)
	case strings.EqualFold(mimeType, "video/VP9"):
		return vp9Keyframe(payload)
	case strings.EqualFold(mimeType, "video/H264"):
		return h264Keyframe(payload)
	case strings.EqualFold(mimeType, "video/AV1"):
		return av1Keyframe(payload)
	default:
		// An unknown video codec cannot be switched safely. Returning false
		// means the forwarder stays on its current layer, which degrades
		// adaptation rather than the picture.
		return false
	}
}

// vp8Keyframe parses RFC 7741.
//
// The payload descriptor is variable length: a required first octet whose
// X bit signals an extension octet, which in turn signals up to three more.
// After it, the first byte of the payload header carries P in bit 0, where
// P == 0 means a key frame. The S bit and a zero PID identify the first
// packet of the frame; without both, we are mid-frame and cannot tell.
func vp8Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	i := 0
	x := p[0]&0x80 != 0
	s := p[0]&0x10 != 0
	pid := p[0] & 0x07
	i++

	if x {
		if len(p) <= i {
			return false
		}
		ext := p[i]
		i++
		if ext&0x80 != 0 { // I: PictureID present
			if len(p) <= i {
				return false
			}
			if p[i]&0x80 != 0 { // 15-bit PictureID
				i += 2
			} else {
				i++
			}
		}
		if ext&0x40 != 0 { // L: TL0PICIDX
			i++
		}
		if ext&0x20 != 0 || ext&0x10 != 0 { // T/K: TID/KEYIDX share an octet
			i++
		}
	}

	if !s || pid != 0 {
		return false // not the first packet of the frame
	}
	if len(p) <= i {
		return false
	}
	// Payload header, first byte: bit 0 is the frame type. 0 means key frame.
	return p[i]&0x01 == 0
}

// vp9Keyframe parses draft-ietf-payload-vp9.
//
// The P bit in the descriptor means "inter-picture predicted". A picture with
// P == 0 references nothing, which is the definition we need. B marks the
// beginning of a frame.
func vp9Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	desc := p[0]
	inter := desc&0x40 != 0 // P
	begin := desc&0x08 != 0 // B
	return begin && !inter
}

// h264Keyframe walks NAL units, handling the aggregation and fragmentation
// forms defined in RFC 6184.
//
// An IDR (type 5) is a keyframe. SPS (7) and PPS (8) are parameter sets that
// in practice immediately precede one and are sent in the same packet or the
// same STAP-A, so treating them as a frame start is both conventional and
// safe: a decoder handed SPS/PPS followed by an IDR is fully initialised.
func h264Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	const (
		nalSTAPA = 24
		nalFUA   = 28
		nalFUB   = 29
		nalIDR   = 5
		nalSPS   = 7
		nalPPS   = 8
	)

	typ := p[0] & 0x1f
	switch typ {
	case nalIDR, nalSPS, nalPPS:
		return true

	case nalSTAPA:
		// Single-time aggregation: a series of length-prefixed NALs.
		i := 1
		for i+2 <= len(p) {
			size := int(p[i])<<8 | int(p[i+1])
			i += 2
			if size <= 0 || i+size > len(p) {
				return false
			}
			switch p[i] & 0x1f {
			case nalIDR, nalSPS, nalPPS:
				return true
			}
			i += size
		}
		return false

	case nalFUA, nalFUB:
		// Fragmentation: only the first fragment tells us anything, and the
		// original NAL type lives in the FU header.
		if len(p) < 2 {
			return false
		}
		start := p[1]&0x80 != 0
		if !start {
			return false
		}
		return p[1]&0x1f == nalIDR

	default:
		return false
	}
}

// av1Keyframe inspects the AV1 aggregation header and the OBUs behind it.
//
// The aggregation header's N bit means "this packet starts a new coded video
// sequence", which is precisely a keyframe and is the cheap path. Failing
// that, a sequence header OBU (type 1) in the first element means the same
// thing, because a sequence header is only emitted at a decodable entry
// point.
func av1Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	agg := p[0]
	if agg&0x08 != 0 { // N: new coded video sequence
		return true
	}
	// Z set means the first OBU is a continuation of a previous packet, so
	// there is no frame start here to look at.
	if agg&0x80 != 0 {
		return false
	}
	if len(p) < 2 {
		return false
	}

	// Element length is a LEB128 unless this is the only element (W == 0).
	i := 1
	w := (agg >> 4) & 0x03
	if w == 0 {
		var shift uint
		for i < len(p) {
			b := p[i]
			i++
			if b&0x80 == 0 {
				break
			}
			shift += 7
			if shift > 28 {
				return false
			}
		}
	}
	if i >= len(p) {
		return false
	}
	// OBU header: bit 7 is forbidden, bits 6..3 are the type.
	obuType := (p[i] >> 3) & 0x0f
	const obuSequenceHeader = 1
	return obuType == obuSequenceHeader
}
