//go:build windows

package main

import _ "embed"

// The window, taskbar and notification-area icon.
//
// Embedded rather than fetched: it is needed before the first frame, and an
// application whose icon depends on the network is an application with a blank
// icon whenever the network is slow.
//
//go:embed aether.ico
var brandIcon []byte
