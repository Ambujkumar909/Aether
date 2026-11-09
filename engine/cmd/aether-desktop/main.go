//go:build windows

// Aether for Windows.
//
// Why this is written in Go rather than as a Tauri or Electron shell.
//
// The feature that justifies a desktop application at all is ringing when
// nothing is open: a browser tab must exist to receive a call, and a tray
// process does not. That means the app has to hold the messaging socket open
// itself and decode the binary wire format -- which already exists here, in
// internal/wire, tested and byte-compatible with the browser. A Rust or
// JavaScript shell would need a second implementation of that format, and this
// codebase has already paid for a protocol implemented twice: the interop
// harness exists because the two opcode tables silently disagreed and frames
// went nowhere with nothing in either log to say why.
//
// So the shell is Go, the window is the WebView2 runtime that Windows already
// ships, and there is exactly one implementation of the protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const appName = "Aether"

func main() {
	// Before anything else, and before any window exists. A process that has
	// not said this is told the screen is 96 DPI, renders at that size, and
	// has the result bitmap-stretched to the real scaling factor -- which is
	// what makes a window look like a remote desktop session.
	dpiOK := declareDPIAware()

	// -install / -uninstall are the setup program (install_windows.go).
	if handled, code := installCommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	// Two roles in one executable (see supervise.go): the parent supervises,
	// the child -- marked with -child -- is the application. The parent
	// needs only the settings directory, for the log.
	args, child := isChild(os.Args[1:])
	if !child {
		dir, err := configDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot find a place to keep settings:", err)
			os.Exit(1)
		}
		os.Exit(supervise(dir, args))
	}

	server := flag.String("server", "", "the Aether server to connect to, e.g. https://172.22.187.37:8443")
	reset := flag.Bool("reset", false, "forget the saved server and settings")
	verbose := flag.Bool("v", false, "log what the agent is doing")
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(1)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("role", "app")

	dir, err := configDir()
	if err != nil {
		log.Error("cannot find a place to keep settings", "err", err)
		os.Exit(1)
	}
	if *reset {
		if err := os.Remove(filepath.Join(dir, configFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Error("could not reset", "err", err)
			os.Exit(1)
		}
		fmt.Println("Settings cleared.")
		return
	}

	cfg, err := loadConfig(dir)
	if err != nil {
		log.Warn("settings unreadable, starting fresh", "err", err)
		cfg = Config{}
	}

	// A server given on the command line wins and is remembered, so the app is
	// configured once and never asks again.
	if *server != "" {
		normalised, err := NormaliseServer(*server)
		if err != nil {
			fmt.Fprintf(os.Stderr, "That does not look like a server address: %v\n", err)
			os.Exit(2)
		}
		cfg.Server = normalised
		if err := saveConfig(dir, cfg); err != nil {
			log.Warn("could not remember the server", "err", err)
		}
	}

	if cfg.Server == "" {
		fmt.Fprintf(os.Stderr, `%s has not been told which server to use.

Run it once with the address your administrator gave you:

    aether-desktop.exe -server https://aether.example.com

or, on a local network:

    aether-desktop.exe -server https://192.168.1.20:8443

It is remembered after that.
`, appName)
		os.Exit(2)
	}

	// Which executable this is. Two copies of the app existed on one machine
	// once -- a stale one at the repository root and the current one beside
	// the source -- and every report about "the app" could have been about
	// either. The path and its build time make the log unambiguous.
	exe, _ := os.Executable()
	cleanOldUpdate(exe) // the version this one replaced, if any
	built := ""
	if st, err := os.Stat(exe); err == nil {
		built = st.ModTime().Format("2006-01-02 15:04:05")
	}
	log.Info("starting", "server", cfg.Server, "settings", dir, "dpiAware", dpiOK, "exe", exe, "built", built)

	// One window, however many times somebody double-clicks. A second launch
	// raises the window that already exists.
	if !claimSingleInstance("Local\\AetherDesktopSingleInstance") {
		log.Info("already running; raised the existing window")
		return
	}

	// Ask the server whether it is there before opening a window at it.
	// Without this the person gets a WebView2 error page -- the browser's
	// failure message, inside something that is not a browser, with no
	// address bar and nothing to act on.
	if r := Probe(context.Background(), cfg.Server); !r.OK {
		fmt.Fprintf(os.Stderr, "%s could not open.\n\n%s\n%s\n\nTo point Aether somewhere else:\n\n    aether-desktop.exe -server https://your-server:8443\n",
			appName, r.Problem, r.Hint)
		showProblem(appName+" could not open", r.Problem+"\n\n"+r.Hint+
			"\n\nTo point Aether somewhere else, run it once with:\n"+
			"aether-desktop.exe -server https://your-server:8443")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, dir, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}

// NormaliseServer turns whatever somebody types into a base URL, or explains
// why it cannot.
//
// People paste all of these: a bare address, a host and port, a full URL, a
// URL with the meeting path still on the end, and a trailing slash. Every one
// of them means the same server, and refusing four of the five because the
// fifth is the canonical spelling is the kind of thing that makes software
// feel hostile on the first screen anybody sees.
func NormaliseServer(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", errors.New("no address given")
	}
	// A scheme is assumed rather than demanded. HTTPS, because the browser
	// refuses camera and microphone access anywhere else.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("%q is not a web address", in)
	}
	if u.Host == "" {
		return "", errors.New("no host in that address")
	}
	// Everything after the host is the application's business, not the
	// server's identity.
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// WebSocketBase is the same server, spelled for a socket.
func WebSocketBase(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u.String(), nil
}

func run(ctx context.Context, cfg Config, dir string, log *slog.Logger) error {
	agent := NewAgent(cfg, dir, log)

	// The ring listener runs whether or not a window is open. That is the
	// whole reason this program exists: a browser tab has to be open to be
	// rung, and a tray process does not.
	go agent.WatchForCalls(ctx)

	// The window. Closing it leaves the agent running, the way every other
	// communications client on this desktop behaves.
	return agent.RunWindow(ctx)
}
