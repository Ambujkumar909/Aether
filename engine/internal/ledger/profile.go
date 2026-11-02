package ledger

import "math"

// Transport selects the per-packet layer 3/4 cost of a media path.
type Transport uint8

const (
	TransportUDP Transport = iota
	TransportTCP
	TransportTURNChannel
	TransportTURNIndication
)

func (t Transport) overhead() int {
	switch t {
	case TransportTCP:
		return TCPHeader
	case TransportTURNChannel:
		return UDPHeader + TURNChannelData
	case TransportTURNIndication:
		return UDPHeader + TURNSendIndication
	default:
		return UDPHeader
	}
}

func (t Transport) String() string {
	switch t {
	case TransportTCP:
		return "TCP"
	case TransportTURNChannel:
		return "TURN/channel"
	case TransportTURNIndication:
		return "TURN/indication"
	default:
		return "UDP"
	}
}

// Path describes the network assumptions a budget is computed against.
// The defaults are deliberately the honest case rather than the flattering
// one: IPv4, UDP, GCM tags, and layer 2 framing counted.
type Path struct {
	IPv6          bool
	Transport     Transport
	SRTPTagBytes  int
	CountEthernet bool
}

func DefaultPath() Path {
	return Path{Transport: TransportUDP, SRTPTagBytes: SRTPTagGCM, CountEthernet: true}
}

// PerPacketOverhead is every byte a media packet costs that is not payload.
func (p Path) PerPacketOverhead(exts []RTPExtension) int {
	n := RTPFixedHeader + ExtensionsSize(exts) + p.SRTPTagBytes + p.Transport.overhead()
	if p.IPv6 {
		n += IPv6Header
	} else {
		n += IPv4Header
	}
	if p.CountEthernet {
		n += EthernetOverhead
	}
	return n
}

// Stream is one media flow expressed analytically.
//
// PayloadBitrate is the encoder target, which is the number every product
// quotes. Every other field on this struct exists to convert that fiction
// into the bitrate a real link actually has to carry.
type Stream struct {
	Name           string
	PayloadBitrate int     // bits per second of codec output
	PacketsPerSec  float64 // from ptime for audio, from bitrate and MTU for video
	Extensions     []RTPExtension

	// DTXActivity is the fraction of wall time the source actually emits.
	// Opus DTX drops a talking-head microphone to roughly one packet per
	// 400 ms during silence. In a four-person call any given person speaks
	// perhaps a quarter of the time, so this is a lossless, quality-neutral
	// 75% saving on three of the four audio streams. Almost nobody harvests
	// it, because almost nobody enables DTX by default, for fear of clipping
	// the first syllable. That fear is addressable with a short lookahead;
	// the bandwidth it costs to avoid the work is not.
	DTXActivity float64

	// DTXPacketsPerSec is the comfort-noise rate during silence.
	DTXPacketsPerSec float64
}

// Breakdown is per-layer accounting for a stream, a participant, or a whole
// session. Everything in this system that reports bandwidth reports one of
// these, never a single scalar, because a single scalar is how the industry
// hides its overhead.
type Breakdown struct {
	Label string

	MediaPayload int64 // the only bytes that carry picture or sound
	RTPHeader    int64
	RTPExtension int64
	SRTPTag      int64
	Transport    int64 // UDP / TCP / TURN framing
	IP           int64
	Ethernet     int64

	Packets int64
	Seconds float64
}

// Total is every byte, including layer 2 framing.
func (b Breakdown) Total() int64 {
	return b.MediaPayload + b.RTPHeader + b.RTPExtension + b.SRTPTag +
		b.Transport + b.IP + b.Ethernet
}

// TotalL3 excludes Ethernet framing, matching what an ISP meter or a cloud
// egress bill would count.
func (b Breakdown) TotalL3() int64 { return b.Total() - b.Ethernet }

// Overhead is every byte that is not picture or sound.
func (b Breakdown) Overhead() int64 { return b.Total() - b.MediaPayload }

// Efficiency is the fraction of transmitted bytes carrying signal. Raising
// this number is the entire purpose of the package, and no competing product
// reports it at all.
func (b Breakdown) Efficiency() float64 {
	t := b.Total()
	if t == 0 {
		return 0
	}
	return float64(b.MediaPayload) / float64(t)
}

// Bitrate is the true wire bitrate in bits per second.
func (b Breakdown) Bitrate() float64 {
	if b.Seconds == 0 {
		return 0
	}
	return float64(b.Total()) * 8 / b.Seconds
}

// Add accumulates another breakdown, rolling a session up from its streams.
func (b *Breakdown) Add(o Breakdown) {
	b.MediaPayload += o.MediaPayload
	b.RTPHeader += o.RTPHeader
	b.RTPExtension += o.RTPExtension
	b.SRTPTag += o.SRTPTag
	b.Transport += o.Transport
	b.IP += o.IP
	b.Ethernet += o.Ethernet
	b.Packets += o.Packets
	if o.Seconds > b.Seconds {
		b.Seconds = o.Seconds
	}
}

// Project computes the exact byte breakdown of a stream over a duration.
func (s Stream) Project(p Path, seconds float64) Breakdown {
	b := Breakdown{Label: s.Name, Seconds: seconds}

	activity := s.DTXActivity
	if activity <= 0 || activity > 1 {
		activity = 1
	}

	activeSecs := seconds * activity
	activePkts := s.PacketsPerSec * activeSecs
	payloadPerPkt := 0.0
	if s.PacketsPerSec > 0 {
		payloadPerPkt = float64(s.PayloadBitrate) / 8 / s.PacketsPerSec
	}

	silentSecs := seconds - activeSecs
	silentPkts := s.DTXPacketsPerSec * silentSecs
	const comfortNoiseBytes = 3 // Opus CNG frames are tiny by construction

	extBytes := ExtensionsSize(s.Extensions)
	txBytes := p.Transport.overhead()
	ipBytes := IPv4Header
	if p.IPv6 {
		ipBytes = IPv6Header
	}
	ethBytes := 0
	if p.CountEthernet {
		ethBytes = EthernetOverhead
	}

	totalPkts := activePkts + silentPkts
	b.Packets = int64(math.Round(totalPkts))
	b.MediaPayload = int64(math.Round(activePkts*payloadPerPkt + silentPkts*comfortNoiseBytes))
	b.RTPHeader = int64(math.Round(totalPkts * RTPFixedHeader))
	b.RTPExtension = int64(math.Round(totalPkts * float64(extBytes)))
	b.SRTPTag = int64(math.Round(totalPkts * float64(p.SRTPTagBytes)))
	b.Transport = int64(math.Round(totalPkts * float64(txBytes)))
	b.IP = int64(math.Round(totalPkts * float64(ipBytes)))
	b.Ethernet = int64(math.Round(totalPkts * float64(ethBytes)))
	return b
}

// ---------------------------------------------------------------------------
// Canonical stream constructors
// ---------------------------------------------------------------------------

// OpusStream builds an audio profile.
//
// ptimeMs is the packetisation interval, and it is the most under-exploited
// knob in real-time audio. Changing it alters neither the codec, nor the
// bitrate, nor the quality by a single bit; moving from 20 ms to 60 ms
// simply removes two thirds of the per-packet overhead. The cost is 40 ms of
// added latency, which matters enormously for whoever is speaking right now
// and not at all for anybody else. So we spend 20 ms on the dominant speaker
// and 60 ms on everyone else, and no listener can tell.
func OpusStream(name string, bitrate int, ptimeMs, activity float64, exts []RTPExtension) Stream {
	return Stream{
		Name:             name,
		PayloadBitrate:   bitrate,
		PacketsPerSec:    1000 / ptimeMs,
		Extensions:       exts,
		DTXActivity:      activity,
		DTXPacketsPerSec: 2.5, // one comfort-noise frame per 400 ms
	}
}

// VideoStream builds a video profile. The packet rate is derived from
// bitrate and MTU rather than from frame rate, because video packetisation
// is bitrate-bound: a 2 Mbps stream at a 1200 byte payload MTU is about 208
// packets per second whether it runs at 30 fps or 60.
func VideoStream(name string, bitrate, mtuPayload int, exts []RTPExtension) Stream {
	return Stream{
		Name:           name,
		PayloadBitrate: bitrate,
		PacketsPerSec:  float64(bitrate) / 8 / float64(mtuPayload),
		Extensions:     exts,
		DTXActivity:    1,
	}
}

// EffectiveMTUPayload is the payload room left in a path MTU after IP, UDP,
// RTP, extensions, and the SRTP tag.
//
// Encoders are usually handed 1200 as a folk constant. Depending on the
// negotiated extension set the true figure moves by tens of bytes, and being
// wrong in either direction costs real money: too low wastes capacity on
// every single packet, too high causes fragmentation that then gets blamed
// on the network.
func EffectiveMTUPayload(p Path, exts []RTPExtension, pathMTU int) int {
	o := p.PerPacketOverhead(exts)
	if p.CountEthernet {
		o -= EthernetOverhead // MTU is a layer 3 concept
	}
	return pathMTU - o
}
