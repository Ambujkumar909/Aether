package control

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Guard is the authentication boundary, matching the other modules.
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
	// Every route requires a signed-in participant. Note that none of them
	// is gated on an administrative permission: rule 1 means control is not
	// something a role confers, so there is deliberately no admin path to
	// starting a session. The only admin capability here is revocation,
	// which is checked inside the service.
	mux.Handle("POST /api/control/request",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.request)))
	mux.Handle("POST /api/control/{id}/grant",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.grant)))
	mux.Handle("POST /api/control/{id}/deny",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.deny)))
	mux.Handle("POST /api/control/{id}/revoke",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.revoke)))
	mux.Handle("POST /api/control/{id}/extend",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.extend)))
	mux.Handle("POST /api/control/{id}/escalate",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.escalate)))
	mux.Handle("GET /api/control/pending",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.pending)))
	mux.Handle("GET /api/control/{id}",
		h.guard.Protect("meeting.join", http.HandlerFunc(h.byID)))
}

func (h *Handler) actor(r *http.Request) Actor {
	id, email := h.guard.ActorOf(r)
	return Actor{ID: id, Email: email, Admin: h.guard.IsAdmin(r)}
}

func (h *Handler) request(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MeetingID    string `json:"meetingId"`
		SubjectID    string `json:"subjectId"`
		SubjectEmail string `json:"subjectEmail"`
		SubjectName  string `json:"subjectName"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed body")
		return
	}
	scope := Scope(body.Scope)
	if scope == "" {
		scope = ScopeView
	}
	ip, ua := httpx.ClientIP(r, h.TrustProxy), r.UserAgent()

	sess, err := h.svc.Request(r.Context(), h.actor(r), body.MeetingID,
		Actor{ID: body.SubjectID, Email: body.SubjectEmail, Name: body.SubjectName},
		scope, ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sess)
}

func (h *Handler) grant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	ip, ua := httpx.ClientIP(r, h.TrustProxy), r.UserAgent()

	sess, err := h.svc.Grant(r.Context(), h.actor(r), r.PathValue("id"),
		time.Duration(body.Seconds)*time.Second, ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

func (h *Handler) deny(w http.ResponseWriter, r *http.Request) {
	sess, err := h.svc.Deny(r.Context(), h.actor(r), r.PathValue("id"), reasonOf(w, r),
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	sess, err := h.svc.Revoke(r.Context(), h.actor(r), r.PathValue("id"), reasonOf(w, r),
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

func (h *Handler) extend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	sess, err := h.svc.Extend(r.Context(), h.actor(r), r.PathValue("id"),
		time.Duration(body.Seconds)*time.Second,
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

func (h *Handler) escalate(w http.ResponseWriter, r *http.Request) {
	sess, err := h.svc.Escalate(r.Context(), h.actor(r), r.PathValue("id"),
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sess)
}

func (h *Handler) pending(w http.ResponseWriter, r *http.Request) {
	id, _ := h.guard.ActorOf(r)
	list, err := h.svc.PendingFor(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if list == nil {
		list = []*Session{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (h *Handler) byID(w http.ResponseWriter, r *http.Request) {
	sess, err := h.svc.ByID(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	// Only the two parties and an administrator may read a session. A
	// control session names who was driving whose machine and when, which is
	// nobody else's business.
	actor := h.actor(r)
	if actor.ID != sess.SubjectID && actor.ID != sess.ControllerID && !actor.Admin {
		httpx.Error(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

func reasonOf(w http.ResponseWriter, r *http.Request) string {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	if len(body.Reason) > 200 {
		return body.Reason[:200]
	}
	return body.Reason
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "no such session")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden",
			"only the person whose machine it is can answer this")
	case errors.Is(err, ErrSelfControl):
		httpx.Error(w, http.StatusBadRequest, "self_control",
			"you cannot request control of your own machine")
	case errors.Is(err, ErrAlreadyActive):
		httpx.Error(w, http.StatusConflict, "already_active",
			"that participant already has a control session running")
	case errors.Is(err, ErrBadScope):
		httpx.Error(w, http.StatusBadRequest, "bad_scope", "unknown scope")
	case errors.Is(err, ErrBadState):
		httpx.Error(w, http.StatusConflict, "bad_state",
			"that session is not in a state that allows this")
	default:
		h.log.Error("control request failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
