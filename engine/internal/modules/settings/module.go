package settings

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Guard is the authentication boundary, matching the other modules.
type Guard interface {
	Protect(permission string, next http.Handler) http.Handler
	ActorOf(r *http.Request) (id, email string)
	IsAdmin(r *http.Request) bool
}

type Module struct {
	Service    *Service
	guard      Guard
	log        *slog.Logger
	trustProxy bool
}

func NewModule(db *database.DB, rec audit.Recorder, guard Guard,
	log *slog.Logger, trustProxy bool) *Module {
	return &Module{
		Service:    NewService(NewSQLRepository(db), rec, log),
		guard:      guard,
		log:        log,
		trustProxy: trustProxy,
	}
}

func (m *Module) Name() string { return "settings" }

// Every change is audited, so audit must be running first.
func (m *Module) Requires() []string { return []string{"audit"} }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	if err := m.Service.Load(ctx); err != nil {
		return err
	}
	k.Provide("settings.reader", Reader(m.Service))
	k.Provide("settings.service", m.Service)
	return nil
}

func (m *Module) Stop(ctx context.Context) error { return nil }

func (m *Module) Routes(mux *http.ServeMux) {
	// Reading deployment policy is gated on user administration rather than
	// on merely being signed in. The catalogue names what this deployment can
	// do -- whether recording exists, whether remote control is possible --
	// which is reconnaissance for anybody deciding what to try.
	mux.Handle("GET /api/settings",
		m.guard.Protect("user.read", http.HandlerFunc(m.list)))
	mux.Handle("PUT /api/settings/{key}",
		m.guard.Protect("user.write", http.HandlerFunc(m.put)))
}

func (m *Module) actor(r *http.Request) Actor {
	id, email := m.guard.ActorOf(r)
	return Actor{ID: id, Email: email, Admin: m.guard.IsAdmin(r)}
}

func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	vals, err := m.Service.All(r.Context(), m.actor(r))
	if err != nil {
		m.fail(w, err)
		return
	}
	// The catalogue rides along, so the console renders from the server's
	// description rather than from a copy that drifts out of step with it.
	httpx.JSON(w, http.StatusOK, map[string]any{
		"catalogue": Catalogue,
		"settings":  vals,
	})
}

func (m *Module) put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed body")
		return
	}
	ip, ua := httpx.ClientIP(r, m.trustProxy), r.UserAgent()
	if err := m.Service.Set(r.Context(), m.actor(r),
		Key(r.PathValue("key")), body.Value, ip, ua); err != nil {
		m.fail(w, err)
		return
	}
	vals, _ := m.Service.All(r.Context(), m.actor(r))
	httpx.JSON(w, http.StatusOK, map[string]any{
		"catalogue": Catalogue,
		"settings":  vals,
	})
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden",
			"only an administrator can change deployment policy")
	case errors.Is(err, ErrUnknownKey):
		httpx.Error(w, http.StatusNotFound, "not_found", "no such setting")
	case errors.Is(err, ErrBadValue):
		httpx.Error(w, http.StatusBadRequest, "bad_value",
			"that value is not allowed for this setting")
	default:
		m.log.Error("settings request failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
