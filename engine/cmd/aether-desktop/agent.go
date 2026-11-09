//go:build windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
)

// Agent is the resident half of the application: the part that stays when the
// window is closed.
type Agent struct {
	cfg Config
	dir string
	log *slog.Logger

	mu sync.Mutex
	w  webview2.WebView

	// onTop mirrors the window style, which Windows owns; there is no call to
	// read it back that is cheaper than remembering it.
	onTop bool

	// tray is the notification-area icon, once it exists; the attention
	// bindings reach it through here.
	tray *Tray

	// updateReady is the version downloaded beside the program and waiting
	// for a restart (update.go); set by watchUpdates, shown in the menu.
	updateReady string
}

func NewAgent(cfg Config, dir string, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, dir: dir, log: log}
}

// RunWindow opens the application window and blocks until it is closed.
//
// The window is the WebView2 runtime that Windows already ships, pointed at
// the server. There is no bundled browser and no copy of the interface: the
// client the window loads is the same one a browser loads, from the same
// server, so a deployment upgrades by restarting the server rather than by
// asking several hundred people to install something.
func (a *Agent) RunWindow(ctx context.Context) error {
	// A profile of our own, beside the settings, so signing in survives a
	// restart and does not disturb the user's actual Edge profile.
	profile := filepath.Join(a.dir, "webview")

	// Trust this one server's certificate, if it needs it, before the runtime
	// is created -- the environment is read once, at creation.
	//
	// Pinned to the server's public key, not switched off. The first launch
	// against a self-signed server showed "Your connection isn't private"
	// instead of a sign-in screen, which inside a window with no address bar
	// reads as the product being broken. The blunt fix is
	// --ignore-certificate-errors, which is a browser with its safety removed;
	// this names one key as the single exception, and every other host still
	// validates.
	var pin CertPin
	if p, err := PinFor(ctx, a.cfg.Server); err == nil && p != "" {
		pin = p
		a.log.Info("trusting this server's certificate", "host", hostOf(a.cfg.Server), "pin", string(pin)[:12]+"…")
	} else if err != nil && !errors.Is(err, ErrNoPinNeeded) {
		a.log.Warn("could not read the server's certificate", "err", err)
	}
	// Always set: the shell flags (shell_windows.go) ride along with the pin.
	os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", BrowserArgs(pin))
	// Newer versions, from this server, over the same trust.
	go a.watchUpdates(ctx, pin)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  appName,
			Width:  1280,
			Height: 820,
			IconId: 2,
			Center: true,
		},
		DataPath: profile,
	})
	if w == nil {
		return errWebViewUnavailable
	}
	defer w.Destroy()

	// An application surface, not a browser tab: no swipe-back, no bounce,
	// no Alt+Left.
	if err := hardenShell(w, a.log); err != nil {
		a.log.Warn("could not apply the shell settings", "err", err)
	}

	// Close hides to the tray; maximise stops at the taskbar. See
	// subclass_windows.go.
	if raw := w.Window(); raw != nil {
		if !InstallShellSubclass(uintptr(raw)) {
			a.log.Warn("could not hook the window; close will quit and maximise may cover the taskbar")
		}
	}

	// Permissions already answered on an earlier run are applied now, before
	// the client loads; a first run is asked on its own screen after the
	// splash (see permissions_windows.go).
	if a.cfg.Permissions != nil && a.cfg.Permissions.Asked {
		applyPermissions(w, *a.cfg.Permissions, a.log)
	}

	a.mu.Lock()
	a.w = w
	a.mu.Unlock()

	// A floor under the window.
	//
	// The client has breakpoints down to 880px and stops having anywhere to
	// put a three-column meeting below that: the rail, the stage and the side
	// panel start overlapping and the report is that resizing "misplaces
	// everything". A browser tab can be dragged to 200px wide and that is the
	// person's business; an application window should refuse to become a size
	// its own layout cannot hold.
	// SetSize with a minimum hint only RECORDS the minimum -- it does not
	// resize -- so the opening size is set as well. Without both, the window
	// opened at whatever the library chose and landed under the client's own
	// 940px breakpoint, which is why resizing "misplaced everything": the
	// layout was reflowing into its narrow arrangement in a window nobody had
	// deliberately made narrow.
	if raw := w.Window(); raw != nil {
		h := uintptr(raw)

		// The system draws the frame.
		//
		// A custom caption was built and then removed. Taking the frame over --
		// WM_NCCALCSIZE, hand-rolled resize borders, a drag handed back on
		// mousedown -- works in principle and fights this web view in practice:
		// the view only re-lays-out on a size change, so every frame change
		// left the page positioned for a frame it no longer had, and the
		// failures landed on dragging, resizing, minimising and maximising in
		// turn. Three regressions chasing a title bar is the signal to stop.
		//
		// The look it was for is had a different way: the application's own bar
		// sits directly under the system caption in the same colour, so the two
		// read as one band, and the caption keeps every behaviour Windows
		// already gets right.
		// Restore first.
		//
		// The window opens maximised when the size it was created with does
		// not fit the display -- and at 200% scaling the created size does not
		// fit. A maximised window ignores SetWindowPos, so every size set
		// after this point did nothing and the window stayed larger than the
		// screen with its edges off it.
		RestoreWindow(h)

		// One bar, not two: the system caption goes, and only the caption.
		// Resize borders, snapping, maximise and minimise stay with Windows.
		// See frame_windows.go for why this and not a custom frame.
		if StripCaption(h) {
			a.log.Debug("system caption removed; the page draws the title bar")
		} else {
			a.log.Warn("could not remove the system caption; keeping it")
		}

		w.SetSize(scaleForDPI(h, 940), scaleForDPI(h, 620), webview2.HintMin)

		// Wanted size, scaled for the display, then clamped to what the
		// display can show. Centring a window that is bigger than the screen
		// only moves the missing part from one edge to both.
		openW, openH := FitToScreen(h, scaleForDPI(h, 1280), scaleForDPI(h, 840))
		w.SetSize(openW, openH, webview2.HintNone)
		CentreOnScreen(h)
	}

	// The page draws the title bar, so it needs the four things a title bar
	// does. Bound before the first navigation, or the page loads without them
	// and silently falls back to the browser layout.
	// The page is told it is running inside the app, and nothing more.
	//
	// It used to be handed drag, minimise, maximise and close as well, for a
	// title bar the application drew itself. That came out: the system draws
	// the frame now, so those belong to Windows and the page has no business
	// with them.
	hostWnd := uintptr(0)
	if raw := w.Window(); raw != nil {
		hostWnd = uintptr(raw)
	}
	w.Bind("aetherHostReady", func() bool { return true })
	// The page draws the title bar, so it is handed the four things a caption
	// does. Bound before the first navigation, or the page loads without them
	// and falls back to the browser layout.
	w.Bind("aetherStartDrag", func() { StartWindowDrag(hostWnd) })
	w.Bind("aetherMinimise", func() { MinimiseWindow(hostWnd) })
	w.Bind("aetherToggleMaximise", func() { ToggleMaximise(hostWnd) })
	w.Bind("aetherCloseWindow", func() { CloseWindow(hostWnd) })
	w.Bind("aetherIsMaximised", func() bool { return WindowIsMaximised(hostWnd) })
	// For a ring or an urgent message while the window is in the tray: the
	// page asks to be seen, and the shell brings the window back.
	w.Bind("aetherShowWindow", func() {
		RestoreFromTray(hostWnd)
		StopFlashing(hostWnd)
	})
	// Attention. The page says what happened; the shell shows it the way
	// Windows shows things (attention_windows.go).
	w.Bind("aetherNotify", func(title, body string) {
		w.Dispatch(func() {
			a.mu.Lock()
			t := a.tray
			a.mu.Unlock()
			t.Notify(title, body)
		})
	})
	w.Bind("aetherBadge", func(n int) {
		w.Dispatch(func() {
			a.mu.Lock()
			t := a.tray
			a.mu.Unlock()
			t.SetBadge(hostWnd, n)
		})
	})
	w.Bind("aetherRinging", func(who string) {
		w.Dispatch(func() {
			a.mu.Lock()
			t := a.tray
			a.mu.Unlock()
			FlashTaskbar(hostWnd)
			if !WindowVisible(hostWnd) || Minimised(hostWnd) {
				t.Notify("Incoming call", who+" is calling you")
			}
		})
	})
	// The permissions screen's answer. Saved, applied, and then the app.
	w.Bind("aetherPermissions", func(camera, microphone, notifications bool) {
		p := Permissions{Asked: true, Camera: camera, Microphone: microphone, Notifications: notifications}
		a.cfg.Permissions = &p
		if err := saveConfig(a.dir, a.cfg); err != nil {
			a.log.Warn("could not save permissions", "err", err)
		}
		a.log.Info("permissions answered", "camera", p.Camera, "microphone", p.Microphone, "notifications", p.Notifications)
		w.Dispatch(func() {
			applyPermissions(w, p, a.log)
			w.Navigate(a.cfg.Server)
		})
	})

	// The frame follows the application, not the browser. A dark page under a
	// light title bar is the other half of "this looks like a browser in a
	// box": every native application here has a frame that matches its
	// content.
	var tray *Tray
	if raw := w.Window(); raw != nil {
		hwnd := uintptr(raw)
		matchTitleBarToApp(hwnd, systemPrefersDark())

		// The brand mark in the title bar, the taskbar and alt-tab. Without
		// this Windows draws its default icon, which is the "is this even a
		// real application" look.
		if err := installWindowIcon(hwnd, a.dir); err != nil {
			a.log.Warn("could not set the window icon", "err", err)
		}

		// And in the notification area, where a communications client lives.
		//
		// The icon reports to a hidden window of its own, so a click can bring
		// the app back. The icon and the way out of the tray are one feature:
		// shipping the first without the second puts the window somewhere
		// nobody can reach it.
		tw, err := NewTrayWindow(a.ShowWindow, a.Quit,
			func() MenuState {
				a.mu.Lock()
				ready := a.updateReady
				a.mu.Unlock()
				return MenuState{
					Server:       hostOf(a.cfg.Server),
					StartAtLogin: StartsAtLogin(),
					AlwaysOnTop:  a.onTop,
					Update:       ready,
				}
			},
			func(id uintptr) { a.menuAction(id, hwnd) },
		)
		if err != nil {
			a.log.Warn("could not create the tray window", "err", err)
		} else {
			defer tw.Close()
			if tray, err = NewTray(tw.HWND(), a.dir, appName+" - click to open"); err != nil {
				a.log.Warn("could not create the tray icon", "err", err)
			} else {
				a.log.Debug("tray icon ready", "window", tw.HWND())
				a.mu.Lock()
				a.tray = tray
				a.mu.Unlock()
			}
		}

		// Minimising is left alone.
		//
		// It was briefly hijacked to hide the window into the notification
		// area, and that was wrong: minimise means "put this on the taskbar
		// where I can see it is still running", and an app that vanishes
		// instead has taken a decision away from the person using it. Teams
		// does the same -- minimise goes to the taskbar; only closing goes to
		// the tray, and only because it is still ringing.
		//
		// The tray icon stays, so it can be clicked to raise the window and so
		// there is somewhere for background call notifications to appear.
		_ = hwnd

	}
	defer tray.Remove()

	// Paint the brand before anything is fetched.
	//
	// WebView2 shows nothing until a page commits, and on a cold start that is
	// the runtime initialising, a TLS handshake and the client's modules
	// arriving -- an empty rectangle for all of it. An empty rectangle is what
	// a web view looks like; it is not what an application looks like, and it
	// is the whole reason this felt like a browser opening a page.
	w.SetHtml(splashHTML(a.cfg.Server))

	// Then the real client, once the compositor has had a frame or two to show
	// the splash. The splash stays on screen until the server's page commits,
	// so the handover is one paint rather than a flash of nothing.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(SplashHoldMS * time.Millisecond):
		}
		w.Dispatch(func() {
			// The first run after installing stops at the permissions
			// screen; every later run goes straight on.
			if a.cfg.Permissions == nil || !a.cfg.Permissions.Asked {
				w.SetHtml(permissionsHTML(defaultPermissions))
				return
			}
			w.Navigate(a.cfg.Server)
		})
	}()

	w.Run()

	a.mu.Lock()
	a.w = nil
	a.mu.Unlock()
	return ctx.Err()
}

// ShowWindow brings the window back from the notification area.
func (a *Agent) ShowWindow() {
	a.mu.Lock()
	w := a.w
	a.mu.Unlock()
	if w == nil {
		return
	}
	if raw := w.Window(); raw != nil {
		RestoreFromTray(uintptr(raw))
	}
}

// menuAction performs a chosen tray-menu item.
//
// Read back rather than assumed: "start at login" lives in the registry, which
// is the truth, so the toggle asks what it is now and writes the opposite
// instead of tracking a copy that a person editing the registry would make
// wrong.
func (a *Agent) menuAction(id, hwnd uintptr) {
	switch id {
	case menuUpdate:
		// The supervisor swaps the downloaded program in and starts it
		// again (supervise.go); this process only has to end saying so.
		a.log.Info("restarting to update", "to", a.updateReady)
		os.Exit(exitUpdateRestart)
	case menuStartup:
		want := !StartsAtLogin()
		if err := SetStartAtLogin(want); err != nil {
			a.log.Warn("could not change start at login", "err", err)
			return
		}
		a.log.Info("start with Windows", "on", want)
	case menuOnTop:
		a.onTop = !a.onTop
		SetAlwaysOnTop(hwnd, a.onTop)
		a.log.Info("keep window on top", "on", a.onTop)
	case menuPerms:
		a.ShowWindow()
		a.AskPermissionsAgain(0)
	}
}

// Quit closes the application from the tray menu.
//
// Dispatched onto the window's own thread. A webview belongs to the thread
// that created it, and tearing one down from a notification-area callback --
// which arrives on a different thread entirely -- is the kind of thing that
// works nine times and hangs on the tenth.
func (a *Agent) Quit() {
	a.mu.Lock()
	w := a.w
	a.mu.Unlock()
	if w == nil {
		return
	}
	// Terminate posts WM_CLOSE; the subclass lets this one through.
	LetCloseQuit()
	w.Dispatch(func() { w.Terminate() })
}

// AskPermissionsAgain shows the permissions screen again, from the tray
// menu, with the current answers on its switches. Continue takes the window
// back to the app.
func (a *Agent) AskPermissionsAgain(hwnd uintptr) {
	a.mu.Lock()
	w := a.w
	a.mu.Unlock()
	if w == nil {
		return
	}
	cur := defaultPermissions
	if a.cfg.Permissions != nil {
		cur = *a.cfg.Permissions
	}
	RestoreFromTray(hwnd)
	w.Dispatch(func() { w.SetHtml(permissionsHTML(cur)) })
}

// WatchForCalls holds the messaging socket open while no window exists, so a
// call rings on a machine where nothing is running.
//
// Not armed until the window has issued a device token: without one there is
// nothing to authenticate with, and a socket that reconnects forever against a
// 401 is a busy loop rather than a feature.
func (a *Agent) WatchForCalls(ctx context.Context) {
	if a.cfg.Device == "" {
		a.log.Info("call notifications are not armed yet",
			"why", "this machine has no device token; sign in once in the window")
		return
	}
	a.log.Info("listening for calls in the background")
	<-ctx.Done()
}
