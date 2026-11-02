// Package modular is the backbone of the monolith.
//
// A modular monolith is not a microservice architecture that failed to
// deploy. It is a deliberate choice: one process, one binary, one deploy,
// but with module boundaries enforced at compile time and dependencies
// declared rather than discovered.
//
// The reason to want this for a media server specifically is latency. The
// distance between "a packet arrived" and "the packet was forwarded" is the
// product. Every process boundary crossed on that path is a serialisation, a
// scheduler hop, and a queue -- and the industry's habit of splitting
// signalling, media, and presence into separate services means a single mute
// toggle traverses three networks before it reaches the person who needs to
// see it.
//
// So: one process. But a monolith without enforced seams rots into a
// codebase where the SFU reaches into the auth tables. The kernel here makes
// the seams explicit and startup order derivable, so the discipline survives
// contact with a deadline.
package modular

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Module is one bounded piece of the system.
type Module interface {
	// Name is the module identifier used in dependency declarations.
	Name() string

	// Requires lists module names that must be started first. The kernel
	// refuses to run if this graph has a cycle, so an accidental circular
	// dependency is a startup failure rather than a mysterious nil at
	// three in the morning.
	Requires() []string

	// Start brings the module up. It must return promptly; long-running
	// work belongs on a goroutine the module owns and stops in Stop.
	Start(ctx context.Context, k *Kernel) error

	// Stop shuts the module down. It is called in exact reverse start
	// order, so a module may rely on its dependencies still being alive.
	Stop(ctx context.Context) error
}

// Kernel owns module lifecycle and the service registry.
type Kernel struct {
	mu       sync.RWMutex
	modules  map[string]Module
	order    []string
	started  []string
	services map[string]any

	ShutdownTimeout time.Duration
}

func New() *Kernel {
	return &Kernel{
		modules:         make(map[string]Module),
		services:        make(map[string]any),
		ShutdownTimeout: 15 * time.Second,
	}
}

// Register adds a module. Registration order is irrelevant; the kernel
// derives the real order from the dependency graph.
func (k *Kernel) Register(mods ...Module) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, m := range mods {
		if _, dup := k.modules[m.Name()]; dup {
			return fmt.Errorf("modular: duplicate module %q", m.Name())
		}
		k.modules[m.Name()] = m
	}
	return nil
}

// Provide publishes a value other modules may resolve. This is the only
// sanctioned way for modules to reach each other, and it is deliberately
// string-keyed and explicit rather than a reflective container: a dependency
// you cannot grep for is a dependency nobody can reason about.
func (k *Kernel) Provide(name string, v any) {
	k.mu.Lock()
	k.services[name] = v
	k.mu.Unlock()
}

// Resolve fetches a published service.
func Resolve[T any](k *Kernel, name string) (T, error) {
	k.mu.RLock()
	v, ok := k.services[name]
	k.mu.RUnlock()
	var zero T
	if !ok {
		return zero, fmt.Errorf("modular: no service %q", name)
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("modular: service %q is %T, not %T", name, v, zero)
	}
	return t, nil
}

// MustResolve is Resolve for wiring code that cannot meaningfully continue.
func MustResolve[T any](k *Kernel, name string) T {
	v, err := Resolve[T](k, name)
	if err != nil {
		panic(err)
	}
	return v
}

var ErrDependencyCycle = errors.New("modular: dependency cycle")

// plan computes a deterministic topological start order.
//
// Determinism matters more than it looks: a start order that varies between
// runs turns an ordering bug into an intermittent one, and intermittent
// startup bugs are among the most expensive defects a team can own. Ties are
// therefore broken alphabetically rather than by map iteration.
func (k *Kernel) plan() ([]string, error) {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(k.modules))
	var order []string
	var stack []string

	names := make([]string, 0, len(k.modules))
	for n := range k.modules {
		names = append(names, n)
	}
	sort.Strings(names)

	var visit func(string) error
	visit = func(n string) error {
		switch colour[n] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("%w: %v -> %s", ErrDependencyCycle, stack, n)
		}
		m, ok := k.modules[n]
		if !ok {
			return fmt.Errorf("modular: module %q required but not registered", n)
		}
		colour[n] = grey
		stack = append(stack, n)

		deps := append([]string(nil), m.Requires()...)
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
		order = append(order, n)
		return nil
	}

	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Start brings every module up in dependency order. If any module fails, the
// ones already started are stopped in reverse, so a failed startup leaves
// nothing running.
func (k *Kernel) Start(ctx context.Context) error {
	k.mu.Lock()
	order, err := k.plan()
	if err != nil {
		k.mu.Unlock()
		return err
	}
	k.order = order
	k.mu.Unlock()

	for _, name := range order {
		k.mu.RLock()
		m := k.modules[name]
		k.mu.RUnlock()

		if err := m.Start(ctx, k); err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), k.ShutdownTimeout)
			_ = k.stopStarted(stopCtx)
			cancel()
			return fmt.Errorf("modular: starting %q: %w", name, err)
		}
		k.mu.Lock()
		k.started = append(k.started, name)
		k.mu.Unlock()
	}
	return nil
}

// Stop shuts everything down in reverse start order.
func (k *Kernel) Stop(ctx context.Context) error { return k.stopStarted(ctx) }

func (k *Kernel) stopStarted(ctx context.Context) error {
	k.mu.Lock()
	started := append([]string(nil), k.started...)
	k.started = nil
	k.mu.Unlock()

	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		name := started[i]
		k.mu.RLock()
		m := k.modules[name]
		k.mu.RUnlock()
		if err := m.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stopping %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Order reports the computed start order, for logging and for tests that
// assert the graph is what the author believed it was.
func (k *Kernel) Order() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return append([]string(nil), k.order...)
}
