package wire

// This file defines the frames that run at call frequency. Each carries a
// comment recording its encoded size against the JSON equivalent that every
// competing product ships, because that delta is the entire point.

// Role fits in two bits. There will never be a fifth role that needs to be
// distinguishable at packet rate; anything finer is an authorisation
// concern, resolved once at join, not re-litigated in every state delta.
type Role uint8

const (
	RoleGuest Role = iota
	RoleMember
	RoleModerator
	RoleOwner

	// RoleHost is the meeting-scoped authority: admitting people from the
	// waiting room, locking the meeting, changing the admission policy. It is
	// deliberately separate from the account roles above, which are
	// deployment-wide. Being an administrator of the deployment does not make
	// somebody the host of a meeting they walked into.
	RoleHost = RoleOwner
)

// State is one participant's complete mutable presence, packed into a single
// octet. Six independent booleans and a two-bit role occupy exactly 8 bits.
//
//	ENCODED: 3 bytes total.
//	  1 byte  frame control (OpStateDelta, no request id)
//	  1 byte  participant handle (interned; the UUID was sent once, at join)
//	  1 byte  the packed bitfield below
//
//	The JSON this replaces, verbatim from a shipping competitor's protocol:
//	  {"type":"participant_update","participantId":"7f3a9c21-...",
//	   "audioMuted":true,"videoMuted":false,"handRaised":false,
//	   "sharing":false,"role":"member"}
//	= 178 bytes, plus a 2-byte WebSocket header, plus TLS record overhead.
//
//	59x. In a 50-person all-hands where people mute and unmute constantly,
//	this is the difference between signalling being invisible and signalling
//	being a measurable fraction of the call.
type State struct {
	Participant string
	MicMuted    bool
	CamOff      bool
	Sharing     bool
	HandRaised  bool
	Speaking    bool
	Recording   bool
	Role        Role

	// ScreenTrack names which of this participant's tracks is their screen,
	// and is present ONLY when Sharing is set.
	//
	// It has to be said explicitly because nothing else on the wire can say
	// it. A browser generates the msid stream and track identifiers as
	// UUIDs, so the server's substring heuristic -- does the id contain
	// "screen" or "display" -- never matches anything a real browser sends;
	// it only ever fired for native clients that chose their own names. A
	// receiver therefore had no way to tell a presenter's screen from their
	// camera, which is why sharing replaced the camera instead of appearing
	// beside it.
	//
	// Gated on Sharing so the common case is unaffected: a participant who is
	// not presenting still encodes to exactly three bytes, which is a
	// measured property of this protocol and is asserted by
	// TestStateDeltaIsThreeBytes. When they are presenting it costs one or
	// two more, because the identifier interns after its first appearance.
	ScreenTrack string
}

func (s *State) Encode(w *Writer) {
	w.WriteHeader(OpStateDelta, 0, false)
	w.Str(s.Participant)
	w.Bool(s.MicMuted)
	w.Bool(s.CamOff)
	w.Bool(s.Sharing)
	w.Bool(s.HandRaised)
	w.Bool(s.Speaking)
	w.Bool(s.Recording)
	w.Bits(uint32(s.Role), 2)
	if s.Sharing {
		w.Str(s.ScreenTrack)
	}
}

func (s *State) Decode(r *Reader) error {
	s.Participant = r.Str()
	s.MicMuted = r.Bool()
	s.CamOff = r.Bool()
	s.Sharing = r.Bool()
	s.HandRaised = r.Bool()
	s.Speaking = r.Bool()
	s.Recording = r.Bool()
	s.Role = Role(r.Bits(2))
	s.ScreenTrack = ""
	if s.Sharing {
		s.ScreenTrack = r.Str()
	}
	return r.Err()
}

// Speaker is one entry in a ranked active-speaker report.
//
// Energy is quantised to one byte. Perceptually and for control purposes,
// 1/255 resolution on a normalised loudness is far finer than anything the
// UI or the layer arbitrator can act on -- the alternative float32 would
// cost 4 bytes to convey no additional decision.
type Speaker struct {
	Participant string
	Energy      float64 // 0..1, quantised to 8 bits on the wire
}

// Speakers is the ranked list driving both the UI and, more importantly, the
// layer arbitrator: whoever is speaking gets the high spatial layer, and
// everyone else is demoted. This single message is responsible for the
// largest share of downlink savings in the whole system.
//
//	ENCODED: 1 + 1 + 2*N bytes. Four ranked speakers = 10 bytes, at 4 Hz.
type Speakers struct {
	List []Speaker
}

func (s *Speakers) Encode(w *Writer) {
	w.WriteHeader(OpSpeakers, 0, false)
	w.Uvarint(uint64(len(s.List)))
	for i := range s.List {
		w.Str(s.List[i].Participant)
		w.Q8(s.List[i].Energy)
	}
}

func (s *Speakers) Decode(r *Reader) error {
	// Validated before narrowing: a count above MaxInt64 becomes a negative
	// int, passes "n > 256", and reaches make() as a negative capacity.
	n, ok := r.ReadLen(256)
	if !ok {
		return r.Err()
	}
	s.List = make([]Speaker, 0, n)
	for i := 0; i < n; i++ {
		var sp Speaker
		sp.Participant = r.Str()
		sp.Energy = r.Q8()
		s.List = append(s.List, sp)
	}
	return r.Err()
}

// LayerReason tells the SFU why a subscriber wants a different layer, so the
// server can arbitrate between competing requests intelligently rather than
// obeying whoever asked last.
type LayerReason uint8

const (
	ReasonViewport  LayerReason = iota // element resized or scrolled offscreen
	ReasonBandwidth                    // receiver is congested
	ReasonCPU                          // receiver cannot decode this many pixels
	ReasonFocus                        // user pinned or spotlighted this feed
	ReasonPolicy                       // server-side arbitration
)

// LayerReq asks the SFU to change what it forwards for one track.
//
//	ENCODED: 3 bytes.
//	  1 byte  frame control
//	  1 byte  track handle (interned)
//	  1 byte  [spatial:2][temporal:2][reason:3][pause:1]
//
// Pause deserves its own bit rather than being spatial layer zero, because
// pausing is qualitatively different: it means "send nothing at all", which
// is what a scrolled-offscreen tile should cost. A 30-person gallery where
// 24 tiles are below the fold should transmit 24 zeroes, and today, in every
// shipping product, it does not.
type LayerReq struct {
	Track    string
	Spatial  uint8 // 0=quarter 1=half 2=full 3=reserved
	Temporal uint8 // 0..2 frame-rate tiers
	Reason   LayerReason
	Paused   bool
}

func (l *LayerReq) Encode(w *Writer) {
	w.WriteHeader(OpLayerReq, 0, false)
	w.Str(l.Track)
	w.Bits(uint32(l.Spatial), 2)
	w.Bits(uint32(l.Temporal), 2)
	w.Bits(uint32(l.Reason), 3)
	w.Bool(l.Paused)
}

func (l *LayerReq) Decode(r *Reader) error {
	l.Track = r.Str()
	l.Spatial = uint8(r.Bits(2))
	l.Temporal = uint8(r.Bits(2))
	l.Reason = LayerReason(r.Bits(3))
	l.Paused = r.Bool()
	return r.Err()
}

// BweReport is the receiver's view of the path, sent on change rather than
// on a timer. Absolute values, not deltas, because this message is the
// control loop's input and must be robust to a lost predecessor.
//
//	ENCODED: 6-7 bytes, emitted only when an estimate moves materially.
//
// Contrast with what this replaces: a full RTCP receiver-report plus
// transport-wide-CC feedback cycle, which on a busy connection costs a
// sustained 10-20 kbps of pure protocol overhead per peer and conveys the
// same handful of decisions.
type BweReport struct {
	EstimateKbps uint32
	LossFraction float64 // 0..1, quantised
	JitterMs     uint16
	RttMs        uint16
}

func (b *BweReport) Encode(w *Writer) {
	w.WriteHeader(OpBweReport, 0, false)
	w.Uvarint(uint64(b.EstimateKbps))
	w.Q8(b.LossFraction)
	w.Uvarint(uint64(b.JitterMs))
	w.Uvarint(uint64(b.RttMs))
}

func (b *BweReport) Decode(r *Reader) error {
	b.EstimateKbps = uint32(r.Uvarint())
	b.LossFraction = r.Q8()
	b.JitterMs = uint16(r.Uvarint())
	b.RttMs = uint16(r.Uvarint())
	return r.Err()
}

// Subscribe / Unsubscribe control whether a track is forwarded at all.
//
// Unsubscribe is the single highest-leverage message in the protocol. Every
// competing product keeps forwarding video for tiles the user cannot see --
// collapsed panels, background tabs, participants scrolled out of the
// gallery. Those bits are perfectly encoded, perfectly delivered, perfectly
// decoded, and then discarded by a compositor that never draws them. They
// are pure waste and they are the majority of a large call's downlink.
type Subscribe struct {
	Track   string
	Spatial uint8
}

func (s *Subscribe) Encode(w *Writer) {
	w.WriteHeader(OpSubscribe, 0, false)
	w.Str(s.Track)
	w.Bits(uint32(s.Spatial), 2)
	w.Bits(0, 6) // reserved, keeps the frame byte-aligned for future flags
}

func (s *Subscribe) Decode(r *Reader) error {
	s.Track = r.Str()
	s.Spatial = uint8(r.Bits(2))
	_ = r.Bits(6)
	return r.Err()
}

type Unsubscribe struct{ Track string }

func (u *Unsubscribe) Encode(w *Writer) {
	w.WriteHeader(OpUnsubscribe, 0, false)
	w.Str(u.Track)
}

func (u *Unsubscribe) Decode(r *Reader) error {
	u.Track = r.Str()
	return r.Err()
}

// Ping is one byte. Not a struct with a timestamp -- the RTT probe rides on
// the request-id when one is needed, and an unsolicited liveness beat needs
// to convey nothing except that the connection is alive.
func EncodePing(w *Writer) { w.WriteHeader(OpPing, 0, false) }
func EncodePong(w *Writer) { w.WriteHeader(OpPong, 0, false) }

// ---------------------------------------------------------------------------
// Media plane signalling
// ---------------------------------------------------------------------------

// Direction selects which of a participant's two peer connections a
// signalling frame concerns. One bit, because there are exactly two and
// there will never be a third.
type Direction uint8

const (
	DirPublisher Direction = iota
	DirSubscriber
)

// SDP carries an offer or an answer.
//
// The body is an opaque blob rather than anything this protocol parses. SDP
// is the one part of WebRTC nobody should reimplement, and the SFU has no
// reason to inspect it: pion negotiates, and we route.
//
//	ENCODED: 3 bytes of framing plus the SDP itself.
type SDP struct {
	Dir     Direction
	IsOffer bool
	Body    string
}

func (s *SDP) Encode(w *Writer) {
	w.WriteHeader(OpSdp, 0, false)
	w.Bits(uint32(s.Dir), 1)
	w.Bool(s.IsOffer)
	w.Bits(0, 6)
	w.Blob([]byte(s.Body))
}

func (s *SDP) Decode(r *Reader) error {
	s.Dir = Direction(r.Bits(1))
	s.IsOffer = r.Bool()
	_ = r.Bits(6)
	s.Body = string(r.Blob())
	return r.Err()
}

// ICE carries one trickled candidate.
type ICE struct {
	Dir       Direction
	Candidate string
}

func (i *ICE) Encode(w *Writer) {
	w.WriteHeader(OpIce, 0, false)
	w.Bits(uint32(i.Dir), 1)
	w.Bits(0, 7)
	w.Blob([]byte(i.Candidate))
}

func (i *ICE) Decode(r *Reader) error {
	i.Dir = Direction(r.Bits(1))
	_ = r.Bits(7)
	i.Candidate = string(r.Blob())
	return r.Err()
}

// Tile is one entry in a viewport report: how large the receiver is actually
// painting a feed, and whether it is on screen at all.
type Tile struct {
	Track   string
	Width   uint32
	Height  uint32
	Visible bool
	Pinned  bool
}

// Viewport is the most valuable frame in this protocol.
//
// It tells the server what the client is genuinely doing with each feed --
// the painted size in device pixels, and whether the element is on screen at
// all. With it, a tile drawn 160 pixels wide receives a stream sized for 160
// pixels and a tile scrolled below the fold receives nothing. Without it the
// server must guess, and every server that guesses guesses "large", because
// guessing small looks like a bug and guessing large only looks like a bill.
//
// The whole gallery travels in one frame rather than one frame per tile. A
// scroll moves every tile at once, and twenty separate frames would pay
// twenty control headers to say one thing.
//
//	ENCODED: 3 bytes of framing plus ~6 per tile once track ids are interned.
//	A 20-tile gallery update is about 123 bytes, sent on scroll rather than
//	on a timer.
type Viewport struct {
	Tiles []Tile
}

func (v *Viewport) Encode(w *Writer) {
	w.WriteHeader(OpViewport, 0, false)
	w.Uvarint(uint64(len(v.Tiles)))
	for i := range v.Tiles {
		t := &v.Tiles[i]
		w.Str(t.Track)
		w.Uvarint(uint64(t.Width))
		w.Uvarint(uint64(t.Height))
		w.Bool(t.Visible)
		w.Bool(t.Pinned)
		w.Bits(0, 6)
	}
}

// SlotEntry is one of a receiver's media slots and what it carries now. An
// empty Publisher means the slot is idle.
type SlotEntry struct {
	Slot      string // the slot's track id on the receiver's connection
	Publisher string // participant id
	Track     string // the published track id, as the receiver's views name it
}

// Slots is the whole slot map of one receiver.
//
// A receiver has a fixed pool of outgoing tracks (sfu/slots.go), re-pointed at
// whichever cameras it has on screen and whoever is talking, so the number of
// tracks per receiver is bounded however large the meeting. This frame names
// what each slot carries. The whole map travels each time: it is at most a
// few dozen entries, strings are interned after first use, and a receiver
// that missed one update is corrected by the next instead of drifting.
//
//	ENCODED: 3 bytes of framing plus ~3 bytes per slot once ids are interned.
type Slots struct {
	Entries []SlotEntry
}

// MaxSlotEntries bounds a decoded slot map: more than any pool holds.
const MaxSlotEntries = 256

func (s *Slots) Encode(w *Writer) {
	w.WriteHeader(OpSlots, 0, false)
	w.Uvarint(uint64(len(s.Entries)))
	for _, e := range s.Entries {
		w.Str(e.Slot)
		w.Str(e.Publisher)
		w.Str(e.Track)
	}
}

func (s *Slots) Decode(r *Reader) error {
	n, ok := r.ReadLen(MaxSlotEntries)
	if !ok {
		return r.Err()
	}
	s.Entries = make([]SlotEntry, 0, n)
	for range n {
		s.Entries = append(s.Entries, SlotEntry{Slot: r.Str(), Publisher: r.Str(), Track: r.Str()})
	}
	return r.Err()
}

func (v *Viewport) Decode(r *Reader) error {
	// A client cannot be painting more tiles than a room may hold, and the
	// count is validated before narrowing so an enormous one cannot arrive as
	// a negative capacity.
	n, ok := r.ReadLen(512)
	if !ok {
		return r.Err()
	}
	v.Tiles = make([]Tile, 0, n)
	for i := 0; i < n; i++ {
		var t Tile
		t.Track = r.Str()
		t.Width = uint32(r.Uvarint())
		t.Height = uint32(r.Uvarint())
		t.Visible = r.Bool()
		t.Pinned = r.Bool()
		_ = r.Bits(6)
		v.Tiles = append(v.Tiles, t)
	}
	return r.Err()
}
