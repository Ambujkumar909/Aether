package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// checkHealth asks the server already running on addr for /healthz and returns
// the process exit code: 0 healthy, 1 not. It exists for the container's health
// check: the image has no shell and no curl, so the binary checks itself.
//
// It dials loopback, whatever host addr names, and does not verify the
// certificate -- that is issued for the public hostname, not 127.0.0.1, and a
// probe from inside the container proves the server answers, not who it is.
func checkHealth(addr string, useTLS bool) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return 1
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback self-probe; see above
		},
	}
	resp, err := c.Get(scheme + "://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
