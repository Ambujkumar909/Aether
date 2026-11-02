// Package ledger accounts for every byte this system puts on a wire.
//
// The premise: you cannot optimise what you do not measure, and nobody in
// this industry measures honestly. Products quote "bitrate" meaning the
// encoder's target, which is the one number that excludes all the overhead
// the user actually pays for. A 24 kbps Opus stream costs 52.8 kbps of real
// network capacity. That 120% surcharge is invisible in every dashboard
// shipping today, and it is the first thing this package makes visible.
package ledger

// ---------------------------------------------------------------------------
// Layer 2
// ---------------------------------------------------------------------------

// EthernetOverhead is the per-frame cost of Ethernet II on the wire:
// 8 byte preamble+SFD, 6 dst, 6 src, 2 ethertype, 4 FCS, and the 12 byte
// interframe gap the medium is obliged to leave idle. IEEE 802.3.
//
// Most tooling excludes this and is therefore wrong about small packets by
// a wide margin: at 50 audio packets per second it is 15.2 kbps of genuine,
// unavailable-for-anything-else link capacity. We count it separately so a
// report can be read either way, honestly, rather than silently.
const EthernetOverhead = 8 + 6 + 6 + 2 + 4 + 12 // 38

// ---------------------------------------------------------------------------
// Layer 3 / 4
// ---------------------------------------------------------------------------

const (
	IPv4Header = 20 // RFC 791, no options
	IPv6Header = 40 // RFC 8200, no extension headers
	UDPHeader  = 8  // RFC 768
	// TCP with the timestamp option the modern stacks negotiate: 20 fixed
	// plus 12 of options, padded. Relevant because a TURN/TCP or WSS fallback
	// path pays this on every media packet.
	TCPHeader = 32
)

// ---------------------------------------------------------------------------
// RTP and SRTP
// ---------------------------------------------------------------------------

const (
	// RTPFixedHeader is RFC 3550's 12 byte prologue: V/P/X/CC, M/PT,
	// sequence, timestamp, SSRC.
	RTPFixedHeader = 12

	// RTPExtProfileHeader is the 4 byte one-byte-form extension preamble
	// (0xBEDE + length), RFC 8285.
	RTPExtProfileHeader = 4

	// SRTPAuthTagSHA1_80 is the classic AES-CM-128 + HMAC-SHA1-80 tag.
	SRTPAuthTagSHA1_80 = 10
	// SRTPTagGCM is the AEAD tag for AES-128-GCM, RFC 7714. Larger than the
	// SHA1 tag but it replaces the cipher's separate integrity pass, and it
	// is what any competent modern deployment negotiates.
	SRTPTagGCM = 16
)

// RTPExtension describes one negotiated header extension. Size is the data
// octets; the one-byte form adds a single ID/length octet per element.
type RTPExtension struct {
	Name      string
	Data      int
	Droppable bool // safe to stop sending once the session is established
	Why       string
}

// StandardExtensions is what a browser negotiates by default today. The
// Droppable flags are the interesting column: MID and RID exist only to
// disambiguate streams before the receiver has learned the SSRC mapping.
// RFC 8843 permits ceasing to transmit them thereafter, and essentially
// nobody does, so every packet of every call carries identification the
// receiver worked out in the first second and has known ever since.
var StandardExtensions = []RTPExtension{
	{"abs-send-time", 3, false, "sender-side pacing reference, needed continuously"},
	{"transport-cc", 2, false, "per-packet feedback sequence, the congestion control input"},
	{"mid", 1, true, "stream identification; redundant once SSRC is bound (RFC 8843)"},
	{"rid", 1, true, "simulcast layer identification; redundant once SSRC is bound"},
	{"audio-level", 1, true, "only needed from streams the SFU is actually ranking"},
	{"video-orientation", 1, true, "meaningless for screenshare and for any fixed camera"},
}

// ExtensionsSize computes the encoded size of a set of extensions in the
// one-byte form, including the 0xBEDE profile header and the padding to the
// mandatory 4 byte boundary. Returns 0 for an empty set, since the X bit is
// then simply clear and no preamble is emitted at all.
func ExtensionsSize(exts []RTPExtension) int {
	if len(exts) == 0 {
		return 0
	}
	n := 0
	for _, e := range exts {
		n += 1 + e.Data // one-byte header form: 4 bit ID, 4 bit length
	}
	total := RTPExtProfileHeader + n
	if r := total % 4; r != 0 {
		total += 4 - r
	}
	return total
}

// DroppableExtensionsSize is what we can stop paying for after the session
// stabilises, per packet.
func DroppableExtensionsSize() int {
	var keep, all []RTPExtension
	for _, e := range StandardExtensions {
		all = append(all, e)
		if !e.Droppable {
			keep = append(keep, e)
		}
	}
	return ExtensionsSize(all) - ExtensionsSize(keep)
}

// ---------------------------------------------------------------------------
// Relay
// ---------------------------------------------------------------------------

const (
	// TURNChannelData is the 4 byte framing of RFC 5766 channel data, the
	// efficient TURN mode.
	TURNChannelData = 4
	// TURNSendIndication is the STUN-message mode: a 20 byte STUN header
	// plus XOR-PEER-ADDRESS and DATA attribute headers. Roughly 36 bytes,
	// and it is what a relay costs when channel binding was never done.
	// On a 60 byte audio payload this is a 60% surcharge on top of every
	// other surcharge.
	TURNSendIndication = 36
)

// ---------------------------------------------------------------------------
// Signalling transports
// ---------------------------------------------------------------------------

const (
	// TLS13Record is a 5 byte record header, a 1 byte inner content type,
	// and a 16 byte AEAD tag. RFC 8446. Paid per record, not per byte, which
	// is precisely why coalescing small control frames matters so much.
	TLS13Record = 5 + 1 + 16

	// WSHeaderServer is the RFC 6455 frame header a server sends: 2 bytes
	// for payloads under 126, 4 under 64 KiB.
	WSHeaderServerSmall = 2
	WSHeaderServerMed   = 4
	// WSMask is the 4 byte masking key RFC 6455 obliges every client frame
	// to carry, plus the XOR pass over the payload. Unavoidable, which is
	// another argument for sending fewer, larger client frames.
	WSMask = 4

	// H2FrameHeader is HTTP/2's 9 byte frame prologue, RFC 9113.
	H2FrameHeader = 9
)

// HTTP11RequestHeaderBytes is a representative uncompressed request header
// block from a real product's REST signalling path: method, path, host,
// user-agent, accept, content-type, authorization bearer, cookie, origin,
// and the rest. This is what polling or per-event REST costs *per event*.
const HTTP11RequestHeaderBytes = 780

// H2HPACKIncrementalBytes is the same header block after HPACK dynamic-table
// compression on an established connection: only the path and a couple of
// varying fields survive as literals. RFC 7541.
const H2HPACKIncrementalBytes = 38
