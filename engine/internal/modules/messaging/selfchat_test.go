package messaging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// A directory of the two accounts these tests use. Real SQLite underneath,
// because the queries are the thing under test.
type selfDir struct{}

func (selfDir) LookupByEmail(ctx context.Context, email string) (string, string, error) {
	switch strings.ToLower(email) {
	case "me@example.com":
		return "u-me", "Me", nil
	case "other@example.com":
		return "u-you", "Other", nil
	}
	return "", "", ErrNotFound
}

func (selfDir) LookupByID(ctx context.Context, id string) (string, string, error) {
	switch id {
	case "u-me":
		return "me@example.com", "Me", nil
	case "u-you":
		return "other@example.com", "Other", nil
	}
	return "", "", ErrNotFound
}

func serviceForTest(t *testing.T) *Service {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "msg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(NewSQLRepository(db), selfDir{}, NewHub(quiet), quiet)
}

// The thread you keep with yourself. It used to be refused outright, which was
// the wrong answer to the right question: a message to yourself is where a
// link, a code or a thought goes on its way between two machines, and it is
// the most-used thread in every product that has one.
//
// The property that matters most here is that it is ONE member, not two. A
// self conversation modelled as "you and you" has to survive every query that
// counts distinct members, and the first one that does not shows somebody
// their own notes as a chat with a stranger.

func selfService(t *testing.T) (*Service, Actor) {
	t.Helper()
	s := serviceForTest(t)
	return s, Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
}

func TestYourOwnAddressOpensYourOwnNotes(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()

	c, err := s.OpenDirect(ctx, me, "me@example.com")
	if err != nil {
		t.Fatalf("opening a thread with yourself: %v", err)
	}
	if len(c.Members) != 1 || c.Members[0].UserID != me.ID {
		t.Fatalf("self conversation has %d members, want just you", len(c.Members))
	}
	if !c.SelfChat {
		t.Error("the self conversation is not marked as one")
	}
	if !strings.Contains(c.Display, "(You)") {
		t.Errorf("display is %q; it should say who it is", c.Display)
	}

	// However it is spelled.
	upper, err := s.OpenDirect(ctx, me, "Me@Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if upper.ID != c.ID {
		t.Fatal("a differently-cased address made a second self conversation")
	}
}

func TestOpeningYourNotesTwiceIsOneThread(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()
	first, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("two self conversations: %s and %s", first.ID, second.ID)
	}
}

// The collision this is built to avoid: your notes must never be mistaken for
// a conversation with somebody else, and a real conversation must never be
// mistaken for your notes.
func TestNotesAreNotConfusedWithARealConversation(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()

	notes, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	withOther, err := s.OpenDirect(ctx, me, "other@example.com")
	if err != nil {
		t.Fatalf("opening a normal conversation: %v", err)
	}
	if notes.ID == withOther.ID {
		t.Fatal("a conversation with somebody else resolved to the self thread")
	}
	if withOther.SelfChat {
		t.Error("a two-person conversation was marked as self chat")
	}
	if strings.Contains(withOther.Display, "(You)") {
		t.Errorf("a conversation with somebody else is titled %q", withOther.Display)
	}

	// And asking for the notes again still finds the notes, not the other
	// thread, now that both exist.
	again, _ := s.OpenSelf(ctx, me)
	if again.ID != notes.ID {
		t.Fatal("with a real conversation present, the self lookup found the wrong one")
	}
	if len(again.Members) != 1 {
		t.Fatalf("the self thread grew to %d members", len(again.Members))
	}
}

// The same collision, with the rows the other way round.
//
// Written after a mutation slipped through the test above: relaxing the
// member count from "exactly one" to "at least one" still passed, because the
// notes thread happened to be the older row and the query takes the first
// match. Creating the real conversation FIRST makes the ordering work against
// the lookup instead of for it, which is the only version of this test that
// proves the count is doing the work.
func TestNotesAreFoundEvenWhenAnOlderConversationExists(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()

	withOther, err := s.OpenDirect(ctx, me, "other@example.com")
	if err != nil {
		t.Fatal(err)
	}
	notes, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if notes.ID == withOther.ID {
		t.Fatal("the notes lookup returned an older two-person conversation")
	}

	again, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != notes.ID {
		t.Fatalf("the notes lookup returned %s, want %s", again.ID, notes.ID)
	}
	if len(again.Members) != 1 {
		t.Fatalf("the thread found has %d members, so it is not the notes", len(again.Members))
	}
	if !again.SelfChat {
		t.Error("the thread found is not marked as self chat")
	}
}

// Two people each keeping notes must not end up in each other's.
func TestNotesArePerAccount(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}

	mine, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	yours, err := s.OpenSelf(ctx, you)
	if err != nil {
		t.Fatal(err)
	}
	if mine.ID == yours.ID {
		t.Fatal("two people share one notes thread")
	}
}

// A message sent to yourself comes back when the thread is read, which is the
// only thing the feature is actually for.
func TestAMessageToYourselfIsThereWhenYouLook(t *testing.T) {
	s, me := selfService(t)
	ctx := context.Background()
	notes, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, me, notes.ID, "the wifi password is hunter2"); err != nil {
		t.Fatalf("sending to your own notes: %v", err)
	}
	msgs, err := s.Messages(ctx, me, notes.ID, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Body != "the wifi password is hunter2" {
		t.Fatalf("read back %d messages: %v", len(msgs), msgs)
	}
}

// The organisation boundary, enforced where a conversation is created.
//
// The directory can filter its results all it likes; if this path still
// obliges, the setting is a label. These run against the real service with the
// policy port wired the way main wires it.

func orgOnly(s *Service, domain string) {
	s.MayMessage = func(ctx context.Context, from, to string) bool {
		at := func(e string) string {
			i := strings.LastIndexByte(e, '@')
			if i < 0 {
				return ""
			}
			return strings.ToLower(e[i+1:])
		}
		return at(from) == domain && at(to) == domain
	}
}

func TestAConversationOutsideTheOrganisationIsRefused(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	orgOnly(s, "example.com")
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}

	_, err := s.OpenDirect(ctx, me, "outsider@gmail.example")
	if !errors.Is(err, ErrExternalClosed) {
		t.Fatalf("opening an outside conversation returned %v, want ErrExternalClosed", err)
	}

	// A colleague is unaffected.
	if _, err := s.OpenDirect(ctx, me, "other@example.com"); err != nil {
		t.Fatalf("a colleague was refused too: %v", err)
	}
}

// The refusal must not double as a way to ask whether an address is
// registered: an unknown outside address and a known one answer identically.
func TestTheRefusalDoesNotDiscloseWhoExists(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	orgOnly(s, "example.com")
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}

	_, known := s.OpenDirect(ctx, me, "outsider@gmail.example")
	_, unknown := s.OpenDirect(ctx, me, "nobody-at-all@gmail.example")
	if known == nil || unknown == nil {
		t.Fatal("one of them was allowed through")
	}
	if known.Error() != unknown.Error() {
		t.Fatalf("a registered address answered %q and an unregistered one %q", known, unknown)
	}
}

// One excluded address does not discard a group. A typo or an outside guest
// among five colleagues should leave the five.
func TestAGroupSkipsOutsidersRatherThanFailing(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	orgOnly(s, "example.com")
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}

	c, err := s.CreateGroup(ctx, me, "Planning",
		[]string{"other@example.com", "outsider@gmail.example"})
	if err != nil {
		t.Fatalf("the group was discarded: %v", err)
	}
	for _, m := range c.Members {
		if m.Email == "outsider@gmail.example" {
			t.Fatal("an outsider was added to the group")
		}
	}
	if len(c.Members) != 2 {
		t.Fatalf("the group has %d members, want me and the colleague", len(c.Members))
	}
}

// With no policy wired -- the default -- nothing is restricted.
func TestWithNoPolicyEverybodyIsReachable(t *testing.T) {
	s := serviceForTest(t)
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	if _, err := s.OpenDirect(context.Background(), me, "other@example.com"); err != nil {
		t.Fatalf("an unrestricted deployment refused a conversation: %v", err)
	}
}
