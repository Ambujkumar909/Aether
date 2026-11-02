package messaging

import (
	"net/http"
	"strconv"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Teams over HTTP (teams.go). Every route needs a session (the guard), and
// the service checks team membership or ownership before it reads anything.

// Guessing a team code: ten tries a minute per account. 31^7 codes make a
// guess hopeless, and this keeps it so.
var codeLimit = httpx.NewLimiter(10.0/60.0, 10)

func (h *Handler) teamRoutes(mux *http.ServeMux, p func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/messaging/teams", p(h.myTeams))
	mux.Handle("POST /api/messaging/teams", p(h.createTeam))
	mux.Handle("GET /api/messaging/teams/browse", p(h.browseTeams))
	mux.Handle("POST /api/messaging/teams/join", httpx.RateLimit(codeLimit, func(r *http.Request) string {
		id, _ := h.guard.ActorOf(r)
		return "code:" + id
	}, p(h.joinByCode)))
	mux.Handle("POST /api/messaging/teams/{id}/join", p(h.joinTeam))
	mux.Handle("PATCH /api/messaging/teams/{id}", p(h.updateTeam))
	mux.Handle("DELETE /api/messaging/teams/{id}", p(h.deleteTeam))
	mux.Handle("GET /api/messaging/teams/{id}/members", p(h.teamMembers))
	mux.Handle("POST /api/messaging/teams/{id}/members", p(h.addTeamMembers))
	mux.Handle("DELETE /api/messaging/teams/{id}/members/{user}", p(h.removeTeamMember))
	mux.Handle("PUT /api/messaging/teams/{id}/members/{user}/role", p(h.teamRole))
	mux.Handle("POST /api/messaging/teams/{id}/code", p(h.teamCode))
	mux.Handle("DELETE /api/messaging/teams/{id}/code", p(h.teamCodeRemove))
	mux.Handle("POST /api/messaging/teams/{id}/channels", p(h.createTeamChannel))
}

func (h *Handler) myTeams(w http.ResponseWriter, r *http.Request) {
	ts, err := h.svc.MyTeams(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"teams": ts})
}

func (h *Handler) createTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	t, err := h.svc.CreateTeam(r.Context(), h.actor(r), req.Name, req.Description, req.Visibility)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

func (h *Handler) browseTeams(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := h.svc.BrowseTeams(r.Context(), h.actor(r), r.URL.Query().Get("q"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"teams": ts})
}

func (h *Handler) joinTeam(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.JoinTeam(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) joinByCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	t, err := h.svc.JoinTeamByCode(r.Context(), h.actor(r), req.Code)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) updateTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Visibility  *string `json:"visibility"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	t, err := h.svc.UpdateTeam(r.Context(), h.actor(r), r.PathValue("id"), req.Name, req.Description, req.Visibility)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) deleteTeam(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteTeam(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) teamMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := h.svc.TeamMembersOf(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"members": ms})
}

func (h *Handler) addTeamMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails []string `json:"emails"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if len(req.Emails) > MaxAddAtOnce {
		httpx.Error(w, http.StatusBadRequest, "too_many", "add at most 50 people at once")
		return
	}
	skipped, err := h.svc.AddTeamMembers(r.Context(), h.actor(r), r.PathValue("id"), req.Emails)
	if err != nil {
		h.fail(w, err)
		return
	}
	if skipped == nil {
		skipped = []string{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skipped": skipped})
}

func (h *Handler) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.LeaveTeam(r.Context(), h.actor(r), r.PathValue("id"), r.PathValue("user")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) teamRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if err := h.svc.SetTeamRole(r.Context(), h.actor(r), r.PathValue("id"), r.PathValue("user"), req.Role); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) teamCode(w http.ResponseWriter, r *http.Request) {
	code, err := h.svc.TeamCode(r.Context(), h.actor(r), r.PathValue("id"), false)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"code": code})
}

func (h *Handler) teamCodeRemove(w http.ResponseWriter, r *http.Request) {
	if _, err := h.svc.TeamCode(r.Context(), h.actor(r), r.PathValue("id"), true); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createTeamChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Topic   string `json:"topic"`
		Private bool   `json:"private"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	c, err := h.svc.CreateTeamChannel(r.Context(), h.actor(r), r.PathValue("id"), req.Name, req.Topic, req.Private)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}
