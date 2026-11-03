package messaging

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// decodePushedMessage reads an OpConvMessage frame as encodeConvMessage
// writes it, returning the message id and its poll's total (-1: no poll).
// Every string is read: they teach the receive dictionary.
func decodePushedMessage(r *wire.Reader) (id string, total int) {
	r.Str()
	id = r.Str()
	r.Str()
	r.Str()
	r.Uvarint()
	r.Bits(8)
	r.Blob()
	if r.Str() != "" {
		r.Str()
		r.Str()
		r.Bits(8)
		r.Blob()
	}
	for range r.Uvarint() {
		r.Str()
		r.Uvarint()
		for range r.Uvarint() {
			r.Str()
		}
	}
	for range r.Uvarint() {
		r.Str()
		r.Str()
		r.Str()
		r.Uvarint()
	}
	for range r.Uvarint() {
		r.Str()
	}
	r.Str()
	r.Blob()
	n := r.Uvarint()
	if n == 0 {
		return id, -1
	}
	for range n {
		r.Uvarint()
		for range r.Uvarint() {
			r.Str()
		}
	}
	return id, int(r.Uvarint())
}

// slowMembers stands in for a push that is busy: in a 200-member room the
// gap between reading the tally and reaching a given member is the fan-out
// to everyone before them. Without that gap the reordering this test is
// about almost never shows on one socket.
type slowMembers struct{ *SQLRepository }

func (r slowMembers) MemberIDs(ctx context.Context, convID string) ([]string, error) {
	time.Sleep(time.Duration(rand.IntN(4000)) * time.Microsecond)
	return r.SQLRepository.MemberIDs(ctx, convID)
}

// A hundred people vote at once: the tally a member is pushed never goes
// backwards, and the last one is the true one. (Each vote used to read the tally and push it; two pushes
// read moments apart could reach a member in the opposite order, leaving
// them on an older count for good.)
func TestConcurrentVotesConvergeOnEveryMember(t *testing.T) {
	s, me, you, _, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Vote", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	const voters = 100
	var ms []Member
	for i := range voters {
		ms = append(ms, Member{UserID: fmt.Sprint("v", i), Email: fmt.Sprint("v", i, "@example.com"), Role: RoleMember})
	}
	if err := s.repo.(*SQLRepository).AddMembers(ctx, g.ID, ms); err != nil {
		t.Fatal(err)
	}
	s.repo = slowMembers{s.repo.(*SQLRepository)}
	poll, err := s.Compose(ctx, me, g.ID, "Ship?", "", nil, nil, SendOptions{Meta: `{"poll":{"q":"Ship?","options":["Yes","No"]}}`})
	if err != nil {
		t.Fatal(err)
	}
	sk, cli, rx := newSock(t, s.hub, me.ID, 4096)
	pctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sk.writePump(pctx)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range voters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Vote(ctx, Actor{ID: fmt.Sprint("v", i)}, poll.ID, []int{i % 2}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()

	in := &frames{cli: cli}
	last, pushes, backwards := -1, 0, 0
	for {
		rc, rcancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		data, err := in.next(rc)
		rcancel()
		if err != nil {
			break // quiet
		}
		r := wire.NewReader(data, rx)
		if h, err := r.ReadHeader(); err != nil || h.Op != wire.OpConvMessage {
			continue
		}
		if id, total := decodePushedMessage(r); id == poll.ID && total >= 0 {
			if total < last {
				backwards++
			}
			last = total
			pushes++
		}
	}
	// Votes only arrive here, so a member is never shown a smaller count
	// after a larger one: what is shown is what is stored, in order.
	if backwards > 0 {
		t.Fatalf("the tally a member was shown went backwards %d times in %d pushes", backwards, pushes)
	}
	if last != voters {
		t.Fatalf("after %d votes the last tally pushed was %d (of %d pushes)", voters, last, pushes)
	}
	t.Logf("%d votes, %d pushes of the poll to this member", voters, pushes)
}

// gatedMembers holds the first MemberIDs after armed until released: a push
// caught between reading the tally and reaching anyone.
type gatedMembers struct {
	*SQLRepository
	armed   chan struct{} // closed when the gate is to catch the next call
	held    chan struct{} // closed when a call is being held
	release chan struct{}
	once    *sync.Once
}

func (r gatedMembers) MemberIDs(ctx context.Context, convID string) ([]string, error) {
	select {
	case <-r.armed:
		r.once.Do(func() {
			close(r.held)
			<-r.release
		})
	default:
	}
	return r.SQLRepository.MemberIDs(ctx, convID)
}

// The last vote lands while an earlier vote's push is between reading the
// tally and sending it: that push must go round again, so the member ends on
// the last vote's tally, not one short.
func TestAVoteDuringAPushIsNotLost(t *testing.T) {
	s, me, you, _, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Vote", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	var ms []Member
	for i := range 3 {
		ms = append(ms, Member{UserID: fmt.Sprint("v", i), Email: fmt.Sprint("v", i, "@example.com"), Role: RoleMember})
	}
	if err := s.repo.(*SQLRepository).AddMembers(ctx, g.ID, ms); err != nil {
		t.Fatal(err)
	}
	gate := gatedMembers{SQLRepository: s.repo.(*SQLRepository), armed: make(chan struct{}), held: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	s.repo = gate
	poll, err := s.Compose(ctx, me, g.ID, "Ship?", "", nil, nil, SendOptions{Meta: `{"poll":{"q":"Ship?","options":["Yes","No"]}}`})
	if err != nil {
		t.Fatal(err)
	}
	sk, cli, rx := newSock(t, s.hub, me.ID, 64)
	pctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sk.writePump(pctx)

	if _, err := s.Vote(ctx, Actor{ID: "v0"}, poll.ID, []int{0}); err != nil {
		t.Fatal(err)
	}
	close(gate.armed)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Vote(ctx, Actor{ID: "v1"}, poll.ID, []int{0}) // its push reads 2, then is held
	}()
	<-gate.held
	if _, err := s.Vote(ctx, Actor{ID: "v2"}, poll.ID, []int{1}); err != nil { // lands meanwhile
		t.Fatal(err)
	}
	close(gate.release)
	<-done

	in := &frames{cli: cli}
	last := -1
	for {
		rc, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		data, err := in.next(rc)
		rcancel()
		if err != nil {
			break
		}
		r := wire.NewReader(data, rx)
		if h, err := r.ReadHeader(); err != nil || h.Op != wire.OpConvMessage {
			continue
		}
		if id, total := decodePushedMessage(r); id == poll.ID && total >= 0 {
			last = total
		}
	}
	if last != 3 {
		t.Fatalf("the member ended on a tally of %d; three people voted", last)
	}
}
