package scheduling

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Guard is the authorisation dependency, satisfied by the identity module.
// Declared here as a narrow interface so this package never imports it.
type Guard interface {
	Protect(permission string, next http.Handler) http.Handler
	ActorOf(r *http.Request) (id, email string)
	IsAdmin(r *http.Request) bool
}

type Handler struct {
	svc        *Service
	guard      Guard
	log        *slog.Logger
	TrustProxy bool
}

func NewHandler(svc *Service, guard Guard, log *slog.Logger, trustProxy bool) *Handler {
	return &Handler{svc: svc, guard: guard, log: log, TrustProxy: trustProxy}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	p := func(next http.HandlerFunc) http.Handler {
		// Scheduling is available to anyone who may host a meeting.
		return h.guard.Protect("meeting.host", next)
	}
	mux.Handle("GET /api/scheduling/meetings", p(h.list))
	mux.Handle("POST /api/scheduling/meetings", p(h.create))
	mux.Handle("GET /api/scheduling/meetings/{id}", p(h.get))
	mux.Handle("PATCH /api/scheduling/meetings/{id}", p(h.update))
	mux.Handle("DELETE /api/scheduling/meetings/{id}", p(h.remove))
	mux.Handle("POST /api/scheduling/meetings/{id}/cancel", p(h.cancel))
	mux.Handle("POST /api/scheduling/meetings/{id}/respond", p(h.respond))
	mux.Handle("GET /api/scheduling/conflicts", p(h.conflicts))
	mux.Handle("GET /api/scheduling/freebusy", p(h.freeBusy))
	// The caller's own recent meetings -- the ones they were actually in.
	mux.Handle("GET /api/scheduling/history", p(h.history))
	mux.Handle("GET /api/scheduling/rooms/{room}/attendance", p(h.attendance))
	// Q&A in the meeting in progress.
	mux.Handle("GET /api/scheduling/rooms/{room}/qna", p(h.qnaList))
	mux.Handle("POST /api/scheduling/rooms/{room}/qna", p(h.qnaAsk))
	mux.Handle("PUT /api/scheduling/rooms/{room}/qna/settings", p(h.qnaSettings))
	mux.Handle("POST /api/scheduling/rooms/{room}/qna/{id}/like", p(h.qnaLike))
	mux.Handle("POST /api/scheduling/rooms/{room}/qna/{id}/replies", p(h.qnaReply))
	mux.Handle("POST /api/scheduling/rooms/{room}/qna/{id}/moderate", p(h.qnaModerate))
	// The meeting transcript: JSON for the panel, .vtt or .txt to download.
	mux.Handle("GET /api/scheduling/rooms/{room}/transcript", p(h.transcript))
	// Meeting notes.
	mux.Handle("GET /api/scheduling/rooms/{room}/notes", p(h.notesGet))
	mux.Handle("POST /api/scheduling/rooms/{room}/notes", p(h.notesCreate))
	mux.Handle("POST /api/scheduling/rooms/{room}/notes/items", p(h.noteAdd))
	mux.Handle("PATCH /api/scheduling/rooms/{room}/notes/items/{id}", p(h.noteEdit))
	mux.Handle("DELETE /api/scheduling/rooms/{room}/notes/items/{id}", p(h.noteDelete))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.History(r.Context(), h.actor(r), 50)
	if err != nil {
		h.fail(w, err)
		return
	}
	if rows == nil {
		rows = []*Attendance{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"history": rows})
}

// attendance is "Download attendance list": a CSV, as an attachment, never
// rendered inline.
func (h *Handler) attendance(w http.ResponseWriter, r *http.Request) {
	body, err := h.svc.AttendanceCSV(r.Context(), h.actor(r), r.PathValue("room"), time.Now().UTC())
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="attendance.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// transcript is the transcript panel's list, or the download (format=vtt or
// txt), always as an attachment: it is text people said, never rendered.
func (h *Handler) transcript(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var session int64
	if v := q.Get("session"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			httpx.Error(w, http.StatusBadRequest, "bad_session", "unknown session")
			return
		}
		session = n
	}
	session, lines, err := h.svc.Transcript(r.Context(), h.actor(r), r.PathValue("room"), session)
	if err != nil {
		h.fail(w, err)
		return
	}
	var body []byte
	switch q.Get("format") {
	case "", "json":
		httpx.JSON(w, http.StatusOK, map[string]any{"session": session, "lines": lines})
		return
	case "vtt":
		body = TranscriptVTT(session, lines)
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="transcript.vtt"`)
	case "txt":
		body = TranscriptText(session, lines)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="transcript.txt"`)
	default:
		httpx.Error(w, http.StatusBadRequest, "bad_format", "format must be json, vtt or txt")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// Q&A. Bodies are bounded before they are read (64 KB), and again, in
// characters, by the service.
func qnaBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_json", "the request body is not valid")
		return false
	}
	return true
}

func (h *Handler) qnaList(w http.ResponseWriter, r *http.Request) {
	qs, set, err := h.svc.QnA(r.Context(), h.actor(r), r.PathValue("room"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if qs == nil {
		qs = []*Question{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"questions": qs, "settings": set})
}

func (h *Handler) qnaAsk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body      string `json:"body"`
		Anonymous bool   `json:"anonymous"`
	}
	if !qnaBody(w, r, &in) {
		return
	}
	q, err := h.svc.Ask(r.Context(), h.actor(r), r.PathValue("room"), in.Body, in.Anonymous)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, q)
}

func (h *Handler) qnaReply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body      string `json:"body"`
		Anonymous bool   `json:"anonymous"`
	}
	if !qnaBody(w, r, &in) {
		return
	}
	if err := h.svc.ReplyTo(r.Context(), h.actor(r), r.PathValue("room"), r.PathValue("id"), in.Body, in.Anonymous); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) qnaLike(w http.ResponseWriter, r *http.Request) {
	liked, err := h.svc.Like(r.Context(), h.actor(r), r.PathValue("room"), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"liked": liked})
}

func (h *Handler) qnaModerate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	if !qnaBody(w, r, &in) {
		return
	}
	if err := h.svc.Moderate(r.Context(), h.actor(r), r.PathValue("room"), r.PathValue("id"), in.Action); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) qnaSettings(w http.ResponseWriter, r *http.Request) {
	var in QnASettings
	if !qnaBody(w, r, &in) {
		return
	}
	if err := h.svc.SetQnA(r.Context(), h.actor(r), r.PathValue("room"), in); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) notesGet(w http.ResponseWriter, r *http.Request) {
	created, items, err := h.svc.Notes(r.Context(), h.actor(r), r.PathValue("room"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"created": created, "items": items})
}

func (h *Handler) notesCreate(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.CreateNotes(r.Context(), h.actor(r), r.PathValue("room")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) noteAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Section string `json:"section"`
		Body    string `json:"body"`
	}
	if !qnaBody(w, r, &in) {
		return
	}
	it, err := h.svc.AddNote(r.Context(), h.actor(r), r.PathValue("room"), in.Section, in.Body)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, it)
}

// A conflicting edit answers 409 with the item as it now is, so the editor
// can see the other person's change instead of losing it.
func (h *Handler) noteEdit(w http.ResponseWriter, r *http.Request) {
	var e NoteEdit
	if !qnaBody(w, r, &e) {
		return
	}
	it, err := h.svc.EditNote(r.Context(), h.actor(r), r.PathValue("room"), r.PathValue("id"), e)
	if errors.Is(err, ErrNoteConflict) && it != nil {
		httpx.JSON(w, http.StatusConflict, map[string]any{"error": "someone else changed this item", "code": "notes_conflict", "item": it})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, it)
}

func (h *Handler) noteDelete(w http.ResponseWriter, r *http.Request) {
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	err := h.svc.DeleteNote(r.Context(), h.actor(r), r.PathValue("room"), r.PathValue("id"), version)
	if errors.Is(err, ErrNoteConflict) {
		httpx.Error(w, http.StatusConflict, "notes_conflict", "someone else changed this item")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) actor(r *http.Request) Actor {
	id, email := h.guard.ActorOf(r)
	return Actor{ID: id, Email: email, Admin: h.guard.IsAdmin(r)}
}

func (h *Handler) ipua(r *http.Request) (string, string) {
	return httpx.ClientIP(r, h.TrustProxy), r.UserAgent()
}

// wire is the request/response body. Times arrive as RFC3339 so the client
// never has to agree with the server about an epoch or a timezone.
type wire struct {
	Room        string    `json:"room"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    Category  `json:"category"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`
	Invitees    []string  `json:"invitees"`
	// Emails, each one of the invitees.
	CoOrganizers []string    `json:"coOrganizers"`
	Admission    string      `json:"admission"`
	Recurrence   *Recurrence `json:"recurrence"`
}

func (w wire) toMeeting() *Meeting {
	m := &Meeting{
		Room: w.Room, Title: w.Title, Description: w.Description,
		Category: w.Category, StartsAt: w.StartsAt, EndsAt: w.EndsAt,
		// Normalised at the boundary, so nothing further in has to wonder
		// whether it is looking at a value a client chose.
		Admission:  NormaliseAdmission(w.Admission),
		Recurrence: w.Recurrence,
	}
	if m.Recurrence != nil && m.Recurrence.Freq == "" {
		m.Recurrence = nil // "does not repeat", sent as an empty rule
	}
	co := map[string]bool{}
	for _, e := range w.CoOrganizers {
		co[strings.ToLower(strings.TrimSpace(e))] = true
	}
	for _, e := range w.Invitees {
		k := strings.ToLower(strings.TrimSpace(e))
		m.Invitees = append(m.Invitees, Invitee{Email: e, Response: RespPending, CoOrganizer: co[k]})
		delete(co, k)
	}
	// Anyone named a co-organizer but not invited: the service refuses it.
	for e := range co {
		if e != "" {
			m.Invitees = append(m.Invitees, Invitee{Email: e, CoOrganizer: true, Response: respNotInvited})
		}
	}
	return m
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{
		Status:   Status(q.Get("status")),
		Category: Category(q.Get("category")),
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From = t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To = t
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}

	ms, err := h.svc.List(r.Context(), h.actor(r), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Meeting{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"meetings": ms})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.ByID(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body wire
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip, ua := h.ipua(r)
	m, err := h.svc.Create(r.Context(), h.actor(r), body.toMeeting(), ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	var body wire
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip, ua := h.ipua(r)
	m, err := h.svc.Update(r.Context(), h.actor(r), r.PathValue("id"), body.toMeeting(), ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

// cancel ends a meeting, or with {"occurrence": <start ms>} one occurrence
// of a series. The body is optional: no body is the whole meeting.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Occurrence int64 `json:"occurrence"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &body); err != nil {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
			return
		}
	}
	ip, ua := h.ipua(r)
	if err := h.svc.CancelOccurrence(r.Context(), h.actor(r), r.PathValue("id"), body.Occurrence, ip, ua); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// freeBusy is the scheduling assistant: ?emails=a,b&from=&to= (RFC3339),
// answering spans only. Bounded by the service (20 people, 14 days).
func (h *Handler) freeBusy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_range", "from and to must be RFC3339")
		return
	}
	var emails []string
	for _, e := range strings.Split(q.Get("emails"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			emails = append(emails, e)
		}
	}
	busy, err := h.svc.FreeBusy(r.Context(), h.actor(r), emails, from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"busy": busy})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	ip, ua := h.ipua(r)
	if err := h.svc.Delete(r.Context(), h.actor(r), r.PathValue("id"), ip, ua); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Response Response `json:"response"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip, ua := h.ipua(r)
	m, err := h.svc.Respond(r.Context(), h.actor(r), r.PathValue("id"), body.Response, ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

func (h *Handler) conflicts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_range", "from and to must be RFC3339")
		return
	}
	ms, err := h.svc.Conflicts(r.Context(), h.actor(r), from, to, q.Get("exclude"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Meeting{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"conflicts": ms})
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "no such meeting")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "only the organiser can change this")
	case errors.Is(err, ErrBadTitle):
		httpx.Error(w, http.StatusBadRequest, "bad_title", "a title is required")
	case errors.Is(err, ErrBadTimeRange):
		httpx.Error(w, http.StatusBadRequest, "bad_range", "the end must be after the start")
	case errors.Is(err, ErrBadCategory):
		httpx.Error(w, http.StatusBadRequest, "bad_category", "unknown category")
	case errors.Is(err, ErrTooLong):
		httpx.Error(w, http.StatusBadRequest, "too_long", "a meeting may not exceed 24 hours")
	case errors.Is(err, ErrTooFar):
		httpx.Error(w, http.StatusBadRequest, "too_far", "that is more than two years away")
	case errors.Is(err, ErrBadRecurrence):
		httpx.Error(w, http.StatusBadRequest, "bad_recurrence", strings.TrimPrefix(err.Error(), "scheduling: "))
	case errors.Is(err, ErrQnAClosed):
		httpx.Error(w, http.StatusConflict, "qna_closed", "Q&A is closed")
	case errors.Is(err, ErrQnABody):
		httpx.Error(w, http.StatusBadRequest, "qna_body", "a question or reply must be 1 to 1000 characters")
	case errors.Is(err, ErrQnAFull):
		httpx.Error(w, http.StatusConflict, "qna_full", "this meeting has reached its limit of questions or replies")
	case errors.Is(err, ErrQnAAnon):
		httpx.Error(w, http.StatusForbidden, "qna_anonymous", "anonymous posts are not allowed in this meeting")
	case errors.Is(err, ErrQnANotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "no such question")
	case errors.Is(err, ErrQnAAction):
		httpx.Error(w, http.StatusBadRequest, "qna_action", "unknown moderation action")
	case errors.Is(err, ErrNotesNone):
		httpx.Error(w, http.StatusConflict, "notes_none", "the organizer hasn't created meeting notes yet")
	case errors.Is(err, ErrNoteSection):
		httpx.Error(w, http.StatusBadRequest, "notes_section", "unknown notes section")
	case errors.Is(err, ErrNoteBody):
		httpx.Error(w, http.StatusBadRequest, "notes_body", "a note must be 1 to 2000 characters")
	case errors.Is(err, ErrNoteNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "no such item")
	case errors.Is(err, ErrNotesFull):
		httpx.Error(w, http.StatusConflict, "notes_full", "these notes have reached their limit of items")
	case errors.Is(err, ErrCoOrganizer):
		httpx.Error(w, http.StatusBadRequest, "co_organizer", "co-organizers must be people you invited, at most 10")
	default:
		h.log.Error("scheduling request failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
