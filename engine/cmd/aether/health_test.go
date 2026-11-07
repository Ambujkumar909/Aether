package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthCheckReportsWhatTheServerSays(t *testing.T) {
	status := http.StatusOK
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	})
	for _, useTLS := range []bool{false, true} {
		srv := httptest.NewUnstartedServer(h)
		if useTLS {
			srv.StartTLS()
		} else {
			srv.Start()
		}
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		addr := ":" + port // what -addr usually is: every interface
		status = http.StatusOK
		if got := checkHealth(addr, useTLS); got != 0 {
			t.Errorf("tls=%v healthy server: exit %d", useTLS, got)
		}
		status = http.StatusServiceUnavailable
		if got := checkHealth(addr, useTLS); got != 1 {
			t.Errorf("tls=%v unhealthy server: exit %d", useTLS, got)
		}
		// The wrong scheme is a failure, not a pass.
		status = http.StatusOK
		if got := checkHealth(addr, !useTLS); got != 1 {
			t.Errorf("tls=%v probed with the wrong scheme: exit %d", useTLS, got)
		}
		srv.Close()
		if got := checkHealth(addr, useTLS); got != 1 {
			t.Errorf("tls=%v nothing listening: exit %d", useTLS, got)
		}
	}
	if checkHealth("garbage", false) != 1 {
		t.Error("a malformed address passed")
	}
}
