package scheduling

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Meeting notes, as Teams lays them out (strings from its package: "Meeting
// notes", "Agenda", "Notes", "Follow-up tasks", "Add meeting notes", "The
// organizer hasn't created meeting notes yet"). Teams keeps them in a shared
// Loop page; here they are three lists of items, stored with the meeting's
// room and pushed to the people in it.
//
// Items, not one text: two people editing different items never collide,
// and two editing the same item are caught by its version -- the second save
// is refused with the item as it now is, never silently overwritten
// (CLAUDE.md rule 7, accuracy). The organizer creates the notes; after that
// anyone in the meeting adds, edits, ticks off and removes items.

const (
	MaxNoteLen     = 2000
	MaxNoteItems   = 500 // a room's notes, all sections together
	MaxAssigneeLen = 100
)

var (
	ErrNotesNone    = errors.New("scheduling: the organizer hasn't created meeting notes yet")
	ErrNoteSection  = errors.New("scheduling: unknown notes section")
	ErrNoteBody     = errors.New("scheduling: a note must be 1 to 2000 characters")
	ErrNoteConflict = errors.New("scheduling: someone else changed this item")
	ErrNoteNotFound = errors.New("scheduling: no such item")
	ErrNotesFull    = errors.New("scheduling: these notes have reached their limit of items")
)

var noteSections = map[string]bool{"agenda": true, "notes": true, "tasks": true}

// NoteItem is one entry in a section.
type NoteItem struct {
	ID        string    `json:"id"`
	Section   string    `json:"section"` // agenda | notes | tasks
	Body      string    `json:"body"`
	Done      bool      `json:"done"`     // tasks
	Assignee  string    `json:"assignee"` // tasks: a name, as typed
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Deleted   bool      `json:"deleted,omitempty"` // only in a push
}

// NoteEdit is a change to an item; nil fields are unchanged.
type NoteEdit struct {
	Body     *string `json:"body"`
	Done     *bool   `json:"done"`
	Assignee *string `json:"assignee"`
	Version  int     `json:"version"`
}

// --- storage ----------------------------------------------------------------

func (r *SQLRepository) NotesCreated(ctx context.Context, room string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM meeting_notes WHERE room = ?`, room).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (r *SQLRepository) CreateNotes(ctx context.Context, room, by string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO meeting_notes (room, created_by, created_at) VALUES (?,?,?) ON CONFLICT(room) DO NOTHING`, room, by, at.UnixMilli())
	return err
}

func (r *SQLRepository) NoteItems(ctx context.Context, room, id string) ([]*NoteItem, error) {
	q := `SELECT id, section, body, done, assignee, version, updated_at FROM meeting_note_items WHERE room = ?`
	args := []any{room}
	if id != "" {
		q += ` AND id = ?`
		args = append(args, id)
	}
	q += ` ORDER BY created_at, id`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NoteItem
	for rows.Next() {
		var it NoteItem
		var up int64
		if err := rows.Scan(&it.ID, &it.Section, &it.Body, &it.Done, &it.Assignee, &it.Version, &up); err != nil {
			return nil, err
		}
		it.UpdatedAt = time.UnixMilli(up).UTC()
		out = append(out, &it)
	}
	return out, rows.Err()
}

func (r *SQLRepository) AddNoteItem(ctx context.Context, room string, it *NoteItem) error {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM meeting_note_items WHERE room = ?`, room).Scan(&n); err != nil {
		return err
	}
	if n >= MaxNoteItems {
		return ErrNotesFull
	}
	now := it.UpdatedAt.UnixMilli()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO meeting_note_items (id, room, section, body, done, assignee, version, created_at, updated_at)
		VALUES (?,?,?,?,0,'',1,?,?)`, it.ID, room, it.Section, it.Body, now, now)
	return err
}

// EditNoteItem applies an edit only if the item is still at the version the
// editor saw; the version goes up by one. ErrNoteConflict otherwise.
func (r *SQLRepository) EditNoteItem(ctx context.Context, room, id string, e NoteEdit, at time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE meeting_note_items
		SET body = COALESCE(?, body), done = COALESCE(?, done), assignee = COALESCE(?, assignee),
		    version = version + 1, updated_at = ?
		WHERE id = ? AND room = ? AND version = ?`,
		e.Body, e.Done, e.Assignee, at.UnixMilli(), id, room, e.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return r.missingOrConflict(ctx, room, id)
	}
	return nil
}

func (r *SQLRepository) DeleteNoteItem(ctx context.Context, room, id string, version int) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM meeting_note_items WHERE id = ? AND room = ? AND version = ?`, id, room, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return r.missingOrConflict(ctx, room, id)
	}
	return nil
}

func (r *SQLRepository) missingOrConflict(ctx context.Context, room, id string) error {
	var one int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM meeting_note_items WHERE id = ? AND room = ?`, id, room).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNoteNotFound
	}
	return ErrNoteConflict
}

// --- the service ------------------------------------------------------------

// Notes returns whether the notes exist and their items.
func (s *Service) Notes(ctx context.Context, a Actor, room string) (bool, []*NoteItem, error) {
	if _, ok := s.viewer(room, a.ID); !ok {
		return false, nil, ErrForbidden
	}
	created, err := s.repo.NotesCreated(ctx, room)
	if err != nil || !created {
		return false, []*NoteItem{}, err
	}
	items, err := s.repo.NoteItems(ctx, room, "")
	if items == nil {
		items = []*NoteItem{}
	}
	return true, items, err
}

// CreateNotes: "Add meeting notes for everyone". The organizer and
// co-organizers only.
func (s *Service) CreateNotes(ctx context.Context, a Actor, room string) error {
	v, ok := s.viewer(room, a.ID)
	if !ok || !v.Host {
		return ErrForbidden
	}
	if err := s.repo.CreateNotes(ctx, room, v.ID, time.Now().UTC()); err != nil {
		return err
	}
	s.pushNotes(room, map[string]any{"created": true})
	return nil
}

func (s *Service) notesOpen(ctx context.Context, a Actor, room string) error {
	if _, ok := s.viewer(room, a.ID); !ok {
		return ErrForbidden
	}
	created, err := s.repo.NotesCreated(ctx, room)
	if err != nil {
		return err
	}
	if !created {
		return ErrNotesNone
	}
	return nil
}

func (s *Service) AddNote(ctx context.Context, a Actor, room, section, body string) (*NoteItem, error) {
	if err := s.notesOpen(ctx, a, room); err != nil {
		return nil, err
	}
	if !noteSections[section] {
		return nil, ErrNoteSection
	}
	body, err := cleanBody(body, MaxNoteLen)
	if err != nil {
		return nil, ErrNoteBody
	}
	it := &NoteItem{ID: newID(), Section: section, Body: body, Version: 1, UpdatedAt: time.Now().UTC()}
	if err := s.repo.AddNoteItem(ctx, room, it); err != nil {
		return nil, err
	}
	s.pushNotes(room, map[string]any{"item": it})
	return it, nil
}

// EditNote changes an item. On a conflict it returns the item as it now is,
// so the editor sees what the other person wrote.
func (s *Service) EditNote(ctx context.Context, a Actor, room, id string, e NoteEdit) (*NoteItem, error) {
	if err := s.notesOpen(ctx, a, room); err != nil {
		return nil, err
	}
	if e.Body != nil {
		b, err := cleanBody(*e.Body, MaxNoteLen)
		if err != nil {
			return nil, ErrNoteBody
		}
		e.Body = &b
	}
	if e.Assignee != nil && len([]rune(*e.Assignee)) > MaxAssigneeLen {
		return nil, ErrNoteBody
	}
	err := s.repo.EditNoteItem(ctx, room, id, e, time.Now().UTC())
	cur, rerr := s.repo.NoteItems(ctx, room, id)
	if errors.Is(err, ErrNoteConflict) && rerr == nil && len(cur) == 1 {
		return cur[0], ErrNoteConflict
	}
	if err != nil {
		return nil, err
	}
	if rerr != nil || len(cur) != 1 {
		return nil, ErrNoteNotFound
	}
	s.pushNotes(room, map[string]any{"item": cur[0]})
	return cur[0], nil
}

func (s *Service) DeleteNote(ctx context.Context, a Actor, room, id string, version int) error {
	if err := s.notesOpen(ctx, a, room); err != nil {
		return err
	}
	if err := s.repo.DeleteNoteItem(ctx, room, id, version); err != nil {
		return err
	}
	s.pushNotes(room, map[string]any{"item": NoteItem{ID: id, Deleted: true}})
	return nil
}

func (s *Service) pushNotes(room string, v any) {
	if s.NotesPush == nil {
		return
	}
	body, _ := json.Marshal(v)
	s.NotesPush(room, body)
}
