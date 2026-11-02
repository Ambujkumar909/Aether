package audit

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Module wires repository to service to handler, per the module contract.
type Module struct {
	Service *Service
	handler *Handler
	log     *slog.Logger
}

func NewModule(db *database.DB, guard Guard, log *slog.Logger, trustProxy bool) *Module {
	repo := NewSQLRepository(db)
	svc := NewService(repo, log)
	return &Module{
		Service: svc,
		handler: NewHandler(svc, guard, log, trustProxy),
		log:     log,
	}
}

func (m *Module) Name() string       { return "audit" }
func (m *Module) Requires() []string { return nil }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	if err := m.Service.Start(ctx); err != nil {
		return err
	}
	seq, hash := m.Service.Head(ctx)
	m.log.Info("audit chain loaded", "head", seq, "hash", shortHash(hash))
	k.Provide("audit.recorder", Recorder(m.Service))
	k.Provide("audit.reader", Reader(m.Service))
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	m.Service.Stop()
	return nil
}

// Routes registers the endpoints this module owns.
func (m *Module) Routes(mux *http.ServeMux) { m.handler.Routes(mux) }

// Recorder exposes the narrow write interface to other modules. Nothing else
// in the process can append to the log.
func (m *Module) Recorder() Recorder { return m.Service }

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
