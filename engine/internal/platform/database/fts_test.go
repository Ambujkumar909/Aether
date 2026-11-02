package database

import (
	"context"
	"path/filepath"
	"testing"
)

// The directory search is built on FTS5 with the trigram tokenizer, so a
// substring of a name is an index lookup rather than a scan of every account.
// This test pins the dependency: if the SQLite build ever loses FTS5 or the
// tokenizer, it fails here with a plain message rather than at first search.
func TestFTS5TrigramIsAvailable(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx,
		`CREATE VIRTUAL TABLE probe USING fts5(name, tokenize='trigram')`); err != nil {
		t.Fatalf("FTS5 with the trigram tokenizer is not available in this SQLite build: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO probe(name) VALUES ('Priya Sharma'), ('Rahul Verma')`); err != nil {
		t.Fatal(err)
	}
	var got string
	// A substring in the middle of a word, case-insensitive: what a person
	// types. Trigram matching is what makes this an index lookup.
	if err := db.QueryRowContext(ctx, `SELECT name FROM probe WHERE probe MATCH 'HARM'`).Scan(&got); err != nil {
		t.Fatalf("trigram substring match: %v", err)
	}
	if got != "Priya Sharma" {
		t.Fatalf("got %q", got)
	}
}
