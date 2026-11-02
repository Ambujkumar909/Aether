//go:build windows

package main

import (
	"errors"
	"log/slog"
	"reflect"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
)

// The web view is an application surface, not a browser tab.
//
// Left to itself WebView2 keeps three browser habits that have no place in
// an app window. A touch or precision-touchpad drag past the edge of a page
// bounces the whole page and, sideways, navigates history -- so a swipe on
// the home screen went "back" to the splash page, a document with no title
// bar and nothing but a logo animation, and stayed there. Alt+Left, the
// browser-back key and the mouse's back button did the same. And a dragged
// image or link lifted a ghost copy off the page.
//
// Three layers, because each catches what the others cannot: the runtime
// setting for swipe navigation and the accelerator-key hook here; the
// Chromium feature flag in shellBrowserArgs, which reaches the gesture
// recogniser the setting does not on some runtimes; and the client, which
// turns off overscroll and consumes the mouse's back button (the shell never
// sees mouse buttons). The splash itself is the last line: reached by going
// back, it forwards to the app at once (see splashHTML).

// shellBrowserArgs are handed to the runtime as additional browser arguments
// alongside any certificate pin.
const shellBrowserArgs = "--disable-features=OverscrollHistoryNavigation,msOverscrollHistoryNavigation"

// BrowserArgs is everything the runtime is told: the pin, if any, and the
// shell flags.
func BrowserArgs(pin CertPin) string {
	if p := BrowserArgsFor(pin); p != "" {
		return p + " " + shellBrowserArgs
	}
	return shellBrowserArgs
}

var errNoBrowser = errors.New("the web view exposes no browser to configure")

// hardenShell applies the application-surface settings to a created view.
func hardenShell(w webview2.WebView, log *slog.Logger) error {
	chromium, err := browserOf(w)
	if err != nil {
		return err
	}
	settings, err := chromium.GetSettings()
	if err != nil {
		return err
	}
	if err := settings.PutIsSwipeNavigationEnabled(false); err != nil {
		// An old runtime without the setting; the feature flag and the client
		// still hold.
		log.Warn("could not disable swipe navigation", "err", err)
	}
	chromium.AcceleratorKeyCallback = blocksNavigationKey
	return nil
}

// browserOf reaches the library's embedded Chromium. The library does not
// export it; the field is read by name so an upgrade that renames it fails
// here, loudly, rather than silently leaving the habits on.
func browserOf(w webview2.WebView) (*edge.Chromium, error) {
	v := reflect.ValueOf(w)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil, errNoBrowser
	}
	f := v.Elem().FieldByName("browser")
	if !f.IsValid() || !f.CanAddr() {
		return nil, errNoBrowser
	}
	iface := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
	c, ok := iface.(*edge.Chromium)
	if !ok || c == nil {
		return nil, errNoBrowser
	}
	return c, nil
}

// Virtual keys. The names are Windows'.
const (
	vkMenu           = 0x12
	vkControl        = 0x11
	vkLeft           = 0x25
	vkRight          = 0x27
	vkF5             = 0x74
	vkR              = 0x52
	vkBrowserBack    = 0xA6
	vkBrowserForward = 0xA7
	vkBrowserRefresh = 0xA8
)

var procGetKeyState = syscall.NewLazyDLL("user32.dll").NewProc("GetKeyState")

func keyHeld(vk int) bool {
	r, _, _ := procGetKeyState.Call(uintptr(vk))
	return int16(r) < 0
}

// blocksNavigationKey is the accelerator hook: true for a key the browser
// would turn into history navigation or a reload, which the app swallows.
// Everything else -- Ctrl+C, Ctrl+K, F11, the zoom keys -- passes through.
func blocksNavigationKey(vk uint) bool {
	return navigationKey(int(vk), keyHeld(vkMenu), keyHeld(vkControl))
}

// navigationKey is the decision, separated so it can be tested without a
// keyboard.
func navigationKey(vk int, alt, ctrl bool) bool {
	switch vk {
	case vkBrowserBack, vkBrowserForward, vkBrowserRefresh, vkF5:
		return true
	case vkLeft, vkRight:
		return alt
	case vkR:
		return ctrl
	}
	return false
}
