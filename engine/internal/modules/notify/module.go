package notify

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/webpush"
)

// Module wires repository to service to handler.
type Module struct {
	Service *Service
	handler *Handler
}

// NewModule builds the module. A nil key disables push: subscriptions are
// refused with a clear answer and Notify is a no-op.
func NewModule(db *database.DB, guard Guard, log *slog.Logger, key *webpush.Key, contact string) *Module {
	var sender *webpush.Sender
	if key != nil {
		sender = webpush.NewSender(key, contact)
	}
	svc := NewService(NewSQLRepository(db), sender, log)
	return &Module{Service: svc, handler: NewHandler(svc, guard, log)}
}

func (m *Module) Name() string { return "notify" }

// Subscriptions reference sessions, which identity owns.
func (m *Module) Requires() []string { return []string{"identity"} }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("notify.service", m.Service)
	m.Service.Start(4)
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	m.Service.Stop()
	return nil
}

func (m *Module) Routes(mux *http.ServeMux) { m.handler.Routes(mux) }
