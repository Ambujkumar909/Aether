package sfu

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// A receiver's media slots.
//
// Every receiver used to get its own outgoing track for every track in the
// room. That is N^2 tracks: 9,900 at 100 people and 89,700 at 300, each with
// pion's per-sender buffers and an RTCP goroutine, and a subscriber
// description with N media sections renegotiated on every join. At 300 the
// server held 13 GB and 55,762 goroutines and still could not deliver
// (ARCHITECTURE 6bu).
//
// Now each receiver has a pool of slots: outgoing tracks re-pointed at
// whichever cameras it has on screen and whoever is talking. Every track in
// the room still has a Downstream per receiver -- the cheap part: viewport,
// pin, speaker rank -- but only the ones that are wanted hold a slot. The
// pool grows on demand to a cap and is never renegotiated to re-point it: a
// slot switching cameras is the forwarder's resume, which re-anchors on the
// new camera's next keyframe with no gap in the outbound sequence. The
// receiver learns what each slot carries from a Slots frame.
const (
	// MaxVideoSlots is the most cameras one receiver is sent at once, per
	// codec: a 7 x 7 gallery, the largest any client draws.
	MaxVideoSlots = 49
	// MaxAudioSlots is the most voices one receiver is sent at once. Muted
	// participants send nothing; of those who are not, the most recently
	// active and highest-ranked speakers are heard.
	MaxAudioSlots = 6
	// audioActive is how recently a microphone must have sent a packet to be
	// worth a slot.
	audioActive = 2 * time.Second
	// InitialVideoSlots is how many cameras a receiver is sent before it has
	// reported what it shows: a first page of the gallery.
	InitialVideoSlots = 9
)

func (p *Peer) hasViews() bool {
	p.slotMu.Lock()
	defer p.slotMu.Unlock()
	return len(p.views) > 0
}

// slot is one outgoing track on a receiver's subscriber connection.
type slot struct {
	id    string
	kind  webrtc.RTPCodecType
	mime  string
	local *webrtc.TrackLocalStaticRTP
	q     *sendQueue // nil writes inline (hand-built downstreams in tests)

	mu    sync.Mutex
	fwd   *Forwarder
	owner *Downstream // nil: idle
	used  time.Time   // when the owner was last wanted, for eviction
}

func newSlot(id string, kind webrtc.RTPCodecType, mime string, local *webrtc.TrackLocalStaticRTP, q *sendQueue) *slot {
	return &slot{id: id, kind: kind, mime: mime, local: local, q: q, fwd: newForwarderFor(kind)}
}

// write runs one packet of d's track through the slot's forwarder. Under the
// slot's lock and only while d owns it: a packet of a camera that has just
// lost the slot must never reach the camera that took it over.
func (s *slot) write(d *Downstream, layer LayerID, pkt *rtp.Packet, kf bool) (Decision, LayerID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != d {
		return Drop, LayerNone, false
	}
	if s.fwd.WriteRTP(layer, pkt, kf) == Forward {
		return Forward, LayerNone, false
	}
	l, waiting := s.fwd.Waiting()
	return Drop, l, waiting
}

// send hands a forwarded packet to the receiver's writer.
func (s *slot) send(d *Downstream, pkt rtp.Packet) {
	if s.q != nil {
		s.q.push(s, d, pkt)
		return
	}
	if err := s.local.WriteRTP(&pkt); err == nil {
		d.count(len(pkt.Payload))
	}
}

// selectLayer asks for a layer of d's track; true if a keyframe is needed.
func (s *slot) selectLayer(d *Downstream, layer LayerID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != d {
		return false
	}
	return s.fwd.Select(layer)
}

func (s *slot) current() LayerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fwd.Current()
}

// take gives the slot to d. The forwarder is paused first, so the layer the
// allocator selects next resumes on d's keyframe, one past the last packet
// this slot sent.
func (s *slot) take(d *Downstream, now time.Time) (prev *Downstream) {
	s.mu.Lock()
	prev = s.owner
	s.fwd.Select(LayerNone)
	s.owner, s.used = d, now
	s.mu.Unlock()
	if prev != nil {
		prev.slot.CompareAndSwap(s, nil)
		prev.track.detach(prev)
		prev.forgetReport() // the receiver can no longer report it by track id
	}
	if d != nil {
		d.slot.Store(s)
		d.track.attach(d)
	}
	return prev
}

func (s *slot) ownerOf() *Downstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// requestKeyframe passes a receiver's PLI to the track the slot carries.
func (s *slot) requestKeyframe() {
	s.mu.Lock()
	d, l := s.owner, s.fwd.Current()
	s.mu.Unlock()
	if d != nil && l != LayerNone {
		d.track.RequestKeyframe(l)
	}
}

// addSlot creates an outgoing track on the subscriber connection. Adding a
// track renegotiates; it happens only when the pool grows.
func (p *Peer) addSlot(kind webrtc.RTPCodecType, mime string) (*slot, error) {
	p.slotMu.Lock()
	p.slotSeq++
	prefix := "v"
	if kind == webrtc.RTPCodecTypeAudio {
		prefix = "a"
	}
	id := fmt.Sprint(prefix, p.slotSeq)
	p.slotMu.Unlock()

	local, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mime}, id, SlotStream)
	if err != nil {
		return nil, fmt.Errorf("create slot track: %w", err)
	}
	sender, err := p.sub.AddTrack(local)
	if err != nil {
		return nil, fmt.Errorf("add slot track: %w", err)
	}
	s := newSlot(id, kind, mime, local, p.out)
	// Drain the sender's RTCP: keyframe requests go to whatever the slot
	// carries, receiver reports feed this receiver's estimate.
	go p.readSlotRTCP(sender, s)
	return s, nil
}

// SlotStream is the stream id every slot track carries, so a client can tell
// a slot (re-pointed by Slots frames) from anything else.
const SlotStream = "slots"

func (p *Peer) readSlotRTCP(sender *webrtc.RTPSender, s *slot) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, pkt := range pkts {
			switch rr := pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.requestKeyframe()
			case *rtcp.ReceiverReport:
				for _, rep := range rr.Reports {
					p.observeLoss(float64(rep.FractionLost)/256, time.Now())
				}
			}
		}
	}
}

// view is what a receiver reported for one participant's tile, by person
// rather than by track: the client cannot name a track it has not been sent.
type view struct {
	w, h            int
	visible, pinned bool
}

// SetPersonView records a receiver's report for one participant's tile.
func (p *Peer) SetPersonView(person string, w, h int, visible, pinned bool) {
	p.slotMu.Lock()
	if p.views == nil {
		p.views = make(map[string]view)
	}
	old, had := p.views[person]
	p.views[person] = view{w, h, visible, pinned}
	p.slotMu.Unlock()
	if !had || old.visible != visible {
		p.Log.Debug("person view", "peer", p.ID, "person", person, "visible", visible, "w", w, "h", h)
	}
}

// effectiveView is what to assume about a downstream: its own report if the
// receiver has made one for it, else the report for its publisher's tile,
// else visible at a modest size (a joiner must not stare at black tiles
// until the first report lands).
func (p *Peer) effectiveView(d *Downstream) (w, h int, visible, pinned bool, rank int) {
	w, h, visible, pinned, rank = d.Viewport()
	if d.reported() {
		return
	}
	p.slotMu.Lock()
	v, ok := p.views[d.track.PeerID]
	p.slotMu.Unlock()
	if ok && !d.track.IsScreenShare() {
		return v.w, v.h, v.visible, v.pinned, rank
	}
	return
}

// kick asks for a rebind soon, off the caller's goroutine, coalescing bursts.
// A camera published into a room of 300 subscribes 299 receivers; doing each
// one's rebind (and any slot it adds) inline held up pion's track callback,
// and with it the publisher's first keyframe request, for all 299.
func (p *Peer) kick() {
	if p.kicking.CompareAndSwap(false, true) {
		go func() {
			p.kicking.Store(false) // a kick during this rebind runs another
			p.rebind()
		}()
	}
}

// rebind decides which downstreams hold slots, growing the pool to its cap
// as needed, and tells the receiver if anything changed. Called when the
// room changes and on every allocation cycle.
func (p *Peer) rebind() {
	if p.closed.Load() {
		return
	}
	p.bindMu.Lock()
	defer p.bindMu.Unlock()

	now := time.Now()
	type cand struct {
		d                     *Downstream
		pinned, screen, bound bool
		rank                  int
		last                  int64
	}
	groups := map[string][]cand{} // kind/mime -> candidates
	for _, d := range p.Downstreams() {
		t := d.track
		key := t.Kind.String() + "/" + t.MimeType
		c := cand{d: d, bound: d.slot.Load() != nil, last: t.lastPacket.Load()}
		_, _, visible, pinned, rank := p.effectiveView(d)
		c.pinned, c.rank, c.screen = pinned, rank, t.IsScreenShare()
		if t.Kind == webrtc.RTPCodecTypeAudio {
			if now.UnixNano()-c.last > int64(audioActive) {
				continue
			}
		} else if !visible {
			continue
		}
		groups[key] = append(groups[key], c)
	}

	changed := false
	for key, cs := range groups {
		// Pinned, then a screen, then the higher-ranked speaker, then what
		// already has a slot (no churn), then the most recent audio, then the
		// track id: a total order, so two cycles never disagree.
		slices.SortFunc(cs, func(a, b cand) int {
			if a.pinned != b.pinned {
				return boolFirst(a.pinned)
			}
			if a.screen != b.screen {
				return boolFirst(a.screen)
			}
			if ra, rb := rankKey(a.rank), rankKey(b.rank); ra != rb {
				return cmp.Compare(ra, rb)
			}
			if a.bound != b.bound {
				return boolFirst(a.bound)
			}
			if a.last != b.last {
				return cmp.Compare(b.last, a.last)
			}
			return cmp.Compare(a.d.TrackID, b.d.TrackID)
		})
		limit := MaxVideoSlots
		if cs[0].d.track.Kind == webrtc.RTPCodecTypeAudio {
			limit = MaxAudioSlots
		} else if !p.hasViews() {
			// Before the receiver has said what it shows, every camera is
			// assumed visible, and a joiner of a 100-person meeting was sent
			// 49 cameras at once: 400 Mbit/s into the load test's receivers,
			// which fell 20 s behind, reported the backlog as loss and drove
			// every estimate to the floor (ARCHITECTURE 6bv). A first page of
			// the gallery until the first report, which follows within a
			// render.
			limit = InitialVideoSlots
		}
		cs = cs[:min(len(cs), limit)]
		wanted := make(map[*Downstream]bool, len(cs))
		for _, c := range cs {
			wanted[c.d] = true
		}
		pool := p.slotsFor(key)
		for _, s := range pool {
			if o := s.ownerOf(); o != nil && wanted[o] {
				s.mu.Lock()
				s.used = now
				s.mu.Unlock()
			}
		}
		for _, c := range cs {
			if c.d.slot.Load() != nil {
				continue
			}
			// An idle slot; else grow the pool; else take one from a track
			// no longer wanted (a tile just scrolled off keeps its slot
			// while there is room, so scrolling back is instant).
			s := idleSlot(pool)
			if s == nil && len(pool) < limit {
				ns, err := p.addSlot(c.d.track.Kind, c.d.track.MimeType)
				if err != nil {
					p.Log.Warn("slot not added", "peer", p.ID, "err", err)
					break
				}
				p.slotMu.Lock()
				p.slots[key] = append(p.slots[key], ns)
				pool = p.slots[key]
				p.slotMu.Unlock()
				s = ns
			}
			if s == nil {
				s = evictable(pool, wanted)
			}
			if s == nil {
				break // the pool is full of wanted tracks
			}
			s.take(c.d, now)
			changed = true
			// Start low; the allocator promotes within a cycle. Audio has one layer.
			if c.d.track.Kind == webrtc.RTPCodecTypeVideo {
				c.d.Select(c.d.track, LayerQuarter)
			} else {
				c.d.Select(c.d.track, LayerFull)
			}
		}
	}
	if changed || p.slotsDirty.Swap(false) {
		p.sendSlots()
	}
}

func idleSlot(pool []*slot) *slot {
	for _, s := range pool {
		if s.ownerOf() == nil {
			return s
		}
	}
	return nil
}

// evictable is the slot held longest by something no longer wanted.
func evictable(pool []*slot, wanted map[*Downstream]bool) *slot {
	var best *slot
	var bestUsed time.Time
	for _, s := range pool {
		s.mu.Lock()
		o, used := s.owner, s.used
		s.mu.Unlock()
		if o != nil && !wanted[o] && (best == nil || used.Before(bestUsed)) {
			best, bestUsed = s, used
		}
	}
	return best
}

func (p *Peer) slotsFor(key string) []*slot {
	p.slotMu.Lock()
	defer p.slotMu.Unlock()
	return slices.Clone(p.slots[key])
}

// release frees whatever slot d holds (its track went away).
func (p *Peer) release(d *Downstream) {
	if s := d.slot.Load(); s != nil {
		s.mu.Lock()
		if s.owner == d {
			s.owner = nil
			s.fwd.Select(LayerNone)
		}
		s.mu.Unlock()
		d.slot.CompareAndSwap(s, nil)
		d.track.detach(d)
		p.slotsDirty.Store(true)
	}
}

// sendSlots tells the receiver what every slot carries.
func (p *Peer) sendSlots() {
	if p.SendSlots == nil {
		return
	}
	p.slotMu.Lock()
	var all []*slot
	for _, pool := range p.slots {
		all = append(all, pool...)
	}
	p.slotMu.Unlock()
	slices.SortFunc(all, func(a, b *slot) int { return cmp.Compare(a.id, b.id) })
	out := make([]SlotBinding, 0, len(all))
	for _, s := range all {
		b := SlotBinding{Slot: s.id}
		if o := s.ownerOf(); o != nil {
			b.Publisher, b.Track = o.track.PeerID, o.track.ID
		}
		out = append(out, b)
	}
	p.Log.Debug("slots", "peer", p.ID, "map", out)
	p.SendSlots(out)
}

// SlotBinding is one slot and what it carries; empty Publisher means idle.
type SlotBinding struct{ Slot, Publisher, Track string }

func boolFirst(a bool) int {
	if a {
		return -1
	}
	return 1
}

// rankKey orders speaker ranks: 0 first, unranked (-1) last.
func rankKey(r int) int {
	if r < 0 {
		return 1 << 30
	}
	return r
}
