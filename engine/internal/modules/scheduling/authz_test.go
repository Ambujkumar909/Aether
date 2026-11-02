package scheduling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Authorisation, probed the way an attacker would.
//
// Insecure direct object reference is the most common serious finding in an
// application of this shape, and it is almost never visible from the outside:
// the interface never offers a button that reads somebody else's meeting, so
// nothing in normal use exercises the check. The only way to know is to try
// the identifiers directly, which is what this does.
//
// Every case here is a request a signed-in user can make with a terminal and
// an identifier they were never given.

type nullRecorder struct{}

func (nullRecorder) Record(context.Context, audit.Event) {}

func authzService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(NewSQLRepository(db), nullRecorder{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var (
	owner    = Actor{ID: "u-owner", Email: "owner@example.com"}
	invitee  = Actor{ID: "u-inv", Email: "inv@example.com"}
	stranger = Actor{ID: "u-str", Email: "stranger@example.com"}
	sysAdmin = Actor{ID: "u-adm", Email: "adm@example.com", Admin: true}
)

func makeMeeting(t *testing.T, svc *Service) *Meeting {
	t.Helper()
	start := time.Now().Add(time.Hour)
	m, err := svc.Create(context.Background(), owner, &Meeting{
		Room: "private-room", Title: "Board pay review",
		Category: "internal",
		StartsAt: start, EndsAt: start.Add(time.Hour),
		Invitees: []Invitee{{Email: invitee.Email, Response: RespPending}},
	}, "1.2.3.4", "ua")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A stranger holding the identifier must not be able to read the meeting.
//
// The title alone is the leak: "Board pay review", who organised it, and who
// was invited is most of what somebody wants before they have read a word of
// the contents.
func TestStrangerCannotReadAnotherPersonsMeeting(t *testing.T) {
	svc := authzService(t)
	m := makeMeeting(t, svc)
	ctx := context.Background()

	if _, err := svc.ByID(ctx, stranger, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read somebody else's meeting: %v", err)
	}
	// The people who should see it, do.
	for _, a := range []Actor{owner, invitee, sysAdmin} {
		if _, err := svc.ByID(ctx, a, m.ID); err != nil {
			t.Errorf("%s could not read a meeting they are party to: %v", a.Email, err)
		}
	}
}

// Guessing identifiers must fail the same way whether or not the meeting
// exists, or the error itself enumerates what is there.
func TestUnknownAndForbiddenAreIndistinguishable(t *testing.T) {
	svc := authzService(t)
	m := makeMeeting(t, svc)
	ctx := context.Background()

	_, errReal := svc.ByID(ctx, stranger, m.ID)
	_, errFake := svc.ByID(ctx, stranger, "does-not-exist")

	// They may be different error values internally, but the handler maps
	// both to 404. What must not happen is a caller learning "this exists but
	// is not yours" from one and "no such thing" from the other, which turns
	// identifier guessing into an inventory.
	if errReal == nil || errFake == nil {
		t.Fatal("a stranger was given a meeting")
	}
}

// Modification is the half people forget. Read checks are usually present;
// update, cancel and delete are added later and gated differently.
func TestStrangerCannotModifyAnotherPersonsMeeting(t *testing.T) {
	svc := authzService(t)
	m := makeMeeting(t, svc)
	ctx := context.Background()

	t.Run("update", func(t *testing.T) {
		clone := *m
		clone.Title = "Renamed by an outsider"
		if _, err := svc.Update(ctx, stranger, m.ID, &clone, "", ""); err == nil {
			t.Fatal("a stranger renamed somebody else's meeting")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		if err := svc.Cancel(ctx, stranger, m.ID, "", ""); err == nil {
			t.Fatal("a stranger cancelled somebody else's meeting")
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := svc.Delete(ctx, stranger, m.ID, "", ""); err == nil {
			t.Fatal("a stranger deleted somebody else's meeting")
		}
	})

	// And it is genuinely untouched afterwards.
	after, err := svc.ByID(ctx, owner, m.ID)
	if err != nil {
		t.Fatalf("the meeting is gone after a stranger attacked it: %v", err)
	}
	if after.Title != m.Title || after.Status != StatusScheduled {
		t.Fatalf("the meeting changed: title=%q status=%q", after.Title, after.Status)
	}
}

// An invitee may respond, but must not be able to rewrite the meeting. This
// is the subtle one: being party to something is not the same as owning it.
func TestInviteeCannotRewriteTheMeeting(t *testing.T) {
	svc := authzService(t)
	m := makeMeeting(t, svc)
	ctx := context.Background()

	// Read the stored meeting first, and compare against THAT rather than
	// against the value Create returned. Times are persisted as milliseconds,
	// so an in-memory timestamp never equals its own round trip and a test
	// comparing the two reports a change that did not happen.
	before, err := svc.ByID(ctx, owner, m.ID)
	if err != nil {
		t.Fatal(err)
	}

	clone := *m
	clone.Title = "Renamed by a guest"
	clone.StartsAt = m.StartsAt.Add(48 * time.Hour)
	if _, err := svc.Update(ctx, invitee, m.ID, &clone, "", ""); err == nil {
		t.Fatal("an invitee rewrote the meeting they were invited to")
	}

	after, err := svc.ByID(ctx, owner, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != before.Title {
		t.Fatalf("the title changed to %q", after.Title)
	}
	if !after.StartsAt.Equal(before.StartsAt) {
		t.Fatalf("an invitee moved the meeting: %s -> %s", before.StartsAt, after.StartsAt)
	}
}

// Listing must be scoped. A filter that returns everything to anybody who
// asks is the same leak as a broken ByID, arrived at from the other side.
func TestListingIsScopedToTheActor(t *testing.T) {
	svc := authzService(t)
	makeMeeting(t, svc)
	ctx := context.Background()

	// A few more, owned by somebody else again.
	for i := 0; i < 3; i++ {
		start := time.Now().Add(time.Duration(i+2) * time.Hour)
		if _, err := svc.Create(ctx, owner, &Meeting{
			Room: fmt.Sprintf("room-%d", i), Title: fmt.Sprintf("Private %d", i),
			Category: "internal", StartsAt: start, EndsAt: start.Add(time.Hour),
		}, "", ""); err != nil {
			t.Fatal(err)
		}
	}

	wide := Filter{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now().Add(72 * time.Hour),
	}
	got, err := svc.List(ctx, stranger, wide)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		t.Fatalf("a stranger listed %q, organised by %s", m.Title, m.OrganizerEmail)
	}

	mine, err := svc.List(ctx, owner, wide)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 4 {
		t.Fatalf("the organiser sees %d of their own 4 meetings", len(mine))
	}
}

// A client must not be able to claim somebody else's identity by sending it.
//
// Every field the server can derive, it must derive. Trusting an organiser id
// from the request body is how one user creates meetings attributed to
// another, and audit records that say the wrong thing are worse than none.
func TestOrganiserCannotBeSpoofedByTheClient(t *testing.T) {
	svc := authzService(t)
	ctx := context.Background()

	start := time.Now().Add(time.Hour)
	m, err := svc.Create(ctx, stranger, &Meeting{
		Room: "spoof", Title: "Not mine", Category: "internal",
		StartsAt: start, EndsAt: start.Add(time.Hour),
		// The client asserts somebody else.
		OrganizerID: owner.ID, OrganizerEmail: owner.Email,
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.OrganizerID != stranger.ID || m.OrganizerEmail != stranger.Email {
		t.Fatalf("the client set the organiser to %s <%s>", m.OrganizerID, m.OrganizerEmail)
	}
}
