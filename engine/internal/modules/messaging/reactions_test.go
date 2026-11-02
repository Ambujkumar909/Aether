package messaging

import (
	"context"
	"testing"
	"time"
)

// Reactions, quoted replies and saved messages -- the three things Teams has
// that this did not. Against real SQLite, because what is under test is the
// SQL: the composite keys that make reacting idempotent, the grouping, and the
// scoping that stops a reply quoting a conversation you are not in.

func chatFixture(t *testing.T) (*Service, Actor, Actor, *Conversation) {
	t.Helper()
	s := serviceForTest(t)
	ctx := context.Background()
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, err := s.OpenDirect(ctx, me, "other@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return s, me, you, c
}

func TestReactingTwiceIsReactingOnce(t *testing.T) {
	s, me, _, c := chatFixture(t)
	ctx := context.Background()
	m, err := s.Send(ctx, me, c.ID, "ship it")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := s.React(ctx, me, m.ID, "\U0001F44D", true); err != nil {
			t.Fatal(err)
		}
	}
	got := reactionsOf(t, s, me, c.ID, m.ID)
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("three identical reactions produced %v", got)
	}
	if !got[0].Mine {
		t.Error("my own reaction is not marked as mine")
	}
}

func TestReactionsAreGroupedByEmojiAndCounted(t *testing.T) {
	s, me, you, c := chatFixture(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, c.ID, "ship it")

	// Teams allows several different emoji from one person on one message.
	if err := s.React(ctx, me, m.ID, "\U0001F44D", true); err != nil {
		t.Fatal(err)
	}
	if err := s.React(ctx, me, m.ID, "\U0001F389", true); err != nil {
		t.Fatal(err)
	}
	if err := s.React(ctx, you, m.ID, "\U0001F44D", true); err != nil {
		t.Fatal(err)
	}

	got := reactionsOf(t, s, me, c.ID, m.ID)
	if len(got) != 2 {
		t.Fatalf("want two groups, got %v", got)
	}
	byEmoji := map[string]Reaction{}
	for _, g := range got {
		byEmoji[g.Emoji] = g
	}
	if byEmoji["\U0001F44D"].Count != 2 {
		t.Fatalf("thumbs up counted %d, want 2", byEmoji["\U0001F44D"].Count)
	}
	if !byEmoji["\U0001F389"].Mine {
		t.Error("my party popper is not marked mine")
	}

	// And the other person sees the same counts with their own "mine".
	other := reactionsOf(t, s, you, c.ID, m.ID)
	for _, g := range other {
		if g.Emoji == "\U0001F389" && g.Mine {
			t.Error("somebody else's reaction was reported as theirs")
		}
	}
}

func TestRemovingAReactionIsIdempotent(t *testing.T) {
	s, me, _, c := chatFixture(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, c.ID, "ship it")
	_ = s.React(ctx, me, m.ID, "\U0001F44D", true)

	// Twice. Two clicks racing must not produce a failure to explain.
	for i := 0; i < 2; i++ {
		if err := s.React(ctx, me, m.ID, "\U0001F44D", false); err != nil {
			t.Fatalf("removing a reaction failed on pass %d: %v", i, err)
		}
	}
	if got := reactionsOf(t, s, me, c.ID, m.ID); len(got) != 0 {
		t.Fatalf("the reaction survived removal: %v", got)
	}
}

// Membership is the whole guard. The client sends a message id and nothing
// else, so without this anybody could react to any message on the deployment.
func TestReactingNeedsMembership(t *testing.T) {
	s, me, _, c := chatFixture(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, c.ID, "internal")

	stranger := Actor{ID: "u-nobody", Email: "nobody@example.com", Name: "Nobody"}
	if err := s.React(ctx, stranger, m.ID, "\U0001F44D", true); err == nil {
		t.Fatal("a stranger reacted to a conversation they are not in")
	}
}

func TestNonsenseReactionsAreRefused(t *testing.T) {
	s, me, _, c := chatFixture(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, c.ID, "hello")
	for _, e := range []string{"", "   ", "this is a whole sentence not an emoji"} {
		if err := s.React(ctx, me, m.ID, e, true); err == nil {
			t.Errorf("%q was accepted as a reaction", e)
		}
	}
}

// A quoted reply carries the message it answers, so the client draws the quote
// without a request per message.
func TestAReplyCarriesWhatItQuotes(t *testing.T) {
	s, me, you, c := chatFixture(t)
	ctx := context.Background()
	first, _ := s.Send(ctx, me, c.ID, "shall we ship on Friday?")

	reply, err := s.SendReply(ctx, you, c.ID, "yes", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != first.ID {
		t.Fatalf("the reply points at %q, want %q", reply.ReplyTo, first.ID)
	}
	if reply.Quoted == nil || reply.Quoted.Body != "shall we ship on Friday?" {
		t.Fatalf("the reply does not carry its quote: %+v", reply.Quoted)
	}
	if reply.Quoted.SenderName != "Me" {
		t.Fatalf("the quote names %q", reply.Quoted.SenderName)
	}
}

// The leak this is built to prevent: the reply target is an arbitrary id from
// the client, and the quoted text is loaded and returned on read. Quoting a
// message from a conversation you are not in would print its contents to you.
func TestAReplyCannotQuoteAnotherConversation(t *testing.T) {
	s, me, you, c := chatFixture(t)
	ctx := context.Background()

	// A conversation "me" is definitively not in: the other person's own
	// notes, which have exactly one member and it is not me.
	private, err := s.OpenSelf(ctx, you)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.Send(ctx, you, private.ID, "the merger closes Tuesday")
	if err != nil {
		t.Fatal(err)
	}
	if secret.ConversationID == c.ID {
		t.Fatal("the fixture produced the same conversation twice")
	}

	reply, err := s.SendReply(ctx, me, c.ID, "look at this", secret.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != "" {
		t.Fatal("a reply quoted a message from a conversation the sender is not in")
	}
	if reply.Quoted != nil {
		t.Fatalf("the contents leaked through the quote: %q", reply.Quoted.Body)
	}

	// And on read, from the other side, it is still not there.
	msgs, _ := s.Messages(ctx, you, c.ID, time.Time{}, 50)
	s.Decorate(ctx, you, msgs)
	for _, m := range msgs {
		if m.Quoted != nil && m.Quoted.Body == "the merger closes Tuesday" {
			t.Fatal("the quoted text reached the conversation on read")
		}
	}
}

// Deleting a message deletes it everywhere it appears, including inside
// somebody else's quote of it. A delete that leaves the text in a quote is
// decorative.
func TestDeletingAMessageEmptiesItsQuotes(t *testing.T) {
	s, me, you, c := chatFixture(t)
	ctx := context.Background()
	first, _ := s.Send(ctx, me, c.ID, "something regrettable")
	_, _ = s.SendReply(ctx, you, c.ID, "noted", first.ID)

	if err := s.Delete(ctx, me, first.ID); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, you, c.ID, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	s.Decorate(ctx, you, msgs)
	for _, m := range msgs {
		if m.Quoted != nil && m.Quoted.Body != "" {
			t.Fatalf("a deleted message is still readable in a quote: %q", m.Quoted.Body)
		}
	}
}

// Saved messages are per person: somebody else saving a message is not a fact
// about the message.
func TestSavingIsPrivateToTheSaver(t *testing.T) {
	s, me, you, c := chatFixture(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, c.ID, "the wifi password")

	if err := s.SaveMessage(ctx, me, m.ID, true); err != nil {
		t.Fatal(err)
	}
	mine, err := s.Saved(ctx, me, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != m.ID {
		t.Fatalf("my saved list is %v", mine)
	}
	theirs, _ := s.Saved(ctx, you, 50)
	if len(theirs) != 0 {
		t.Fatalf("my save appeared in somebody else's list: %v", theirs)
	}

	// And unsaving removes it, twice over.
	for i := 0; i < 2; i++ {
		if err := s.SaveMessage(ctx, me, m.ID, false); err != nil {
			t.Fatalf("unsaving failed on pass %d: %v", i, err)
		}
	}
	if again, _ := s.Saved(ctx, me, 50); len(again) != 0 {
		t.Fatalf("the message stayed saved: %v", again)
	}
}

func reactionsOf(t *testing.T, s *Service, a Actor, convID, msgID string) []Reaction {
	t.Helper()
	msgs, err := s.Messages(context.Background(), a, convID, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	s.Decorate(context.Background(), a, msgs)
	for _, m := range msgs {
		if m.ID == msgID {
			return m.Reactions
		}
	}
	t.Fatalf("message %s not found", msgID)
	return nil
}
