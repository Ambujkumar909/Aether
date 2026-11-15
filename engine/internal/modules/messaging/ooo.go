package messaging

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Out of office.
//
// A line a person leaves for anyone who writes to them while they are away,
// with an optional window. It is not a status. A status is how reachable
// you are this minute and the rooms and the idle clock can overrule it;
// this is "do not expect an answer", and it stands whatever the dot says.
//
// It rides on the status, on the same read and the same push and so to the
// same people, but only while it is in effect: a leave planned for next
// week is not announced this week. The window's two edges are found by the
// status loop with two indexed range reads (whose window began or ended
// since the last tick), so the cost is the number of people whose state
// changed, and nothing at all when nobody's did.
//
// Nothing is sent on a person's behalf. There is no automatic reply: the
// writer is told above the box they are typing in, before they send.

const (
	// MaxOOOMessage bounds the line; one over is refused, not cut.
	MaxOOOMessage = 280
	// MaxOOOAhead is how far ahead a window may start or end.
	MaxOOOAhead = 366 * 24 * time.Hour
	// maxOOOCrossed bounds one tick's reading of the edges. More than this
	// many windows turning in fifteen seconds wait for the next tick; the
	// read path is exact meanwhile.
	maxOOOCrossed = 1000
)

var ErrBadOOO = errors.New("messaging: not an out of office")

// OOO is an out of office. On a Status only Message and Until are set, and
// only while it is in effect; From and Active are for its owner.
type OOO struct {
	Message string     `json:"message"`
	From    *time.Time `json:"from,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
	Active  bool       `json:"active,omitempty"`
}

// SetOOO turns out of office on, now or from a time, until a time or until
// it is turned off. A start in the past means now.
func (s *Service) SetOOO(ctx context.Context, a Actor, message string, from, until *time.Time) (OOO, error) {
	message, ok := cleanLine(message, MaxOOOMessage)
	if !ok || message == "" {
		return OOO{}, ErrBadOOO
	}
	now := time.Now().UTC()
	start := now
	if from != nil && from.After(now) {
		start = from.UTC()
	}
	if start.Sub(now) > MaxOOOAhead {
		return OOO{}, ErrBadOOO
	}
	var end time.Time
	if until != nil {
		end = until.UTC()
		if !end.After(start) || end.Sub(now) > MaxOOOAhead {
			return OOO{}, ErrBadOOO
		}
	}
	was, err := s.repo.OOOActive(ctx, []string{a.ID}, now)
	if err != nil {
		return OOO{}, err
	}
	if err := s.repo.SetOOO(ctx, a.ID, message, start, end, now); err != nil {
		return OOO{}, err
	}
	// Peers are told when what they see changed: it is in effect now, or it
	// was and has just been moved into the future.
	if _, shown := was[a.ID]; shown || !start.After(now) {
		s.announceStatus(ctx, a.ID, s.autoNow(), now)
	}
	out := OOO{Message: message, From: &start, Active: !start.After(now)}
	if !end.IsZero() {
		out.Until = &end
	}
	return out, nil
}

// ClearOOO turns it off. Peers are told only if they were being shown it.
func (s *Service) ClearOOO(ctx context.Context, a Actor) error {
	now := time.Now().UTC()
	was, err := s.repo.OOOActive(ctx, []string{a.ID}, now)
	if err != nil {
		return err
	}
	if err := s.repo.ClearOOO(ctx, a.ID); err != nil {
		return err
	}
	if _, shown := was[a.ID]; shown {
		s.announceStatus(ctx, a.ID, s.autoNow(), now)
	}
	return nil
}

// MyOOO is the caller's own, with its window, unless it has run out.
func (s *Service) MyOOO(ctx context.Context, a Actor) (OOO, bool, error) {
	return s.repo.OOOOf(ctx, a.ID, time.Now().UTC())
}

// announceStatus pushes what a person reads as right now -- their status,
// their presence, their out of office -- to their peers and their own
// devices. The status loop and the out of office both end here.
func (s *Service) announceStatus(ctx context.Context, id string, auto map[string]string, now time.Time) {
	manual, has, _ := s.manualStatus(ctx, id)
	st, ok := effective(id, manual, has, auto, now)
	if !ok {
		// Nothing picked, nothing automatic: available, said out loud so a
		// dot that turned red turns back.
		st = Status{UserID: id, Status: StatusAvailable, UpdatedAt: now}
	}
	s.presenceOf(ctx, id, &st)
	if peers, err := s.repo.PeersOf(ctx, id); err == nil {
		for _, p := range peers {
			s.pushAuto(p, st)
		}
	}
	s.pushAuto(id, st)
}

// --- repository -------------------------------------------------------------

func (r *SQLRepository) SetOOO(ctx context.Context, userID, message string, start, end, now time.Time) error {
	var endMs int64
	if !end.IsZero() {
		endMs = end.UnixMilli()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_ooo(user_id, message, starts_at, ends_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET message=excluded.message, starts_at=excluded.starts_at,
			ends_at=excluded.ends_at, updated_at=excluded.updated_at`,
		userID, message, start.UnixMilli(), endMs, now.UnixMilli())
	return err
}

func (r *SQLRepository) ClearOOO(ctx context.Context, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM messaging_ooo WHERE user_id = ?`, userID)
	return err
}

// OOOOf is one person's own row, unless its window has closed.
func (r *SQLRepository) OOOOf(ctx context.Context, userID string, now time.Time) (OOO, bool, error) {
	var o OOO
	var start, end int64
	err := r.db.QueryRowContext(ctx,
		`SELECT message, starts_at, ends_at FROM messaging_ooo WHERE user_id = ?`, userID).Scan(&o.Message, &start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return OOO{}, false, nil
	}
	if err != nil {
		return OOO{}, false, err
	}
	if end > 0 && end <= now.UnixMilli() {
		return OOO{}, false, nil
	}
	from := time.UnixMilli(start).UTC()
	o.From, o.Active = &from, start <= now.UnixMilli()
	if end > 0 {
		t := time.UnixMilli(end).UTC()
		o.Until = &t
	}
	return o, true, nil
}

// OOOActive is who among ids is out of office at now, as their peers see
// it: the line and when it ends. One query however many ids.
func (r *SQLRepository) OOOActive(ctx context.Context, ids []string, now time.Time) (map[string]OOO, error) {
	out := map[string]OOO{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := placeholders(ids)
	ms := now.UnixMilli()
	rows, err := r.db.QueryContext(ctx, `
		SELECT user_id, message, ends_at FROM messaging_ooo
		 WHERE user_id IN (`+ph+`) AND starts_at <= ? AND (ends_at = 0 OR ends_at > ?)`, append(args, ms, ms)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var o OOO
		var end int64
		if err := rows.Scan(&id, &o.Message, &end); err != nil {
			return nil, err
		}
		if end > 0 {
			t := time.UnixMilli(end).UTC()
			o.Until = &t
		}
		out[id] = o
	}
	return out, rows.Err()
}

// OOOCrossed is whose window began or ended in (after, upto]: each half is
// a range read on its own index.
//
// A start counts only when it was set ahead of time (starts_at after
// updated_at). One that began the moment it was set has been announced
// already, by SetOOO; without the condition the loop found that start in
// its next reading and sent every peer the same frame a second time, which
// is how it was found: 106 B on the wire, twice, in the tshark capture.
func (r *SQLRepository) OOOCrossed(ctx context.Context, after, upto time.Time) ([]string, error) {
	a, b := after.UnixMilli(), upto.UnixMilli()
	rows, err := r.db.QueryContext(ctx, `
		SELECT user_id FROM messaging_ooo WHERE starts_at > ? AND starts_at <= ? AND starts_at > updated_at
		UNION
		SELECT user_id FROM messaging_ooo WHERE ends_at > ? AND ends_at <= ?
		LIMIT ?`, a, b, a, b, maxOOOCrossed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- handlers ---------------------------------------------------------------

func (h *Handler) myOOO(w http.ResponseWriter, r *http.Request) {
	o, ok, err := h.svc.MyOOO(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		httpx.JSON(w, http.StatusOK, map[string]any{"ooo": nil})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ooo": o})
}

func (h *Handler) setOOO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string     `json:"message"`
		From    *time.Time `json:"from"`
		Until   *time.Time `json:"until"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	o, err := h.svc.SetOOO(r.Context(), h.actor(r), req.Message, req.From, req.Until)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ooo": o})
}

func (h *Handler) clearOOO(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.ClearOOO(r.Context(), h.actor(r)); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ooo": nil})
}
