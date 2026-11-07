package signal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// Gateway is the control-plane module.
// RosterPerson is one person in a room as the meeting chat needs to know
// them: the account (empty for a guest) and the name they joined with.
type RosterPerson struct {
	UserID string
	Name   string
}

type Gateway struct {
	// Identify resolves the authenticated account behind a request. Injected
	// rather than imported so the transport does not depend on the identity
	// module's types.
	Identify func(*http.Request) (id, email string)

	// RoomPolicy reports how a scheduled room admits people and who organised
	// it. Injected for the same reason, and nil for a deployment with no
	// scheduling: an ad-hoc room then keeps the room package's own default,
	// which is trusted rather than open.
	RoomPolicy func(ctx context.Context, room string) (admission, organizerID string, ok bool)

	// TrustProxy reads the client's address from X-Forwarded-For, as the
	// HTTP modules do with the same flag.
	TrustProxy bool

	// Invited reports whether an account holds an invitation to a scheduled
	// room, as organiser or invitee. Injected like RoomPolicy, and nil for a
	// deployment without scheduling -- where every room is ad-hoc and there
	// is no invitation to hold.
	Invited func(ctx context.Context, room, userID, email string) bool

	// CoOrganizer reports whether an account was chosen as a co-organizer
	// of the meeting scheduled in room ("Choose co-organizers").
	CoOrganizer func(ctx context.Context, room, userID, email string) bool

	// OnJoin and OnLeave record attendance for the dashboard's "recent"
	// list. Injected like RoomPolicy so the transport never imports the
	// scheduling module. OnJoin returns an opaque visit id that OnLeave
	// closes; either may be nil.
	OnJoin  func(ctx context.Context, userID, room, displayName string) string
	OnLeave func(ctx context.Context, visitID string)

	// OnPresence is called with an account id whenever that person's room
	// state changes in a way a status shows: joined, left, started or
	// stopped sharing. Guests (no account) are not reported. May be nil.
	OnPresence func(userID string)

	// OnEmpty is called when the last participant leaves a room, which is
	// when a meeting is actually over -- whatever its scheduled end time
	// said. May be nil.
	OnEmpty func(ctx context.Context, room string)

	// Recording and transcription (recording.go). RecordingAllowed is the
	// deployment's "Recording allowed" ceiling; OnRecording audits a start
	// or stop; OnTranscript stores a final line of the transcript, under
	// the meeting's session (its room's epoch). Any may be nil.
	RecordingAllowed func(ctx context.Context) bool
	OnRecording      func(ctx context.Context, userID, room string, recording, on bool)
	OnTranscript     func(ctx context.Context, room string, session time.Time, userID, name, text string, at time.Time)

	// OnRoster is called after every join with everybody now in the room.
	// The messaging module uses it to give a meeting of three or more its
	// own conversation. Injected so this package never imports messaging;
	// may be nil.
	OnRoster func(ctx context.Context, room string, people []RosterPerson)

	Rooms  *room.Registry
	Media  *sfu.Registry
	Meter  *ledger.Meter
	Log    *slog.Logger
	Origin []string

	// Connections are indexed both ways. The set drives shutdown; the
	// by-id map keeps broadcast at O(N) rather than O(N^2), which stops
	// mattering theoretically and starts mattering physically somewhere
	// around forty participants.
	mu    sync.RWMutex
	conns map[*Conn]struct{}

	// scheduledHost remembers which account organised each room, so they take
	// over hosting whenever they arrive.
	scheduledHost map[string]string

	// Breakout tickets and the timers that close rooms (breakout.go).
	tickets  map[string]ticket
	brTimers map[string]*time.Timer
	byID     map[string]*Conn
	// lastKeyReq: when each socket publisher was last asked for a keyframe.
	lastKeyReq map[string]time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewGateway(rooms *room.Registry, media *sfu.Registry, meter *ledger.Meter, log *slog.Logger) *Gateway {
	return &Gateway{
		Rooms: rooms, Media: media, Meter: meter, Log: log,
		conns:         make(map[*Conn]struct{}),
		byID:          make(map[string]*Conn),
		scheduledHost: make(map[string]string),
	}
}

func (g *Gateway) Name() string       { return "signal" }
func (g *Gateway) Requires() []string { return []string{"rooms", "media"} }

func (g *Gateway) Start(ctx context.Context, k *modular.Kernel) error {
	ctx, g.cancel = context.WithCancel(context.Background())
	g.wg.Add(1)
	go g.reaper(ctx)
	k.Provide("signal.gateway", g)
	return nil
}

func (g *Gateway) Stop(ctx context.Context) error {
	if g.cancel != nil {
		g.cancel()
	}
	g.mu.Lock()
	for c := range g.conns {
		c.CloseWithReason(websocket.StatusGoingAway, "server shutting down")
	}
	g.conns = make(map[*Conn]struct{})
	g.byID = make(map[string]*Conn)
	// A breakout timer must not fire into a server that has stopped.
	for id, t := range g.brTimers {
		t.Stop()
		delete(g.brTimers, id)
	}
	g.mu.Unlock()
	g.wg.Wait()
	return nil
}

// reaper removes empty rooms so an idle node does not accumulate them.
func (g *Gateway) reaper(ctx context.Context) {
	defer g.wg.Done()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The room's media router goes with it. It used to stay: Drop had
			// no caller, and every room ever used kept a router for the life
			// of the process (found by the stress run, ARCHITECTURE 6br).
			if n := g.Rooms.Reap(2*time.Minute, g.dropMedia); n > 0 {
				g.Log.Info("reaped empty rooms", "count", n)
			}
		}
	}
}

// dropMedia stops and removes a reaped room's router.
func (g *Gateway) dropMedia(id string) {
	if g.Media != nil {
		g.Media.Drop(id)
	}
}

// ServeHTTP upgrades to WebSocket and runs the session.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	name := r.URL.Query().Get("name")
	if roomID == "" {
		http.Error(w, "room required", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = "Guest"
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: g.Origin,
		// Compression is deliberately disabled. permessage-deflate on a
		// stream of 3 byte binary frames costs CPU and a per-message
		// dictionary reset to achieve nothing: the protocol is already
		// denser than a general-purpose compressor can make it, and DEFLATE
		// on short binary payloads routinely expands them.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		g.Log.Warn("websocket upgrade failed", "err", err)
		return
	}

	// Bounded, but large enough for the media fallback. This was 64 KB when
	// the socket carried only control frames, and a VP8 keyframe is routinely
	// 50-150 KB: the first keyframe over the fallback path would have closed
	// the connection with a size error and nothing to say why. 1 MB admits
	// any sane keyframe and still refuses an allocation attack; the encoder
	// is configured well under it.
	ws.SetReadLimit(1 << 20)

	c := NewConn(ws, g.Meter.Child("conn"))
	c.RoomID = roomID
	c.ParticipantID = newID()
	// The person's address, by the same rule as every other module: behind
	// a proxy, every connection's RemoteAddr is the proxy's, and "N others
	// are on this same device" went to everybody in every meeting (found by
	// the stress run, ARCHITECTURE 6br).
	c.Addr = httpx.ClientIP(r, g.TrustProxy)
	if g.Identify != nil {
		c.UserID, c.UserEmail = g.Identify(r)
	}
	if t := r.URL.Query().Get("bt"); len(t) <= maxTicketLen {
		c.Ticket = t
	}

	g.mu.Lock()
	g.conns[c] = struct{}{}
	g.byID[c.ParticipantID] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.conns, c)
		delete(g.byID, c.ParticipantID)
		g.mu.Unlock()
		c.Close()
	}()

	g.run(r.Context(), c, roomID, name)
}

func (g *Gateway) run(ctx context.Context, c *Conn, roomID, name string) {
	rm := g.Rooms.GetOrCreate(roomID)

	// Apply what the organiser chose when they scheduled the meeting.
	//
	// Applied once, on the first arrival, and never again: a policy that
	// re-asserted itself on every join would silently undo a host who
	// loosened or tightened the room mid-meeting, which is a decision the
	// person actually present is better placed to make than a row written
	// last week.
	if g.RoomPolicy != nil && rm.HostID() == "" {
		if admission, organiser, ok := g.RoomPolicy(ctx, roomID); ok {
			rm.SetPolicy(room.Admission(admission))
			// Under the lock: this map is read from every other connection's
			// goroutine, and an unsynchronised write to it is both a data
			// race and, on a map that grew concurrently, a crash.
			g.mu.Lock()
			g.scheduledHost[roomID] = organiser
			g.mu.Unlock()
		}
	}

	// The admission gate.
	//
	// This sits before Join and before the media plane deliberately, and that
	// placement is the entire security property. A waiting room implemented
	// after either one is a label rather than a control: the connection would
	// already be a participant, already be named in the roster, already be
	// receiving broadcasts, and the only thing "waiting" would mean is that a
	// tile was hidden. Every real failure of a waiting room has that shape.
	//
	// Held here, a knocker has no room membership and no peer connection, so
	// there is nothing to leak to them even by mistake, and no future frame
	// type has to remember to exclude them.
	if !g.admit(ctx, c, rm, roomID, name) {
		return
	}

	p := &room.Participant{
		ID:          c.ParticipantID,
		UserID:      c.UserID,
		DisplayName: name,
		Role:        wire.RoleMember,
		Send:        c.Send,
		State:       wire.State{Participant: c.ParticipantID, Role: wire.RoleMember},
		Addr:        c.Addr,
	}
	roster, err := rm.Join(p)
	if err != nil {
		c.CloseWithReason(websocket.StatusTryAgainLater, err.Error())
		return
	}
	c.Meter = p.Meter
	g.adviseSameDevice(rm, p)

	// Attendance opens here, after the join is real, and closes in the
	// defer below, however the connection ends.
	visit := ""
	if g.OnJoin != nil {
		visit = g.OnJoin(ctx, c.UserID, roomID, name)
	}
	if g.OnPresence != nil && c.UserID != "" {
		g.OnPresence(c.UserID)
	}
	if g.OnRoster != nil {
		people := make([]RosterPerson, 0, len(roster)+1)
		for _, q := range roster {
			people = append(people, RosterPerson{UserID: q.UserID, Name: q.DisplayName})
		}
		people = append(people, RosterPerson{UserID: c.UserID, Name: name})
		g.OnRoster(ctx, roomID, people)
	}

	// The organiser hosts their own meeting, whenever they arrive. Without
	// this the first person through the door keeps hosting even after the
	// person who called the meeting shows up, and cannot hand it over.
	if c.UserID != "" {
		g.mu.RLock()
		organiser := g.scheduledHost[roomID]
		g.mu.RUnlock()
		prevHost := rm.HostID()
		if organiser != "" && organiser == c.UserID {
			rm.SetHost(p.ID)
		} else if rm.IsHostAccount(c.UserID) && !rm.IsHost(p.ID) {
			// The host came back. Being admitted past the lock is only half
			// of it: they must also host again, or they are in the room with
			// the controls greyed out and no way to unlock the door they
			// just walked through.
			rm.SetHost(p.ID)
		}
		// A co-organizer chosen when the meeting was scheduled arrives as one.
		// Set before the arrival is announced, so everyone sees the role.
		if !rm.IsHost(p.ID) && g.CoOrganizer != nil && g.CoOrganizer(ctx, roomID, c.UserID, c.UserEmail) {
			_, _ = rm.SetRole(p.ID, wire.RoleHost)
		}
		// Whoever hosted before keeps the role but is no longer the
		// organizer; everybody already here is told (the arrival's own
		// roster entry carries the organizer bit).
		if h := rm.HostID(); prevHost != "" && h != prevHost {
			if role, ok := rm.Role(prevHost); ok {
				g.broadcastRole(rm, prevHost, role)
			}
		}
	}

	// Bring the media plane up for this participant. Both peer connections
	// are created now, before any track exists, so that ICE and DTLS finish
	// while the user is still reading the room rather than after they click
	// unmute.
	router := g.Media.GetOrCreate(roomID)
	if router == nil {
		// The media plane has shut down while this join was in flight.
		c.CloseWithReason(websocket.StatusGoingAway, "server shutting down")
		return
	}
	// Meeting controls reach the media plane through here. The router knows
	// nothing about hosts; the room does.
	router.SetPermit(func(peerID, kind string) bool { return rm.May(peerID, kind) })

	// Signalling is attached IN the AddPeer call, not after it. AddPeer
	// subscribes the newcomer to every existing track, and each subscription
	// fires a renegotiation that needs somewhere to go. Set afterwards, the
	// first subscriber offer raced the assignment and lost.
	peer, err := router.AddPeer(p.ID, sfu.Signalling{
		SendSDP: func(dir sfu.Direction, isOffer bool, body string) {
			m := wire.SDP{Dir: wire.Direction(dir), IsOffer: isOffer, Body: body}
			c.Emit(func(w *wire.Writer) { m.Encode(w) })
		},
		SendICE: func(dir sfu.Direction, cand string) {
			m := wire.ICE{Dir: wire.Direction(dir), Candidate: cand}
			c.Emit(func(w *wire.Writer) { m.Encode(w) })
		},
		SendSlots: func(bs []sfu.SlotBinding) {
			m := wire.Slots{Entries: make([]wire.SlotEntry, len(bs))}
			for i, b := range bs {
				m.Entries[i] = wire.SlotEntry{Slot: b.Slot, Publisher: b.Publisher, Track: b.Track}
			}
			c.Emit(func(w *wire.Writer) { m.Encode(w) })
		},
	})
	if err != nil {
		g.Log.Error("media plane failed", "id", p.ID, "err", err)
		c.CloseWithReason(websocket.StatusInternalError, "media unavailable")
		return
	}

	g.Log.Info("participant joined", "room", roomID, "id", p.ID, "name", name,
		"size", rm.Size())

	defer func() {
		if g.OnLeave != nil {
			// Background context: the request context is already cancelled
			// by the time a closed socket unwinds, and a cancelled write
			// would leave the visit open forever.
			g.OnLeave(context.Background(), visit)
		}
		router.RemovePeer(p.ID)
		wasLit := rm.IsSpotlit(p.ID)
		hostBefore := rm.HostID()
		_ = rm.Leave(p.ID) // also takes them off the spotlight
		// Leaving hosts hand over (room.Leave). Say so, or the new host sits
		// with greyed-out controls in a meeting nobody can admit anyone to.
		if h := rm.HostID(); h != "" && h != hostBefore {
			g.broadcastRole(rm, h, wire.RoleHost)
		}
		// After Leave, so the reading the status loop takes no longer
		// finds them in the room.
		if g.OnPresence != nil && p.UserID != "" {
			g.OnPresence(p.UserID)
		}
		// The last one out ends the meeting. Read after Leave, so "nobody
		// else is here" is the state including this departure.
		if g.OnEmpty != nil && rm.Size() == 0 && !rm.BreakoutsOpen() {
			g.OnEmpty(context.Background(), roomID)
		}
		g.announce(rm, p.ID, func(w *wire.Writer) {
			w.WriteHeader(wire.OpRoster, 0, false)
			w.Bool(false) // false = left
			w.Str(p.ID)
		})
		if wasLit {
			g.broadcastSpotlights(rm)
		}
		// Their recording stops with them: it was on their computer.
		if rm.LeaveRecording(p.ID) {
			g.announceAll(rm, encodeRecState(rm.Recording()))
		}
		rm.LeaveTogether(p.ID)
		// Left a breakout room: the managers' list counts one fewer.
		if parent := room.BreakoutParent(roomID); parent != "" {
			if prm, ok := g.Rooms.Get(parent); ok {
				g.broadcastBreakoutState(prm)
			}
		}
		g.Log.Info("participant left", "room", roomID, "id", p.ID, "size", rm.Size())
	}()

	// Welcome: assign identity and hand over the roster in one batch, which
	// the coalescing writer turns into a single WebSocket message.
	c.Emit(func(w *wire.Writer) {
		w.WriteHeader(wire.OpWelcome, 0, false)
		w.Str(p.ID)
		w.Str(rm.ID)
		w.Uvarint(uint64(rm.Epoch().UnixMilli()))
	})
	for _, q := range roster {
		q := q
		c.Emit(func(w *wire.Writer) {
			w.WriteHeader(wire.OpRoster, 0, false)
			w.Bool(true) // true = present
			w.Str(q.ID)
			w.Str(q.DisplayName)
			w.Bits(uint32(q.Role), 2)
			// One spare bit marks the organizer: Teams offers no "Make an
			// attendee" on them, and the client has to know who they are.
			w.Bits(b2u(q.ID == rm.HostID()), 1)
			w.Bits(0, 4)
			// The account behind the participant, empty for a guest. One
			// interned string, on join and leave only, and it is what lets
			// the in-meeting chat address a person rather than a tile:
			// a private message needs somebody to send it to, and a
			// conversation that outlives the meeting needs an account.
			w.Str(q.UserID)
		})
	}

	// The controls in force, so an arriving client greys out the right buttons
	// straight away rather than after the first change.
	perms := rm.Permissions()
	c.Emit(func(w *wire.Writer) {
		w.WriteHeader(wire.OpPermissions, 0, false)
		w.Bool(perms.Mic)
		w.Bool(perms.Camera)
		w.Bool(perms.Share)
		w.Bool(perms.Chat)
		w.Bool(!perms.Reactions)
		w.Bits(0, 3)
	})

	// Entering a breakout room: its name, clock and a way back; and the
	// managers' room list now counts one more.
	if parent := room.BreakoutParent(roomID); parent != "" {
		g.breakoutInfo(c, roomID)
		if prm, ok := g.Rooms.Get(parent); ok {
			g.broadcastBreakoutState(prm)
		}
	}

	// The whiteboard, if one is on: the arrival draws the same board.
	if wb := rm.Whiteboard(); wb.Open {
		c.Emit(encodeBoard(wb))
	}

	// Recording or transcription, if either is on: the arrival is told
	// before they say anything. Nothing if neither.
	if rs := rm.Recording(); rs.Recording || rs.Transcribing || rs.Captions {
		c.Emit(encodeRecState(rs))
	}

	// Together mode, if it is on: the arrival takes their seat.
	if ts := rm.Together(); ts.On {
		c.Emit(encodeTogether(ts))
	}

	// Whose mic or camera a host has disabled -- this arrival's own included,
	// when an account block came back with them. Nothing if nobody.
	for _, b := range rm.BlockedList() {
		c.Emit(encodeBlock(b))
	}

	// Who is on the stage, only if anybody is: a meeting without a spotlight
	// costs an arrival nothing.
	if lit := rm.Spotlights(); len(lit) > 0 {
		c.Emit(encodeSpotlights(lit))
	}

	// Tell everyone else this person arrived.
	g.announce(rm, p.ID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpRoster, 0, false)
		w.Bool(true)
		w.Str(p.ID)
		w.Str(p.DisplayName)
		w.Bits(uint32(p.Role), 2)
		w.Bits(b2u(p.ID == rm.HostID()), 1)
		w.Bits(0, 4)
		w.Str(p.UserID)
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := c.Read(ctx, func(h wire.Header, r *wire.Reader) error {
			return g.dispatch(rm, router, peer, p, c, h, r)
		})
		if err != nil {
			if websocket.CloseStatus(err) == -1 {
				g.Log.Debug("connection ended", "id", p.ID, "err", err)
			}
			return
		}
	}
}

// dispatch routes one decoded frame.
func (g *Gateway) dispatch(rm *room.Room, router *sfu.Router, peer *sfu.Peer,
	p *room.Participant, c *Conn, h wire.Header, r *wire.Reader) error {

	switch h.Op {
	case wire.OpPing:
		c.Emit(wire.EncodePong)

	case wire.OpStateDelta:
		var s wire.State
		if err := s.Decode(r); err != nil {
			return err
		}
		// The sender does not get to claim an arbitrary identity or promote
		// its own role. Both are server-authoritative, and a client that
		// asserts otherwise is ignored rather than trusted.
		s.Participant = p.ID
		s.Role = p.Role
		// Nothing changed: nothing goes to anybody (rule 2). And a person
		// toggling for its own sake is held to a rate before the fan-out.
		if s == p.State || !rm.Allowed(p.ID, room.AllowState, time.Now()) {
			return nil
		}
		sharingChanged := p.State.Sharing != s.Sharing
		p.State = s
		if sharingChanged && g.OnPresence != nil && p.UserID != "" {
			g.OnPresence(p.UserID)
		}
		// One presenter at a time (room/presenter.go): starting a share takes
		// over from whoever was presenting; stopping gives the stage back.
		if sharingChanged {
			if s.Sharing {
				g.claimShare(rm, router, p.ID)
			} else {
				rm.ReleasePresenter(p.ID)
			}
		}

		// Tell the media plane which track is a screen. Only the publisher
		// knows, and the allocator judges a screen on legibility rather than
		// smoothness, so without this every share is optimised as if it were
		// a face.
		if router != nil {
			if s.Sharing {
				router.MarkScreenTrack(p.ID, s.ScreenTrack)
			} else {
				router.MarkScreenTrack(p.ID, "")
			}
		}

		g.announce(rm, p.ID, func(w *wire.Writer) { s.Encode(w) })

	case wire.OpPermissions:
		mic := r.Bool()
		cam := r.Bool()
		share := r.Bool()
		chat := r.Bool()
		// Inverted on the wire so the spare bits an older client sends as
		// zero mean "reactions allowed", the default.
		noReactions := r.Bool()
		r.Bits(3)
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to change meeting controls", "id", p.ID)
			return nil
		}
		rm.SetPermissions(room.Permissions{Mic: mic, Camera: cam, Share: share, Chat: chat, Reactions: !noReactions})

		// Applied to what is ALREADY flowing, not only to what happens next.
		// A host turning sharing off means the share currently on everybody's
		// screen stops, which is the entire reason they reached for it.
		if router != nil {
			router.ApplyPermissions()
		}
		g.broadcastPermissions(rm)

	case wire.OpMediaFrame:
		// Media over the socket: the fallback for a network where no UDP path
		// and no relay pair will form. The server never decodes it; it reads
		// the kind to apply the meeting's controls, then hands the payload on
		// with the sender's id in front. Refused, like RTP, when the host has
		// turned that capability off -- a fallback path that bypassed the
		// controls would make them a lie.
		kind := r.Bits(1)
		key := r.Bits(1)
		// Spare bits are forwarded verbatim, not zeroed. Bit 0 of these is
		// the screen flag; anything the client puts here is the client's.
		spare := r.Bits(6)
		// Audio carries no timestamp: chunks are contiguous and the receiver
		// counts them (wire.js encodeMediaFrame explains the bytes this
		// saves). Video keeps its capture time for the decoder. The payload
		// is the tail of the frame, so it carries no length of its own.
		var ts uint64
		if kind == 1 {
			ts = r.Uvarint()
		}
		payload := r.Tail()
		if err := r.Err(); err != nil {
			return err
		}
		what := "mic"
		if kind == 1 {
			// A video frame with the screen flag is governed by the "share"
			// control, not "camera" -- the same distinction the RTP path draws
			// via forwardKind. Otherwise turning off screen sharing would not
			// stop a share carried over the socket.
			what = "camera"
			if spare&1 == 1 {
				what = "share"
			}
		}
		if !rm.May(p.ID, what) || !rm.AllowedN(p.ID, room.AllowMedia, float64(len(payload)), time.Now()) {
			return nil
		}
		body := append([]byte(nil), payload...)
		// The header, per receiver (the publisher's id is interned in each
		// receiver's dictionary); the payload follows it, shared.
		enc := func(w *wire.Writer) {
			w.WriteHeader(wire.OpMediaFrame, 0, false)
			w.Str(p.ID)
			w.Bits(kind, 1)
			w.Bits(key, 1)
			w.Bits(spare, 6)
			if kind == 1 {
				w.Uvarint(ts)
			}
		}
		// A camera only to those with it on screen (room/sockview.go);
		// audio and a screen to everyone. A receiver that is behind has the
		// frame dropped, not the meeting (Conn.EmitMedia), and the publisher
		// is asked for a key frame to recover from.
		audience := rm.CameraAudience(p.ID)
		if what != "camera" {
			audience = audience[:0]
			for _, q := range rm.Snapshot() {
				if q.ID != p.ID {
					audience = append(audience, q)
				}
			}
		}
		for _, q := range audience {
			if qc := g.connFor(q.ID); qc != nil && q.Send != nil {
				if qc.EmitMedia(p.ID, kind == 1, key == 1, body, enc) {
					g.forwardKeyframeReq(rm, q.ID, p.ID)
				}
			}
		}

	case wire.OpMuteAll:
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to mute everybody", "id", p.ID)
			return nil
		}
		muted := rm.MuteAll()
		g.Log.Info("host muted everybody", "host", p.ID, "count", len(muted))
		// Told to everyone, so each client shows its own microphone as off
		// and every roster agrees about who is muted.
		for _, id := range muted {
			if q := g.connFor(id); q != nil {
				q.Emit(func(w *wire.Writer) {
					w.WriteHeader(wire.OpMuteAll, 0, false)
				})
			}
		}
		g.announceAll(rm, func(w *wire.Writer) {
			w.WriteHeader(wire.OpMuteAll, 0, false)
		})

	case wire.OpLobbyDecide:
		// Only a host decides who comes in. A member asking to admit somebody
		// is refused rather than ignored, because silently dropping it would
		// look identical to a host whose click did nothing.
		id := r.Str()
		admit := r.Bool()
		all := r.Bool()
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to admit", "id", p.ID, "room", rm.ID)
			return nil
		}
		if all {
			n := rm.DecideAll(admit)
			g.Log.Info("host decided on the whole lobby",
				"host", p.ID, "admit", admit, "count", n)
		} else if _, err := rm.Decide(id, admit); err != nil {
			return nil
		}
		g.pushLobbyList(rm)

	case wire.OpSpotlight:
		id := r.Str()
		on := r.Bool()
		r.Bits(7)
		if err := r.Err(); err != nil {
			return err
		}
		host := rm.IsHost(p.ID)
		changed := false
		switch {
		case on && host:
			// Full or absent: refused without a reply. The client already
			// holds the whole list and says "No more people can be
			// spotlighted" before it asks.
			changed, _ = rm.Spotlight(id)
		case !on && id == "" && host:
			changed = rm.ClearSpotlights()
		case !on && (host || id == p.ID):
			// "Exit spotlight": the one thing a non-host may do here, and only
			// to themselves.
			changed = rm.Unspotlight(id)
		default:
			g.Log.Warn("refused spotlight change", "id", p.ID, "target", id, "on", on)
		}
		if changed {
			g.broadcastSpotlights(rm)
		}

	case wire.OpBreakout:
		return g.handleBreakout(rm, p, r)

	case wire.OpWhiteboard:
		if !rm.Allowed(p.ID, room.AllowBoard, time.Now()) {
			return nil
		}
		return g.handleWhiteboard(rm, p, r)

	case wire.OpRecording:
		return g.handleRecording(context.Background(), rm, p, r)

	case wire.OpTogether:
		return g.handleTogether(rm, p, r)

	case wire.OpSetRole:
		id := r.Str()
		role := wire.Role(r.Bits(2))
		r.Bits(6)
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to change a role", "id", p.ID, "target", id)
			return nil
		}
		if changed, err := rm.SetRole(id, role); err == nil && changed {
			g.broadcastRole(rm, id, role)
		}

	case wire.OpBlock:
		id := r.Str()
		mic := r.Bool()
		cam := r.Bool()
		r.Bits(6)
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to disable someone's mic or camera", "id", p.ID, "target", id)
			return nil
		}
		if changed, err := rm.SetBlocked(id, mic, cam); err != nil || !changed {
			return nil
		}
		// Applied to what is already flowing, as the meeting-wide controls are.
		if router != nil {
			router.ApplyPermissions()
		}
		g.announceAll(rm, encodeBlock(room.Blocked{ID: id, Mic: mic, Cam: cam}))

	case wire.OpRemove:
		id := r.Str()
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to remove someone", "id", p.ID, "target", id)
			return nil
		}
		if rm.Removable(p.ID, id) != nil {
			return nil
		}
		if q := g.connFor(id); q != nil {
			g.Log.Info("host removed a participant", "host", p.ID, "removed", id)
			q.EmitNow(func(w *wire.Writer) { w.WriteHeader(wire.OpRemove, 0, false) })
			q.CloseWithReason(websocket.StatusPolicyViolation, "removed")
		}

	case wire.OpLowerHands:
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to lower all hands", "id", p.ID)
			return nil
		}
		for _, s := range rm.LowerHands() {
			s := s
			// The owner is told to lower it, so their own button agrees;
			// everybody, the owner included, gets the new state at once, so a
			// client that ignores the request still shows the hand down
			// everywhere else.
			if q := g.connFor(s.Participant); q != nil {
				q.Emit(func(w *wire.Writer) { w.WriteHeader(wire.OpLowerHands, 0, false) })
			}
			g.announceAll(rm, func(w *wire.Writer) { s.Encode(w) })
		}

	case wire.OpEndMeeting:
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to end the meeting", "id", p.ID)
			return nil
		}
		g.endBreakouts(rm)
		ids := rm.End()
		g.Log.Info("host ended the meeting", "host", p.ID, "room", rm.ID, "count", len(ids))
		// Each socket is told, flushed, then closed. Its own run loop then
		// unwinds as for any departure -- leave, attendance, last-one-out --
		// so ending is not a second teardown path to keep correct.
		for _, id := range ids {
			if q := g.connFor(id); q != nil {
				q.EmitNow(func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
				q.CloseWithReason(websocket.StatusNormalClosure, "meeting ended")
			}
		}

	case wire.OpMeetingPolicy:
		policy := r.Str()
		locked := r.Bool()
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to change meeting policy", "id", p.ID)
			return nil
		}
		rm.SetPolicy(room.Admission(policy))
		// "You can't lock the meeting while breakout rooms are open."
		if locked && rm.BreakoutsOpen() {
			locked = rm.Locked()
		}
		rm.SetLocked(locked)
		g.Log.Info("meeting policy changed",
			"host", p.ID, "policy", rm.Policy(), "locked", rm.Locked())
		// Everybody is told, because whether the room is open changes what
		// the people already inside should expect.
		g.announceAll(rm, func(w *wire.Writer) {
			w.WriteHeader(wire.OpMeetingPolicy, 0, false)
			w.Str(string(rm.Policy()))
			w.Bool(rm.Locked())
			w.Bits(0, 7)
		})

	case wire.OpSpeakers:
		// A client reporting its own microphone energy. Cheaper and more
		// accurate than server-side analysis, because the client already
		// computed it for its own level meter, and it works even when the
		// stream is end-to-end encrypted and the server cannot listen.
		var sp wire.Speakers
		if err := sp.Decode(r); err != nil {
			return err
		}
		if len(sp.List) == 0 || !rm.Allowed(p.ID, room.AllowSpeakers, time.Now()) {
			return nil
		}
		ranked, changed := rm.ObserveEnergy(p.ID, sp.List[0].Energy)
		if changed {
			out := wire.Speakers{List: ranked}
			g.announceAll(rm, func(w *wire.Writer) { out.Encode(w) })

			// Speaker rank is not decoration. It is the largest single input
			// to how downlink bandwidth is divided, so it goes straight to
			// the allocator rather than only to the UI.
			ranks := make(map[string]int, len(ranked))
			for i, s := range ranked {
				ranks[s.Participant] = i
			}
			if router != nil {
				router.SetSpeakerRank(ranks)
			}
		}

	case wire.OpBweReport:
		var b wire.BweReport
		if err := b.Decode(r); err != nil {
			return err
		}
		// Consumed by the per-receiver allocator. Recorded here so that the
		// telemetry view can show what the client believes about its link
		// next to what we measured leaving the server.
		p.Meter.CountBytes(ledger.ClassSignalControl, 0)

	case wire.OpSdp:
		var m wire.SDP
		if err := m.Decode(r); err != nil {
			return err
		}
		dir := sfu.Direction(m.Dir)
		g.Log.Debug("sdp in", "id", p.ID, "dir", dir.String(), "offer", m.IsOffer, "bytes", len(m.Body))
		if m.IsOffer {
			// Only the publisher connection is ever client-offered. An offer
			// on the subscriber connection means a confused or hostile
			// client, and answering it would create exactly the glare the
			// two-connection split exists to prevent.
			if dir != sfu.Publisher {
				g.Log.Warn("offer on the subscriber connection, ignored", "id", p.ID)
				return nil
			}
			if err := peer.HandleOffer(m.Body); err != nil {
				g.Log.Warn("publisher offer failed", "id", p.ID, "err", err)
			}
			return nil
		}
		if dir != sfu.Subscriber {
			g.Log.Warn("answer on the publisher connection, ignored", "id", p.ID)
			return nil
		}
		if err := peer.HandleAnswer(m.Body); err != nil {
			g.Log.Warn("subscriber answer failed", "id", p.ID, "err", err)
		}

	case wire.OpIce:
		var m wire.ICE
		if err := m.Decode(r); err != nil {
			return err
		}
		if err := peer.HandleICE(sfu.Direction(m.Dir), m.Candidate); err != nil {
			g.Log.Debug("ice candidate rejected", "id", p.ID, "err", err)
		}

	case wire.OpViewport:
		var m wire.Viewport
		if err := m.Decode(r); err != nil {
			return err
		}
		views := make(map[string]bool, len(m.Tiles))
		for _, t := range m.Tiles {
			if router != nil {
				router.SetViewport(p.ID, t.Track,
					int(t.Width), int(t.Height), t.Visible, t.Pinned)
			}
			views[t.Track] = views[t.Track] || t.Visible
		}
		// The same report names people for the socket path: their cameras
		// are sent only while on screen, and one that comes into view is
		// asked for a key frame to start from.
		for _, pub := range rm.SetSocketView(p.ID, views) {
			g.forwardKeyframeReq(rm, p.ID, pub)
		}

	case wire.OpKeyframeReq:
		track := r.Str()
		if err := r.Err(); err != nil {
			return err
		}
		if t, ok := router.Track(track); ok {
			t.RequestKeyframe(t.SubscribedLayer(p.ID))
		} else {
			// Not an SFU track: a socket-path publisher, named by participant
			// id. The request goes to that publisher's connection, once per
			// keyframeReqWindow however many watchers ask -- so a room where
			// everyone's decoder stumbled at once costs the sender one
			// keyframe, not one per watcher.
			g.forwardKeyframeReq(rm, p.ID, track)
		}

	case wire.OpChat:
		// Refused here rather than hidden in the client. A participant whose
		// chat has been turned off can still construct the frame; what stops
		// the message is that nothing dispatches it.
		if !rm.May(p.ID, "chat") || !rm.Allowed(p.ID, room.AllowChat, time.Now()) {
			return nil
		}
		body := r.Blob()
		if err := r.Err(); err != nil {
			return err
		}
		msg := append([]byte(nil), body...)
		g.announceAll(rm, func(w *wire.Writer) {
			w.WriteHeader(wire.OpChat, 0, false)
			w.Str(p.ID)
			w.Blob(msg)
		})

	case wire.OpReaction:
		// Gated on its own control, Teams' "Allow reactions".
		if !rm.May(p.ID, "reactions") {
			return nil
		}
		emoji := r.Str()
		if err := r.Err(); err != nil {
			return err
		}
		// Two code points is a flag or a keycap; more than that is somebody
		// pasting a paragraph into the reaction field.
		if len([]rune(emoji)) > 8 {
			return nil
		}
		if !rm.Allowed(p.ID, room.AllowReaction, time.Now()) {
			return nil // over the allowance: dropped, silently, like the browser's own throttle
		}
		g.announceAll(rm, func(w *wire.Writer) {
			w.WriteHeader(wire.OpReaction, 0, false)
			w.Str(p.ID)
			w.Str(emoji)
		})

	default:
		// Unknown opcodes are ignored, not fatal. A newer client talking to
		// an older server must degrade, not disconnect.
		g.Log.Debug("unknown opcode", "op", h.Op, "id", p.ID)
	}
	return r.Err()
}

// admit holds a new arrival in the lobby until a host lets them in, and
// reports whether they may proceed into the meeting.
//
// Returns false for every outcome that is not admission -- denied, expired,
// locked, disconnected -- having already told the client why and closed the
// connection where appropriate.
func (g *Gateway) admit(ctx context.Context, c *Conn, rm *room.Room,
	roomID, name string) bool {

	// Authenticated means they signed in and the server verified it, not that
	// they claimed to be somebody. RequireAuth has already run by this point,
	// so reaching here at all implies a valid session; the distinction that
	// remains is between a member of this deployment and a guest, which is
	// what the trusted policy turns on.
	authenticated := c.UserID != ""

	if parent := room.BreakoutParent(roomID); parent != "" {
		if g.admitBreakout(c, roomID, parent) {
			return true
		}
		g.notifyLobby(c, "refused", "The room isn't open yet, or you might have been assigned to a different one.")
		c.CloseWithReason(websocket.StatusPolicyViolation, "not admitted")
		return false
	}
	if c.Ticket != "" && g.useTicket(c.Ticket, roomID, c.UserID) {
		return true // coming back from a breakout room: let in once already
	}

	// The host walks back into their own locked meeting. A lock is a door
	// the host closed, and a door that also locks out the person holding the
	// key is not a lock, it is a trap: the host reloads, the room forgets
	// them, and the meeting is sealed with everybody still inside and nobody
	// able to reopen it. Checked before the lock, and also before the waiting
	// room, because a host who has to knock on their own door has nobody to
	// answer it.
	if rm.IsHostAccount(c.UserID) {
		return true
	}

	if rm.Locked() {
		g.notifyLobby(c, "locked", "This meeting has been locked by the host.")
		c.CloseWithReason(websocket.StatusPolicyViolation, "meeting locked")
		return false
	}

	// An invitation IS the permission. The organiser already decided, at the
	// moment they were actually thinking about it, who was coming; making
	// them decide again at the door -- while the invitee waits, and while the
	// organiser may not even have arrived yet -- adds no security and loses
	// people. The waiting room is for whoever has the link but was never
	// asked, which is exactly who it was invented for.
	//
	// After the lock, deliberately: a lock is a door the host closed just
	// now, and it outranks a list written last week.
	if g.Invited != nil && authenticated &&
		g.Invited(ctx, roomID, c.UserID, c.UserEmail) {
		g.Log.Info("invitee admitted without waiting",
			"room", roomID, "id", c.ParticipantID)
		return true
	}

	if !rm.MustWait(authenticated) {
		return true
	}

	w := &room.Waiter{
		ID:            c.ParticipantID,
		DisplayName:   name,
		Authenticated: authenticated,
		UserID:        c.UserID,
	}
	if err := rm.Knock(w); err != nil {
		reason := "Nobody can be admitted right now."
		if errors.Is(err, room.ErrLocked) {
			reason = "This meeting has been locked by the host."
		}
		g.notifyLobby(c, "refused", reason)
		c.CloseWithReason(websocket.StatusPolicyViolation, "not admitted")
		return false
	}

	g.notifyLobby(c, "waiting", "Waiting for someone to let you in.")
	g.pushLobbyList(rm)

	state := w.Wait(ctx.Done(), room.LobbyTimeout)
	// However this ended, the host's list must stop showing them.
	rm.Withdraw(c.ParticipantID)
	g.pushLobbyList(rm)

	switch state {
	case room.WaitAdmitted:
		g.notifyLobby(c, "admitted", "")
		g.Log.Info("participant admitted", "room", roomID, "id", c.ParticipantID)
		return true
	case room.WaitExpired:
		g.notifyLobby(c, "expired", "Nobody let you in. Try again later.")
		c.CloseWithReason(websocket.StatusPolicyViolation, "not admitted")
	case room.WaitEnded:
		g.notifyLobby(c, "ended", "The host ended the meeting.")
		c.CloseWithReason(websocket.StatusNormalClosure, "meeting ended")
	default:
		g.notifyLobby(c, "denied", "You were not admitted to this meeting.")
		c.CloseWithReason(websocket.StatusPolicyViolation, "not admitted")
	}
	return false
}

// notifyLobby tells one waiter where their knock stands.
func (g *Gateway) notifyLobby(c *Conn, status, message string) {
	c.EmitNow(func(w *wire.Writer) {
		w.WriteHeader(wire.OpLobbyStatus, 0, false)
		w.Str(status)
		w.Str(message)
	})
}

// pushLobbyList tells the hosts who is waiting.
//
// Only hosts. The list names people who are not in the meeting, and showing
// it to everybody would leak who tried to join and failed -- which is exactly
// the information a waiting room exists to keep from the room.
func (g *Gateway) pushLobbyList(rm *room.Room) {
	waiting := rm.Waiting()
	for _, q := range rm.Snapshot() {
		if !rm.IsHost(q.ID) {
			continue
		}
		qc := g.connFor(q.ID)
		if qc == nil {
			continue
		}
		qc.Emit(func(w *wire.Writer) {
			w.WriteHeader(wire.OpLobbyList, 0, false)
			w.Uvarint(uint64(len(waiting)))
			for _, x := range waiting {
				w.Str(x.ID)
				w.Str(x.DisplayName)
				w.Bool(x.Authenticated)
				w.Bits(0, 7)
			}
		})
	}
}

// broadcastPermissions tells the room what attendees may currently do.
//
// Everybody, not just the host: each client needs it to grey out the controls
// it is no longer allowed to use. Greying out is all the client does with it,
// because the answer that matters is the server's.
func (g *Gateway) broadcastPermissions(rm *room.Room) {
	perms := rm.Permissions()
	g.announceAll(rm, func(w *wire.Writer) {
		w.WriteHeader(wire.OpPermissions, 0, false)
		w.Bool(perms.Mic)
		w.Bool(perms.Camera)
		w.Bool(perms.Share)
		w.Bool(perms.Chat)
		w.Bool(!perms.Reactions)
		w.Bits(0, 3)
	})
}

// announce encodes the frame separately for each recipient so that every
// connection can use its own interning dictionary.
// adviseSameDevice tells the newcomer, and everyone already here from the
// same address, that they share a machine: one frame each, once, at join.
// The echo this warns of cannot be fixed by any server; the person can fix
// it in a second by muting one microphone or wearing headphones.
func (g *Gateway) adviseSameDevice(rm *room.Room, p *room.Participant) {
	n := rm.SameAddr(p.Addr, p.ID)
	if n == 0 {
		return
	}
	tell := func(id string, others int) {
		if qc := g.connFor(id); qc != nil {
			qc.Emit(func(w *wire.Writer) {
				w.WriteHeader(wire.OpSameDevice, 0, false)
				w.Uvarint(uint64(others))
			})
		}
	}
	tell(p.ID, n)
	for _, q := range rm.Snapshot() {
		if q.ID != p.ID && q.Addr == p.Addr {
			tell(q.ID, rm.SameAddr(q.Addr, q.ID))
		}
	}
}

// keyframeReqWindow bounds how often a socket publisher is asked for a
// keyframe, whoever asks.
const keyframeReqWindow = 300 * time.Millisecond

func (g *Gateway) forwardKeyframeReq(rm *room.Room, from, publisher string) {
	if publisher == "" || publisher == from {
		return
	}
	inRoom := false
	for _, q := range rm.Snapshot() {
		if q.ID == publisher {
			inRoom = true
			break
		}
	}
	if !inRoom {
		return
	}
	g.mu.Lock()
	if g.lastKeyReq == nil {
		g.lastKeyReq = make(map[string]time.Time)
	}
	now := time.Now()
	if last, ok := g.lastKeyReq[publisher]; ok && now.Sub(last) < keyframeReqWindow {
		g.mu.Unlock()
		return
	}
	g.lastKeyReq[publisher] = now
	g.mu.Unlock()
	if qc := g.connFor(publisher); qc != nil {
		qc.Emit(func(w *wire.Writer) {
			w.WriteHeader(wire.OpKeyframeReq, 0, false)
			w.Str(publisher)
		})
	}
}

func (g *Gateway) announce(rm *room.Room, exclude string, enc func(*wire.Writer)) {
	for _, q := range rm.Snapshot() {
		if q.ID == exclude || q.Send == nil {
			continue
		}
		if qc := g.connFor(q.ID); qc != nil {
			qc.Emit(enc)
		}
	}
}

// claimShare makes id the room's presenter. If somebody else was, everyone
// is told who stopped and who took over (OpScreenShare {stopped, by}) -- the
// replaced presenter's client stops its capture on that -- and their screen
// stops being forwarded now, not when their client gets round to it: May
// already refuses "share" to anyone but the presenter, and ApplyPermissions
// unsubscribes what is no longer allowed. The replaced track keeps its
// screen marking, which is what keeps it refused.
func (g *Gateway) claimShare(rm *room.Room, router *sfu.Router, id string) {
	replaced := rm.ClaimPresenter(id)
	if replaced == "" {
		return
	}
	g.Log.Info("presenter taken over", "room", rm.ID, "from", replaced, "to", id)
	g.announceAll(rm, func(w *wire.Writer) {
		w.WriteHeader(wire.OpScreenShare, 0, false)
		w.Str(replaced)
		w.Str(id)
	})
	if router != nil {
		router.ApplyPermissions()
	}
}

// encodeSpotlights writes the whole spotlight list (wire.OpSpotlight).
func encodeSpotlights(ids []string) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpSpotlight, 0, false)
		w.Uvarint(uint64(len(ids)))
		for _, id := range ids {
			w.Str(id)
		}
	}
}

func encodeBlock(b room.Blocked) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpBlock, 0, false)
		w.Str(b.ID)
		w.Bool(b.Mic)
		w.Bool(b.Cam)
		w.Bits(0, 6)
	}
}

// PushQnA sends a Q&A change to the people in a room's meeting: everyone, or,
// when restricted, its hosts and the one author (scheduling/qna.go).
func (g *Gateway) PushQnA(roomID string, body []byte, restricted bool, authorID string) {
	g.pushJSON(roomID, wire.OpQnA, body, restricted, authorID)
}

// PushNotes sends a meeting-notes change to everyone in a room's meeting.
func (g *Gateway) PushNotes(roomID string, body []byte) {
	g.pushJSON(roomID, wire.OpNotes, body, false, "")
}

// pushJSON: one change, as a blob, to the people in a room's meeting.
func (g *Gateway) pushJSON(roomID string, op wire.Op, body []byte, restricted bool, authorID string) {
	rm, ok := g.Rooms.Get(roomID)
	if !ok {
		return
	}
	b := append([]byte(nil), body...)
	for _, p := range rm.Snapshot() {
		if restricted && !rm.IsHost(p.ID) && (authorID == "" || p.UserID != authorID) {
			continue
		}
		if q := g.connFor(p.ID); q != nil {
			q.Emit(func(w *wire.Writer) {
				w.WriteHeader(op, 0, false)
				w.Blob(b)
			})
		}
	}
}

func (g *Gateway) broadcastRole(rm *room.Room, id string, role wire.Role) {
	g.announceAll(rm, func(w *wire.Writer) {
		w.WriteHeader(wire.OpSetRole, 0, false)
		w.Str(id)
		w.Bits(uint32(role), 2)
		w.Bits(b2u(id == rm.HostID()), 1) // organizer, as in the roster
		w.Bits(0, 5)
	})
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func (g *Gateway) broadcastSpotlights(rm *room.Room) {
	g.announceAll(rm, encodeSpotlights(rm.Spotlights()))
}

func (g *Gateway) announceAll(rm *room.Room, enc func(*wire.Writer)) {
	g.announce(rm, "", enc)
}

func (g *Gateway) connFor(id string) *Conn {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byID[id]
}

// Connections reports the live connection count.
func (g *Gateway) Connections() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.conns)
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
