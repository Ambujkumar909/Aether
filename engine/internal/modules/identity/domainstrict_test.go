package identity

import (
	"context"
	"testing"
)

// Two domains that look related are two organisations.
//
// ddecor.com and ddecor.in share a label and nothing else: they can be two
// companies, two legal entities, or one company and somebody who registered
// the other ccTLD. Treating them as one because the middle word matches would
// hand a stranger the staff list on the strength of a domain registration,
// which is not a credential. Only what an administrator has explicitly listed
// counts, and this pins that.

func strictFixture(t *testing.T, listed ...string) (*Service, map[string]*User) {
	t.Helper()
	s, _ := testService(t)
	people := map[string]*User{}
	for _, e := range []string{
		"ambuj@ddecor.com",
		"priya@ddecor.com",
		"someone@ddecor.in",
		"other@ddecor.co.uk",
		"third@notddecor.com",
	} {
		people[e] = mkUser(t, s, e, RoleMember)
	}
	s.OrgDomains = func(ctx context.Context) []string { return listed }
	s.Contacts = func(ctx context.Context, id string) ([]string, error) { return nil, nil }
	s.DirectoryScope = func(ctx context.Context) string { return "connections" }
	return s, people
}

func TestARelatedLookingDomainIsADifferentOrganisation(t *testing.T) {
	s, people := strictFixture(t, "ddecor.com")
	me := people["ambuj@ddecor.com"]

	got := mustSearch(t, s, me, "")
	if len(got) != 1 || got[0].Email != "priya@ddecor.com" {
		t.Fatalf("the address book holds %v; want only the listed domain", emails(got))
	}

	// None of these is ddecor.com, however much they look like it.
	for _, e := range emails(mustSearch(t, s, me, "e")) {
		if e != "priya@ddecor.com" {
			t.Errorf("a search reached %s, which is not in the listed domain", e)
		}
	}
}

// A suffix is not a match either: ddecor.com must not swallow x.ddecor.com,
// and notddecor.com must not be swallowed by ddecor.com.
func TestDomainsMatchWholly(t *testing.T) {
	ctx := context.Background()
	s, people := strictFixture(t, "ddecor.com")

	cases := []struct {
		email string
		in    bool
	}{
		{"ambuj@ddecor.com", true},
		{"someone@ddecor.in", false},
		{"other@ddecor.co.uk", false},
		{"third@notddecor.com", false},
		{"nobody@mail.ddecor.com", false},
	}
	for _, c := range cases {
		got := s.MayMessage(ctx, "ambuj@ddecor.com", c.email)
		// Under "connections" everything is reachable, so ask the
		// organisation question directly instead.
		s.DirectoryScope = func(ctx context.Context) string { return "organisation" }
		got = s.MayMessage(ctx, "ambuj@ddecor.com", c.email)
		s.DirectoryScope = func(ctx context.Context) string { return "connections" }
		if got != c.in {
			t.Errorf("%s in organisation = %v, want %v", c.email, got, c.in)
		}
	}
	_ = people
}

// Listing both is a decision an administrator makes, and then they are one.
func TestTwoDomainsBecomeOneOnlyWhenBothAreListed(t *testing.T) {
	s, people := strictFixture(t, "ddecor.com", "ddecor.in")
	me := people["ambuj@ddecor.com"]
	got := emails(mustSearch(t, s, me, ""))
	if len(got) != 2 {
		t.Fatalf("with both listed the book holds %v; want the two colleagues", got)
	}
}
