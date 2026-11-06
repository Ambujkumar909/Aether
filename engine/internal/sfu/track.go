package sfu

import (
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// PublishedTrack is one camera, microphone, or screen, as offered by one
// participant, in however many encodings they chose to send.
//
// Simulcast means the same camera arrives as two or three independent RTP
// streams distinguished by RID. SVC means it arrives as one stream whose
// layers are selectable inside the bitstream. Both end up here; the
// difference is handled at selection time, not at reception.
type PublishedTrack struct {
	ID       string
	PeerID   string
	Kind     webrtc.RTPCodecType
	MimeType string
	// IsScreen changes how the allocator judges this track: spatial detail is
	// weighted 0.9 against 0.1 for motion, because text has to be legible and
	// a shared terminal at 5 fps beats a blurry one at 30.
	//
	// It is guarded because it is set late. The only reliable signal that a
	// track is a screen arrives in a control frame from the publisher, which
	// can land after the media does; guessing from the msid, which is what
	// this used to do, never worked for a browser because those identifiers
	// are UUIDs.
	screenMu sync.RWMutex
	isScreen bool

	mu     sync.RWMutex
	layers map[LayerID]*inboundLayer
	subs   map[string]*Downstream
	outs   atomic.Pointer[[]*Downstream] // subs holding a slot; copy on write

	// lastPacket is when the track last sent anything (unix ns): an audio
	// track is worth a slot only while its microphone is sending. onWake runs
	// when it starts again after a quiet spell, so its slots are bound now
	// rather than at the next allocation cycle, a second of speech later.
	lastPacket atomic.Int64
	onWake     atomic.Pointer[func(*PublishedTrack)]

	Meter *ledger.Meter

	// keyframe requests are deduplicated per layer: twenty subscribers
	// switching to the same layer at once must not produce twenty PLIs,
	// because the publisher would answer every one of them with a full
	// keyframe and bury its own uplink exactly when the network is already
	// the reason everyone switched. Per LAYER, not per track: a request for
	// the full layer must not swallow one for the half layer made in the
	// same instant (it did, and that receiver's tile froze for good).
	lastPLI [LayerFull + 1]atomic.Int64
	pli     func(layer LayerID)

	closed atomic.Bool
}

type inboundLayer struct {
	id     LayerID
	remote *webrtc.TrackRemote
	width  int
	height int

	// bitrate is measured, never configured. A static slide deck nominally
	// at 1.2 Mbps transmits almost nothing, and an allocator budgeting
	// against the nameplate figure starves every other track for capacity
	// that was never going to be used.
	bytes    atomic.Int64
	lastCalc time.Time
	lastByte int64
	rate     atomic.Int64
	rateMu   sync.Mutex
}

// PLIInterval is the shortest gap between keyframe requests for one layer.
//
// Short enough that a subscriber waiting to switch is not left staring at a
// stale layer; long enough that a burst of simultaneous requests collapses
// into one. A keyframe is the most expensive thing a publisher can send, so
// the deduplication window is a bandwidth decision, not a tidiness one.
const PLIInterval = 500 * time.Millisecond

func NewPublishedTrack(id, peerID string, kind webrtc.RTPCodecType,
	mime string, isScreen bool, meter *ledger.Meter, pli func(LayerID)) *PublishedTrack {
	return &PublishedTrack{
		ID: id, PeerID: peerID, Kind: kind, MimeType: mime, isScreen: isScreen,
		layers: make(map[LayerID]*inboundLayer),
		subs:   make(map[string]*Downstream),
		Meter:  meter,
		pli:    pli,
	}
}

// AddLayer registers an inbound encoding and starts reading it.
func (t *PublishedTrack) AddLayer(id LayerID, remote *webrtc.TrackRemote) {
	l := &inboundLayer{
		id: id, remote: remote,
		width:    int(remote.Codec().RTPCodecCapability.ClockRate), // placeholder until first frame
		height:   0,
		lastCalc: time.Now(),
	}
	// Resolution is not reliably known from SDP for simulcast rungs, so it
	// is seeded from the conventional ladder and corrected if the publisher
	// reports otherwise.
	l.width, l.height = conventionalSize(id)

	t.mu.Lock()
	t.layers[id] = l
	t.mu.Unlock()

	go t.readLayer(l)
}

// conventionalSize is the ladder a publisher is asked to produce. It is a
// starting assumption for the allocator, refined by measurement.
func conventionalSize(id LayerID) (w, h int) {
	switch id {
	case LayerQuarter:
		return 320, 180
	case LayerHalf:
		return 640, 360
	default:
		return 1280, 720
	}
}

// readLayer is the hot loop: one goroutine per inbound encoding.
func (t *PublishedTrack) readLayer(l *inboundLayer) {
	for {
		pkt, _, err := l.remote.ReadRTP()
		if err != nil {
			if err == io.EOF || t.closed.Load() {
				return
			}
			return
		}
		l.bytes.Add(int64(len(pkt.Payload)))
		l.recalc()
		now := time.Now().UnixNano()
		if prev := t.lastPacket.Swap(now); now-prev > int64(audioActive) {
			if wake := t.onWake.Load(); wake != nil {
				(*wake)(t)
			}
		}
		t.dispatch(l.id, pkt)
	}
}

// recalc updates the measured bitrate on a one-second window.
func (l *inboundLayer) recalc() {
	l.rateMu.Lock()
	defer l.rateMu.Unlock()
	now := time.Now()
	elapsed := now.Sub(l.lastCalc)
	if elapsed < time.Second {
		return
	}
	total := l.bytes.Load()
	delta := total - l.lastByte
	l.rate.Store(int64(float64(delta) * 8 / elapsed.Seconds()))
	l.lastByte = total
	l.lastCalc = now
}

// dispatch fans one inbound packet out to every subscriber.
//
// Each subscriber gets its own copy of the RTP HEADER, because the forwarder
// rewrites sequence and timestamp in place and every subscriber is at a
// different point in its own outbound space. The payload is shared: it is
// never mutated, and copying it per subscriber would multiply the memory
// traffic of the busiest loop in the process by the size of the room.
func (t *PublishedTrack) dispatch(layer LayerID, pkt *rtp.Packet) {
	kf := t.Kind == webrtc.RTPCodecTypeVideo && IsKeyframe(t.MimeType, pkt.Payload)

	outs := t.outs.Load() // only the receivers with a slot on this track
	if outs == nil {
		return
	}
	for _, d := range *outs {
		s := d.slot.Load()
		if s == nil {
			continue
		}
		local := forwardCopy(pkt) // header by value, payload shared
		dec, waitL, waiting := s.write(d, layer, &local, kf)
		if dec != Forward {
			// Waiting on a keyframe from this layer: ask again (rate-limited
			// per layer by RequestKeyframe). Keeps asking until one comes.
			if !kf && t.Kind == webrtc.RTPCodecTypeVideo && waiting && waitL == layer {
				t.RequestKeyframe(waitL)
			}
			continue
		}
		s.send(d, local)
	}
}

// attach adds d to the receivers dispatch sends to. Copy on write: dispatch
// runs for every packet and must not allocate or lock to find them.
func (t *PublishedTrack) attach(d *Downstream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var cur []*Downstream
	if p := t.outs.Load(); p != nil {
		cur = *p
	}
	if slices.Contains(cur, d) {
		return
	}
	next := append(slices.Clone(cur), d)
	t.outs.Store(&next)
}

// detach removes d from the receivers dispatch sends to.
func (t *PublishedTrack) detach(d *Downstream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.outs.Load()
	if p == nil || !slices.Contains(*p, d) {
		return
	}
	next := slices.DeleteFunc(slices.Clone(*p), func(x *Downstream) bool { return x == d })
	t.outs.Store(&next)
}

// forwardCopy is the packet as it leaves for one subscriber: the header by
// value, the payload shared, and the publisher's header extensions removed.
//
// Those extensions describe the PUBLISHER's connection: its MID for this
// track, its simulcast RID, under extension ids the publisher negotiated.
// Forwarded as they were, every subscriber received another connection's
// section tags. Chrome ignores a MID tag while no section of the bundle maps
// that extension id, and routes by it once one does -- so the moment anyone
// in the room turned a camera on (a video section, which maps MID), every
// subscriber's audio was routed by the publishers' tags and dropped, for the
// rest of the call, with no error anywhere. Measured in Chrome with three
// participants: C turns on a camera, and A and B stop hearing each other.
// They are also bytes on every packet that no receiver can use (MID + RID +
// audio level: 12-20 bytes of a ~100-byte audio packet), which CLAUDE.md
// rule 2 forbids paying for.
func forwardCopy(pkt *rtp.Packet) rtp.Packet {
	local := *pkt
	local.Header.Extension = false
	local.Header.ExtensionProfile = 0
	local.Header.Extensions = nil
	return local
}

// RequestKeyframe asks the publisher for a keyframe on a layer, subject to
// the deduplication window.
func (t *PublishedTrack) RequestKeyframe(layer LayerID) {
	if t.pli == nil || layer > LayerFull {
		return
	}
	slot := &t.lastPLI[layer]
	now := time.Now().UnixNano()
	last := slot.Load()
	if now-last < int64(PLIInterval) {
		return
	}
	if !slot.CompareAndSwap(last, now) {
		return
	}
	t.pli(layer)
}

// LayerSSRC is the publisher's SSRC for one layer: where a keyframe request
// for that layer has to go.
func (t *PublishedTrack) LayerSSRC(layer LayerID) (webrtc.SSRC, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	l, ok := t.layers[layer]
	if !ok || l.remote == nil {
		return 0, false
	}
	return l.remote.SSRC(), true
}

// Layers reports the currently published encodings, with measured bitrates,
// in the shape the allocator consumes.
func (t *PublishedTrack) Layers() []Layer {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Layer, 0, len(t.layers))
	for id, l := range t.layers {
		rate := int(l.rate.Load())
		if rate == 0 {
			// Before the first measurement window closes, fall back to the
			// conventional ladder so a freshly published track is allocatable
			// immediately rather than being invisible for a second.
			rate = conventionalBitrate(id)
		}
		out = append(out, Layer{
			Spatial: uint8(id),
			Width:   l.width,
			Height:  l.height,
			FPS:     conventionalFPS(id),
			Bitrate: rate,
		})
	}
	return out
}

func conventionalBitrate(id LayerID) int {
	switch id {
	case LayerQuarter:
		return 85_000
	case LayerHalf:
		return 350_000
	default:
		return 750_000
	}
}

func conventionalFPS(id LayerID) float64 {
	if id == LayerQuarter {
		return 15
	}
	return 30
}

// Subscribe attaches a downstream and returns it.
func (t *PublishedTrack) Subscribe(d *Downstream) {
	t.mu.Lock()
	if d.track == nil {
		d.track = t
	}
	t.subs[d.SubscriberID] = d
	t.mu.Unlock()
	if d.slot.Load() != nil {
		t.attach(d)
	}
}

// Unsubscribe detaches a downstream.
func (t *PublishedTrack) Unsubscribe(subscriberID string) {
	t.mu.Lock()
	d := t.subs[subscriberID]
	delete(t.subs, subscriberID)
	t.mu.Unlock()
	if d != nil {
		t.detach(d)
	}
}

// SubscribedLayer reports which rung a given subscriber is receiving, or
// LayerNone if it is not subscribed. Used when a keyframe request arrives
// from downstream and has to be aimed at the right inbound encoding.
func (t *PublishedTrack) SubscribedLayer(subscriberID string) LayerID {
	t.mu.RLock()
	d, ok := t.subs[subscriberID]
	t.mu.RUnlock()
	if !ok {
		return LayerNone
	}
	return d.Current()
}

// IsScreenShare reports whether this track carries a screen.
func (t *PublishedTrack) IsScreenShare() bool {
	t.screenMu.RLock()
	defer t.screenMu.RUnlock()
	return t.isScreen
}

// MarkScreen records that this track is a screen share, as declared by its
// publisher. Idempotent.
func (t *PublishedTrack) MarkScreen(v bool) {
	t.screenMu.Lock()
	t.isScreen = v
	t.screenMu.Unlock()
}

// SubscriberCount reports how many receivers this track feeds.
func (t *PublishedTrack) SubscriberCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subs)
}

// Close stops every read loop.
func (t *PublishedTrack) Close() {
	t.closed.Store(true)
	t.mu.Lock()
	t.subs = make(map[string]*Downstream)
	t.outs.Store(nil)
	t.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Downstream
// ---------------------------------------------------------------------------

// Downstream is one (published track, subscriber) pair: what the receiver is
// doing with the track, and the slot carrying it, if any (slots.go).
//
// It is the cheap half of a subscription. Every receiver has one for every
// track in the room; only the ones that are wanted hold a slot, which is the
// outgoing track and its forwarder.
type Downstream struct {
	SubscriberID string
	TrackID      string

	track *PublishedTrack
	slot  atomic.Pointer[slot] // nil: not being sent
	meter *ledger.Meter

	// path describes the wire so that byte accounting reflects the real
	// per-packet overhead rather than counting payload alone.
	path     ledger.Path
	extBytes int

	// What the receiver is actually doing with this feed. Reported by the
	// client from a ResizeObserver and an IntersectionObserver rather than
	// inferred from a layout name, because "gallery tile" is 90 pixels on a
	// phone and 400 on a monitor, and the difference is the whole budget.
	vpMu        sync.RWMutex
	viewportW   int
	viewportH   int
	visible     bool
	pinned      bool
	speakerRank int
	// hasReport: the receiver reported on this track by its id, which it can
	// only do while a slot carries it. Cleared when the slot is lost, so the
	// report for the publisher's tile decides again (Peer.effectiveView).
	hasReport bool
}

func newDownstream(subscriberID, trackID string, meter *ledger.Meter, path ledger.Path, extBytes int) *Downstream {
	return &Downstream{
		SubscriberID: subscriberID, TrackID: trackID, meter: meter,
		path: path, extBytes: extBytes,
		// A new subscription is assumed visible at a modest size until the
		// client says otherwise. Assuming invisible would leave a joiner
		// staring at black tiles until their first viewport report lands.
		viewportW: 640, viewportH: 360, visible: true, speakerRank: -1,
	}
}

// NewDownstream is a subscription with its own private output, permanently
// bound: for tests that drive a track directly.
func NewDownstream(subscriberID, trackID string, local *webrtc.TrackLocalStaticRTP,
	meter *ledger.Meter, path ledger.Path, extBytes int) *Downstream {
	d := newDownstream(subscriberID, trackID, meter, path, extBytes)
	s := newSlot(trackID, local.Kind(), "", local, nil)
	s.owner = d
	d.slot.Store(s)
	return d
}

// SetViewport records what the receiver is painting.
func (d *Downstream) SetViewport(w, h int, visible, pinned bool) {
	d.vpMu.Lock()
	d.viewportW, d.viewportH = w, h
	d.visible, d.pinned = visible, pinned
	d.hasReport = true
	d.vpMu.Unlock()
}

func (d *Downstream) reported() bool {
	d.vpMu.RLock()
	defer d.vpMu.RUnlock()
	return d.hasReport
}

func (d *Downstream) forgetReport() {
	d.vpMu.Lock()
	d.hasReport = false
	d.vpMu.Unlock()
}

// SetSpeakerRank records where this track's publisher sits in the room
// ranking. Negative means unranked.
func (d *Downstream) SetSpeakerRank(rank int) {
	d.vpMu.Lock()
	d.speakerRank = rank
	d.vpMu.Unlock()
}

// Viewport reads the receiver-side context the allocator needs.
func (d *Downstream) Viewport() (w, h int, visible, pinned bool, rank int) {
	d.vpMu.RLock()
	defer d.vpMu.RUnlock()
	return d.viewportW, d.viewportH, d.visible, d.pinned, d.speakerRank
}

// newForwarderFor picks the forwarder by media kind. Video waits for
// keyframes; audio must not, because it has none.
func newForwarderFor(kind webrtc.RTPCodecType) *Forwarder {
	if kind == webrtc.RTPCodecTypeVideo {
		return NewForwarder()
	}
	return NewAudioForwarder()
}

// Select changes the layer this subscriber receives, requesting a keyframe if
// the switch needs one. Without a slot there is nothing to select.
func (d *Downstream) Select(t *PublishedTrack, layer LayerID) {
	if s := d.slot.Load(); s != nil && s.selectLayer(d, layer) {
		t.RequestKeyframe(layer)
	}
}

// Current reports the layer being delivered.
func (d *Downstream) Current() LayerID {
	if s := d.slot.Load(); s != nil && s.ownerOf() == d {
		return s.current()
	}
	return LayerNone
}

// Pending reports the layer the slot is switching to and whether it is
// waiting on a keyframe: for telemetry, where a stuck switch shows itself.
func (d *Downstream) Pending() (LayerID, bool) {
	if s := d.slot.Load(); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, waiting := s.fwd.Waiting()
		return s.fwd.Target(), waiting
	}
	return LayerNone, false
}

// Slot reports the id of the slot carrying this track, or "".
func (d *Downstream) Slot() string {
	if s := d.slot.Load(); s != nil {
		return s.id
	}
	return ""
}

// count books one forwarded packet against the subscriber's ledger, whole
// stack included. This is the point at which the numbers in the telemetry
// panel become measurements rather than estimates.
func (d *Downstream) count(payloadLen int) {
	d.meter.CountRTP(d.path, d.extBytes, payloadLen)
}

// Stats exposes the counters of the slot carrying this track.
func (d *Downstream) Stats() (packets, switches, keyframeWaits uint64) {
	if s := d.slot.Load(); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fwd.Stats()
	}
	return 0, 0, 0
}
