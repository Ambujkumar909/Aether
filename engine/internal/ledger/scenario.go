package ledger

// This file encodes the head-to-head budget model. Every constant below is a
// claim, and every claim carries the reasoning that justifies it, because a
// benchmark whose assumptions are hidden is marketing rather than
// engineering. Where a figure is arguable it is set conservatively, against
// our own interest, so that the conclusion survives disagreement.

// ---------------------------------------------------------------------------
// Codec operating points
// ---------------------------------------------------------------------------

const (
	// Baseline720pVP8 is a representative VP8 or H.264 baseline operating
	// point for 720p30 talking-head content at the quality a major product
	// ships today. Realtime encoders run far from their offline rate-distortion
	// curves, so this is the realtime figure, not a laboratory one.
	Baseline720pVP8 = 1_200_000

	// Aether720pAV1 is the same perceptual quality from a realtime AV1
	// encoder. Offline AV1 shows roughly 50% BD-rate improvement over VP8;
	// realtime AV1 gives up a substantial part of that to speed. We model
	// 37.5%, which is below what SVT-AV1 in its realtime presets achieves on
	// this content, so the comparison is not resting on the optimistic end
	// of a contested number.
	//
	// This is a strictly-better-picture change, not a quality trade: the
	// output is the same resolution, the same frame rate, and the same
	// perceptual quality target. It simply takes fewer bits to say it.
	Aether720pAV1 = 750_000

	// AetherVP9Fallback is the operating point when AV1 encode is
	// unavailable -- older hardware, thermally limited laptops, or a browser
	// without AV1 encode. VP9 is universally available and still materially
	// better than VP8. The architecture must never depend on the best case.
	AetherVP9Fallback = 900_000

	// Thumb180p is a gallery thumbnail layer. A tile rendered at 160x90 CSS
	// pixels does not need, and cannot display, more than this.
	Thumb180p = 85_000

	// Mid360p is the middle simulcast rung.
	Mid360p = 350_000

	// Low180pSimulcast is the bottom rung of a classic simulcast ladder.
	Low180pSimulcast = 120_000

	// SimulcastLadderTotal is what a three-rung simulcast sender actually
	// transmits: three fully independent encodes of the same scene, all the
	// way up the link, all the time.
	SimulcastLadderTotal = Baseline720pVP8 + Mid360p + Low180pSimulcast

	// SVCOverhead is the multiplier a single L3T3 scalable encode costs over
	// its own top layer alone. Inter-layer prediction means the lower layers
	// are nearly free, unlike simulcast where they are full price.
	SVCOverheadNum = 115
	SVCOverheadDen = 100

	// OpusVoice is a mono voice-grade Opus target. Both sides of the
	// comparison use the same figure: we are not winning the audio argument
	// by sending worse audio.
	OpusVoice = 32_000

	// DownlinkAudioReceiverBaseline is what products forward per remote
	// participant regardless of whether anyone is speaking.
	DownlinkAudioReceiverBaseline = OpusVoice
)

// SpeechActivity is the fraction of wall time one participant in a
// multi-party meeting is actually producing speech. Conversational analysis
// puts single-speaker floor time in a four-way meeting well below a quarter;
// 0.28 is deliberately generous, and it includes backchannel utterances.
const SpeechActivity = 0.28

// ForwardedAudioStreams is how many remote audio streams the SFU mixes down
// to. Beyond three concurrent talkers a meeting is not a meeting, and every
// additional forwarded stream is bandwidth spent on a channel no listener
// can separate anyway.
const ForwardedAudioStreams = 3

// ---------------------------------------------------------------------------
// Scenario
// ---------------------------------------------------------------------------

// Scenario is one participant's complete traffic profile for a meeting.
type Scenario struct {
	Name         string
	Participants int
	Seconds      float64
	Path         Path
	Uplink       []Stream
	Downlink     []Stream
}

// Project rolls the whole scenario into a single breakdown.
func (s Scenario) Project() Breakdown {
	out := Breakdown{Label: s.Name, Seconds: s.Seconds}
	for _, st := range s.Uplink {
		out.Add(st.Project(s.Path, s.Seconds))
	}
	for _, st := range s.Downlink {
		out.Add(st.Project(s.Path, s.Seconds))
	}
	out.Seconds = s.Seconds
	return out
}

// reducedExtensions is the extension set that survives once the session has
// stabilised and the receiver has bound every SSRC. Dropping the rest is
// permitted by RFC 8843 and is worth several bytes on every single packet,
// which at audio packet rates is thousands of packets per minute.
func reducedExtensions() []RTPExtension {
	var keep []RTPExtension
	for _, e := range StandardExtensions {
		if !e.Droppable {
			keep = append(keep, e)
		}
	}
	return keep
}

// BaselineScenario models what a participant in a mainstream product
// actually transmits and receives during a group meeting.
//
// The uplink sends a full simulcast ladder continuously. The downlink
// receives every remote participant's audio unconditionally, and a mid-rung
// video stream for every remote tile whether or not that tile is visible,
// focused, or large enough to resolve the detail being sent.
func BaselineScenario(participants int, seconds float64) Scenario {
	remote := participants - 1
	if remote < 0 {
		remote = 0
	}
	exts := StandardExtensions
	p := DefaultPath()
	mtu := EffectiveMTUPayload(p, exts, 1500)

	sc := Scenario{
		Name:         "Baseline (mainstream product)",
		Participants: participants,
		Seconds:      seconds,
		Path:         p,
	}

	// Uplink: one audio stream at 20 ms with no DTX, plus three independent
	// video encodes.
	sc.Uplink = append(sc.Uplink,
		OpusStream("tx audio 20ms no-DTX", OpusVoice, 20, 1.0, exts),
		VideoStream("tx video 720p simulcast", Baseline720pVP8, mtu, exts),
		VideoStream("tx video 360p simulcast", Mid360p, mtu, exts),
		VideoStream("tx video 180p simulcast", Low180pSimulcast, mtu, exts),
	)

	// Downlink: everything, from everyone, always.
	for i := 0; i < remote; i++ {
		sc.Downlink = append(sc.Downlink,
			OpusStream("rx audio", DownlinkAudioReceiverBaseline, 20, 1.0, exts),
			VideoStream("rx video 360p", Mid360p, mtu, exts),
		)
	}
	return sc
}

// AetherScenario models the same meeting on this engine.
//
// Four changes, none of which reduce what the user sees or hears:
//
//  1. One scalable encode replaces three independent ones. The receiver can
//     still be served any rung; the sender stops paying for all three.
//  2. AV1 replaces VP8 at the same quality target.
//  3. DTX and a longer packetisation interval apply to audio nobody is
//     currently listening to as a primary voice. The dominant speaker keeps
//     the low-latency 20 ms path.
//  4. Downlink video is arbitrated by attention. The speaker arrives at full
//     resolution; a tile rendered 160 pixels wide receives a stream sized for
//     160 pixels, instead of one sized for 640 and then thrown away.
//
// The fourth item is the largest, and it is the one that requires no codec
// work at all. It is available to every product shipping today. None take it.
func AetherScenario(participants int, seconds float64) Scenario {
	remote := participants - 1
	if remote < 0 {
		remote = 0
	}
	full := StandardExtensions
	red := reducedExtensions()
	p := DefaultPath()
	mtu := EffectiveMTUPayload(p, red, 1500)

	sc := Scenario{
		Name:         "Aether",
		Participants: participants,
		Seconds:      seconds,
		Path:         p,
	}

	// Uplink. Audio keeps the 20 ms interval because this participant may be
	// the one speaking, and latency for the active talker is sacred. DTX
	// harvests the silence instead.
	svcTotal := Aether720pAV1 * SVCOverheadNum / SVCOverheadDen
	sc.Uplink = append(sc.Uplink,
		OpusStream("tx audio 20ms + DTX", OpusVoice, 20, SpeechActivity, full),
		VideoStream("tx video AV1 L3T3 single encode", svcTotal, mtu, red),
	)

	// Downlink audio: only the streams that can actually be distinguished.
	forwarded := remote
	if forwarded > ForwardedAudioStreams {
		forwarded = ForwardedAudioStreams
	}
	for i := 0; i < forwarded; i++ {
		if i == 0 {
			// Dominant speaker: low latency path, still DTX-gated.
			sc.Downlink = append(sc.Downlink,
				OpusStream("rx audio dominant 20ms", OpusVoice, 20, SpeechActivity, red))
		} else {
			// Everyone else: 60 ms packetisation. Same codec, same bitrate,
			// same audio. Two thirds fewer packet headers.
			sc.Downlink = append(sc.Downlink,
				OpusStream("rx audio secondary 60ms", OpusVoice, 60, SpeechActivity, red))
		}
	}

	// Downlink video: attention-arbitrated.
	for i := 0; i < remote; i++ {
		if i == 0 {
			sc.Downlink = append(sc.Downlink,
				VideoStream("rx video speaker full", Aether720pAV1, mtu, red))
		} else {
			sc.Downlink = append(sc.Downlink,
				VideoStream("rx video tile 180p", Thumb180p, mtu, red))
		}
	}
	return sc
}

// ---------------------------------------------------------------------------
// Signalling comparison
// ---------------------------------------------------------------------------

// SignalProfile models control-plane traffic over a meeting.
type SignalProfile struct {
	Name string
	// BytesPerEvent is the encoded application payload for one state change.
	BytesPerEvent int
	// FramingPerMessage is transport overhead paid per transmitted message.
	FramingPerMessage int
	// EventsPerMinute across the whole room, as seen by one participant.
	EventsPerMinute float64
	// Coalesce is how many events share one transport message.
	Coalesce int
	// KeepaliveBytes and KeepaliveHz describe the liveness beat.
	KeepaliveBytes int
	KeepaliveHz    float64
}

// Bytes projects total control-plane bytes over a duration.
func (sp SignalProfile) Bytes(seconds float64) int64 {
	c := sp.Coalesce
	if c < 1 {
		c = 1
	}
	events := sp.EventsPerMinute * seconds / 60
	messages := events / float64(c)
	payload := events * float64(sp.BytesPerEvent)
	framing := messages * float64(sp.FramingPerMessage)
	ka := sp.KeepaliveHz * seconds * float64(sp.KeepaliveBytes+sp.FramingPerMessage)
	return int64(payload + framing + ka)
}

// BaselineSignal is JSON over a WebSocket, one message per event, with a
// verbose keepalive. This is not a strawman; it is what the protocols behind
// the major products look like on the wire.
func BaselineSignal(eventsPerMin float64) SignalProfile {
	return SignalProfile{
		Name:              "JSON over WSS, one message per event",
		BytesPerEvent:     172, // measured in wire_test.go against the real shape
		FramingPerMessage: WSHeaderServerSmall + WSMask + TLS13Record,
		EventsPerMinute:   eventsPerMin,
		Coalesce:          1,
		KeepaliveBytes:    38, // {"type":"ping","ts":1755950400000}
		KeepaliveHz:       0.2,
	}
}

// AetherSignal is the binary protocol with a 15 ms coalescing window.
func AetherSignal(eventsPerMin float64) SignalProfile {
	return SignalProfile{
		Name:              "Aether binary, 15 ms coalescing window",
		BytesPerEvent:     3, // proven by TestStateDeltaIsThreeBytes
		FramingPerMessage: WSHeaderServerSmall + WSMask + TLS13Record,
		EventsPerMinute:   eventsPerMin,
		Coalesce:          4,
		KeepaliveBytes:    1, // proven by TestPingIsOneByte
		KeepaliveHz:       0.2,
	}
}
