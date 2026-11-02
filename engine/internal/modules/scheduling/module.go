package scheduling

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Module wires repository to service to handler, per the module contract.
type Module struct {
	Service *Service
	handler *Handler
}

func NewModule(db *database.DB, rec audit.Recorder, guard Guard,
	log *slog.Logger, trustProxy bool) *Module {
	repo := NewSQLRepository(db)
	svc := NewService(repo, rec, log)
	return &Module{
		Service: svc,
		handler: NewHandler(svc, guard, log, trustProxy),
	}
}

func (m *Module) Name() string { return "scheduling" }

// Scheduling records every change, so audit must be up first.
func (m *Module) Requires() []string { return []string{"audit"} }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("scheduling.reader", Reader(m.Service))
	k.Provide("scheduling.service", m.Service)
	return nil
}

func (m *Module) Stop(ctx context.Context) error { return nil }

func (m *Module) Routes(mux *http.ServeMux) { m.handler.Routes(mux) }
