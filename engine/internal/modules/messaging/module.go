package messaging

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// Module wires repository to service to handler, per the module contract.
type Module struct {
	Service *Service
	Hub     *Hub
	handler *Handler
}

func NewModule(db *database.DB, dir Directory, guard Guard,
	log *slog.Logger, origins []string, filesDir string) *Module {
	hub := NewHub(log)
	repo := NewSQLRepository(db)
	svc := NewService(repo, dir, hub, log)
	if filesDir != "" {
		if fs, err := NewDiskStore(filesDir); err == nil {
			svc.UseFiles(fs)
		} else {
			log.Error("file sharing disabled: cannot open the files directory", "dir", filesDir, "err", err)
		}
	}
	h := NewHandler(svc, hub, guard, log)
	h.Origin = origins
	return &Module{Service: svc, Hub: hub, handler: h}
}

func (m *Module) Name() string       { return "messaging" }
func (m *Module) Requires() []string { return nil }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("messaging.service", m.Service)
	k.Provide("messaging.hub", m.Hub)
	// Scheduled messages are delivered by this loop for the life of the
	// process; it stops with the kernel's context.
	go m.Service.RunScheduler(ctx)
	go m.Service.RunAutoStatus(ctx, 15*time.Second)
	// The search index is refilled once if a migration emptied it.
	if n, err := m.Service.repo.ReindexIfNeeded(ctx); err != nil {
		m.Service.log.Warn("search index rebuild failed", "err", err)
	} else if n > 0 {
		m.Service.log.Info("search index rebuilt", "messages", n)
	}
	return nil
}

func (m *Module) Stop(ctx context.Context) error { return nil }

func (m *Module) Routes(mux *http.ServeMux) { m.handler.Routes(mux) }
