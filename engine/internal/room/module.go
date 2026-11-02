package room

import (
	"context"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/modular"
)

// Module exposes the room registry to the kernel.
type Module struct {
	Registry *Registry
	Meter    *ledger.Meter
}

func NewModule(m *ledger.Meter) *Module {
	return &Module{Registry: NewRegistry(m), Meter: m}
}

func (m *Module) Name() string       { return "rooms" }
func (m *Module) Requires() []string { return nil }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("rooms.registry", m.Registry)
	return nil
}

func (m *Module) Stop(ctx context.Context) error { return nil }
