package identity

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Module wires repository to service to handler.
type Module struct {
	Service *Service
	Handler *Handler
	log     *slog.Logger

	cancel context.CancelFunc
}

func NewModule(db *database.DB, rec audit.Recorder, log *slog.Logger,
	secure, trustProxy, allowSignup bool) *Module {
	repo := NewSQLRepository(db)
	svc := NewService(repo, rec, log)
	h := NewHandler(svc, log, secure, trustProxy)
	h.AllowSignup = allowSignup
	return &Module{
		Service: svc,
		Handler: h,
		log:     log,
	}
}

func (m *Module) Name() string { return "identity" }

// identity depends on audit because every authentication decision is recorded
// before it is acted on, and the kernel enforces that ordering at startup.
func (m *Module) Requires() []string { return []string{"audit"} }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("identity.reader", Reader(m.Service))
	k.Provide("identity.service", m.Service)
	k.Provide("identity.handler", m.Handler)

	bg, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.housekeeping(bg)
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}

// housekeeping removes sessions that can no longer authenticate anything.
func (m *Module) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := m.Service.PurgeExpiredSessions(ctx); err != nil {
				m.log.Warn("session purge failed", "err", err)
			} else if n > 0 {
				m.log.Info("purged expired sessions", "count", n)
			}
		}
	}
}

func (m *Module) Routes(mux *http.ServeMux) { m.Handler.Routes(mux) }

// ---------------------------------------------------------------------------
// audit.Guard
// ---------------------------------------------------------------------------

// Protect satisfies the Guard interface every module uses to gate a route:
// authenticate, check the permission, AND enforce CSRF on anything that
// changes state.
//
// The CSRF check lives here rather than at each call site, and that placement
// is the fix for a real hole. It used to be applied per-route, by hand, in the
// identity module only -- so scheduling, messaging and control, which all
// reach for this same helper, had none. Every state-changing endpoint in three
// modules was reachable from any page the victim happened to have open, with
// their cookies attached.
//
// The worst of them was not the obvious one. Creating a meeting or sending a
// message as somebody else is bad; POST /api/control/{id}/grant let an
// attacker's page make a signed-in victim GRANT REMOTE CONTROL OF THEIR
// MACHINE, which defeats the entire consent model that feature is built on.
//
// Central because "remember to wrap it" is not a control. There are forty-odd
// route registrations across the codebase and one of them will be added next
// month by somebody who has not read this comment; the only version of this
// that stays true is the one where forgetting is impossible.
func (m *Module) Protect(permission string, next http.Handler) http.Handler {
	return m.Handler.RequirePermission(Permission(permission),
		m.Handler.CSRF(m.Handler.Throttle(next)))
}

// ActorOf satisfies audit.Guard.
func (m *Module) ActorOf(r *http.Request) (string, string) {
	u := UserFrom(r.Context())
	if u == nil {
		return "", ""
	}
	return u.ID, u.Email
}

// IsAdmin lets a module widen a query for an administrator without knowing
// what a role is.
func (m *Module) IsAdmin(r *http.Request) bool {
	u := UserFrom(r.Context())
	return u != nil && u.Role == RoleAdmin
}

// NameOf returns the caller's display name.
func (m *Module) NameOf(r *http.Request) string {
	u := UserFrom(r.Context())
	if u == nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Email
}

// ---------------------------------------------------------------------------
// Directory
//
// A deliberately minimal view of the user table for modules that need to
// resolve a person without gaining the ability to change one.
// ---------------------------------------------------------------------------

// LookupByEmail resolves an address to an id and a display name.
func (m *Module) LookupByEmail(ctx context.Context, email string) (string, string, error) {
	norm, err := NormalizeEmail(email)
	if err != nil {
		return "", "", err
	}
	u, err := m.Service.repo.UserByEmail(ctx, norm)
	if err != nil {
		return "", "", err
	}
	name := u.DisplayName
	if name == "" {
		name = u.Email
	}
	return u.ID, name, nil
}

// LookupByID resolves an id to an address and a display name.
func (m *Module) LookupByID(ctx context.Context, id string) (string, string, error) {
	u, err := m.Service.UserByID(ctx, id)
	if err != nil {
		return "", "", err
	}
	name := u.DisplayName
	if name == "" {
		name = u.Email
	}
	return u.Email, name, nil
}
