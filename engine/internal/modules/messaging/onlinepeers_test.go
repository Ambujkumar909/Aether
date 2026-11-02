package messaging

import (
	"context"
	"sort"
	"testing"
	"time"
)

// OnlinePeers: peers with a live socket, nobody else, not the asker.
func TestOnlinePeersListsConnectedPeersOnly(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	// A third person who shares nothing with me and is online anyway.
	stranger := fakeSock("u-stranger")
	s.hub.add(stranger)
	defer s.hub.remove(stranger)
	if got := s.OnlinePeers(ctx, me.ID); len(got) != 0 {
		t.Fatalf("nobody online yet: %v", got)
	}
	ys := fakeSock(you.ID)
	s.hub.add(ys)
	got := s.OnlinePeers(ctx, me.ID)
	sort.Strings(got)
	if len(got) != 1 || got[0] != you.ID {
		t.Fatalf("you online: %v", got)
	}
	// My own sockets are not my peers.
	mine := fakeSock(me.ID)
	s.hub.add(mine)
	if got := s.OnlinePeers(ctx, me.ID); len(got) != 1 {
		t.Fatalf("self excluded: %v", got)
	}
	s.hub.remove(ys)
	if got := s.OnlinePeers(ctx, me.ID); len(got) != 1 {
		t.Fatalf("inside the grace you still count: %v", got)
	}
	s.hub.now = func() time.Time { return time.Now().Add(OfflineGrace + time.Second) }
	if got := s.OnlinePeers(ctx, me.ID); len(got) != 0 {
		t.Fatalf("after you left: %v", got)
	}
	s.hub.now = time.Now
	s.hub.remove(mine)
}
