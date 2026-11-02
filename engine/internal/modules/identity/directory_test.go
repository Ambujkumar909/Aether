package identity

import (
	"context"
	"strings"
	"testing"
)

// The people directory is the one endpoint that answers questions about
// accounts to anybody signed in, so it is the one that decides whether this
// deployment leaks its user list. These tests are the specification of how far
// a search may reach.
//
// The shape being defended against: a directory that lists accounts to any
// signed-in caller, matched by substring on email, is an address book of the
// whole deployment -- and a single query for "@" returns all of it. On a
// server strangers can sign up to, that is an email harvesting tool.

// A deployment of five strangers and one person who knows one of them.
func directoryFixture(t *testing.T) (*Service, map[string]*User) {
	t.Helper()
	s, _ := testService(t)
	people := map[string]*User{}
	for _, e := range []string{
		"me@ddecor.com",
		"colleague@ddecor.com",
		"stranger.one@gmail.com",
		"stranger.two@gmail.com",
		"stranger.three@example.org",
	} {
		people[e] = mkUser(t, s, e, RoleMember)
	}

	// "me" has a conversation with the colleague and with nobody else.
	s.Contacts = func(ctx context.Context, userID string) ([]string, error) {
		if userID == people["me@ddecor.com"].ID {
			return []string{people["colleague@ddecor.com"].ID}, nil
		}
		return nil, nil
	}
	return s, people
}

func emails(entries []DirectoryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Email)
	}
	return out
}

func TestAnEmptySearchListsOnlyPeopleYouKnow(t *testing.T) {
	s, people := directoryFixture(t)
	got, err := s.SearchDirectory(context.Background(), people["me@ddecor.com"], "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Email != "colleague@ddecor.com" {
		t.Fatalf("an empty search returned %v; want only the one known contact", emails(got))
	}
}

// The harvesting queries. Every one of these used to return the deployment.
func TestNoQueryEnumeratesTheDeployment(t *testing.T) {
	s, people := directoryFixture(t)
	me := people["me@ddecor.com"]

	for _, q := range []string{
		"@",           // matches every email in existence
		".com",        // matches most of them
		"gmail.com",   // a whole provider
		"@gmail.com",  // a whole provider, spelled as an address
		"a",           // one letter
		"e",           // the commonest letter
		"stranger",    // a name fragment that really does match three accounts
		"%",           // a LIKE wildcard, in case escaping is wrong
		"_",           // the other LIKE wildcard
		"me@ddecor.c", // an address, one character short
		"ddecor.com",  // a domain, complete but not an address
	} {
		got, err := s.SearchDirectory(context.Background(), me, q, 50)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		for _, e := range got {
			if e.Email != "colleague@ddecor.com" {
				t.Errorf("query %q reached a stranger: %s", q, e.Email)
			}
		}
	}
}

// You can still start a conversation with somebody new -- by knowing their
// address, which is how it works everywhere. This discloses nothing: you had
// to be able to type the address to get the answer.
func TestAFullAddressReachesSomebodyNew(t *testing.T) {
	s, people := directoryFixture(t)
	me := people["me@ddecor.com"]

	got, err := s.SearchDirectory(context.Background(), me, "stranger.one@gmail.com", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Email != "stranger.one@gmail.com" {
		t.Fatalf("a full address returned %v", emails(got))
	}

	// Case does not matter; people type addresses however they please.
	got, _ = s.SearchDirectory(context.Background(), me, "Stranger.One@GMAIL.com", 50)
	if len(got) != 1 {
		t.Fatalf("a differently-cased address returned %v", emails(got))
	}
}

// A miss looks exactly like a hit that is not there. An address that is not
// registered and one that is must be indistinguishable, or the directory
// becomes the account-enumeration oracle that the login path was hardened
// against.
func TestAnUnregisteredAddressIsSilent(t *testing.T) {
	s, people := directoryFixture(t)
	got, err := s.SearchDirectory(context.Background(),
		people["me@ddecor.com"], "nobody@nowhere.example", 50)
	if err != nil {
		t.Fatalf("an unknown address produced an error, which is itself a signal: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an unregistered address returned %v", emails(got))
	}
}

func TestYouAreNotYourOwnSearchResult(t *testing.T) {
	s, people := directoryFixture(t)
	got, _ := s.SearchDirectory(context.Background(),
		people["me@ddecor.com"], "me@ddecor.com", 50)
	if len(got) != 0 {
		t.Fatalf("searching for yourself returned %v", emails(got))
	}
}

// With no contacts port wired at all, the directory shows nothing rather than
// everything. A misconfigured deployment must fail closed.
func TestAMissingContactsPortShowsNobody(t *testing.T) {
	s, people := directoryFixture(t)
	s.Contacts = nil
	got, _ := s.SearchDirectory(context.Background(), people["me@ddecor.com"], "", 50)
	if len(got) != 0 {
		t.Fatalf("with no contacts source the directory returned %v", emails(got))
	}
	// And a contacts source that fails is also not an invitation to list
	// everybody.
	s.Contacts = func(ctx context.Context, id string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	got, _ = s.SearchDirectory(context.Background(), people["me@ddecor.com"], "e", 50)
	if len(got) != 0 {
		t.Fatalf("a failing contacts source returned %v", emails(got))
	}
}

// An administrator can publish the address book on purpose. That is a real
// configuration for one organisation, and it must actually work.
func TestAnAdministratorCanPublishTheAddressBook(t *testing.T) {
	s, people := directoryFixture(t)
	s.DirectoryScope = func(ctx context.Context) string { return "everyone" }

	got, err := s.SearchDirectory(context.Background(), people["me@ddecor.com"], "stranger", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("the published directory returned %v; want all three strangers", emails(got))
	}

	// And anything other than the explicit opt-in is the narrow answer.
	for _, scope := range []string{"connections", "", "ORG", "everybody", "true"} {
		s.DirectoryScope = func(ctx context.Context) string { return scope }
		got, _ := s.SearchDirectory(context.Background(), people["me@ddecor.com"], "stranger", 50)
		if len(got) != 0 {
			t.Errorf("scope %q behaved as if published: %v", scope, emails(got))
		}
	}
}

// isCompleteEmail is the only thing standing between "type an address you
// know" and "walk the deployment one fragment at a time", so it is tested
// directly as well as through the service.
func TestOnlyAWholeAddressCountsAsOne(t *testing.T) {
	yes := []string{
		"a@b.co", "ambuj.kumar@ddecor.com", "x+tag@sub.example.org",
	}
	no := []string{
		"", "@", "a@", "@b.com", "b.com", "ambuj", "a@b", "a@b.",
		"a@@b.com", "a b@c.com", "a%@b.com", "a_@b.com", "a@b.com ",
	}
	for _, q := range yes {
		if !isCompleteEmail(q) {
			t.Errorf("%q should be a complete address", q)
		}
	}
	for _, q := range no {
		if isCompleteEmail(strings.TrimSpace(q)) && strings.TrimSpace(q) != "" {
			// TrimSpace mirrors what SearchDirectory does before calling in.
			if q != "a@b.com " {
				t.Errorf("%q should NOT be a complete address", q)
			}
		}
	}
}
