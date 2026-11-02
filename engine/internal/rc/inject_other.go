//go:build !windows

package rc

import "errors"

// ErrUnsupportedPlatform is returned by every injector method on a platform
// with no implementation.
//
// It fails closed rather than silently doing nothing. A no-op injector would
// let a control session appear to be established, be granted by the user,
// audited as active, and quietly do nothing -- which is worse than refusing,
// because the operator keeps clicking and the user watches a session they
// believe is live.
var ErrUnsupportedPlatform = errors.New("rc: input injection is not implemented on this platform")

// UnsupportedInjector refuses everything.
type UnsupportedInjector struct{}

// NewInjector returns the platform injector for this build.
func NewInjector() Injector { return &UnsupportedInjector{} }

func (UnsupportedInjector) MouseMove(x, y uint16) error        { return ErrUnsupportedPlatform }
func (UnsupportedInjector) MouseButton(b Button, d bool) error { return ErrUnsupportedPlatform }
func (UnsupportedInjector) MouseWheel(dx, dy int16) error      { return ErrUnsupportedPlatform }
func (UnsupportedInjector) Key(vk uint16, down bool) error     { return ErrUnsupportedPlatform }
func (UnsupportedInjector) Text(s string) error                { return ErrUnsupportedPlatform }
