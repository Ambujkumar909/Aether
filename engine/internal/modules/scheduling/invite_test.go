package scheduling

import (
	"context"
	"testing"
	"time"
)

// The invitation is the permission. These run against real SQLite because the
// thing under test is the SQL: the case-insensitive address match, the
// organiser-or-invitee disjunction, the cancelled-meeting exclusion.

func invitedFixture(t *testing.T) (*Service, *Meeting) {
	t.Helper()
	s := attendanceService(t)
	start := time.Now().Add(-time.Minute)
	m, err := s.Create(context.Background(),
		Actor{ID: "u-organiser", Email: "Boss@Example.com"},
		&Meeting{
			Title: "Design review", Category: CatInternal,
			StartsAt: start, EndsAt: start.Add(time.Hour),
			Admission: AdmissionKnock,
			Invitees: []Invitee{
				{Email: "guest@example.com", UserID: "u-guest"},
				{Email: "Only.By.Mail@Example.com"},
			},
		}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestAnInvitationLetsYouIn(t *testing.T) {
	s, m := invitedFixture(t)
	ctx := context.Background()

	cases := []struct {
		what   string
		userID string
		email  string
		want   bool
	}{
		{"the organiser by account", "u-organiser", "", true},
		{"the organiser by address, cased differently", "", "boss@EXAMPLE.com", true},
		// An invitation names an address, never an account: normaliseInvitees
		// drops the id on purpose, because you invite people who have not
		// signed up yet. So an id alone matches nobody, and the join path
		// hands over the session's address alongside it, which does.
		{"an invitee by account alone", "u-guest", "", false},
		{"an invitee by address", "", "guest@example.com", true},
		{"a signed-in invitee, id and address", "u-guest", "guest@example.com", true},
		{"an invitee who has no account yet", "", "only.by.mail@example.com", true},
		{"somebody holding only the link", "u-stranger", "stranger@example.com", false},
		{"nobody at all", "", "", false},
	}
	for _, c := range cases {
		if got := s.InvitedToRoom(ctx, m.Room, c.userID, c.email); got != c.want {
			t.Errorf("%s: invited=%v, want %v", c.what, got, c.want)
		}
	}

	// A different room's invitation is not this room's.
	if s.InvitedToRoom(ctx, "some-other-room", "u-guest", "guest@example.com") {
		t.Error("an invitation to one meeting opened another")
	}
}

// A cancelled meeting invites nobody. The link still resolves, the invitee
// list is still on disk, and neither is permission to walk into whatever is
// happening in that room now.
func TestACancelledMeetingInvitesNobody(t *testing.T) {
	s, m := invitedFixture(t)
	ctx := context.Background()
	if err := s.Cancel(ctx, Actor{ID: "u-organiser", Email: "Boss@Example.com"}, m.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if s.InvitedToRoom(ctx, m.Room, "u-guest", "guest@example.com") {
		t.Error("a cancelled meeting still admitted its invitee")
	}
	if s.InvitedToRoom(ctx, m.Room, "u-organiser", "") {
		t.Error("a cancelled meeting still admitted its organiser")
	}
}

// A meeting ends when the last person leaves, whatever the clock said. It
// does NOT end because the clock says so: an hour that runs to ninety minutes
// is one meeting, and nothing here cuts it off.
func TestTheLastPersonOutEndsTheMeeting(t *testing.T) {
	s, m := invitedFixture(t)
	ctx := context.Background()

	s.RoomEmptied(ctx, m.Room)

	after, err := s.repo.ByID(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusCompleted {
		t.Fatalf("status after everybody left = %q, want %q", after.Status, StatusCompleted)
	}
}

// Gathering early and dispersing is not holding the meeting. A meeting that
// has not started yet stays scheduled, or a mis-click on the link at 9 a.m.
// would quietly cancel the 3 p.m. review.
func TestAMeetingThatNeverStartedIsNotCompleted(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	m, err := s.Create(ctx, Actor{ID: "u-organiser", Email: "boss@example.com"},
		&Meeting{
			Title: "Later", Category: CatInternal,
			StartsAt: start, EndsAt: start.Add(time.Hour),
		}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.RoomEmptied(ctx, m.Room)
	after, _ := s.repo.ByID(ctx, m.ID)
	if after.Status != StatusScheduled {
		t.Fatalf("a meeting that had not started became %q", after.Status)
	}
}

// A cancelled meeting is not "completed" because somebody wandered into its
// room and left again.
func TestEmptyingARoomDoesNotUncancelAMeeting(t *testing.T) {
	s, m := invitedFixture(t)
	ctx := context.Background()
	if err := s.Cancel(ctx, Actor{ID: "u-organiser", Email: "Boss@Example.com"}, m.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	s.RoomEmptied(ctx, m.Room)
	after, _ := s.repo.ByID(ctx, m.ID)
	if after.Status != StatusCancelled {
		t.Fatalf("a cancelled meeting became %q", after.Status)
	}
}

// Live is occupancy, not the clock. A meeting an hour past its end time with
// people still in it is live; one inside its window with nobody in it is not.
func TestLiveFollowsTheRoomNotTheClock(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	occupied := map[string]int{}
	s.Occupancy = func(room string) int { return occupied[room] }

	start := time.Now().Add(-2 * time.Hour)
	over, err := s.Create(ctx, Actor{ID: "u-a", Email: "a@x"}, &Meeting{
		Title: "Overrunning", Category: CatInternal,
		StartsAt: start, EndsAt: start.Add(time.Hour),
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := s.Create(ctx, Actor{ID: "u-a", Email: "a@x"}, &Meeting{
		Title: "Nobody came", Category: CatInternal,
		StartsAt: time.Now().Add(-time.Minute), EndsAt: time.Now().Add(time.Hour),
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	occupied[over.Room] = 2

	ms, err := s.List(ctx, Actor{ID: "u-a", Email: "a@x"}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range ms {
		seen[m.ID] = m.Live
	}
	if !seen[over.ID] {
		t.Error("a meeting past its end with people in it is not live")
	}
	if seen[quiet.ID] {
		t.Error("an empty room inside its window reported live")
	}
}
