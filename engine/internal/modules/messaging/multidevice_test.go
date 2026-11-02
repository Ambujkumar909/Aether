package messaging

import (
	"context"
	"testing"
	"time"
)

// A message goes to the sender's other devices and never back to the one
// that sent it; a sender without a device id is not pushed at all.
func TestOwnMessageReachesOtherDevicesNotTheSendingOne(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	laptop := fakeSock(me.ID)
	laptop.device = "laptop"
	phone := fakeSock(me.ID)
	phone.device = "phone"
	legacy := fakeSock(me.ID) // a client with no device id
	yours := fakeSock(you.ID)
	for _, sk := range []*sock{laptop, phone, legacy, yours} {
		s.hub.add(sk)
		defer s.hub.remove(sk)
	}
	m := &Message{ID: newID(), ConversationID: conv, SenderID: me.ID, SenderName: "Me", Body: "from the laptop", CreatedAt: time.Now()}
	s.fanout(ctx, conv, me.ID, "laptop", m)
	if n := len(laptop.out); n != 0 {
		t.Fatalf("the sending device was echoed %d frame(s)", n)
	}
	if n := len(phone.out); n != 1 {
		t.Fatalf("the phone got %d frame(s), want 1", n)
	}
	if n := len(legacy.out); n != 1 {
		t.Fatalf("a device-less tab of the sender got %d, want 1 (it cannot be the sender)", n)
	}
	if n := len(yours.out); n != 1 {
		t.Fatalf("the peer got %d, want 1", n)
	}
	// No device id on the send: the older contract, the sender is skipped.
	s.fanout(ctx, conv, me.ID, "", m)
	if len(laptop.out)+len(phone.out)+len(legacy.out) != 2 {
		t.Fatalf("a send without a device id must not push to the sender at all")
	}
	if n := len(yours.out); n != 2 {
		t.Fatalf("the peer got %d, want 2", n)
	}
}
