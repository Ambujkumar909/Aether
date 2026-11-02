package messaging

import (
	"context"
	"testing"

	"github.com/aether-rtc/aether/internal/wire"
)

// Choosing to appear offline tells peers "left" (a presence frame) and then
// the status; lifting it tells them "back". Counted on the peer's socket.
func TestAppearOfflinePushesPresenceToPeers(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	mine := fakeSock(me.ID)
	yours := fakeSock(you.ID)
	s.hub.add(mine)
	s.hub.add(yours)
	defer s.hub.remove(mine)
	defer s.hub.remove(yours)
	ops := func() []wire.Op {
		var out []wire.Op
		for {
			select {
			case b := <-yours.out:
				r := wire.NewReader(b, nil)
				h, err := r.ReadHeader()
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, h.Op)
			default:
				return out
			}
		}
	}
	ops() // drain
	if _, err := s.SetStatus(ctx, me, StatusOffline, "", nil); err != nil {
		t.Fatal(err)
	}
	got := ops()
	if len(got) != 2 || got[0] != wire.OpPresence || got[1] != wire.OpStatus {
		t.Fatalf("appear offline pushed %v, want [presence status]", got)
	}
	if _, err := s.SetStatus(ctx, me, StatusAvailable, "", nil); err != nil {
		t.Fatal(err)
	}
	got = ops()
	if len(got) != 2 || got[0] != wire.OpPresence || got[1] != wire.OpStatus {
		t.Fatalf("lifting pushed %v, want [presence status]", got)
	}
	// Busy to away: a status only, no presence.
	s.SetStatus(ctx, me, StatusBusy, "", nil)
	if got = ops(); len(got) != 1 || got[0] != wire.OpStatus {
		t.Fatalf("busy pushed %v, want [status]", got)
	}
}
