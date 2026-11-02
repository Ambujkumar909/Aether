package control

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// SweepInterval is how often lapsed grants are noticed.
//
// A grant is enforced by deadline at the point of injection, so nothing can be
// injected past it whatever this sweep does; the sweep exists to record the
// expiry on the audit chain and to tell both clients, so the user sees the
// session close rather than watching a stale banner. Five seconds is well
// inside what a person reads as immediate.
const SweepInterval = 5 * time.Second

type Module struct {
	Service *Service
	handler *Handler
	log     *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func NewModule(db *database.DB, rec audit.Recorder, guard Guard,
	log *slog.Logger, trustProxy bool) *Module {
	svc := NewService(NewSQLRepository(db), rec, log)
	return &Module{
		Service: svc,
		handler: NewHandler(svc, guard, log, trustProxy),
		log:     log,
	}
}

func (m *Module) Name() string { return "control" }

// Control records every transition, so audit must be running first.
func (m *Module) Requires() []string { return []string{"audit"} }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("control.reader", Reader(m.Service))
	k.Provide("control.service", m.Service)

	loopCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go m.sweep(loopCtx)
	return nil
}

// Stop halts the sweep. Idempotent, and safe if Start never ran.
func (m *Module) Stop(ctx context.Context) error {
	m.once.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait()
	})
	return nil
}

func (m *Module) Routes(mux *http.ServeMux) { m.handler.Routes(mux) }

func (m *Module) sweep(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			gone, err := m.Service.ExpireDue(ctx, now.UTC())
			if err != nil {
				m.log.Error("control expiry sweep failed", "err", err)
				continue
			}
			for _, s := range gone {
				m.log.Info("control session expired",
					"session", s.ID, "controller", s.ControllerEmail,
					"subject", s.SubjectEmail)
			}
		}
	}
}
