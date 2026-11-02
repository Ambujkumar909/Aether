package sfu

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/modular"
	"github.com/pion/webrtc/v4"
)

// How often the media plane re-reads the default-route interface. One
// connected-but-unsent UDP socket and an interface list: microseconds. A
// joiner after an address change waits at most this long for media that
// can connect.
const addressWatchInterval = 3 * time.Second

// Module exposes the media plane to the kernel.
type Module struct {
	Registry *Registry
}

// NewModule builds the media plane. mediaPort is the single UDP+TCP port all
// media uses; 0 disables the shared port and lets every connection pick its
// own, which is the behaviour that does not survive a firewall.
func NewModule(meter *ledger.Meter, log *slog.Logger, ice []webrtc.ICEServer, bindHost string, mediaPort int) (*Module, error) {
	rg := NewRegistry(meter, log, ice)
	if mediaPort > 0 {
		// Bind the addresses the policy chose, not the operator's web bind:
		// browsers never offer a loopback candidate, so media on 127.0.0.1
		// alone can never connect to one. bindHost is honoured only when it
		// is a specific non-loopback address.
		addrs := rg.Addrs.BindAddresses()
		fixed := false
		if ip := net.ParseIP(bindHost); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			addrs = []string{bindHost}
			fixed = true
		}
		t, err := ListenMedia(addrs, mediaPort, log)
		if err != nil {
			return nil, err
		}
		rg.Transport = t
		// The operator named an address: that is theirs to keep. Otherwise
		// follow the machine's address as it changes.
		if !fixed && len(addrs) > 0 {
			go rg.WatchAddresses(addressWatchInterval, nil)
		}
	}
	return &Module{Registry: rg}, nil
}

func (m *Module) Name() string       { return "media" }
func (m *Module) Requires() []string { return nil }

func (m *Module) Start(ctx context.Context, k *modular.Kernel) error {
	k.Provide("media.registry", m.Registry)
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	m.Registry.Close()
	return nil
}
