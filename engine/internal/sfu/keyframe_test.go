package sfu

import (
	"math/rand"
	"testing"
)

// These vectors are hand-built from the payload specifications. A false
// positive here re-anchors the outbound stream onto an undecodable frame, so
// the negative cases matter at least as much as the positive ones.

func TestVP8Keyframe(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		// Descriptor 0x10: X=0, S=1, PID=0. Payload header 0x00: P=0.
		{"minimal keyframe", []byte{0x10, 0x00, 0x00, 0x00}, true},
		// Same but P=1: an inter frame.
		{"minimal delta", []byte{0x10, 0x01, 0x00, 0x00}, false},
		// S=0: a continuation packet tells us nothing.
		{"continuation packet", []byte{0x00, 0x00, 0x00}, false},
		// PID != 0: not the first partition.
		{"later partition", []byte{0x11, 0x00, 0x00}, false},
		// X=1 with I set and a 7-bit PictureID, then the payload header.
		{"extended, 7-bit pictureid", []byte{0x90, 0x80, 0x7f, 0x00}, true},
		// X=1, I set, 15-bit PictureID (high bit of the id byte set).
		{"extended, 15-bit pictureid", []byte{0x90, 0x80, 0x80, 0x01, 0x00}, true},
		// X=1 with I, L and T all set: descriptor consumes four octets.
		{"extended, all fields", []byte{0x90, 0xf0, 0x01, 0x02, 0x03, 0x00}, true},
		{"truncated after descriptor", []byte{0x90, 0x80}, false},
		{"empty", []byte{}, false},
	}
	for _, c := range cases {
		if got := IsKeyframe("video/VP8", c.payload); got != c.want {
			t.Errorf("VP8 %s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVP9Keyframe(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"begin, not predicted", []byte{0x08, 0x00}, true},
		{"begin, predicted", []byte{0x48, 0x00}, false},
		{"not begin", []byte{0x00, 0x00}, false},
		{"empty", []byte{}, false},
	}
	for _, c := range cases {
		if got := IsKeyframe("video/VP9", c.payload); got != c.want {
			t.Errorf("VP9 %s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestH264Keyframe(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"IDR", []byte{0x65, 0xaa}, true},
		{"SPS", []byte{0x67, 0xaa}, true},
		{"PPS", []byte{0x68, 0xaa}, true},
		{"non-IDR slice", []byte{0x61, 0xaa}, false},
		// STAP-A carrying SPS then PPS.
		{"STAP-A with SPS", []byte{0x78, 0x00, 0x02, 0x67, 0x11, 0x00, 0x02, 0x68, 0x22}, true},
		{"STAP-A without keyframe", []byte{0x78, 0x00, 0x02, 0x61, 0x11}, false},
		{"STAP-A truncated", []byte{0x78, 0x00, 0x09, 0x61}, false},
		// FU-A first fragment of an IDR: S bit set, type 5.
		{"FU-A start of IDR", []byte{0x7c, 0x85, 0xaa}, true},
		{"FU-A start of non-IDR", []byte{0x7c, 0x81, 0xaa}, false},
		{"FU-A middle fragment", []byte{0x7c, 0x05, 0xaa}, false},
		{"FU-A truncated", []byte{0x7c}, false},
		{"empty", []byte{}, false},
	}
	for _, c := range cases {
		if got := IsKeyframe("video/H264", c.payload); got != c.want {
			t.Errorf("H264 %s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAV1Keyframe(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		// N bit set: a new coded video sequence.
		{"new sequence flag", []byte{0x08, 0x00}, true},
		// W=1 so no length field; OBU type 1 is a sequence header.
		{"sequence header obu", []byte{0x10, 0x08}, true},
		// W=1, OBU type 6 (frame): not an entry point on its own.
		{"frame obu", []byte{0x10, 0x30}, false},
		// Z set: continuation of a previous packet.
		{"continuation", []byte{0x80, 0x08}, false},
		// W=0 so a LEB128 length precedes the OBU header.
		{"leb128 length then seq header", []byte{0x00, 0x02, 0x08, 0x00}, true},
		{"empty", []byte{}, false},
	}
	for _, c := range cases {
		if got := IsKeyframe("video/AV1", c.payload); got != c.want {
			t.Errorf("AV1 %s: got %v want %v", c.name, got, c.want)
		}
	}
}

// An unrecognised codec must never report a keyframe. Guessing would let the
// forwarder switch onto a stream it cannot reason about.
func TestUnknownCodecNeverReportsKeyframe(t *testing.T) {
	for _, mime := range []string{"video/H265", "video/theora", "", "audio/opus"} {
		if IsKeyframe(mime, []byte{0xff, 0x00, 0x10, 0x08}) {
			t.Errorf("%q reported a keyframe", mime)
		}
	}
}

// No detector may panic on arbitrary bytes. This runs on every packet from
// every publisher, so it is directly reachable by anything that can join.
func TestDetectorsSurviveGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	mimes := []string{"video/VP8", "video/VP9", "video/H264", "video/AV1"}
	for i := 0; i < 50000; i++ {
		b := make([]byte, rng.Intn(24))
		rng.Read(b)
		mime := mimes[rng.Intn(len(mimes))]
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic on %s %v: %v", mime, b, p)
				}
			}()
			IsKeyframe(mime, b)
		}()
	}
}
