package identity

import (
	"context"
	"testing"
)

// The directory as an index lookup. What these hold: a substring anywhere in
// a name or address is found, case-insensitively; one or two characters are a
// prefix; the index follows renames and removals without anybody remembering
// to update it; and nothing a person types can become a query operator.

func TestDirectoryFindsSubstringsAnywhere(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]

	for q, want := range map[string]string{
		"HARM":   "priya.sharma@ddecor.com", // middle of a surname, wrong case
		"iya sh": "priya.sharma@ddecor.com", // across the space
		"verma@": "rahul.verma@ddecor.com",  // the address
		"Rahul":  "rahul.verma@ddecor.com",
	} {
		got, err := s.SearchDirectory(ctx, me, q, 50)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(got) != 1 || got[0].Email != want {
			t.Errorf("searching %q returned %v, want [%s]", q, emails(got), want)
		}
	}
}

func TestShortQueriesArePrefixes(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]

	got, _ := s.SearchDirectory(ctx, me, "pr", 50)
	if len(got) != 1 || got[0].Email != "priya.sharma@ddecor.com" {
		t.Fatalf("\"pr\" returned %v, want Priya", emails(got))
	}
	// "ma" is inside Sharma and Verma and Kumar, but starts nobody's name or
	// address: a two-character query is a prefix, not a substring.
	if got, _ := s.SearchDirectory(ctx, me, "ma", 50); len(got) != 0 {
		t.Fatalf("\"ma\" returned %v, want nothing (prefix semantics below three characters)", emails(got))
	}
	// Case does not matter for a prefix either.
	if got, _ := s.SearchDirectory(ctx, me, "PR", 50); len(got) != 1 {
		t.Fatalf("\"PR\" returned %v", emails(got))
	}
}

func TestDirectoryIndexFollowsRenamesAndRemovals(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]
	u := people["rahul.verma@ddecor.com"]

	u.DisplayName = "Rahul Deshpande"
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SearchDirectory(ctx, me, "deshp", 50); len(got) != 1 {
		t.Fatalf("after a rename the new name is not found: %v", emails(got))
	}
	if got, _ := s.SearchDirectory(ctx, me, "verma", 50); len(got) != 0 {
		// The address still contains "verma", so search the old NAME only.
		_ = got
	}
	if got, _ := s.SearchDirectory(ctx, me, "Rahul Verma", 50); len(got) != 0 {
		t.Fatalf("after a rename the old name is still found: %v", emails(got))
	}

	if err := s.repo.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SearchDirectory(ctx, me, "deshp", 50); len(got) != 0 {
		t.Fatalf("a removed account is still found: %v", emails(got))
	}
}

func TestQueryTextCannotBecomeAnOperator(t *testing.T) {
	s, people := orgFixture(t)
	ctx := context.Background()
	me := people["ambuj.kumar@ddecor.com"]

	for _, q := range []string{
		`pri" OR "ver`, // a quote trying to close the phrase
		`priya*`,       // a prefix operator
		`NOT priya`,    // a boolean
		`"priya"`,      // already quoted
		`{name}: pri`,  // a column filter
		`(pri) AND (ver)`,
		"pri\x00ya",
		"日本語の名前",
	} {
		got, err := s.SearchDirectory(ctx, me, q, 50)
		if err != nil {
			t.Fatalf("%q: a typed query must never error: %v", q, err)
		}
		// Whatever comes back, it came back through the phrase, not an
		// operator: nothing here should widen to the whole directory.
		if len(got) >= 3 {
			t.Fatalf("%q widened the search to %v", q, emails(got))
		}
	}
}
