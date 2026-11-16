package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aether-rtc/aether/internal/modules/control"
	"github.com/aether-rtc/aether/internal/modules/messaging"
	"github.com/aether-rtc/aether/internal/modules/notify"
	"github.com/aether-rtc/aether/internal/modules/scheduling"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Every query a person's export runs, in every module, reaches that person's
// rows through an index and runs on the real schema: an export must not read
// a table of everybody's rows to find one person's, and a column renamed
// under one of these queries must fail here, not in somebody's download.
func TestEveryExportQueryRunsAndIsIndexBacked(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sections := messaging.PersonalSections("u")
	sections = append(sections, scheduling.PersonalSections("u", "u@example.com")...)
	sections = append(sections, notify.PersonalSections("u")...)
	sections = append(sections, control.PersonalSections("u")...)
	names := map[string]bool{}
	for _, sec := range sections {
		if names[sec.Name] {
			t.Fatalf("two sections are named %q: one would hide the other in the file", sec.Name)
		}
		names[sec.Name] = true
		rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+sec.Query, sec.Args...)
		if err != nil {
			t.Fatalf("%s does not run on the schema: %v", sec.Name, err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			plan += d + "; "
		}
		rows.Close()
		if strings.Contains(plan, "SCAN") {
			t.Fatalf("%s reads everybody's rows: %s", sec.Name, plan)
		}
	}
	for _, reserved := range []string{"exported_at", "about", "account", "sessions", "two_step_sign_in", "your_actions"} {
		if names[reserved] {
			t.Fatalf("a module names a section %q, which is the identity module's", reserved)
		}
	}
}
