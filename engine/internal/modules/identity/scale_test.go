package identity

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// Scale: the directory at fifty thousand people.
//
// Every keystroke in the people search is one of these queries. At an
// organisation of this size the whole company shares one domain, so the
// domain filter narrows nothing and the name match does all the work. The
// threshold is what a keystroke can afford. Skipped under -short.
//
//   go test -run Scale -v ./internal/modules/identity/

func TestScaleDirectorySearchAtFiftyThousand(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("scale test; run without -short and without -race (timings only)")
	}
	s, _ := testService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)

	const people = 50_000
	first := []string{"Ambuj", "Priya", "Rahul", "Divyanshi", "Neha", "Arjun", "Kavya", "Rohan", "Sneha", "Vikram", "Ananya", "Karan"}
	last := []string{"Kumar", "Sharma", "Verma", "Singh", "Gupta", "Mehta", "Patel", "Reddy", "Nair", "Iyer", "Das", "Bose"}

	start := time.Now()
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO identity_users
		(id, email, display_name, role, status, password_hash, created_at, updated_at, failed_logins)
		VALUES (?,?,?,?,?,?,?,?,0)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < people; i++ {
		name := fmt.Sprintf("%s %s", first[i%len(first)], last[(i/len(first))%len(last)])
		email := fmt.Sprintf("%s.%s.%d@ddecor.com", first[i%len(first)], last[(i/len(first))%len(last)], i)
		if _, err := ins.ExecContext(ctx, fmt.Sprintf("u%06d", i), email, name,
			string(RoleMember), string(StatusActive), "x", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d people in %s", people, time.Since(start).Round(time.Millisecond))

	me := mkUser(t, s, "me@ddecor.com", RoleMember)
	s.OrgDomains = func(ctx context.Context) []string { return []string{"ddecor.com"} }
	s.Contacts = func(ctx context.Context, id string) ([]string, error) { return nil, nil }

	measure := func(name string, runs int, fn func(i int)) {
		fn(0)
		ds := make([]time.Duration, 0, runs)
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			fn(i)
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		p50, p95 := ds[len(ds)/2], ds[len(ds)*95/100]
		t.Logf("%-40s p50 %8s  p95 %8s  max %8s", name, p50, p95, ds[len(ds)-1])
		// Measured at 0.5 ms p95 for every shape below with the staged
		// indexed query (LIKE '%q%' was 28 ms; the first FTS attempt 100 ms
		// because the planner drove from the domain index). Three
		// milliseconds is the alarm, not the target.
		if p95 > 3*time.Millisecond {
			t.Errorf("%s: p95 %s -- a scan, not an index", name, p95)
		}
	}

	measure("common substring (\"sha\", thousands match)", 40, func(int) {
		got, err := s.SearchDirectory(ctx, me, "sha", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 20 {
			t.Fatalf("%d results for a substring thousands of names contain", len(got))
		}
	})

	measure("first-name prefix (\"pri\")", 40, func(int) {
		got, err := s.SearchDirectory(ctx, me, "pri", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 20 {
			t.Fatalf("%d results, want the page of 20", len(got))
		}
	})
	measure("rare full name (\"Karan Bose\")", 40, func(int) {
		got, err := s.SearchDirectory(ctx, me, "Karan Bose", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatal("no results for a name that exists")
		}
	})
	measure("no match (\"zzzz\")", 40, func(int) {
		if got, _ := s.SearchDirectory(ctx, me, "zzzz", 20); len(got) != 0 {
			t.Fatalf("%d results for a name nobody has", len(got))
		}
	})
	measure("one letter (\"a\")", 40, func(int) {
		if got, _ := s.SearchDirectory(ctx, me, "a", 20); len(got) != 20 {
			t.Fatal("a one-letter search should fill the page")
		}
	})
	measure("outsider by full address", 40, func(int) {
		if _, err := s.SearchDirectory(ctx, me, "nobody@elsewhere.example", 20); err != nil {
			t.Fatal(err)
		}
	})
}
