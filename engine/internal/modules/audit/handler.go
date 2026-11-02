package audit

import (
	"encoding/csv"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Guard is the authorisation dependency, satisfied by the identity module.
//
// Declaring it here as a narrow interface, rather than importing identity,
// is what keeps the dependency one-way and the module boundary real. The
// permission is a string so this package need not know identity's vocabulary.
type Guard interface {
	Protect(permission string, next http.Handler) http.Handler
	ActorOf(r *http.Request) (id, email string)
}

// Handler exposes the audit log.
type Handler struct {
	svc   *Service
	guard Guard
	log   *slog.Logger

	TrustProxy bool
}

func NewHandler(svc *Service, guard Guard, log *slog.Logger, trustProxy bool) *Handler {
	return &Handler{svc: svc, guard: guard, log: log, TrustProxy: trustProxy}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/audit/events",
		h.guard.Protect("audit.read", http.HandlerFunc(h.list)))
	mux.Handle("GET /api/audit/verify",
		h.guard.Protect("audit.verify", http.HandlerFunc(h.verify)))
	mux.Handle("GET /api/audit/export",
		h.guard.Protect("audit.export", http.HandlerFunc(h.export)))
}

type eventView struct {
	Event
	Summary string `json:"summary"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	events, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.log.Error("audit list failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "could not read the log")
		return
	}
	total, err := h.svc.Count(r.Context(), f)
	if err != nil {
		total = int64(len(events))
	}
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		out = append(out, eventView{Event: e, Summary: Describe(e)})
	}
	headSeq, headHash := h.svc.Head(r.Context())
	httpx.JSON(w, http.StatusOK, map[string]any{
		"events": out, "total": total,
		"headSeq": headSeq, "headHash": headHash,
	})
}

// verify walks the whole chain and reports whether it is intact.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.Verify(r.Context())
	if err != nil {
		h.log.Error("audit verify failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "verification failed")
		return
	}
	if !res.OK {
		// A broken chain is a finding, not a server error. The caller needs
		// the detail, and the operator needs it in the process log too.
		h.log.Error("AUDIT CHAIN BROKEN", "at", res.BrokenAt, "detail", res.Detail)
	}
	httpx.JSON(w, http.StatusOK, res)
}

// export streams the log as CSV, and records that it happened.
//
// Exporting an audit log is itself security-relevant: it is how a complete
// copy of who-did-what leaves the system. So the export is audited, which is
// why this handler writes before it reads.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	id, email := h.guard.ActorOf(r)
	f := parseFilter(r)
	f.Limit = 500

	h.svc.Record(r.Context(), Event{
		Action: ActionAuditExported, Outcome: OutcomeSuccess,
		ActorID: id, ActorEmail: email,
		IP: httpx.ClientIP(r, h.TrustProxy), UserAgent: r.UserAgent(),
		Detail: "filter " + r.URL.RawQuery,
	})

	events, err := h.svc.List(r.Context(), f)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal", "export failed")
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="aether-audit-`+
			time.Now().UTC().Format("20060102-150405")+`.csv"`)

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"seq", "id", "at", "actor_id", "actor_email", "action",
		"subject", "outcome", "ip", "user_agent", "detail", "prev_hash", "hash"})
	for _, e := range events {
		_ = cw.Write([]string{
			strconv.FormatInt(e.Seq, 10), e.ID, e.At.Format(time.RFC3339),
			e.ActorID, e.ActorEmail, string(e.Action), e.Subject,
			string(e.Outcome), e.IP, e.UserAgent, e.Detail, e.PrevHash, e.Hash,
		})
	}
}

func parseFilter(r *http.Request) Filter {
	q := r.URL.Query()
	f := Filter{
		ActorID: q.Get("actor"),
		Action:  Action(q.Get("action")),
		Outcome: Outcome(q.Get("outcome")),
		Search:  strings.TrimSpace(q.Get("q")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Before = n
		}
	}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t
		}
	}
	if v := q.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = t
		}
	}
	return f
}
