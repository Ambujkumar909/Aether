package database

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A person's sections come out as JSON a reader can parse, keys in column
// order, times as dates, a row at a time; an empty section is an empty list;
// a query that fails says which section and ends the file there.
func TestWriteSectionsIsJSONWithDatesAndStopsAtAFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE mine (owner TEXT, id TEXT, n INTEGER, created_at INTEGER, ends_at INTEGER, note TEXT, raw BLOB, score REAL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mine VALUES
		('me', 'a', 7, 1794800000000, 0, 'she said "hi"' || char(10) || '</script>', x'6869', 1.5),
		('me', 'b', NULL, 1794800001000, 1794800002000, '', NULL, 0),
		('you', 'c', 1, 1794800000000, 0, 'not mine', NULL, 0)`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.WriteString(`{"first":true`)
	err = db.WriteSections(ctx, &out, []Section{
		{Name: "things", Query: `SELECT id, n, created_at, ends_at, note, raw, score FROM mine WHERE owner = ? ORDER BY id`, Args: []any{"me"}},
		{Name: "none of these", Query: `SELECT id FROM mine WHERE owner = ?`, Args: []any{"nobody"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out.WriteString("}")
	var got struct {
		First  bool             `json:"first"`
		Things []map[string]any `json:"things"`
		None   []map[string]any `json:"none of these"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if !got.First || len(got.Things) != 2 || got.None == nil || len(got.None) != 0 {
		t.Fatalf("sections: %+v", got)
	}
	a, b := got.Things[0], got.Things[1]
	if a["id"] != "a" || a["n"] != float64(7) || a["created_at"] != "2026-11-16T03:33:20Z" || a["ends_at"] != nil ||
		a["note"] != "she said \"hi\"\n</script>" || a["raw"] != "hi" || a["score"] != 1.5 {
		t.Fatalf("first row: %+v", a)
	}
	if b["n"] != nil || b["ends_at"] != "2026-11-16T03:33:22Z" || b["raw"] != nil || b["note"] != "" {
		t.Fatalf("second row: %+v", b)
	}
	// Keys in the order of the query's columns, so a file reads the way the
	// section was meant.
	raw := out.String()
	if i, j, k := strings.Index(raw, `"id"`), strings.Index(raw, `"created_at"`), strings.Index(raw, `"score"`); !(i < j && j < k) {
		t.Fatalf("key order: %s", raw)
	}
	if strings.Contains(raw, "not mine") {
		t.Fatal("a row of somebody else's was written")
	}

	// A section that cannot be read: the error names it, and what was
	// written before it stays written (the caller ends the file unclosed).
	out.Reset()
	err = db.WriteSections(ctx, &out, []Section{
		{Name: "things", Query: `SELECT id FROM mine WHERE owner = ?`, Args: []any{"me"}},
		{Name: "broken", Query: `SELECT id FROM no_such_table`},
		{Name: "after", Query: `SELECT id FROM mine`},
	})
	if err == nil || !strings.Contains(err.Error(), "export broken") {
		t.Fatalf("a failing section: err=%v", err)
	}
	if !strings.Contains(out.String(), `"things":[`) || strings.Contains(out.String(), `"after"`) {
		t.Fatalf("after a failure: %s", out.String())
	}
}

// A large section is written row by row, not gathered first.
func TestWriteSectionsStreams(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE big (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		INSERT INTO big SELECT i, 'message number ' || i FROM n`); err != nil {
		t.Fatal(err)
	}
	w := &countingWriter{}
	if err := db.WriteSections(ctx, w, []Section{{Name: "big", Query: `SELECT id, body FROM big`}}); err != nil {
		t.Fatal(err)
	}
	if w.writes < 20000 {
		t.Fatalf("%d writes for 20,000 rows: the rows were gathered, not streamed", w.writes)
	}
	if w.largest > 256 {
		t.Fatalf("the largest single write was %d bytes: more than a row", w.largest)
	}
}

type countingWriter struct{ writes, largest int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	c.largest = max(c.largest, len(p))
	return len(p), nil
}
