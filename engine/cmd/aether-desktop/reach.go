//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Reachability, checked before the window opens.
//
// Without this the app hands somebody a WebView2 error page -- "refused to
// connect", in a window with no address bar and no way to change anything.
// That is the browser's failure message shown inside something that is not a
// browser, and it tells the person nothing they can act on: not which address
// was tried, not that it is configurable, not how.
//
// So the address is probed first and the app explains itself in its own words.

// ProbeTimeout is short on purpose. This runs before a window appears, and a
// server that has not answered in this long is not going to make the
// difference between a fast start and a slow one.
const ProbeTimeout = 4 * time.Second

// Reachability is what the probe found, and what to tell somebody about it.
type Reachability struct {
	OK bool
	// Problem is one line, in plain words, naming the address.
	Problem string
	// Hint is what to do about it.
	Hint string
}

// DescribeFailure turns a dial error into something a person can act on.
//
// The three cases are genuinely different and lead to different actions, so
// they are not collapsed into "could not connect": nothing is listening on
// that port, the name does not resolve, or it is simply not answering.
func DescribeFailure(server string, err error) Reachability {
	host := hostOf(server)
	r := Reachability{OK: false}

	switch {
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		r.Problem = fmt.Sprintf("%s did not answer within %s.", host, ProbeTimeout)
		r.Hint = "The server may be starting, or this machine may be on a different network."
	case isDNS(err):
		r.Problem = fmt.Sprintf("The name %q could not be found.", host)
		r.Hint = "Check the spelling, or use the server's address instead of its name."
	case isRefused(err):
		r.Problem = fmt.Sprintf("Nothing is listening at %s.", host)
		r.Hint = "The server is probably not running. Start it, then open Aether again."
	default:
		r.Problem = fmt.Sprintf("Could not reach %s.", host)
		r.Hint = "Check that the server is running and that this machine can reach it."
	}
	return r
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isDNS(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d)
}

func isRefused(err error) bool {
	// Matched on text as well as type: the wrapped syscall error differs
	// between platforms and Go versions, and being wrong here costs only a
	// less specific sentence.
	if errors.Is(err, context.Canceled) {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "refused") ||
		strings.Contains(s, "no connection could be made")
}

// Probe asks the server whether it is there.
//
// The certificate is not verified. This is a deliberate exception and a narrow
// one: the probe reads no response body and carries no credentials, it exists
// only to tell "nothing is there" apart from "something is there", and the
// normal case on a local network is the server's own self-signed certificate.
// The window that follows makes its own trust decision.
func Probe(ctx context.Context, server string) Reachability {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	u, err := url.Parse(server)
	if err != nil {
		return Reachability{Problem: fmt.Sprintf("%q is not a valid address.", server),
			Hint: "Run Aether with -server followed by the address."}
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return DescribeFailure(server, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return DescribeFailure(server, err)
	}
	defer resp.Body.Close()

	// Any answer at all means something is listening and speaking HTTP, which
	// is the question being asked. A 404 from a misconfigured path is still a
	// reachable server, and the client will say so far better than this can.
	return Reachability{OK: true}
}
