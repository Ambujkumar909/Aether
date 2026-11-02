package signal

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// One decoded OpBreakout frame, as a client reads it.
type bframe struct {
	kind                     uint32
	flag1, flag2, flag3      bool
	room, name, parent, from string
	ticket, back, text       string
	closes, delay, limit     uint64
	rooms                    []string // STATE: room names
	present                  []uint64
}

// bdrain reads every frame for id within d and returns the OpBreakout ones,
// decoding every frame that can teach the dictionary so later ones read right.
func (f *presenterFixture) bdrain(id string, d time.Duration) []bframe {
	var out []bframe
	deadline := time.After(d)
	for {
		select {
		case data := <-f.rx[id]:
			var one func([]byte) error
			one = func(frame []byte) error {
				r := wire.NewReader(frame, f.dicts[id])
				h, err := r.ReadHeader()
				if err != nil {
					return nil
				}
				switch h.Op {
				case wire.OpBatch:
					return wire.SplitBatch(r, one)
				case wire.OpBreakout:
					b := bframe{kind: r.Bits(3)}
					switch b.kind {
					case brState:
						b.flag1, b.flag2, b.flag3 = r.Bool(), r.Bool(), r.Bool()
						r.Bits(2)
						b.limit, b.closes = r.Uvarint(), r.Uvarint()
						n := r.Uvarint()
						for i := uint64(0); i < n; i++ {
							r.Str()
							b.rooms = append(b.rooms, r.Str())
							b.present = append(b.present, r.Uvarint())
							m := r.Uvarint()
							for j := uint64(0); j < m; j++ {
								r.Str()
								r.Str()
							}
						}
					case brMove:
						b.flag1, b.flag2 = r.Bool(), r.Bool()
						r.Bits(3)
						b.room, b.name = r.Str(), r.Str()
						b.ticket, b.back = string(r.Blob()), string(r.Blob())
						b.parent, b.closes = r.Str(), r.Uvarint()
					case brReturn:
						b.flag1 = r.Bool()
						r.Bits(4)
						b.parent, b.ticket, b.delay = r.Str(), string(r.Blob()), r.Uvarint()
					case brAnnounce:
						r.Bits(5)
						b.from, b.text = r.Str(), string(r.Blob())
					case brInfo:
						b.flag1 = r.Bool()
						r.Bits(4)
						b.name, b.parent, b.back, b.closes = r.Str(), r.Str(), string(r.Blob()), r.Uvarint()
					}
					out = append(out, b)
				case wire.OpStateDelta:
					var st wire.State
					_ = st.Decode(r)
				}
				return nil
			}
			_ = one(data)
		case <-deadline:
			return out
		}
	}
}

func (f *presenterFixture) setRooms(from string, limitSec uint64, autoMove bool, rooms map[string][]string, order ...string) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpBreakout, 0, false)
		w.Bits(brSet, 3)
		w.Bool(autoMove)
		w.Bool(true)
		w.Bits(0, 3)
		w.Uvarint(limitSec)
		w.Uvarint(uint64(len(order)))
		for _, name := range order {
			w.Str(name)
			w.Uvarint(uint64(len(rooms[name])))
			for _, id := range rooms[name] {
				w.Str(id)
			}
		}
	})
}

func (f *presenterFixture) brCmd(from string, kind uint32) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpBreakout, 0, false)
		w.Bits(kind, 3)
		w.Bits(0, 5)
	})
}

// joinRoom puts a new participant, on a real socket, into another room.
func (f *presenterFixture) joinRoom(roomID, id, user string) *room.Room {
	f.t.Helper()
	c, cli := newTestConn(f.t)
	c.UserID = user
	f.conns[id] = c
	f.rx[id] = frames(f.t, cli)
	_, f.dicts[id] = wire.NewPreloadedPair()
	f.g.mu.Lock()
	f.g.byID[id] = c
	f.g.mu.Unlock()
	rm := f.g.Rooms.GetOrCreate(roomID)
	p := &room.Participant{ID: id, UserID: user, DisplayName: id, Send: func([]byte) {}}
	if _, err := rm.Join(p); err != nil {
		f.t.Fatal(err)
	}
	f.parts[id] = p
	return rm
}

func kinds(bs []bframe, k uint32) []bframe {
	var out []bframe
	for _, b := range bs {
		if b.kind == k {
			out = append(out, b)
		}
	}
	return out
}

// Set and open: only a host; each assigned person is moved with a ticket
// for their room and one to come back; tickets are spent once, bound to the
// room and the account.
func TestBreakoutSetOpenMove(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c") // a hosts
	f.conns["b"].UserID, f.parts["b"].UserID = "u-b", "u-b"
	f.conns["a"].UserID, f.parts["a"].UserID = "u-a", "u-a"
	rooms := map[string][]string{"Room 1": {"b"}, "Room 2": {"c"}}

	f.setRooms("b", 0, true, rooms, "Room 1", "Room 2")
	if _, ok := f.rm.BreakoutState(); ok {
		t.Fatal("a member created breakout rooms")
	}
	f.setRooms("a", 0, true, rooms, "Room 1", "Room 2")
	if st := kinds(f.bdrain("a", 400*time.Millisecond), brState); len(st) != 1 || len(st[0].rooms) != 2 || st[0].flag3 {
		t.Fatalf("host's state after set: %+v", st)
	}
	f.brCmd("b", brOpen)
	if f.rm.BreakoutsOpen() {
		t.Fatal("a member opened the rooms")
	}
	f.brCmd("a", brOpen)
	mb := kinds(f.bdrain("b", 400*time.Millisecond), brMove)
	mc := kinds(f.bdrain("c", 400*time.Millisecond), brMove)
	if len(mb) != 1 || mb[0].name != "Room 1" || mb[0].parent != "r" || mb[0].flag1 || len(mc) != 1 || mc[0].name != "Room 2" {
		t.Fatalf("moves: b %+v c %+v", mb, mc)
	}
	if st := kinds(f.bdrain("a", 300*time.Millisecond), brState); len(st) != 1 || !st[0].flag3 {
		t.Fatalf("host not told the rooms are open: %+v", st)
	}
	b := mb[0]
	if f.g.useTicket(b.ticket, mc[0].room, "u-b") {
		t.Fatal("b's ticket opened c's room")
	}
	if f.g.useTicket(b.ticket, b.room, "u-x") {
		t.Fatal("b's ticket admitted another account")
	}
	if !f.g.useTicket(b.ticket, b.room, "u-b") || f.g.useTicket(b.ticket, b.room, "u-b") {
		t.Fatal("the room ticket is not single-use")
	}
	if !f.g.useTicket(b.back, "r", "u-b") || !f.g.useTicket(b.back, "r", "u-b") {
		t.Fatal("the way back is not reusable while it lasts")
	}
	if f.g.useTicket("", "r", "") || f.g.useTicket(strings.Repeat("x", maxTicketLen+1), "r", "") {
		t.Fatal("an empty or over-long ticket was accepted")
	}
}

// Into a breakout room: a ticket or a manager of the open rooms, nothing else.
func TestBreakoutAdmission(t *testing.T) {
	f := newPresenterFixture(t, "a", "b")
	f.conns["a"].UserID, f.parts["a"].UserID = "u-a", "u-a"
	f.setRooms("a", 0, true, map[string][]string{"Room 1": {"b"}}, "Room 1")
	st, _ := f.rm.BreakoutState()
	child := st.Rooms[0].ID
	stranger := &Conn{UserID: "u-z"}
	manager := &Conn{UserID: "u-a"}
	if f.g.admitBreakout(manager, child, "r") {
		t.Fatal("a manager entered rooms that are not open")
	}
	f.brCmd("a", brOpen)
	if f.g.admitBreakout(stranger, child, "r") {
		t.Fatal("a stranger entered a breakout room")
	}
	if !f.g.admitBreakout(manager, child, "r") {
		t.Fatal("a manager cannot enter a room")
	}
	if f.g.admitBreakout(manager, "r~br~forged~9", "r") {
		t.Fatal("a manager entered a room that does not exist")
	}
	f.brCmd("a", brClose)
	if f.g.admitBreakout(manager, child, "r") {
		t.Fatal("a manager entered a closed room")
	}
}

// Closing brings the rooms' people back: 10 s by a host, 30 s by the timer
// (breakoutRoomTimerExpireScreenAutoJoinDuration), with a ticket home.
func TestBreakoutCloseAndTimer(t *testing.T) {
	f := newPresenterFixture(t, "a", "b")
	f.setRooms("a", 0, true, map[string][]string{"Room 1": {"b"}}, "Room 1")
	f.brCmd("a", brOpen)
	st, _ := f.rm.BreakoutState()
	f.joinRoom(st.Rooms[0].ID, "b2", "u-b")
	f.bdrain("b2", 200*time.Millisecond)
	f.brCmd("a", brClose)
	ret := kinds(f.bdrain("b2", 400*time.Millisecond), brReturn)
	if len(ret) != 1 || ret[0].flag1 || ret[0].delay != returnDelayClosed || ret[0].parent != "r" || !f.g.useTicket(ret[0].ticket, "r", "u-b") {
		t.Fatalf("closed by a host: %+v", ret)
	}

	// The timer: one second.
	f.setRooms("a", 1, true, map[string][]string{"Room 1": {"b"}}, "Room 1")
	f.brCmd("a", brOpen)
	st, _ = f.rm.BreakoutState()
	f.joinRoom(st.Rooms[0].ID, "b3", "")
	f.bdrain("b3", 100*time.Millisecond)
	ret = kinds(f.bdrain("b3", 2500*time.Millisecond), brReturn)
	if len(ret) != 1 || !ret[0].flag1 || ret[0].delay != returnDelayExpired {
		t.Fatalf("closed by the timer: %+v", ret)
	}
	if f.rm.BreakoutsOpen() {
		t.Fatal("the timer did not close the rooms")
	}
}

// "Make an announcement": to everyone in the rooms, from a host, bounded.
func TestBreakoutAnnounce(t *testing.T) {
	f := newPresenterFixture(t, "a", "b")
	f.setRooms("a", 0, true, map[string][]string{"Room 1": {"b"}}, "Room 1")
	f.brCmd("a", brOpen)
	st, _ := f.rm.BreakoutState()
	f.joinRoom(st.Rooms[0].ID, "b2", "")
	f.bdrain("b2", 200*time.Millisecond)
	say := func(from, text string) {
		f.send(from, func(w *wire.Writer) {
			w.WriteHeader(wire.OpBreakout, 0, false)
			w.Bits(brAnnounce, 3)
			w.Bits(0, 5)
			w.Blob([]byte(text))
		})
	}
	say("b", "from a member")
	say("a", strings.Repeat("x", room.MaxAnnouncementLen+1))
	say("a", "Five minutes left")
	got := kinds(f.bdrain("b2", 400*time.Millisecond), brAnnounce)
	if len(got) != 1 || got[0].text != "Five minutes left" || got[0].from != "a" {
		t.Fatalf("announcements: %+v", got)
	}
}

// No lock while open; ending the meeting ends the rooms; bounds hold.
func TestBreakoutLockEndAndBounds(t *testing.T) {
	f := newPresenterFixture(t, "a", "b")
	f.setRooms("a", 0, true, map[string][]string{"Room 1": {"b"}}, "Room 1")
	f.brCmd("a", brOpen)
	f.send("a", func(w *wire.Writer) {
		w.WriteHeader(wire.OpMeetingPolicy, 0, false)
		w.Str("trusted")
		w.Bool(true)
		w.Bits(0, 7)
	})
	if f.rm.Locked() {
		t.Fatal("locked while breakout rooms are open")
	}
	st, _ := f.rm.BreakoutState()
	f.joinRoom(st.Rooms[0].ID, "b2", "")
	f.send("a", func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
	if r := f.conns["b2"].CloseReason(); r != "meeting ended" {
		t.Fatalf("a breakout room outlived the meeting (%q)", r)
	}

	// 51 rooms: nothing.
	g := newPresenterFixture(t, "a", "b")
	names := map[string][]string{}
	var order []string
	for i := 0; i < room.MaxBreakoutRooms+1; i++ {
		n := strings.Repeat("r", i%30+1)
		order = append(order, n)
		names[n] = nil
	}
	g.setRooms("a", 0, true, names, order...)
	if _, ok := g.rm.BreakoutState(); ok {
		t.Fatal("51 rooms were created")
	}

	// The ticket store is bounded: full of live tickets, minting refuses.
	g.g.mu.Lock()
	g.g.tickets = make(map[string]ticket, maxTickets)
	for i := 0; i < maxTickets; i++ {
		g.g.tickets[strconv.Itoa(i)] = ticket{exp: time.Now().Add(time.Hour)}
	}
	g.g.mu.Unlock()
	if tok := g.g.mintTicket("r", "", time.Minute, false); tok != "" {
		t.Fatal("minted past the bound")
	}
}
