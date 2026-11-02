package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The first screen anybody sees is "which server?", and people paste all sorts
// of things into it. Refusing four spellings of the same address because the
// fifth is canonical is how software feels hostile before it has done
// anything.

func TestEverySpellingOfTheSameServerIsAccepted(t *testing.T) {
	want := "https://aether.example.com"
	for _, in := range []string{
		"aether.example.com",
		"https://aether.example.com",
		"https://aether.example.com/",
		"https://aether.example.com/?room=weekly",
		"https://aether.example.com/#/meeting",
		"  https://aether.example.com  ",
		"wss://aether.example.com",
	} {
		got, err := NormaliseServer(in)
		if err != nil {
			t.Errorf("%q was refused: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

func TestAPortAndAnAddressSurvive(t *testing.T) {
	cases := map[string]string{
		"172.22.187.37:8443":         "https://172.22.187.37:8443",
		"https://192.168.1.20:8443/": "https://192.168.1.20:8443",
		"localhost:8890":             "https://localhost:8890",
		// Plain HTTP is kept when it is asked for, because that is what a
		// loopback development server serves. It is never assumed.
		"http://localhost:8890": "http://localhost:8890",
	}
	for in, want := range cases {
		got, err := NormaliseServer(in)
		if err != nil {
			t.Errorf("%q was refused: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

// HTTPS is assumed rather than demanded, because the browser refuses camera
// and microphone access anywhere but HTTPS and loopback. Typing a bare host
// and silently getting a window with no camera is the worst of both.
func TestABareHostBecomesHTTPS(t *testing.T) {
	got, err := NormaliseServer("meet.acme.co.uk")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://meet.acme.co.uk" {
		t.Fatalf("got %q", got)
	}
}

func TestNonsenseIsRefusedWithAReason(t *testing.T) {
	for _, in := range []string{"", "   ", "ftp://files.example.com", "https://"} {
		if got, err := NormaliseServer(in); err == nil {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
}

func TestTheSocketAddressMatchesTheServer(t *testing.T) {
	cases := map[string]string{
		"https://aether.example.com": "wss://aether.example.com",
		"http://localhost:8890":      "ws://localhost:8890",
	}
	for in, want := range cases {
		got, err := WebSocketBase(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

// Settings survive a restart, and a half-written file does not stop the app
// from starting -- which is why the write is atomic.

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Config{Server: "https://aether.example.com", Device: "tok", StartAtLogin: true}
	if err := saveConfig(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("read back %+v, want %+v", out, in)
	}
}

func TestNoSettingsYetIsNotAnError(t *testing.T) {
	got, err := loadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("a first run reported an error: %v", err)
	}
	if got.Server != "" {
		t.Fatalf("a first run invented a server: %q", got.Server)
	}
}

func TestUnreadableSettingsAreReportedNotGuessed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(dir); err == nil {
		t.Fatal("corrupt settings were accepted silently")
	}
}

// The write leaves no partial file behind for the next start to trip over.
func TestSavingLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := saveConfig(dir, Config{Server: "https://x.example"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("a temporary file survived: %s", e.Name())
		}
	}
}

func TestTheHostIsExtractedForCertificateTrust(t *testing.T) {
	if got := hostOf("https://172.22.187.37:8443"); got != "172.22.187.37:8443" {
		t.Fatalf("got %q", got)
	}
	if got := hostOf("not a url at all"); got != "" {
		t.Fatalf("nonsense produced a host: %q", got)
	}
}

// Certificate pinning: the app must trust the one server it was pointed at,
// and must not become a browser with its checking switched off.

func TestAPinNamesOneKeyAndNothingElse(t *testing.T) {
	// Two certificates, so the pins can be compared rather than merely
	// computed.
	a := selfSigned(t, "aether.example")
	b := selfSigned(t, "aether.example")

	pinA, pinB := SPKIPin(a), SPKIPin(b)
	if pinA == "" || pinB == "" {
		t.Fatal("a certificate produced no pin")
	}
	if pinA == pinB {
		t.Fatal("two different keys produced the same pin")
	}
	// Stable: the same certificate always pins the same.
	if SPKIPin(a) != pinA {
		t.Fatal("the pin is not stable for one certificate")
	}
	// Base64 of a SHA-256 is 44 characters with its padding.
	if len(pinA) != 44 {
		t.Fatalf("pin is %d characters: %q", len(pinA), pinA)
	}
}

// The switch is passed only when something needs pinning. A deployment with a
// real certificate must get ordinary, unmodified verification.
func TestNothingIsPassedWhenNothingNeedsPinning(t *testing.T) {
	if got := BrowserArgsFor(""); got != "" {
		t.Fatalf("an empty pin still produced %q", got)
	}
	// The shell flags always ride along, pin or no pin.
	if got := BrowserArgs(""); got != shellBrowserArgs {
		t.Fatalf("no pin: %q", got)
	}
	if got := BrowserArgs("abc123"); got != BrowserArgsFor("abc123")+" "+shellBrowserArgs {
		t.Fatalf("with pin: %q", got)
	}
	got := BrowserArgsFor("abc123")
	if got != "--ignore-certificate-errors-spki-list=abc123" {
		t.Fatalf("got %q", got)
	}
	// The blunt switch must never appear: it disables checking for every host
	// the window ever reaches.
	if strings.Contains(got, "--ignore-certificate-errors=") ||
		got == "--ignore-certificate-errors" {
		t.Fatal("certificate checking was disabled wholesale")
	}
}

func TestPlainHTTPNeedsNoPin(t *testing.T) {
	_, err := PinFor(context.Background(), "http://localhost:8890")
	if !errors.Is(err, ErrNoPinNeeded) {
		t.Fatalf("got %v, want ErrNoPinNeeded", err)
	}
}

func selfSigned(t *testing.T, host string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// The splash is a history entry. Reached by going back it must leave at once,
// to the server it was started for, and it must not carry a script-breaking
// address.
func TestSplashForwardsItselfToTheApp(t *testing.T) {
	h := splashHTML("https://meet.example.com:8443/")
	for _, want := range []string{
		`var app = "https://meet.example.com:8443/";`,
		`nav.type === 'back_forward'`,
		`location.replace(app)`,
		`addEventListener('pageshow'`,
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("splash lacks %q", want)
		}
	}
	if strings.Contains(splashHTML(`x"</script><script>alert(1)`), `</script><script>alert`) {
		t.Fatal("the server address is not escaped into the script")
	}
	if !strings.Contains(splashHTML(""), `var app = "";`) {
		t.Fatal("no server: the splash still renders")
	}
}

// Which keys the shell swallows: the ones a browser turns into history or a
// reload, and no others.
func TestNavigationKeysAreSwallowedAndNothingElse(t *testing.T) {
	cases := []struct {
		vk        int
		alt, ctrl bool
		want      bool
	}{
		{vkBrowserBack, false, false, true},
		{vkBrowserForward, false, false, true},
		{vkBrowserRefresh, false, false, true},
		{vkF5, false, false, true},
		{vkLeft, true, false, true},
		{vkRight, true, false, true},
		{vkLeft, false, false, false}, // plain arrows move the caret
		{vkR, true, false, false},     // Alt+R is nothing
		{vkR, false, true, true},      // Ctrl+R reloads
		{0x43, false, true, false},    // Ctrl+C copies
		{0x4B, false, true, false},    // Ctrl+K is the app's search
		{0x7A, false, false, false},   // F11
	}
	for _, c := range cases {
		if got := navigationKey(c.vk, c.alt, c.ctrl); got != c.want {
			t.Errorf("vk=%#x alt=%v ctrl=%v: got %v want %v", c.vk, c.alt, c.ctrl, got, c.want)
		}
	}
}

// Maximised geometry is the work area, expressed relative to the monitor:
// a taskbar at the bottom shortens the height; one on the left moves the
// origin. This is the number that keeps the taskbar visible.
func TestMaximiseStopsAtTheTaskbar(t *testing.T) {
	var mmi minMaxInfo
	// An 8px frame hangs off screen on every side, so the client area is
	// the work area exactly and no edge or corner of frame stays visible.
	applyWorkArea(&mmi, monitorInfo{
		RcMonitor: rect{Left: 0, Top: 0, Right: 2560, Bottom: 1440},
		RcWork:    rect{Left: 0, Top: 0, Right: 2560, Bottom: 1392}, // 48px taskbar at the bottom
	}, 8)
	if mmi.MaxSize != (point{2576, 1408}) || mmi.MaxPosition != (point{-8, -8}) {
		t.Fatalf("bottom taskbar: size %+v pos %+v", mmi.MaxSize, mmi.MaxPosition)
	}
	// A second monitor to the right with the taskbar on its left edge.
	applyWorkArea(&mmi, monitorInfo{
		RcMonitor: rect{Left: 2560, Top: 0, Right: 4480, Bottom: 1080},
		RcWork:    rect{Left: 2620, Top: 0, Right: 4480, Bottom: 1080},
	}, 8)
	if mmi.MaxSize != (point{1876, 1096}) || mmi.MaxPosition != (point{52, -8}) {
		t.Fatalf("left taskbar on a second monitor: size %+v pos %+v", mmi.MaxSize, mmi.MaxPosition)
	}
	// No frame (a runtime that reports none): the plain work area.
	applyWorkArea(&mmi, monitorInfo{RcMonitor: rect{0, 0, 100, 100}, RcWork: rect{0, 0, 100, 90}}, 0)
	if mmi.MaxSize != (point{100, 90}) || mmi.MaxPosition != (point{0, 0}) {
		t.Fatalf("zero border: size %+v pos %+v", mmi.MaxSize, mmi.MaxPosition)
	}
}

// Permissions are remembered: a config that has them is not asked again,
// and the round trip keeps every answer.
func TestPermissionsSurviveARestartAndAreAskedOnce(t *testing.T) {
	dir := t.TempDir()
	c := Config{Server: "https://x:8443/", Permissions: &Permissions{Asked: true, Camera: true, Microphone: true, Notifications: false}}
	if err := saveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	back, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Permissions == nil || *back.Permissions != *c.Permissions {
		t.Fatalf("round trip: %+v", back.Permissions)
	}
	// A fresh install has none, which is what triggers the questions.
	fresh, _ := loadConfig(t.TempDir())
	if fresh.Permissions != nil {
		t.Fatal("a fresh config claims to have been asked")
	}
}

// The permissions screen: the three switches, set from what was decided,
// and the shell call that records the answer. The values are embedded as
// JSON so a stray quote cannot break out of the script.
func TestPermissionsScreenShowsTheDecisionsAndReportsBack(t *testing.T) {
	h := permissionsHTML(Permissions{Asked: true, Camera: true, Microphone: false, Notifications: true})
	for _, want := range []string{
		`id="camera"`, `id="microphone"`, `id="notifications"`,
		`var cur = {"asked":true,"camera":true,"microphone":false,"notifications":true};`,
		`window.aetherPermissions(v('camera'), v('microphone'), v('notifications'))`,
		"Before you start",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("permissions screen lacks %q", want)
		}
	}
	if !defaultPermissions.Camera || !defaultPermissions.Microphone || !defaultPermissions.Notifications {
		t.Fatal("a fresh install should offer everything on")
	}
}

// The badge is drawn, not shipped: the overlay is a red disc with the count,
// the tray copy keeps the mark and puts the disc in its corner. Checked pixel
// by pixel.
func TestBadgePixels(t *testing.T) {
	const s = badgeSize
	at := func(px []byte, x, y int) [4]byte {
		i := (y*s + x) * 4
		return [4]byte{px[i], px[i+1], px[i+2], px[i+3]}
	}
	red := [4]byte{0x3c, 0x3c, 0xe0, 0xff}
	white := [4]byte{0xff, 0xff, 0xff, 0xff}
	clear := [4]byte{}

	overlay := paintBadgePixels(nil, s, 3)
	if at(overlay, 0, 0) != clear {
		t.Fatalf("overlay corner should be transparent: %v", at(overlay, 0, 0))
	}
	if at(overlay, 2, s/2) != red {
		t.Fatalf("overlay edge should be red: %v", at(overlay, 2, s/2))
	}
	// The digit 3: its top bar spans the three columns at the top row of the
	// glyph, which sits centred (glyph 6x10 in 16x16 -> origin 5,3).
	if at(overlay, 5, 3) != white || at(overlay, 10, 3) != white {
		t.Fatalf("digit top bar missing: %v %v", at(overlay, 5, 3), at(overlay, 10, 3))
	}
	// And its second row is hollow on the left (a 3 has no left stroke there).
	if at(overlay, 5, 3+1*2) == white {
		t.Fatal("a 3 should not have a left stroke in its second row")
	}
	// A count past nine draws the disc without a digit.
	many := paintBadgePixels(nil, s, 12)
	if at(many, s/2, s/2) != red {
		t.Fatalf("9+ badge centre should be plain red: %v", at(many, s/2, s/2))
	}

	// Tray: a base icon (all opaque green) keeps its top-left and gets the
	// disc bottom-right.
	base := make([]byte, s*s*4)
	for i := 0; i < len(base); i += 4 {
		base[i], base[i+1], base[i+2], base[i+3] = 0x40, 0xc0, 0x40, 0xff
	}
	tray := paintBadgePixels(base, s, 2)
	if at(tray, 1, 1) != [4]byte{0x40, 0xc0, 0x40, 0xff} {
		t.Fatalf("tray icon lost its own pixels: %v", at(tray, 1, 1))
	}
	if at(tray, s-3, s-3) != red {
		t.Fatalf("tray disc missing bottom-right: %v", at(tray, s-3, s-3))
	}
	if len(paintBadgePixels(nil, s, 1)) != s*s*4 {
		t.Fatal("buffer size")
	}
}

func TestBadgeText(t *testing.T) {
	for n, want := range map[int]string{0: "", -1: "", 1: "1", 9: "9", 10: "9+", 250: "9+"} {
		if got := badgeText(n); got != want {
			t.Errorf("badgeText(%d) = %q, want %q", n, got, want)
		}
	}
}
