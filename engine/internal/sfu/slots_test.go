package sfu

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// A receiver with a real (unconnected) subscriber connection, and the slot
// maps it has been sent.
type slotRx struct {
	p    *Peer
	mu   sync.Mutex
	last []SlotBinding
	maps int
}

func newSlotRx(t *testing.T) *slotRx {
	t.Helper()
	p, err := NewPeer("rx", "room", ledger.NewMeter("rx"), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	r := &slotRx{p: p}
	// The receiver has reported what it shows (a tile of someone not here),
	// so the pool is not held to the first page (InitialVideoSlots).
	p.SetPersonView("nobody", 320, 180, true, false)
	p.SendSlots = func(bs []SlotBinding) {
		r.mu.Lock()
		r.last, r.maps = bs, r.maps+1
		r.mu.Unlock()
	}
	return r
}

func (r *slotRx) bound() map[string]string { // track -> slot
	out := map[string]string{}
	for tid, d := range r.p.Downstreams() {
		if s := d.Slot(); s != "" {
			out[tid] = s
		}
	}
	return out
}

func cams(n int) []*PublishedTrack {
	ts := make([]*PublishedTrack, n)
	for i := range ts {
		ts[i] = NewPublishedTrack(fmt.Sprint("cam-", i), fmt.Sprint("p", i), webrtc.RTPCodecTypeVideo, webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), nil)
	}
	return ts
}

// However large the meeting, a receiver holds at most MaxVideoSlots outgoing
// video tracks, and the slot map it is sent names exactly what each carries.
func TestSlotPoolIsBoundedAndMapped(t *testing.T) {
	rx := newSlotRx(t)
	ts := cams(60)
	for _, tr := range ts {
		if _, err := rx.p.Subscribe(tr); err != nil {
			t.Fatal(err)
		}
	}
	rx.p.rebind() // Subscribe binds in the background; settle it here
	if n := len(rx.p.slotsFor("video/" + webrtc.MimeTypeVP8)); n != MaxVideoSlots {
		t.Fatalf("pool has %d video slots for 60 cameras, want the cap %d", n, MaxVideoSlots)
	}
	b := rx.bound()
	if len(b) != MaxVideoSlots {
		t.Fatalf("%d cameras hold slots, want %d", len(b), MaxVideoSlots)
	}
	seen := map[string]bool{}
	for _, s := range b {
		if seen[s] {
			t.Fatalf("slot %s carries two cameras", s)
		}
		seen[s] = true
	}
	rx.mu.Lock()
	last := rx.last
	rx.mu.Unlock()
	if len(last) != MaxVideoSlots {
		t.Fatalf("slot map has %d entries, want %d", len(last), MaxVideoSlots)
	}
	for _, e := range last {
		if b[e.Track] != e.Slot || e.Publisher != "p"+e.Track[len("cam-"):] {
			t.Fatalf("slot map entry %+v does not match what the slot carries", e)
		}
	}
}

// Tiles going off screen give their slots up to tiles coming on, and a
// camera that left frees its slot for someone still waiting.
func TestHiddenTilesGiveTheirSlotsToVisibleOnes(t *testing.T) {
	rx := newSlotRx(t)
	ts := cams(55)
	for _, tr := range ts {
		rx.p.Subscribe(tr)
	}
	rx.p.rebind()
	before := rx.bound()
	var waiting []string
	for _, tr := range ts {
		if _, ok := before[tr.ID]; !ok {
			waiting = append(waiting, tr.ID)
		}
	}
	if len(waiting) != 55-MaxVideoSlots {
		t.Fatalf("%d cameras without a slot, want %d", len(waiting), 55-MaxVideoSlots)
	}
	// Six of the bound people scroll off screen.
	var hidden []string
	for _, tr := range ts {
		if _, ok := before[tr.ID]; ok && len(hidden) < 6 {
			hidden = append(hidden, tr.ID)
			rx.p.SetPersonView(tr.PeerID, 0, 0, false, false)
		}
	}
	rx.p.rebind()
	after := rx.bound()
	for _, id := range waiting {
		if _, ok := after[id]; !ok {
			t.Fatalf("visible camera %s still has no slot after six tiles went off screen", id)
		}
	}
	for _, id := range hidden {
		if _, ok := after[id]; ok {
			t.Fatalf("hidden camera %s kept a slot a visible one needed", id)
		}
	}
	// One bound camera leaves; a hidden one comes back on screen and gets
	// the freed slot.
	var gone string
	for id := range after {
		gone = id
		break
	}
	rx.p.Unsubscribe(gone)
	rx.p.SetPersonView("p"+hidden[0][len("cam-"):], 320, 180, true, false)
	rx.p.rebind()
	final := rx.bound()
	if _, ok := final[hidden[0]]; !ok {
		t.Fatal("a tile back on screen did not get the slot the departed camera freed")
	}
	if _, ok := final[gone]; ok {
		t.Fatal("an unsubscribed camera still holds a slot")
	}
	if n := len(rx.p.slotsFor("video/" + webrtc.MimeTypeVP8)); n != MaxVideoSlots {
		t.Fatalf("pool grew to %d", n)
	}
}

// Voices: only microphones that are sending get a slot, at most MaxAudioSlots.
func TestAudioSlotsGoToActiveMicrophones(t *testing.T) {
	rx := newSlotRx(t)
	mics := make([]*PublishedTrack, 10)
	for i := range mics {
		mics[i] = NewPublishedTrack(fmt.Sprint("mic-", i), fmt.Sprint("p", i), webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, false, ledger.NewMeter("t"), nil)
		rx.p.Subscribe(mics[i])
	}
	if n := len(rx.bound()); n != 0 {
		t.Fatalf("%d silent microphones hold slots", n)
	}
	now := time.Now().UnixNano()
	for _, m := range mics[:3] {
		m.lastPacket.Store(now)
	}
	rx.p.rebind()
	if n := len(rx.bound()); n != 3 {
		t.Fatalf("%d audio slots bound for 3 talkers", n)
	}
	for _, m := range mics {
		m.lastPacket.Store(now)
	}
	rx.p.rebind()
	if n := len(rx.bound()); n != MaxAudioSlots {
		t.Fatalf("%d audio slots bound for 10 talkers, want the cap %d", n, MaxAudioSlots)
	}
}

// Before a receiver has reported what it shows, it is sent a first page of
// cameras, not 49: a joiner of a large meeting must not be flooded with every
// camera for the moment before its first report.
func TestFirstPageOnlyBeforeTheFirstReport(t *testing.T) {
	p, err := NewPeer("rx", "room", ledger.NewMeter("rx"), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, tr := range cams(60) {
		p.Subscribe(tr)
	}
	p.rebind()
	n := 0
	for _, d := range p.Downstreams() {
		if d.Slot() != "" {
			n++
		}
	}
	if n != InitialVideoSlots {
		t.Fatalf("%d cameras sent before any report, want the first page of %d", n, InitialVideoSlots)
	}
	p.SetPersonView("p50", 320, 180, true, false)
	p.rebind()
	n = 0
	for _, d := range p.Downstreams() {
		if d.Slot() != "" {
			n++
		}
	}
	if n != MaxVideoSlots {
		t.Fatalf("%d cameras after the first report, want %d", n, MaxVideoSlots)
	}
}

// An estimate driven to the floor recovers on silence: reports come only for
// what is sent, and once everything is paused none come at all.
func TestEstimateClimbsFromTheFloorWhenNothingReports(t *testing.T) {
	p, err := NewPeer("rx", "room", ledger.NewMeter("rx"), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	now := time.Now()
	for i := range 60 { // heavy loss for half a minute
		p.observeLoss(0.5, now.Add(time.Duration(i)*500*time.Millisecond))
	}
	floor := p.EstimateRate()
	if floor > 200_000 {
		t.Fatalf("estimate %d after sustained loss; the test needs it near the floor", floor)
	}
	at := now.Add(30 * time.Second)
	p.probeIfQuiet(at.Add(time.Second)) // too soon after the last report
	if p.EstimateRate() != floor {
		t.Fatal("climbed while reports were still recent")
	}
	for i := range 40 {
		p.probeIfQuiet(at.Add(time.Duration(3+3*i) * time.Second))
	}
	if r := p.EstimateRate(); r < 4*floor {
		t.Fatalf("estimate %d after two minutes of silence from a floor of %d: stuck", r, floor)
	}
	if r := p.EstimateRate(); r > initialEstimate*2 {
		t.Fatalf("silence alone talked the estimate up to %d", r)
	}
}
