package signal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// Behind a proxy every connection arrives from the proxy's address. The
// same-device advisory must use the address the proxy forwards when the
// deployment trusts it, or everyone in every meeting is told they share a
// device with everyone else (the 50-person stress run).
func TestSameDeviceUsesTheForwardedAddressBehindAProxy(t *testing.T) {
	for i, tc := range []struct {
		name  string
		trust bool
		ips   [2]string
		told  bool
	}{
		{"trusted proxy, two people", true, [2]string{"10.0.0.1", "10.0.0.2"}, false},
		{"trusted proxy, one machine", true, [2]string{"10.0.0.7", "10.0.0.7"}, true},
		{"no proxy: the header is not believed", false, [2]string{"10.0.0.1", "10.0.0.2"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter := ledger.NewMeter("t")
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
			g := NewGateway(room.NewRegistry(meter), sfu.NewRegistry(meter, quiet, nil), meter, quiet)
			g.Origin = []string{"*"}
			g.TrustProxy = tc.trust
			// Signed in, so the room admits them rather than holding them in the lobby.
			g.Identify = func(r *http.Request) (string, string) {
				u := r.Header.Get("X-Test-User")
				return u, u + "@example.test"
			}
			srv := httptest.NewServer(g)
			t.Cleanup(srv.Close)
			url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/rtc?room=proxy-" + fmt.Sprint(i) + "&name=x"
			told := make(chan struct{}, 4)
			for k, ip := range tc.ips {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Forwarded-For": {ip}, "X-Test-User": {fmt.Sprint("u", k)}}})
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.CloseNow() })
				go func() {
					for {
						_, d, err := c.Read(context.Background())
						if err != nil {
							return
						}
						var each func([]byte) error
						each = func(f []byte) error {
							r := wire.NewReader(f, nil)
							h, err := r.ReadHeader()
							if err != nil {
								return nil
							}
							if h.Op == wire.OpBatch {
								return wire.SplitBatch(r, each)
							}
							if h.Op == wire.OpSameDevice {
								told <- struct{}{}
							}
							return nil
						}
						_ = each(d)
					}
				}()
			}
			select {
			case <-told:
				if !tc.told {
					t.Fatal("told they share a device")
				}
			case <-time.After(1500 * time.Millisecond):
				if tc.told {
					t.Fatal("not told they share a device")
				}
			}
		})
	}
}
