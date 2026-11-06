package sfu

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/dtls/v3"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// A participant gets TWO peer connections, not one.
//
// The publisher connection is always offered by the client; the subscriber
// connection is always offered by the server. That asymmetry is the entire
// point, because it makes renegotiation glare structurally impossible.
//
// With a single bidirectional connection, both ends have reasons to
// renegotiate -- the client when it enables its camera, the server when
// anyone else in the room publishes anything -- and sooner or later both do
// it in the same round trip. The result is two offers in flight, an
// InvalidStateError, and a session that has to be torn down and rebuilt while
// the user watches. The "perfect negotiation" pattern exists to survive this
// and is genuinely difficult to get right under load.
//
// Splitting the roles removes the problem instead of managing it. Each
// connection has exactly one possible offerer, so there is nothing to
// arbitrate. The cost is a second ICE and DTLS handshake at join time, paid
// once, in parallel, before anyone is looking.

// Direction identifies which connection a signalling message concerns.
type Direction uint8

const (
	Publisher Direction = iota
	Subscriber
)

// Peer is one participant's media plane.
type Peer struct {
	// Log records ICE lifecycle. An SFU that does not say why a connection
	// failed cannot be diagnosed from anywhere except the machine it is
	// running on, and by then the call is over.
	Log *slog.Logger

	ID     string
	RoomID string

	pub *webrtc.PeerConnection
	sub *webrtc.PeerConnection

	Meter     *ledger.Meter
	Estimator *Estimator

	// estMu guards Estimator (fed from RTCP, read by the allocation loop)
	// and the loss window below.
	estMu   sync.Mutex
	lossMax float64   // worst fraction lost reported since the last update
	lastEst time.Time // when the estimator last moved

	// SendSDP and SendICE hand signalling frames to the control plane. They
	// must not block: the control plane owns its own outbound queue.
	SendSDP func(dir Direction, isOffer bool, sdp string)
	SendICE func(dir Direction, candidate string)

	// onPublish fires when this peer starts sending a track.
	//
	// Unexported and guarded by mu, because it is written by whoever wires the
	// peer into a room and read by pion's OnTrack callback, which runs on its
	// own goroutine the moment media arrives. As an exported field with no
	// synchronisation those two accesses raced, and the race detector caught
	// it. The consequence was worse than the warning suggests: an
	// unsynchronised write has no visibility guarantee, so the reading
	// goroutine could observe nil and skip the fan-out entirely. The
	// publisher's track would then exist on the server and be forwarded to
	// nobody, which presents as one participant's video never appearing for
	// anyone else, intermittently and unreproducibly.
	onPublish func(t *PublishedTrack)
	// OnUnpublish fires when a published track ends.
	OnUnpublish func(trackID string)

	mu          sync.Mutex
	downstreams map[string]*Downstream
	published   map[string]*PublishedTrack

	// Negotiation on the subscriber connection is serialised. Tracks arrive
	// in bursts -- a five-person room fills in one round trip -- and firing a
	// renegotiation per track would produce five offers where one suffices.
	neg negGate

	// out is this receiver's outbound writer: every forwarded packet goes
	// through it, so publishers never wait on a receiver.
	out *sendQueue

	// The slot pool (slots.go). bindMu serialises rebinding; slotMu guards
	// the pool, the person views and the id counter.
	bindMu     sync.Mutex
	slotMu     sync.Mutex
	slots      map[string][]*slot // kind/mime -> slots
	slotSeq    int
	views      map[string]view // participant id -> their tile, as reported
	slotsDirty atomic.Bool     // a slot was released since the last map sent
	kicking    atomic.Bool     // a rebind is scheduled (kick)

	// SendSlots tells the receiver what each slot carries. Must not block.
	SendSlots func([]SlotBinding)

	closed atomic.Bool
}

// String names the connection a log line is about. Without it these arrive
// as 0 and 1, which is exactly the sort of detail that has to be looked up
// at the moment it is least convenient.
func (d Direction) String() string {
	if d == Publisher {
		return "publisher"
	}
	return "subscriber"
}

// NewPeer builds both connections.
func NewPeer(id, roomID string, meter *ledger.Meter, ice []webrtc.ICEServer, log *slog.Logger, addrs *AddressPolicy, media *MediaTransport) (*Peer, error) {
	api, err := newAPI(addrs, media)
	if err != nil {
		return nil, err
	}
	cfg := webrtc.Configuration{ICEServers: ice}

	pub, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("publisher connection: %w", err)
	}
	sub, err := api.NewPeerConnection(cfg)
	if err != nil {
		_ = pub.Close()
		return nil, fmt.Errorf("subscriber connection: %w", err)
	}

	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &Peer{
		Log: log,
		ID:  id, RoomID: roomID,
		pub: pub, sub: sub,
		out:         newSendQueue(),
		slots:       make(map[string][]*slot),
		Meter:       meter,
		Estimator:   NewEstimator(DefaultConfig(), initialEstimate),
		downstreams: make(map[string]*Downstream),
		published:   make(map[string]*PublishedTrack),
	}
	p.Estimator.AttachBurstPool(id, time.Now())

	pub.OnICECandidate(func(c *webrtc.ICECandidate) { p.emitICE(Publisher, c) })
	sub.OnICECandidate(func(c *webrtc.ICECandidate) { p.emitICE(Subscriber, c) })

	// ICE lifecycle. "failed" here is the difference between a bug in this
	// code and a network that will not carry the media, and without it the
	// two are indistinguishable from the outside: signalling succeeds, the
	// roster fills in, and no audio or video ever arrives.
	p.watchICE(pub, Publisher)
	p.watchICE(sub, Subscriber)
	pub.OnTrack(p.onTrack)

	// The subscriber connection renegotiates whenever the room changes, and
	// it is always the server that offers.
	sub.OnNegotiationNeeded(func() { p.scheduleNegotiation() })

	return p, nil
}

// registerCodecs registers the codecs this system uses, and no others.
//
// It used to register pion's defaults: 29 payload types, among them three
// telephone codecs, five H.264 profiles, AV1 and VP9 profile 2, none of
// which the client sends. Every subscriber's description repeats the list in
// each of its media sections, and on every renegotiation pion re-derives the
// codecs of each section once per payload type, re-parsing the section each
// time: quadratic in the list, for every section, for every renegotiation.
// With 50 people that was 42% of the server's CPU (the WebRTC stress run,
// ARCHITECTURE 6bu). The client prefers VP9 profile 0, then VP8; H.264
// constrained baseline stays for older Safari. Each video codec has its
// retransmission twin, and Opus is the only audio codec any browser needs.
func registerCodecs(m *webrtc.MediaEngine) error {
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	audio := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}
	if err := m.RegisterCodec(audio, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	video := []struct {
		pt, rtx uint8
		mime    string
		fmtp    string
	}{
		{96, 97, webrtc.MimeTypeVP8, ""},
		{98, 99, webrtc.MimeTypeVP9, "profile-id=0"},
		{102, 103, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
	}
	for _, v := range video {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: v.mime, ClockRate: 90000, SDPFmtpLine: v.fmtp, RTCPFeedback: fb},
			PayloadType:        webrtc.PayloadType(v.pt),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", v.pt)},
			PayloadType:        webrtc.PayloadType(v.rtx),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}

// newAPI builds a media engine with the codecs and header extensions this
// system depends on.
func newAPI(addrs *AddressPolicy, media *MediaTransport) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := registerCodecs(m); err != nil {
		return nil, err
	}

	// RID and MID extensions are what make simulcast legible: without them
	// the three encodings of one camera arrive as three unrelated streams
	// with no way to tell which rung is which.
	for _, uri := range []string{
		sdp.SDESMidURI,
		sdp.SDESRTPStreamIDURI,
		sdp.SDESRepairRTPStreamIDURI,
	} {
		if err := m.RegisterHeaderExtension(
			webrtc.RTPHeaderExtensionCapability{URI: uri},
			webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverDirectionRecvonly,
		); err != nil {
			return nil, err
		}
	}
	// Audio level lets the server rank speakers without decoding, and
	// transport-cc is the congestion controller's input.
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI},
		webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverDirectionRecvonly,
	); err != nil {
		return nil, err
	}

	i := &interceptor.Registry{}
	// The default set brings NACK, RTCP reports, and transport-wide
	// congestion control feedback. Loss recovery is not something to
	// hand-roll: see the note in internal/wire/doc.go about what happens
	// when recovery from lost state is expensive.
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	// Which of this machine's addresses may be offered as candidates. See
	// netpolicy.go for why the default is not "all of them".
	se := webrtc.SettingEngine{}
	if addrs != nil {
		se.SetIPFilter(addrs.Allow)
	}
	// One fixed port for all media, and TCP fallback. See transport.go.
	media.Apply(&se)
	// SRTP profiles, strongest first. Media is encrypted per call with keys
	// from the DTLS handshake; AES-GCM (RFC 7714) authenticates the whole
	// packet with a 16-byte tag and is what every current browser offers.
	// The classic AES-CM-128 + HMAC-SHA1-80 stays last so a browser that
	// offers only that (older Safari) is still heard rather than silent --
	// rule 8: every device. Which one a call got is in the ledger.
	se.SetSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_256_GCM, dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80)

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
		webrtc.WithSettingEngine(se),
	), nil
}

// watchICE logs connection state, and on success the pair that actually won.
//
// The selected pair is the useful half. A server with several interfaces --
// a virtual adapter, an unplugged NIC holding a link-local address, the real
// one -- offers a candidate for each, and knowing which was chosen turns
// "media does not work" into a specific, answerable question.
func (p *Peer) watchICE(pc *webrtc.PeerConnection, dir Direction) {
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		switch st {
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected:
			p.Log.Warn("ice state", "peer", p.ID, "room", p.RoomID,
				"dir", dir.String(), "state", st.String())
		case webrtc.ICEConnectionStateConnected:
			pair := selectedPair(pc)
			p.Log.Info("ice connected", "peer", p.ID, "room", p.RoomID,
				"dir", dir.String(), "pair", pair)
		default:
			p.Log.Debug("ice state", "peer", p.ID, "room", p.RoomID,
				"dir", dir.String(), "state", st.String())
		}
	})
}

// selectedPair renders the winning candidate pair, or why it is not available.
func selectedPair(pc *webrtc.PeerConnection) string {
	sctp := pc.SCTP()
	if sctp == nil {
		return "unknown"
	}
	tr := sctp.Transport()
	if tr == nil {
		return "unknown"
	}
	it := tr.ICETransport()
	if it == nil {
		return "unknown"
	}
	pair, err := it.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "unknown"
	}
	return pair.Local.String() + " <- " + pair.Remote.String()
}

func (p *Peer) emitICE(dir Direction, c *webrtc.ICECandidate) {
	if c == nil || p.SendICE == nil {
		return
	}
	b, err := json.Marshal(c.ToJSON())
	if err != nil {
		return
	}
	// Debug, not info: this is per-candidate, several per connection.
	// It exists because "ICE failed" with no record of what either side
	// offered is a dead end, and it was one for most of an afternoon.
	p.Log.Debug("ice candidate out", "peer", p.ID, "dir", dir.String(),
		"cand", c.Typ.String()+" "+c.Protocol.String()+" "+c.Address+":"+fmt.Sprint(c.Port))
	p.SendICE(dir, string(b))
}

// onTrack registers an inbound encoding, creating the published track on
// first sight and attaching further RIDs as additional layers.
func (p *Peer) onTrack(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	if p.closed.Load() {
		return
	}
	trackID := remote.ID()
	layer := layerFromRID(remote.RID())

	p.mu.Lock()
	t, exists := p.published[trackID]
	if !exists {
		t = NewPublishedTrack(
			trackID, p.ID, remote.Kind(),
			remote.Codec().MimeType,
			isScreenShare(remote.StreamID(), trackID),
			p.Meter.Child("track:"+trackID),
			// The request goes to the SSRC of the layer asked for. It used
			// to go to this first layer's SSRC whatever was asked: a
			// receiver switched to any other layer waited for a keyframe
			// nobody had requested, and froze.
			func(l LayerID) {
				if ssrc, ok := t.LayerSSRC(l); ok {
					p.sendPLI(ssrc)
				}
			},
		)
		p.published[trackID] = t
	}
	closed := p.closed.Load()
	onPublish := p.onPublish
	p.mu.Unlock()

	// The peer went away while this track was arriving. Publishing it now
	// would fan a feed out to the room on behalf of somebody who has left.
	if closed {
		t.Close()
		return
	}

	t.AddLayer(layer, remote)

	if !exists && onPublish != nil {
		// Called after the lock is released. onPublish reaches back into the
		// router, which takes its own lock and may call into this peer again;
		// invoking it while holding p.mu would invert the lock order.
		onPublish(t)
	}

	// Audio needs no keyframes and no layer selection; video does, and the
	// first one must be asked for or the subscriber starts on nothing.
	if remote.Kind() == webrtc.RTPCodecTypeVideo {
		p.sendPLI(remote.SSRC())
	}
	_ = recv
}

// SetOnPublish installs the callback fired when this peer publishes a track.
//
// Must be called before the peer's publisher connection is negotiated. In
// practice that is guaranteed: a peer cannot send media until it has answered
// an offer, and the offer is only handled after the peer has been wired into
// a room. The lock is here for visibility rather than for ordering.
func (p *Peer) SetOnPublish(fn func(t *PublishedTrack)) {
	p.mu.Lock()
	p.onPublish = fn
	p.mu.Unlock()
}

// sendPLI asks the publisher for a keyframe.
func (p *Peer) sendPLI(ssrc webrtc.SSRC) {
	if p.pub == nil {
		return
	}
	_ = p.pub.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)},
	})
}

// layerFromRID maps the publisher's RID to a rung. An empty RID means the
// publisher is not simulcasting, which is the single-layer case.
func layerFromRID(rid string) LayerID {
	switch rid {
	case "q", "low", "l":
		return LayerQuarter
	case "h", "mid", "m":
		return LayerHalf
	case "f", "high", "hi", "":
		return LayerFull
	default:
		return LayerFull
	}
}

func isScreenShare(streamID, trackID string) bool {
	for _, s := range []string{strings.ToLower(streamID), strings.ToLower(trackID)} {
		if strings.Contains(s, "screen") || strings.Contains(s, "display") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Signalling
// ---------------------------------------------------------------------------

// HandleOffer processes a client offer on the publisher connection and
// replies with an answer.
func (p *Peer) HandleOffer(sdpStr string) error {
	if err := p.pub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: sdpStr,
	}); err != nil {
		return fmt.Errorf("set remote offer: %w", err)
	}
	answer, err := p.pub.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := p.pub.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local answer: %w", err)
	}
	if p.SendSDP != nil {
		p.SendSDP(Publisher, false, answer.SDP)
	}
	return nil
}

// HandleAnswer processes a client answer on the subscriber connection.
func (p *Peer) HandleAnswer(sdpStr string) error {
	p.Log.Debug("subscriber answer in", "peer", p.ID, "state", p.sub.SignalingState().String())
	if err := p.sub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: sdpStr,
	}); err != nil {
		return fmt.Errorf("set remote answer: %w", err)
	}
	// A renegotiation requested while this one was in flight runs now.
	if p.neg.done() && !p.closed.Load() {
		go p.offer()
	}
	return nil
}

// HandleICE adds a trickled candidate.
func (p *Peer) HandleICE(dir Direction, candidate string) error {
	var c webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidate), &c); err != nil {
		return fmt.Errorf("parse candidate: %w", err)
	}
	pc := p.pub
	if dir == Subscriber {
		pc = p.sub
	}
	p.Log.Debug("ice candidate in", "peer", p.ID, "dir", dir.String(), "cand", c.Candidate)
	return pc.AddICECandidate(c)
}

// negGate serialises subscriber renegotiation: one offer in flight at a
// time, and a request made meanwhile is remembered and run when it ends.
//
// It was two atomic flags, and that lost requests. A track added while an
// offer was out failed to claim the flag; if the answer landed before the
// request marked itself pending, the answer saw nothing pending and the
// request was then marked where nobody would look. The track was on the
// server and never offered: with 50 people joining at once, one receiver had
// 47 of 49 cameras (ARCHITECTURE 6bu). Under one lock there is no window.
type negGate struct {
	mu       sync.Mutex
	inFlight bool
	pending  bool
}

// begin reports whether the caller should make an offer now. If one is
// already out, the request is remembered instead: an offer queued behind it
// would arrive out of order.
func (g *negGate) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight {
		g.pending = true
		return false
	}
	g.inFlight = true
	return true
}

// done ends the offer in flight, answered or failed, and reports whether a
// request made meanwhile means another offer must start now (still in flight).
func (g *negGate) done() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending {
		g.pending = false
		return true
	}
	g.inFlight = false
	return false
}

// scheduleNegotiation offers on the subscriber connection, collapsing bursts.
func (p *Peer) scheduleNegotiation() {
	if p.closed.Load() {
		return
	}
	if p.neg.begin() {
		go p.offer()
	}
}

// offer makes one subscriber offer. Every way it ends without sending one
// goes through neg.done, so a failure never leaves the gate shut: that would
// be a permanent wedge with no symptom except silence.
func (p *Peer) offer() {
	fail := func() {
		if p.neg.done() && !p.closed.Load() {
			go p.offer()
		}
	}
	offer, err := p.sub.CreateOffer(nil)
	if err != nil {
		fail()
		return
	}
	if err := p.sub.SetLocalDescription(offer); err != nil {
		fail()
		return
	}
	if p.SendSDP == nil {
		// Nowhere to send it: a wiring error, not a runtime condition.
		p.Log.Error("subscriber offer with no signalling attached; dropped", "peer", p.ID)
		p.neg.done()
		return
	}
	p.Log.Debug("subscriber offer out", "peer", p.ID, "mlines", strings.Count(offer.SDP, "\nm="))
	p.SendSDP(Subscriber, true, offer.SDP)
}

// ---------------------------------------------------------------------------
// Subscription
// ---------------------------------------------------------------------------

// Subscribe makes this peer a receiver of a published track. It is cheap: a
// Downstream records what the receiver does with the track, and a slot (an
// outgoing track on the subscriber connection) is given to it only while it
// is wanted (slots.go).
func (p *Peer) Subscribe(t *PublishedTrack) (*Downstream, error) {
	if t.PeerID == p.ID {
		// Nobody needs the server to send back their own camera.
		return nil, nil
	}
	if p.closed.Load() {
		return nil, nil
	}
	d := newDownstream(p.ID, t.ID, p.Meter.Child("rx:"+t.ID), ledger.DefaultPath(),
		ledger.ExtensionsSize(ledger.StandardExtensions))
	d.track = t

	p.mu.Lock()
	if _, dup := p.downstreams[t.ID]; dup || p.closed.Load() {
		p.mu.Unlock()
		return nil, nil
	}
	p.downstreams[t.ID] = d
	p.mu.Unlock()

	t.Subscribe(d)
	p.kick()
	return d, nil
}

// observeLoss feeds the bandwidth estimator from the receiver reports this
// subscriber sends for what it is being forwarded.
//
// It was fed by nothing. The estimator started at 1.5 Mbit/s and stayed
// there, so every receiver on every network had about 1.3 Mbit/s for all its
// video: on a fast link a gallery of cameras was held far below what the
// link could carry (against rule 3), and a ninth tile could be paused
// outright (the WebRTC stress run, ARCHITECTURE 6bu). Loss is the signal
// every browser already reports: under 2% the rate climbs (quickly back to a
// remembered ceiling, cautiously past it), over 10% it backs off, between
// the two it holds. At most every half second, on the worst report since.
//
// The delay-based signal (transport-wide congestion feedback, which sees a
// queue building before anything is lost) is Observe, and is not wired yet.
func (p *Peer) observeLoss(fraction float64, now time.Time) {
	p.estMu.Lock()
	defer p.estMu.Unlock()
	if fraction > p.lossMax {
		p.lossMax = fraction
	}
	if p.lastEst.IsZero() {
		p.lastEst = now
		return
	}
	if now.Sub(p.lastEst) < lossUpdateEvery {
		return
	}
	sig := SignalUnderuse // hold
	switch {
	case p.lossMax > lossBackOff:
		sig = SignalOveruse
	case p.lossMax < lossClimb:
		sig = SignalNormal
	}
	p.Estimator.Update(now, sig, 0)
	p.lossMax = 0
	p.lastEst = now
}

// probeIfQuiet climbs the estimate when no receiver report has moved it for
// a while.
//
// Reports arrive only for what is being sent. A receiver whose estimate fell
// until every tile was paused got no more reports, so it stayed at the floor
// for good: with nobody talking, every camera stayed black (the 100-person
// WebRTC run, ARCHITECTURE 6bv). Silence is no evidence of congestion, so a
// quiet spell counts as one clean report. Only below the starting estimate:
// a receiver that has never been measured is not talked up by silence alone.
func (p *Peer) probeIfQuiet(now time.Time) {
	p.estMu.Lock()
	defer p.estMu.Unlock()
	if p.lastEst.IsZero() {
		p.lastEst = now
		return
	}
	if now.Sub(p.lastEst) < quietProbeAfter || p.Estimator.Rate() >= initialEstimate {
		return
	}
	p.Estimator.Update(now, SignalNormal, 0)
	p.lastEst = now
}

// EstimateRate is the bandwidth the allocator may spend on this receiver.
func (p *Peer) EstimateRate() int {
	p.estMu.Lock()
	defer p.estMu.Unlock()
	return p.Estimator.Rate()
}

const (
	lossUpdateEvery = 500 * time.Millisecond
	lossClimb       = 0.02
	lossBackOff     = 0.10
	// quietProbeAfter is how long without a receiver report counts as a clean one.
	quietProbeAfter = 2 * time.Second
	// initialEstimate is where every receiver's estimate starts, bit/s.
	initialEstimate = 1_500_000
)

// Unsubscribe detaches a track from this peer.
func (p *Peer) Unsubscribe(trackID string) {
	p.mu.Lock()
	d, ok := p.downstreams[trackID]
	delete(p.downstreams, trackID)
	p.mu.Unlock()
	if !ok {
		return
	}
	// Its slot goes back to the pool, and someone else on screen may take it.
	p.release(d)
	d.track.Unsubscribe(p.ID)
	p.kick()
}

// Downstreams returns a snapshot for the allocator.
func (p *Peer) Downstreams() map[string]*Downstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*Downstream, len(p.downstreams))
	for k, v := range p.downstreams {
		out[k] = v
	}
	return out
}

// Published returns this peer's own tracks.
func (p *Peer) Published() []*PublishedTrack {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*PublishedTrack, 0, len(p.published))
	for _, t := range p.published {
		out = append(out, t)
	}
	return out
}

// Close tears both connections down.
func (p *Peer) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	for _, t := range p.published {
		t.Close()
	}
	// Cleared, never set to nil.
	//
	// A nil map panics on write, and these maps are written from goroutines
	// this function cannot see: pion delivers OnTrack on its own goroutine,
	// and the router fans a newly published track out to every peer. One
	// participant leaving at the same moment another starts sending therefore
	// panicked with "assignment to entry in nil map", which is not a dropped
	// frame or a stalled connection -- it takes down the whole server process,
	// and with it every other meeting on the node.
	//
	// Nil-ing bought nothing: the maps are freed with the peer either way.
	// Clearing keeps every concurrent write harmless, and the closed flag
	// below is what actually stops new work being accepted.
	clear(p.published)
	clear(p.downstreams)
	p.mu.Unlock()
	p.out.close()

	if p.pub != nil {
		_ = p.pub.Close()
	}
	if p.sub != nil {
		_ = p.sub.Close()
	}
}
