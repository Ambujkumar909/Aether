package sfu

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// Router is the media plane of one room: who is publishing what, who is
// receiving it, and at which rung.
//
// It is also where the allocator stops being a pure function and starts
// spending real bandwidth. Everything upstream of this file decides what
// SHOULD be sent; this is what actually sends it.
type Router struct {
	RoomID string
	Log    *slog.Logger
	Meter  *ledger.Meter
	ICE    []webrtc.ICEServer
	Addrs  *AddressPolicy
	Media  *MediaTransport

	mu     sync.RWMutex
	peers  map[string]*Peer
	tracks map[string]*PublishedTrack

	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup

	// Permit gates every fan-out. See MayForward.
	Permit MayForward
}

func NewRouter(roomID string, meter *ledger.Meter, log *slog.Logger, ice []webrtc.ICEServer, addrs *AddressPolicy, media *MediaTransport) *Router {
	return &Router{
		RoomID: roomID, Log: log, Meter: meter, ICE: ice, Addrs: addrs, Media: media,
		peers:  make(map[string]*Peer),
		tracks: make(map[string]*PublishedTrack),
	}
}

// MayForward decides whether one peer's track may reach anybody else.
//
// Installed by the transport, which knows about hosts and meeting controls;
// the media plane deliberately knows neither. Nil means everything is
// forwarded, which is what a deployment without meeting controls has.
//
// This is where "attendees may not share their screen" becomes true rather
// than merely displayed. Refusing to FORWARD is the only enforcement that
// survives a client that ignores the interface: the sender may keep sending
// into the void, but nothing reaches the room.
type MayForward func(peerID string, kind string) bool

// ErrRouterStopped is returned when a room's media plane has shut down.
var ErrRouterStopped = errors.New("sfu: router has stopped")

// AllocationInterval is how often every receiver's layer assignment is
// recomputed.
//
// Faster is not better. Each pass can move a subscriber between rungs, and a
// rung change costs a keyframe from the publisher. Recomputing at 10 Hz on a
// noisy estimate produces a keyframe storm that consumes the very bandwidth
// the reallocation was trying to conserve -- the same self-defeating shape as
// an expensive recovery path. One second is comfortably faster than a human
// notices a tile sharpen, and slow enough that the estimator has settled.
const AllocationInterval = time.Second

// Start begins the allocation loop. Calling it twice is a no-op rather than a
// second loop, and cancel is written under the lock that Stop reads it with.
//
// Both details matter for the same reason: Start and Stop are reached from
// different goroutines -- the registry creating a room, and shutdown or an
// emptying room stopping it -- so an unsynchronised cancel is a data race, and
// a second Start would orphan the first loop with no way to reach it.
func (r *Router) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(1)
	go r.allocationLoop(ctx)
}

// Stop halts the loop and closes every peer. Safe to call more than once, and
// safe on a router that was never started.
func (r *Router) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
	r.mu.Lock()
	r.closed = true
	for _, p := range r.peers {
		p.Close()
	}
	r.peers = make(map[string]*Peer)
	r.tracks = make(map[string]*PublishedTrack)
	r.mu.Unlock()
}

// permits asks the transport whether this peer may send this kind of media.
// No hook installed means no meeting controls, so everything is forwarded.
func (r *Router) permits(peerID, kind string) bool {
	if r.Permit == nil {
		return true
	}
	return r.Permit(peerID, kind)
}

// forwardKind names what a track counts as for the purpose of meeting
// controls: a screen share is governed separately from a camera, even though
// both are video.
func forwardKind(t *PublishedTrack) string {
	if t.Kind != webrtc.RTPCodecTypeVideo {
		return "mic"
	}
	if t.IsScreenShare() {
		return "share"
	}
	return "camera"
}

// ApplyPermissions re-checks every published track against the current
// controls, and stops forwarding anything no longer allowed.
//
// Needed because a host can tighten the rules mid-meeting, and a check that
// only ran when a track was first published would leave whatever was already
// flowing untouched. Turning sharing off has to stop the share that is on
// screen right now, which is the whole reason a host reaches for it.
func (r *Router) ApplyPermissions() {
	r.mu.RLock()
	tracks := make([]*PublishedTrack, 0, len(r.tracks))
	for _, t := range r.tracks {
		tracks = append(tracks, t)
	}
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	r.mu.RUnlock()

	for _, t := range tracks {
		allowed := r.permits(t.PeerID, forwardKind(t))
		for _, q := range peers {
			if q.ID == t.PeerID {
				continue
			}
			if allowed {
				if _, err := q.Subscribe(t); err != nil {
					r.Log.Warn("resubscribe failed", "peer", q.ID, "track", t.ID, "err", err)
				}
				continue
			}
			q.Unsubscribe(t.ID)
		}
	}
}

// MarkScreenTrack records which of a peer's tracks carries their screen.
//
// The publisher is the only party that knows. A browser generates msid
// identifiers as UUIDs, so nothing in the media itself distinguishes a shared
// screen from a camera, and the substring heuristic this replaces silently
// never fired for any real browser client. The consequence was not cosmetic:
// every screen share was allocated as though it were a face, so the allocator
// optimised it for motion instead of legibility and sent a low spatial layer
// of text.
func (r *Router) MarkScreenTrack(peerID, trackID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, t := range r.tracks {
		if t.PeerID != peerID || t.Kind != webrtc.RTPCodecTypeVideo {
			continue
		}
		t.MarkScreen(trackID != "" && id == trackID)
	}
}

// AddPeer creates a peer and wires it into the room.
// Signalling is how a Peer's SDP and ICE reach the client.
//
// Passed INTO AddPeer rather than set on the returned Peer, because AddPeer
// subscribes the newcomer to every track already in the room, and each
// subscription fires a renegotiation that needs to be sent. With the
// callbacks set afterwards, the first subscriber offer went into a nil
// function and was dropped; the negotiation flag stayed set, nothing ever
// retried, and the client sat with an empty subscriber connection for the
// whole meeting. Timing-dependent -- a goroutine race against an assignment
// two lines later -- so it worked often enough to look like a network problem.
type Signalling struct {
	SendSDP func(dir Direction, isOffer bool, body string)
	SendICE func(dir Direction, candidate string)
}

func (r *Router) AddPeer(id string, sig Signalling) (*Peer, error) {
	// A stopped router must not take on new work. Stop closes every peer it
	// holds and then empties its maps; one added afterwards goes into the
	// fresh map with nobody left to close it, and holds two real peer
	// connections and their goroutines until the process exits.
	//
	// Reachable during ordinary shutdown, because a join already in flight
	// arrives after the media plane has been told to stop.
	r.mu.RLock()
	stopped := r.closed
	r.mu.RUnlock()
	if stopped {
		return nil, ErrRouterStopped
	}

	p, err := NewPeer(id, r.RoomID, r.Meter.Child("peer:"+id), r.ICE, r.Log, r.Addrs, r.Media)
	if err != nil {
		return nil, err
	}

	p.SendSDP = sig.SendSDP
	p.SendICE = sig.SendICE
	p.SetOnPublish(func(t *PublishedTrack) { r.onPublish(t) })

	r.mu.Lock()
	// Checked again under the write lock: the router can stop between the
	// two, and losing that race is what leaks the peer.
	if r.closed {
		r.mu.Unlock()
		p.Close()
		return nil, ErrRouterStopped
	}
	r.peers[id] = p
	existing := make([]*PublishedTrack, 0, len(r.tracks))
	for _, t := range r.tracks {
		existing = append(existing, t)
	}
	r.mu.Unlock()

	// A joiner subscribes to everything already in flight. This happens
	// before their publisher connection is even negotiated, so they see the
	// room immediately rather than after their own camera comes up.
	for _, t := range existing {
		if _, err := p.Subscribe(t); err != nil {
			r.Log.Warn("subscribe on join failed", "peer", id, "track", t.ID, "err", err)
		}
	}
	return p, nil
}

// RemovePeer tears a participant down and unsubscribes everyone from their
// tracks.
func (r *Router) RemovePeer(id string) {
	r.mu.Lock()
	p, ok := r.peers[id]
	delete(r.peers, id)
	var gone []string
	for tid, t := range r.tracks {
		if t.PeerID == id {
			gone = append(gone, tid)
			delete(r.tracks, tid)
		}
	}
	others := make([]*Peer, 0, len(r.peers))
	for _, q := range r.peers {
		others = append(others, q)
	}
	r.mu.Unlock()

	if !ok {
		return
	}
	for _, q := range others {
		for _, tid := range gone {
			q.Unsubscribe(tid)
		}
	}
	p.Close()
}

// onPublish fans a newly published track out to every other participant.
func (r *Router) onPublish(t *PublishedTrack) {
	r.mu.Lock()
	r.tracks[t.ID] = t
	others := make([]*Peer, 0, len(r.peers))
	for _, q := range r.peers {
		if q.ID != t.PeerID {
			others = append(others, q)
		}
	}
	r.mu.Unlock()

	// Meeting controls, applied before anybody is subscribed.
	//
	// Checked here rather than at the client, because a hidden button is not
	// a control: a participant on an older build, or one who opened the
	// console, is unaffected by it. Refusing to fan the track out is what
	// makes "attendees may not share" true.
	if !r.permits(t.PeerID, forwardKind(t)) {
		r.Log.Info("track refused by meeting controls",
			"room", r.RoomID, "peer", t.PeerID, "kind", forwardKind(t))
		return
	}

	r.Log.Info("track published", "room", r.RoomID, "peer", t.PeerID,
		"track", t.ID, "kind", t.Kind.String(), "codec", t.MimeType,
		"subscribers", len(others))

	for _, q := range others {
		if _, err := q.Subscribe(t); err != nil {
			r.Log.Warn("subscribe failed", "peer", q.ID, "track", t.ID, "err", err)
		}
	}
}

// Peer looks a participant up.
func (r *Router) Peer(id string) (*Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.peers[id]
	return p, ok
}

// Track looks a published track up.
func (r *Router) Track(id string) (*PublishedTrack, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tracks[id]
	return t, ok
}

// SetViewport records how large a receiver is actually painting a track, and
// whether it is on screen at all.
//
// This is the single most valuable piece of information the client can send,
// and it is the one no competing product asks for. Without it the server is
// guessing, and its guess is always "big", because guessing small looks like
// a bug and guessing big only looks like a bill.
func (r *Router) SetViewport(peerID, trackID string, w, h int, visible, pinned bool) {
	p, ok := r.Peer(peerID)
	if !ok {
		return
	}
	d, ok := p.Downstreams()[trackID]
	if !ok {
		return
	}
	d.SetViewport(w, h, visible, pinned)
}

// SetSpeakerRank propagates the room's speaker ranking into the media plane,
// where it becomes a bandwidth decision rather than a UI decoration.
func (r *Router) SetSpeakerRank(ranks map[string]int) {
	r.mu.RLock()
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	tracks := make(map[string]*PublishedTrack, len(r.tracks))
	for k, v := range r.tracks {
		tracks[k] = v
	}
	r.mu.RUnlock()

	for _, p := range peers {
		for tid, d := range p.Downstreams() {
			t, ok := tracks[tid]
			if !ok {
				continue
			}
			rank, ranked := ranks[t.PeerID]
			if !ranked {
				rank = -1
			}
			d.SetSpeakerRank(rank)
		}
	}
}

// allocationLoop recomputes every receiver's layer assignment on a timer.
func (r *Router) allocationLoop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(AllocationInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.allocateAll()
		}
	}
}

func (r *Router) allocateAll() {
	r.mu.RLock()
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	tracks := make(map[string]*PublishedTrack, len(r.tracks))
	for k, v := range r.tracks {
		tracks[k] = v
	}
	r.mu.RUnlock()

	for _, p := range peers {
		r.allocatePeer(p, tracks)
	}
}

// allocatePeer decides what one receiver gets, and applies it.
func (r *Router) allocatePeer(p *Peer, tracks map[string]*PublishedTrack) {
	downs := p.Downstreams()
	if len(downs) == 0 {
		return
	}

	subs := make([]Subscription, 0, len(downs))
	byTrack := make(map[string]*Downstream, len(downs))

	for tid, d := range downs {
		t, ok := tracks[tid]
		if !ok {
			continue
		}
		// Audio is not layered and is not subject to the video budget; it is
		// small, it is the thing people actually cannot do without, and
		// degrading it to protect video is always the wrong trade.
		if t.Kind != webrtc.RTPCodecTypeVideo {
			continue
		}
		w, h, visible, pinned, rank := d.Viewport()
		subs = append(subs, Subscription{
			TrackID:       tid,
			Layers:        t.Layers(),
			ViewportW:     w,
			ViewportH:     h,
			Visible:       visible,
			SpeakerRank:   rank,
			Pinned:        pinned,
			IsScreenShare: t.IsScreenShare(),
		})
		byTrack[tid] = d
	}
	if len(subs) == 0 {
		return
	}

	budget := Budget{
		EstimateBps:   p.EstimateRate(),
		HeadroomRatio: 0.15,
	}
	for _, a := range Allocate(subs, budget) {
		d, ok := byTrack[a.TrackID]
		if !ok {
			continue
		}
		t, ok := tracks[a.TrackID]
		if !ok {
			continue
		}
		want := LayerNone
		if a.Selected {
			want = LayerID(a.Layer.Spatial)
		}
		if d.Current() != want {
			d.Select(t, want)
		}
	}
}

// Stats summarises the media plane for telemetry.
type RouterStats struct {
	Peers  int              `json:"peers"`
	Tracks int              `json:"tracks"`
	Feeds  []DownstreamStat `json:"feeds"`
	// Estimates is each receiver's bandwidth estimate, bit/s: what the
	// allocator may spend on them.
	Estimates map[string]int `json:"estimates"`
}

type DownstreamStat struct {
	Subscriber string `json:"subscriber"`
	Track      string `json:"track"`
	Publisher  string `json:"publisher"`
	Layer      string `json:"layer"`
	Packets    uint64 `json:"packets"`
	Switches   uint64 `json:"switches"`
	ViewportW  int    `json:"viewportW"`
	ViewportH  int    `json:"viewportH"`
	Visible    bool   `json:"visible"`
}

// Stats collects a snapshot.
func (r *Router) Stats() RouterStats {
	r.mu.RLock()
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	tracks := make(map[string]*PublishedTrack, len(r.tracks))
	for k, v := range r.tracks {
		tracks[k] = v
	}
	r.mu.RUnlock()

	st := RouterStats{Peers: len(peers), Tracks: len(tracks), Estimates: make(map[string]int, len(peers))}
	for _, p := range peers {
		st.Estimates[p.ID] = p.EstimateRate()
		for tid, d := range p.Downstreams() {
			t := tracks[tid]
			pub := ""
			if t != nil {
				pub = t.PeerID
			}
			w, h, visible, _, _ := d.Viewport()
			pkts, sw, _ := d.Stats()
			st.Feeds = append(st.Feeds, DownstreamStat{
				Subscriber: p.ID, Track: tid, Publisher: pub,
				Layer:   d.Current().String(),
				Packets: pkts, Switches: sw,
				ViewportW: w, ViewportH: h, Visible: visible,
			})
		}
	}
	return st
}
