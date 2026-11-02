package scheduling

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// "Choose co-organizers": from the invitees, at most ten, stored and read
// back, never the organizer, and recognised on arrival until cancelled.
func TestCoOrganizers(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	org := Actor{ID: "u-org", Email: "org@x"}
	start := time.Now().Add(time.Hour)
	mk := func(invitees []Invitee) (*Meeting, error) {
		return s.Create(ctx, org, &Meeting{Title: "Plan", StartsAt: start, EndsAt: start.Add(time.Hour), Invitees: invitees}, "", "")
	}

	// Named but not invited: refused.
	if _, err := mk([]Invitee{{Email: "a@x"}, {Email: "ghost@x", CoOrganizer: true, Response: respNotInvited}}); !errors.Is(err, ErrCoOrganizer) {
		t.Fatalf("an uninvited co-organizer: %v", err)
	}
	// Eleven: refused; ten: fine.
	many := func(n int) []Invitee {
		var out []Invitee
		for i := 0; i < n; i++ {
			out = append(out, Invitee{Email: fmt.Sprintf("p%d@x", i), CoOrganizer: true})
		}
		return out
	}
	if _, err := mk(many(MaxCoOrganizers + 1)); !errors.Is(err, ErrCoOrganizer) {
		t.Fatalf("eleven co-organizers: %v", err)
	}
	if _, err := mk(many(MaxCoOrganizers)); err != nil {
		t.Fatalf("ten co-organizers refused: %v", err)
	}

	// co@x listed twice, once as co-organizer: the choice survives the merge.
	m, err := mk([]Invitee{{Email: "Co@X", CoOrganizer: true}, {Email: "plain@x"}, {Email: "co@x"}, {Email: "org@x", CoOrganizer: true}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ByID(ctx, org, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, inv := range got.Invitees {
		flags[inv.Email] = inv.CoOrganizer
	}
	if !flags["co@x"] || flags["plain@x"] || flags["org@x"] {
		t.Fatalf("stored flags %v: want co@x only (the organizer is not also a co-organizer)", flags)
	}
	if !s.CoOrganizerOf(ctx, m.Room, "", "co@x") || s.CoOrganizerOf(ctx, m.Room, "", "plain@x") || s.CoOrganizerOf(ctx, m.Room, "", "") {
		t.Fatal("arrival check wrong")
	}
	// An edit that keeps the invitees keeps the choice.
	upd, err := s.Update(ctx, org, m.ID, &Meeting{Title: "Plan 2", StartsAt: start, EndsAt: start.Add(time.Hour), Invitees: got.Invitees}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.CoOrganizerOf(ctx, upd.Room, "", "co@x") {
		t.Fatal("an edit dropped the co-organizer")
	}
	if err := s.Cancel(ctx, org, m.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if s.CoOrganizerOf(ctx, m.Room, "", "co@x") {
		t.Fatal("a cancelled meeting still makes co-organizers")
	}
}
