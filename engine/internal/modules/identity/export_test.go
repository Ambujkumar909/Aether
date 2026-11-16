package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// The export is the caller's own, whole, and carries nothing that would let
// its reader sign in: no password hash, no session token or its hash, no
// two-step secret. Nothing of anybody else's is in it. It is recorded.
func TestExportIsTheCallersOwnAndCarriesNoSecret(t *testing.T) {
	s, au := testService(t)
	ctx := context.Background()
	me := mkUser(t, s, "me@example.com", RoleMember)
	mkUser(t, s, "other@example.com", RoleMember)
	_, myToken, _, err := s.Login(ctx, "me@example.com", "correct-horse-battery", "10.1.1.1", "my-browser")
	if err != nil {
		t.Fatal(err)
	}
	_, theirToken, _, err := s.Login(ctx, "other@example.com", "correct-horse-battery", "10.2.2.2", "their-browser")
	if err != nil {
		t.Fatal(err)
	}
	s.Personal = func(userID, email string) []database.Section {
		return []database.Section{{Name: "from_another_module", Query: `SELECT ? AS id, ? AS email`, Args: []any{userID, email}}}
	}
	var buf bytes.Buffer
	if err := s.Export(ctx, me, &buf, "10.1.1.1", "my-browser"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		ExportedAt string           `json:"exported_at"`
		About      string           `json:"about"`
		Account    []map[string]any `json:"account"`
		Sessions   []map[string]any `json:"sessions"`
		Actions    []map[string]any `json:"your_actions"`
		Other      []map[string]any `json:"from_another_module"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if at, err := time.Parse(time.RFC3339, got.ExportedAt); err != nil || time.Since(at) > time.Minute || got.About == "" {
		t.Fatalf("header: %q %q", got.ExportedAt, got.About)
	}
	if len(got.Account) != 1 || got.Account[0]["id"] != me.ID || got.Account[0]["email"] != "me@example.com" || got.Account[0]["role"] != string(RoleMember) {
		t.Fatalf("account: %+v", got.Account)
	}
	if len(got.Sessions) != 1 || got.Sessions[0]["ip"] != "10.1.1.1" || got.Sessions[0]["user_agent"] != "my-browser" {
		t.Fatalf("sessions: %+v", got.Sessions)
	}
	signedIn := false
	for _, a := range got.Actions {
		signedIn = signedIn || a["action"] == string(audit.ActionLoginSucceeded)
	}
	if !signedIn {
		t.Fatalf("my own sign-in is not among my actions: %+v", got.Actions)
	}
	if len(got.Other) != 1 || got.Other[0]["id"] != me.ID || got.Other[0]["email"] != "me@example.com" {
		t.Fatalf("the other modules were asked for somebody else: %+v", got.Other)
	}
	raw := buf.String()
	stored, _ := s.repo.UserByID(ctx, me.ID)
	for what, secret := range map[string]string{
		"my session token": myToken, "their session token": theirToken, "my password hash": stored.PasswordHash(),
		"a password hash column": "password_hash", "a token hash column": "token_hash", "an argon2 hash": "$argon2",
		"their address": "other@example.com", "their network address": "10.2.2.2", "their browser": "their-browser",
	} {
		if secret == "" {
			t.Fatalf("the test has no value for %s", what)
		}
		if strings.Contains(raw, secret) {
			t.Fatalf("the export contains %s", what)
		}
	}
	events, err := au.List(ctx, audit.Filter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, e := range events {
		recorded = recorded || (e.Action == audit.ActionDataExported && e.ActorID == me.ID && e.IP == "10.1.1.1")
	}
	if !recorded {
		t.Fatal("the export was not recorded")
	}
}

// This module's own sections reach their rows by an index on the person.
func TestOwnExportQueriesAreIndexBacked(t *testing.T) {
	s, _ := testService(t)
	db := s.repo.(*SQLRepository).db
	for _, sec := range s.personalSections(&User{ID: "u", Email: "u@example.com"}) {
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+sec.Query, sec.Args...)
		if err != nil {
			t.Fatalf("%s: %v", sec.Name, err)
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
}
