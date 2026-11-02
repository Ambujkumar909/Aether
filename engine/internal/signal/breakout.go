package signal

import (
	"crypto/rand"
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// Breakout rooms on the wire (OpBreakout 0x104), and the tickets that decide
// who may enter one. The rooms themselves are room.Breakouts.
//
// Moving somebody is the client leaving one room and joining another, as a
// reload would -- the honest teardown of two peer connections (leaveMeeting)
// -- carrying a ticket the server minted for exactly that room and person.
// A breakout room admits a ticket or a manager and nobody else; the main
// meeting admits a return ticket past its lobby, because the person was let
// in once already.

// Kinds: the first three bits of every OpBreakout frame.
const (
	brSet      = 0 // host -> server: the rooms and settings
	brState    = 0 // server -> managers: the rooms, settings, who is where
	brOpen     = 1 // host -> server
	brMove     = 1 // server -> one person: go to this room
	brClose    = 2 // host -> server
	brReturn   = 2 // server -> one person: go back to the main meeting
	brAnnounce = 3 // host -> server, and server -> everyone in the rooms
	brInfo     = 4 // server -> someone entering a breakout room: its name and clock
	brDelete   = 5 // host -> server
)

const (
	moveTicketTTL   = 2 * time.Minute
	returnTicketTTL = 30 * time.Minute
	// Seconds the client shows "We'll move you back to the main meeting in
	// N seconds." Teams waits `breakoutRoomTimerExpireScreenAutoJoinDuration
	// = 30000` ms after the timer expires; a host's "Close Rooms" uses the
	// shorter move announcement (10 s, Microsoft's published behaviour; the
	// package fills that string at run time).
	returnDelayClosed  = 10
	returnDelayExpired = 30
	maxTickets         = 100000
	maxTicketLen       = 64
)

type ticket struct {
	room, user string
	exp        time.Time
	multi      bool
}

func randomToken() string {
	var b [18]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// mintTicket issues a ticket for room, bound to an account when there is one
// (a guest's ticket is the bearer's). Expired tickets are swept on the way,
// and the store is bounded.
func (g *Gateway) mintTicket(roomID, user string, ttl time.Duration, multi bool) string {
	tok := randomToken()
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tickets == nil {
		g.tickets = make(map[string]ticket)
	}
	if len(g.tickets) >= maxTickets {
		for k, t := range g.tickets {
			if now.After(t.exp) {
				delete(g.tickets, k)
			}
		}
		if len(g.tickets) >= maxTickets {
			return "" // full of live tickets: nobody is moved rather than memory growing
		}
	}
	g.tickets[tok] = ticket{room: roomID, user: user, exp: now.Add(ttl), multi: multi}
	return tok
}

// useTicket reports whether tok admits user to room, and spends it if it is
// single-use. A ticket bound to an account admits only that account.
func (g *Gateway) useTicket(tok, roomID, user string) bool {
	if tok == "" || len(tok) > maxTicketLen {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tickets[tok]
	if !ok {
		return false
	}
	if time.Now().After(t.exp) {
		delete(g.tickets, tok)
		return false
	}
	if t.room != roomID || (t.user != "" && t.user != user) {
		return false
	}
	if !t.multi {
		delete(g.tickets, tok)
	}
	return true
}

// admitBreakout decides entry to a breakout room: a ticket for it, or a
// manager of its meeting while the rooms are open. Nothing else -- not the
// lobby policy, not an invitation.
func (g *Gateway) admitBreakout(c *Conn, roomID, parent string) bool {
	if g.useTicket(c.Ticket, roomID, c.UserID) {
		return true
	}
	prm, ok := g.Rooms.Get(parent)
	if !ok || c.UserID == "" {
		return false
	}
	st, ok := prm.BreakoutState()
	if !ok || !st.Open || !st.Managers[c.UserID] {
		return false
	}
	_, exists := prm.BreakoutRoomByID(roomID)
	return exists
}

// managersOf is every account hosting the meeting now: the organizer and the
// co-organizers present.
func managersOf(rm *room.Room) map[string]bool {
	out := map[string]bool{}
	host := rm.HostID()
	for _, p := range rm.Snapshot() {
		if p.UserID != "" && (p.ID == host || p.Role >= wire.RoleHost) {
			out[p.UserID] = true
		}
	}
	return out
}

func (g *Gateway) handleBreakout(rm *room.Room, p *room.Participant, r *wire.Reader) error {
	kind := r.Bits(3)
	switch kind {
	case brSet:
		autoMove := r.Bool()
		allowReturn := r.Bool()
		r.Bits(3)
		limit := r.Uvarint()
		n := r.Uvarint()
		if r.Err() != nil {
			return r.Err()
		}
		// Bounded before anything is allocated from the count.
		if n < 1 || n > room.MaxBreakoutRooms {
			return nil
		}
		cfg := room.BreakoutConfig{AutoMove: autoMove, AllowReturn: allowReturn, TimeLimit: time.Duration(limit) * time.Second}
		for i := uint64(0); i < n; i++ {
			cfg.Names = append(cfg.Names, r.Str())
			m := r.Uvarint()
			if m > room.MaxParticipants || r.Err() != nil {
				return r.Err()
			}
			ids := make([]string, 0, m)
			for j := uint64(0); j < m; j++ {
				ids = append(ids, r.Str())
			}
			cfg.Members = append(cfg.Members, ids)
		}
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			g.Log.Warn("non-host tried to set breakout rooms", "id", p.ID)
			return nil
		}
		if err := rm.SetBreakouts(cfg, randomToken()[:10]); err != nil {
			g.Log.Info("breakout rooms refused", "id", p.ID, "err", err)
			return nil
		}
		g.broadcastBreakoutState(rm)

	case brOpen:
		r.Bits(5)
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) {
			return nil
		}
		st, err := rm.OpenBreakouts(time.Now(), managersOf(rm))
		if err != nil {
			return nil
		}
		g.Log.Info("breakout rooms opened", "room", rm.ID, "rooms", len(st.Rooms))
		for _, br := range st.Rooms {
			for _, m := range br.Members {
				g.moveTo(rm, st, br, m.ID)
			}
		}
		if st.TimeLimit > 0 {
			g.mu.Lock()
			if g.brTimers == nil {
				g.brTimers = make(map[string]*time.Timer)
			}
			if t := g.brTimers[rm.ID]; t != nil {
				t.Stop()
			}
			g.brTimers[rm.ID] = time.AfterFunc(st.TimeLimit, func() { g.closeBreakouts(rm, true) })
			g.mu.Unlock()
		}
		g.broadcastBreakoutState(rm)

	case brClose:
		r.Bits(5)
		if err := r.Err(); err != nil {
			return err
		}
		if rm.IsHost(p.ID) {
			g.closeBreakouts(rm, false)
		}

	case brDelete:
		r.Bits(5)
		if err := r.Err(); err != nil {
			return err
		}
		if rm.IsHost(p.ID) && rm.DeleteBreakouts() == nil {
			g.broadcastBreakoutState(rm)
		}

	case brAnnounce:
		r.Bits(5)
		text := r.Blob()
		if err := r.Err(); err != nil {
			return err
		}
		if !rm.IsHost(p.ID) || !utf8.Valid(text) || len(text) == 0 || utf8.RuneCount(text) > room.MaxAnnouncementLen {
			return nil
		}
		st, ok := rm.BreakoutState()
		if !ok || !st.Open {
			return nil
		}
		body := append([]byte(nil), text...)
		for _, br := range st.Rooms {
			crm, ok := g.Rooms.Get(br.ID)
			if !ok {
				continue
			}
			g.announceAll(crm, func(w *wire.Writer) {
				w.WriteHeader(wire.OpBreakout, 0, false)
				w.Bits(brAnnounce, 3)
				w.Bits(0, 5)
				w.Str(p.DisplayName)
				w.Blob(body)
			})
		}
	}
	return nil
}

// moveTo sends one person in the main meeting to their room, with a ticket
// for it and one to come back.
func (g *Gateway) moveTo(rm *room.Room, st room.Breakouts, br room.BreakoutRoom, pid string) {
	q := g.connFor(pid)
	if q == nil {
		return // left since being assigned
	}
	in := g.mintTicket(br.ID, q.UserID, moveTicketTTL, false)
	back := g.mintTicket(rm.ID, q.UserID, returnTicketTTL+st.TimeLimit, true)
	if in == "" || back == "" {
		return
	}
	closes := uint64(0)
	if !st.ClosesAt.IsZero() {
		closes = uint64(st.ClosesAt.Unix())
	}
	q.EmitNow(func(w *wire.Writer) {
		w.WriteHeader(wire.OpBreakout, 0, false)
		w.Bits(brMove, 3)
		w.Bool(!st.AutoMove) // prompt: "Join breakout room?"
		w.Bool(st.AllowReturn)
		w.Bits(0, 3)
		w.Str(br.ID)
		w.Str(br.Name)
		w.Blob([]byte(in))
		w.Blob([]byte(back))
		w.Str(rm.ID)
		w.Uvarint(closes)
	})
}

// closeBreakouts brings everyone in the rooms back: "Your breakout room is
// closing." By a host, or by the timer ("The breakout room timer has
// expired.").
func (g *Gateway) closeBreakouts(rm *room.Room, expired bool) {
	st, ok := rm.CloseBreakouts()
	g.mu.Lock()
	if t := g.brTimers[rm.ID]; t != nil {
		t.Stop()
		delete(g.brTimers, rm.ID)
	}
	g.mu.Unlock()
	if !ok {
		return
	}
	delay := uint64(returnDelayClosed)
	if expired {
		delay = returnDelayExpired
	}
	g.Log.Info("breakout rooms closed", "room", rm.ID, "expired", expired)
	for _, br := range st.Rooms {
		crm, ok := g.Rooms.Get(br.ID)
		if !ok {
			continue
		}
		for _, q := range crm.Snapshot() {
			qc := g.connFor(q.ID)
			if qc == nil {
				continue
			}
			back := g.mintTicket(rm.ID, q.UserID, returnTicketTTL, true)
			qc.EmitNow(func(w *wire.Writer) {
				w.WriteHeader(wire.OpBreakout, 0, false)
				w.Bits(brReturn, 3)
				w.Bool(expired)
				w.Bits(0, 4)
				w.Str(rm.ID)
				w.Blob([]byte(back))
				w.Uvarint(delay)
			})
		}
	}
	g.broadcastBreakoutState(rm)
}

// endBreakouts ends every breakout room with the meeting ("You'll end the
// meeting for everyone" includes the people in the rooms).
func (g *Gateway) endBreakouts(rm *room.Room) {
	st, ok := rm.BreakoutState()
	if !ok {
		return
	}
	g.mu.Lock()
	if t := g.brTimers[rm.ID]; t != nil {
		t.Stop()
		delete(g.brTimers, rm.ID)
	}
	g.mu.Unlock()
	for _, br := range st.Rooms {
		crm, ok := g.Rooms.Get(br.ID)
		if !ok {
			continue
		}
		for _, id := range crm.End() {
			if q := g.connFor(id); q != nil {
				q.EmitNow(func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
				q.CloseWithReason(websocket.StatusNormalClosure, "meeting ended")
			}
		}
	}
}

// breakoutInfo tells someone entering a breakout room what it is: its name,
// when it closes, whether they may go back, and a ticket to do so.
func (g *Gateway) breakoutInfo(c *Conn, roomID string) {
	parent := room.BreakoutParent(roomID)
	prm, ok := g.Rooms.Get(parent)
	if !ok {
		return
	}
	br, ok := prm.BreakoutRoomByID(roomID)
	st, ok2 := prm.BreakoutState()
	if !ok || !ok2 {
		return
	}
	back := g.mintTicket(parent, c.UserID, returnTicketTTL+st.TimeLimit, true)
	closes := uint64(0)
	if !st.ClosesAt.IsZero() {
		closes = uint64(st.ClosesAt.Unix())
	}
	// A manager may always go back; the setting is about attendees.
	allow := st.AllowReturn || st.Managers[c.UserID]
	c.Emit(func(w *wire.Writer) {
		w.WriteHeader(wire.OpBreakout, 0, false)
		w.Bits(brInfo, 3)
		w.Bool(allow)
		w.Bits(0, 4)
		w.Str(br.Name)
		w.Str(parent)
		w.Blob([]byte(back))
		w.Uvarint(closes)
	})
}

// broadcastBreakoutState sends the whole state to everybody who manages the
// rooms: the hosts in the main meeting, and managers who have walked into a
// room. Sent whole on every change (rule 5), including who is in each room.
func (g *Gateway) broadcastBreakoutState(rm *room.Room) {
	st, _ := rm.BreakoutState()
	enc := func(w *wire.Writer) {
		w.WriteHeader(wire.OpBreakout, 0, false)
		w.Bits(brState, 3)
		w.Bool(st.AutoMove)
		w.Bool(st.AllowReturn)
		w.Bool(st.Open)
		w.Bits(0, 2)
		w.Uvarint(uint64(st.TimeLimit / time.Second))
		closes := uint64(0)
		if !st.ClosesAt.IsZero() {
			closes = uint64(st.ClosesAt.Unix())
		}
		w.Uvarint(closes)
		w.Uvarint(uint64(len(st.Rooms)))
		for _, br := range st.Rooms {
			w.Str(br.ID)
			w.Str(br.Name)
			w.Uvarint(uint64(g.Rooms.Occupancy(br.ID)))
			w.Uvarint(uint64(len(br.Members)))
			for _, m := range br.Members {
				w.Str(m.ID)
				w.Str(m.Name)
			}
		}
	}
	for _, q := range rm.Snapshot() {
		if rm.IsHost(q.ID) {
			if qc := g.connFor(q.ID); qc != nil {
				qc.Emit(enc)
			}
		}
	}
	for _, br := range st.Rooms {
		crm, ok := g.Rooms.Get(br.ID)
		if !ok {
			continue
		}
		for _, q := range crm.Snapshot() {
			if q.UserID != "" && st.Managers[q.UserID] {
				if qc := g.connFor(q.ID); qc != nil {
					qc.Emit(enc)
				}
			}
		}
	}
}
