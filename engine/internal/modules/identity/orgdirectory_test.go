package identity

import (
	"context"
	"testing"
)

// The company address book.
//
// Inside one organisation, finding a colleague by typing their name is the
// entire point of having a directory, and making people remember addresses
// for it is the behaviour this exists to remove. The constraint is that it
// must not become the harvesting tool the old directory was: the organisation
// is a list the deployment declares, never "whatever domain the searcher
// happens to use", because on a server people sign up to with personal
// addresses that rule publishes every gmail.com account to every other.

func orgFixture(t *testing.T) (*Service, map[string]*User) {
	t.Helper()
	s, _ := testService(t)
	people := map[string]*User{}
	for _, e := range []string{
		"ambuj.kumar@ddecor.com",
		"priya.sharma@ddecor.com",
		"rahul.verma@ddecor.com",
		"divyanshi@gmail.com",
		"stranger@elsewhere.example",
	} {
		people[e] = mkUser(t, s, e, RoleMember)
	}
	// Names, as a real directory has.
	for email, name := range map[string]string{
		"ambuj.kumar@ddecor.com":     "Ambuj Kumar",
		"priya.sharma@ddecor.com":    "Priya Sharma",
		"rahul.verma@ddecor.com":     "Rahul Verma",
		"divyanshi@gmail.com":        "Divyanshi",
		"stranger@elsewhere.example": "Someone Else",
	} {
		u := people[email]
		u.DisplayName = name
		if err := s.repo.UpdateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	s.OrgDomains = func(ctx context.Context) []string { return []string{"ddecor.com"} }
	s.Contacts = func(ctx context.Context, id string) ([]string, error) { return nil, nil }
	return s, people
}

func TestColleaguesAreFoundByName(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]

	// The thing that was impossible before: a first name.
	got, err := s.SearchDirectory(ctx, me, "priya", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Email != "priya.sharma@ddecor.com" {
		t.Fatalf("searching \"priya\" returned %v", emails(got))
	}

	// A surname works too, and so does a partial one.
	if got, _ := s.SearchDirectory(ctx, me, "verma", 50); len(got) != 1 {
		t.Fatalf("searching a surname returned %v", emails(got))
	}
	if got, _ := s.SearchDirectory(ctx, me, "ver", 50); len(got) != 1 {
		t.Fatalf("searching a partial surname returned %v", emails(got))
	}

	// And the whole team is browsable, minus yourself.
	all, _ := s.SearchDirectory(ctx, me, "", 50)
	if len(all) != 2 {
		t.Fatalf("the address book holds %v; want the two colleagues", emails(all))
	}
}

// The organisation is a boundary, not just an ordering. Somebody outside it is
// not in the address book however their name is spelled.
func TestTheAddressBookStopsAtTheOrganisation(t *testing.T) {
	s, people := orgFixture(t)
	me := people["ambuj.kumar@ddecor.com"]

	for _, q := range []string{"divyanshi", "someone", "else", "gmail", "@", "e", "elsewhere.example"} {
		for _, e := range emails(mustSearch(t, s, me, q)) {
			if e == "divyanshi@gmail.com" || e == "stranger@elsewhere.example" {
				t.Errorf("query %q reached outside the organisation: %s", q, e)
			}
		}
	}

	// But a full address still reaches them, which is how you talk to a
	// customer or a contractor.
	got := mustSearch(t, s, me, "divyanshi@gmail.com")
	if len(got) != 1 || got[0].Email != "divyanshi@gmail.com" {
		t.Fatalf("a full outside address returned %v", emails(got))
	}
}

// Somebody signed in with a personal address on a company deployment is a
// guest. The staff list is not theirs to browse.
func TestAnOutsiderDoesNotGetTheAddressBook(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	guest := people["divyanshi@gmail.com"]

	for _, q := range []string{"", "ambuj", "priya", "ddecor.com", "@ddecor.com", "a"} {
		if got := mustSearch(t, s, guest, q); len(got) != 0 {
			t.Errorf("a guest searching %q saw %v", q, emails(got))
		}
	}

	// They can still reach a colleague whose address they were given.
	got := mustSearch(t, s, guest, "ambuj.kumar@ddecor.com")
	if len(got) != 1 {
		t.Fatalf("a guest could not reach a known address: %v", emails(got))
	}

	// And anybody they already talk to.
	s.Contacts = func(ctx context.Context, id string) ([]string, error) {
		if id == guest.ID {
			return []string{people["ambuj.kumar@ddecor.com"].ID}, nil
		}
		return nil, nil
	}
	if got := mustSearch(t, s, guest, "ambuj"); len(got) != 1 {
		t.Fatalf("a guest could not search their own contacts: %v", emails(got))
	}
	_ = ctx
}

// With no organisation declared -- the default -- nothing changes from the
// contacts-only behaviour. A deployment that has never opened the console
// does not accidentally publish itself.
func TestNoOrganisationDeclaredMeansNoAddressBook(t *testing.T) {
	s, people := orgFixture(t)
	s.OrgDomains = nil
	me := people["ambuj.kumar@ddecor.com"]
	if got := mustSearch(t, s, me, "priya"); len(got) != 0 {
		t.Fatalf("with no organisation configured, searching a name returned %v", emails(got))
	}

	s.OrgDomains = func(ctx context.Context) []string { return nil }
	if got := mustSearch(t, s, me, ""); len(got) != 0 {
		t.Fatalf("an empty organisation list returned %v", emails(got))
	}
}

// A colleague appears once, not twice, when they are also a contact.
func TestAColleagueWhoIsAlsoAContactAppearsOnce(t *testing.T) {
	s, people := orgFixture(t)
	priya := people["priya.sharma@ddecor.com"]
	s.Contacts = func(ctx context.Context, id string) ([]string, error) {
		return []string{priya.ID}, nil
	}
	got := mustSearch(t, s, people["ambuj.kumar@ddecor.com"], "priya")
	if len(got) != 1 {
		t.Fatalf("a colleague who is also a contact appeared %d times: %v", len(got), emails(got))
	}
}

func mustSearch(t *testing.T, s *Service, actor *User, q string) []DirectoryEntry {
	t.Helper()
	got, err := s.SearchDirectory(context.Background(), actor, q, 50)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return got
}

// The strictest scope: colleagues and nobody else.
//
// This is what a locked-down company tenant does, and it is what the setting
// has to mean if it is to be worth having: an outside address typed into the
// search box finds nothing at all, rather than finding somebody the policy
// then refuses to let you message.
func TestUnderOrganisationScopeAnOutsideAddressFindsNothing(t *testing.T) {
	s, people := orgFixture(t)
	me := people["ambuj.kumar@ddecor.com"]
	s.DirectoryScope = func(ctx context.Context) string { return "organisation" }

	if got := mustSearch(t, s, me, "divyanshi@gmail.com"); len(got) != 0 {
		t.Fatalf("a full outside address still resolved: %v", emails(got))
	}
	// The address book itself is unaffected.
	if got := mustSearch(t, s, me, "priya"); len(got) != 1 {
		t.Fatalf("colleagues became unreachable too: %v", emails(got))
	}
}

// The same rule, at the point a conversation is actually created. A policy the
// search box obeys and the API ignores is a label, not a policy.
func TestMayMessageIsTheSameAnswerAsTheDirectory(t *testing.T) {
	s, _ := orgFixture(t)
	ctx := context.Background()
	s.DirectoryScope = func(ctx context.Context) string { return "organisation" }

	inside, outside := "ambuj.kumar@ddecor.com", "divyanshi@gmail.com"

	if !s.MayMessage(ctx, inside, "priya.sharma@ddecor.com") {
		t.Error("two colleagues were refused")
	}
	if s.MayMessage(ctx, inside, outside) {
		t.Error("an insider was allowed to reach outside")
	}
	// Symmetric. A one-way restriction is not one.
	if s.MayMessage(ctx, outside, inside) {
		t.Error("an outsider was allowed to reach in")
	}
	if s.MayMessage(ctx, outside, "other@gmail.com") {
		t.Error("two outsiders were allowed to use this deployment to talk")
	}
}

// The looser rungs are unchanged, so turning this on is a decision and leaving
// it alone costs nothing.
func TestTheLooserScopesStillReachOutside(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]

	for _, scope := range []string{"connections", "everyone", ""} {
		s.DirectoryScope = func(ctx context.Context) string { return scope }
		if !s.MayMessage(ctx, me.Email, "divyanshi@gmail.com") {
			t.Errorf("scope %q refused an outside conversation", scope)
		}
	}

	s.DirectoryScope = func(ctx context.Context) string { return "connections" }
	if got := mustSearch(t, s, me, "divyanshi@gmail.com"); len(got) != 1 {
		t.Fatalf("the default scope stopped resolving a full address: %v", emails(got))
	}
}

// The strictest scope with no organisation declared has no "inside", so it
// permits nothing new. Fail closed, not open.
func TestOrganisationScopeWithNoDomainsPermitsNothingNew(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	s.DirectoryScope = func(ctx context.Context) string { return "organisation" }
	s.OrgDomains = func(ctx context.Context) []string { return nil }

	me := people["ambuj.kumar@ddecor.com"]
	if s.MayMessage(ctx, me.Email, "priya.sharma@ddecor.com") {
		t.Error("with no organisation declared, a new conversation was still permitted")
	}
	// And an outside address is no more reachable than an inside one.
	if s.MayMessage(ctx, me.Email, "divyanshi@gmail.com") {
		t.Error("with no organisation declared, an outside conversation was permitted")
	}
}
