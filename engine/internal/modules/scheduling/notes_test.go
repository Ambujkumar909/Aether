package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func notesFixture(t *testing.T) (*Service, *[]map[string]any) {
	t.Helper()
	s, _ := qnaFixture(t) // room "r": host u-host, attendees u-a, u-b; u-x outside
	var log []map[string]any
	s.NotesPush = func(room string, body []byte) {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		log = append(log, m)
	}
	return s, &log
}

func TestNotesCreateAndSections(t *testing.T) {
	s, log := notesFixture(t)
	ctx := context.Background()
	host, a, x := Actor{ID: "u-host"}, Actor{ID: "u-a"}, Actor{ID: "u-x"}

	if created, _, err := s.Notes(ctx, a, "r"); err != nil || created {
		t.Fatalf("before creation: created=%v err=%v", created, err)
	}
	if _, err := s.AddNote(ctx, a, "r", "notes", "too early"); !errors.Is(err, ErrNotesNone) {
		t.Fatalf("added before the organizer created the notes: %v", err)
	}
	if err := s.CreateNotes(ctx, a, "r"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an attendee created the notes: %v", err)
	}
	if _, _, err := s.Notes(ctx, x, "r"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("someone outside read them: %v", err)
	}
	if err := s.CreateNotes(ctx, host, "r"); err != nil {
		t.Fatal(err)
	}
	if (*log)[len(*log)-1]["created"] != true {
		t.Fatal("creation was not pushed")
	}
	if _, err := s.AddNote(ctx, a, "r", "gossip", "x"); !errors.Is(err, ErrNoteSection) {
		t.Fatalf("unknown section: %v", err)
	}
	for _, sec := range []string{"agenda", "notes", "tasks"} {
		if _, err := s.AddNote(ctx, a, "r", sec, "item in "+sec); err != nil {
			t.Fatalf("%s: %v", sec, err)
		}
	}
	if _, err := s.AddNote(ctx, x, "r", "notes", "outsider"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("someone outside added: %v", err)
	}
	if _, err := s.AddNote(ctx, a, "r", "notes", strings.Repeat("x", MaxNoteLen+1)); !errors.Is(err, ErrNoteBody) {
		t.Fatalf("2001 characters: %v", err)
	}
	if _, err := s.AddNote(ctx, a, "r", "notes", strings.Repeat("é", MaxNoteLen)); err != nil {
		t.Fatalf("2000 characters refused: %v", err)
	}
	_, items, _ := s.Notes(ctx, a, "r")
	if len(items) != 4 || items[0].Section != "agenda" || items[0].Version != 1 {
		t.Fatalf("items %+v", items)
	}
	if last := (*log)[len(*log)-1]; last["item"] == nil {
		t.Fatal("an added item was not pushed")
	}
}

// Two people editing the same item: the second is refused with what the
// first wrote, not silently overwritten.
func TestNotesConcurrentEdits(t *testing.T) {
	s, log := notesFixture(t)
	ctx := context.Background()
	host, a, b := Actor{ID: "u-host"}, Actor{ID: "u-a"}, Actor{ID: "u-b"}
	s.CreateNotes(ctx, host, "r")
	it, _ := s.AddNote(ctx, a, "r", "tasks", "Send the deck")

	one, two := "Send the deck to finance", "Send the deck to legal"
	got, err := s.EditNote(ctx, a, "r", it.ID, NoteEdit{Body: &one, Version: 1})
	if err != nil || got.Version != 2 || got.Body != one {
		t.Fatalf("first edit: %+v %v", got, err)
	}
	cur, err := s.EditNote(ctx, b, "r", it.ID, NoteEdit{Body: &two, Version: 1})
	if !errors.Is(err, ErrNoteConflict) || cur == nil || cur.Body != one {
		t.Fatalf("second edit at the old version: %+v %v", cur, err)
	}
	// Ticking off and assigning, at the right version.
	done, who := true, "Asha"
	got, err = s.EditNote(ctx, b, "r", it.ID, NoteEdit{Done: &done, Assignee: &who, Version: 2})
	if err != nil || !got.Done || got.Assignee != "Asha" || got.Body != one {
		t.Fatalf("tick and assign: %+v %v", got, err)
	}
	long := strings.Repeat("x", MaxAssigneeLen+1)
	if _, err := s.EditNote(ctx, b, "r", it.ID, NoteEdit{Assignee: &long, Version: 3}); !errors.Is(err, ErrNoteBody) {
		t.Fatalf("101-character assignee: %v", err)
	}
	// Delete at a stale version: refused; at the current one: gone, and pushed.
	if err := s.DeleteNote(ctx, a, "r", it.ID, 2); !errors.Is(err, ErrNoteConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.DeleteNote(ctx, a, "r", it.ID, 3); err != nil {
		t.Fatal(err)
	}
	last := (*log)[len(*log)-1]["item"].(map[string]any)
	if last["deleted"] != true || last["id"] != it.ID {
		t.Fatalf("delete push %+v", last)
	}
	if err := s.DeleteNote(ctx, a, "r", it.ID, 3); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
	if _, err := s.EditNote(ctx, a, "r", it.ID, NoteEdit{Body: &one, Version: 3}); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("editing a deleted item: %v", err)
	}
}

func TestNotesBoundsAndIndex(t *testing.T) {
	s, _ := notesFixture(t)
	ctx := context.Background()
	s.CreateNotes(ctx, Actor{ID: "u-host"}, "r")
	repo := s.repo.(*SQLRepository)
	now := time.Now()
	for i := 0; i < MaxNoteItems; i++ {
		if err := repo.AddNoteItem(ctx, "r", &NoteItem{ID: fmt.Sprint("n", i), Section: "notes", Body: "x", UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddNote(ctx, Actor{ID: "u-a"}, "r", "notes", "one more"); !errors.Is(err, ErrNotesFull) {
		t.Fatalf("item 501: %v", err)
	}
	rows, _ := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM meeting_note_items WHERE room = 'r' ORDER BY created_at, id`)
	plan := ""
	for rows.Next() {
		var a, b, c int
		var d string
		_ = rows.Scan(&a, &b, &c, &d)
		plan += d + "; "
	}
	rows.Close()
	if !strings.Contains(plan, "meeting_note_items_room") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("not index-backed: %s", plan)
	}
}
