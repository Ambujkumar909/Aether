//go:build windows

package main

import (
	"errors"
	"net/url"
)

var errWebViewUnavailable = errors.New(
	"the WebView2 runtime is not available on this machine; install the Microsoft Edge WebView2 Runtime")

// hostOf is the host and port of a base URL, empty if it has none.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Host
}
