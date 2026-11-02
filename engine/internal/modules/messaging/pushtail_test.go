package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// The push must carry what the thread draws.
//
// This test reads a pushed frame off a real socket and decodes the tail that
// the reaction bug was missing. Against the old encoding the reader runs off
// the end of the frame while looking for the reply marker, which is exactly
// how the client ended up drawing a message with no reactions on it.
func TestPushCarriesReactionsAndTheQuote(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	s, cli, rx := newSock(t, h, "viewer", 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.writePump(ctx)

	when := time.UnixMilli(1_700_000_000_000)
	h.PushMessage("viewer", &Message{
		ConversationID: "conv-1",
		ID:             "msg-2",
		SenderID:       "u-you",
		SenderName:     "Other",
		CreatedAt:      when,
		Body:           "agreed 👍",
		ReplyTo:        "msg-1",
		Quoted:         &Quote{ID: "msg-1", SenderName: "Me", Body: "shall we ship it?"},
		Reactions: []Reaction{
			{Emoji: "👍", Count: 2, Who: []string{"u-you", "viewer"}},
			{Emoji: "❤️", Count: 14, Who: []string{"a", "b", "c"}}, // more reactors than ids carried
		},
	})

	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	typ, data, err := cli.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatal("frame is not binary")
	}

	r := wire.NewReader(data, rx)
	hdr, err := r.ReadHeader()
	if err != nil || hdr.Op != wire.OpConvMessage {
		t.Fatalf("header: %v op=%v", err, hdr.Op)
	}
	if conv, id := r.Str(), r.Str(); conv != "conv-1" || id != "msg-2" {
		t.Fatalf("head: conv=%q id=%q", conv, id)
	}
	_, _ = r.Str(), r.Str()
	_ = r.Uvarint()
	_, _ = r.Bool(), r.Bool()
	r.Bits(6)
	if body := string(r.Blob()); body != "agreed 👍" {
		t.Fatalf("body %q", body)
	}

	// The tail.
	if replyTo := r.Str(); replyTo != "msg-1" {
		t.Fatalf("replyTo %q, want msg-1", replyTo)
	}
	_ = r.Str() // quoted sender id
	if who := r.Str(); who != "Me" {
		t.Fatalf("quoted sender %q", who)
	}
	if del := r.Bool(); del {
		t.Fatal("quote marked deleted")
	}
	r.Bits(7)
	if q := string(r.Blob()); q != "shall we ship it?" {
		t.Fatalf("quoted body %q", q)
	}

	if n := r.Uvarint(); n != 2 {
		t.Fatalf("%d reaction groups, want 2", n)
	}
	type group struct {
		emoji string
		count uint64
		who   []string
	}
	var got []group
	for i := 0; i < 2; i++ {
		g := group{emoji: r.Str(), count: r.Uvarint()}
		n := r.Uvarint()
		for k := uint64(0); k < n; k++ {
			g.who = append(g.who, r.Str())
		}
		got = append(got, g)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("decoding the tail: %v", err)
	}
	if got[0].emoji != "👍" || got[0].count != 2 || len(got[0].who) != 2 || got[0].who[1] != "viewer" {
		t.Fatalf("first group %+v", got[0])
	}
	// Count and the ids carried are separate facts: the viewer sees 14 and
	// three names, and works out "mine" from the ids it has.
	if got[1].emoji != "❤️" || got[1].count != 14 || len(got[1].who) != 3 {
		t.Fatalf("second group %+v", got[1])
	}
}

// A message that is not a reply, has no reactions, no files, no mentions, no
// priority, no card and no poll costs seven bytes more than the original
// frame: an empty string, five zero counts / empty blobs, and the poll's
// zero option count. Measured, not assumed.
func TestPushTailCostsSevenBytesWhenThereIsNothingToSay(t *testing.T) {
	m := &Message{ConversationID: "c", ID: "m", SenderID: "s", SenderName: "n",
		CreatedAt: time.UnixMilli(1), Body: "hi"}

	old := wire.NewWriter(wire.NewDict())
	old.WriteHeader(wire.OpConvMessage, 0, false)
	old.Str(m.ConversationID)
	old.Str(m.ID)
	old.Str(m.SenderID)
	old.Str(m.SenderName)
	old.Uvarint(uint64(m.CreatedAt.UnixMilli()))
	old.Bool(false)
	old.Bool(false)
	old.Bits(0, 6)
	old.Blob([]byte(m.Body))

	now := wire.NewWriter(wire.NewDict())
	encodeConvMessage(now, m)

	if d := len(now.Bytes()) - len(old.Bytes()); d != 7 {
		t.Fatalf("tail costs %d bytes on a plain message, want 7 (old %d, new %d)",
			d, len(old.Bytes()), len(now.Bytes()))
	}
}
