package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// A person's own data, handed to them: GET /api/identity/export.
//
// One JSON file of everything the server stores that is theirs: the account,
// its sessions, what they did (their own lines of the audit record), and
// whatever each other module says is theirs (Personal, wired in main, so
// this module imports none of them). What they wrote is in it; what other
// people wrote is not, even where they could read it.
//
// It is the signed-in person's own and nobody else's: there is no id in the
// request to get wrong, and an administrator has no version of it. It is
// written a row at a time (database.WriteSections), is never cached, is
// served as a download, and each one is recorded.

const exportAbout = "Everything Aether stores that is yours: your account and sessions, your own actions, " +
	"what you wrote and what you set. Other people's messages are not included. Times are UTC."

// personalSections is this module's own, then the other modules'.
func (s *Service) personalSections(u *User) []database.Section {
	q := func(name, query string) database.Section {
		return database.Section{Name: name, Query: query, Args: []any{u.ID}}
	}
	out := []database.Section{
		q("account", `SELECT id, email, display_name, role, status, created_at, updated_at, last_login_at
			  FROM identity_users WHERE id = ?`),
		// Where and when, never the token: a session's secret is not in the
		// database to give (only its hash is) and would be no use to its owner.
		q("sessions", `SELECT created_at, last_seen, expires_at, revoked_at, ip, user_agent
			  FROM identity_sessions WHERE user_id = ?`),
		q("two_step_sign_in", `SELECT enabled, created_at, enabled_at FROM identity_two_step WHERE user_id = ?`),
		q("your_actions", `SELECT at, action, subject, outcome, ip, user_agent, detail
			  FROM audit_events WHERE actor_id = ?`),
	}
	if s.Personal != nil {
		out = append(out, s.Personal(u.ID, u.Email)...)
	}
	return out
}

// Export writes u's data to w as one JSON object.
func (s *Service) Export(ctx context.Context, u *User, w io.Writer, ip, ua string) error {
	s.record(ctx, audit.Event{
		Action: audit.ActionDataExported, Outcome: audit.OutcomeSuccess,
		ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email, IP: ip, UserAgent: ua,
	})
	about, _ := json.Marshal(exportAbout)
	if _, err := fmt.Fprintf(w, "{\"exported_at\":%q,\n\"about\":%s", time.Now().UTC().Format(time.RFC3339), about); err != nil {
		return err
	}
	if err := s.repo.WriteExport(ctx, w, s.personalSections(u)); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n}\n")
	return err
}

func (r *SQLRepository) WriteExport(ctx context.Context, w io.Writer, sections []database.Section) error {
	return r.db.WriteSections(ctx, w, sections)
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="aether-export-`+time.Now().UTC().Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	// The status is already sent when a row fails: the file ends without its
	// closing brace, which no JSON reader accepts as complete.
	if err := h.svc.Export(r.Context(), u, w, httpx.ClientIP(r, h.TrustProxy), r.UserAgent()); err != nil {
		h.log.Warn("export ended early", "user", u.ID, "err", err)
	}
}
