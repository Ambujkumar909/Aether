//go:build windows

package main

import (
	"encoding/json"
	"log/slog"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
)

// Permissions, asked once, in the app's own voice.
//
// The first run after installing shows one screen -- the same ground, mark
// and type as the sign-in panel -- with the three things a communications
// client needs from the machine: the camera and the microphone for calls,
// notifications for what arrives while you are elsewhere. Each has a switch,
// on by default; Continue records the answers, applies them to the web view
// and goes on to the app. Later starts read the answers and never ask again;
// the tray menu has "Permissions..." to change them.
//
// It is a page rather than a system dialog because a system dialog is what
// an installer shows, in the system's font and colours, and this is the
// first thing a person sees of the product. It is inline and self-contained
// for the same reason the splash is: it renders before the network is
// touched.

// Permissions is what was decided.
type Permissions struct {
	Asked         bool `json:"asked"`
	Camera        bool `json:"camera"`
	Microphone    bool `json:"microphone"`
	Notifications bool `json:"notifications"`
}

// defaultPermissions is what the switches show before anyone has touched
// them: on. Somebody who installs a calling app expects it to call.
var defaultPermissions = Permissions{Camera: true, Microphone: true, Notifications: true}

// applyPermissions tells the web view the answers, so the page's own
// requests are settled without a prompt.
func applyPermissions(w webview2.WebView, p Permissions, log *slog.Logger) {
	chromium, err := browserOf(w)
	if err != nil {
		log.Warn("could not apply permissions to the web view", "err", err)
		return
	}
	state := func(on bool) edge.CoreWebView2PermissionState {
		if on {
			return edge.CoreWebView2PermissionStateAllow
		}
		return edge.CoreWebView2PermissionStateDeny
	}
	chromium.SetPermission(edge.CoreWebView2PermissionKindCamera, state(p.Camera))
	chromium.SetPermission(edge.CoreWebView2PermissionKindMicrophone, state(p.Microphone))
	chromium.SetPermission(edge.CoreWebView2PermissionKindNotifications, state(p.Notifications))
}

// permissionsHTML is the screen. current sets the switches; the page calls
// aetherPermissions(camera, microphone, notifications) when Continue is
// pressed, which the shell binds before the first navigation.
func permissionsHTML(current Permissions) string {
	cur, _ := json.Marshal(current)
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>Before you start</title>
<style>
  :root {
    color-scheme: dark;
    --page: #0e1a20; --card: #13222a; --card-2: #182a33; --line: rgba(255,255,255,.08);
    --ink: #eef4f2; --ink-2: #b7c4c0; --ink-3: #7e8f8a;
    --brand: #3fbf94; --brand-ink: #062016; --brand-soft: rgba(63,191,148,.14);
    --r: 12px;
  }
  * { box-sizing: border-box; }
  html, body {
    height: 100%; margin: 0; background: var(--page); color: var(--ink);
    font-family: 'Segoe UI Variable Text', 'Segoe UI', system-ui, -apple-system, sans-serif;
    font-size: 14px; line-height: 1.5;
    -webkit-user-select: none; user-select: none; cursor: default; overflow: hidden;
  }
  .stage { min-height: 100%; display: grid; place-items: center; padding: 32px 16px; position: relative; }
  .stage::before {
    content: ""; position: absolute; inset: 0; pointer-events: none;
    background: radial-gradient(60% 50% at 50% 30%, rgba(60,170,130,.14), transparent 70%);
  }
  .card {
    position: relative; width: min(520px, 100%);
    background: var(--card); border: 1px solid var(--line); border-radius: 16px;
    padding: 36px 36px 28px; box-shadow: 0 30px 80px rgba(0,0,0,.45);
    animation: rise 560ms cubic-bezier(.2,0,0,1) both;
  }
  .mark { width: 56px; height: 56px; display: block; margin-bottom: 22px; filter: drop-shadow(0 10px 24px rgba(0,0,0,.45)); }
  .eyebrow { font-size: 11px; font-weight: 700; letter-spacing: .12em; text-transform: uppercase; color: var(--brand); margin: 0 0 8px; }
  h1 { font-size: 24px; font-weight: 700; letter-spacing: -.02em; margin: 0 0 8px; text-wrap: balance; }
  .lede { color: var(--ink-2); margin: 0 0 26px; max-width: 46ch; }
  .rows { display: grid; gap: 8px; margin: 0 0 22px; }
  .row {
    display: grid; grid-template-columns: 40px 1fr auto; gap: 14px; align-items: center;
    padding: 14px 16px; border-radius: var(--r); background: var(--card-2); border: 1px solid var(--line);
    cursor: pointer; transition: border-color 120ms ease, background 120ms ease;
  }
  .row:hover { border-color: rgba(63,191,148,.35); }
  .row:has(input:focus-visible) { outline: 2px solid var(--brand); outline-offset: 2px; }
  .ico { width: 40px; height: 40px; border-radius: 10px; display: grid; place-items: center; background: var(--brand-soft); color: var(--brand); }
  .ico svg { width: 20px; height: 20px; }
  .row b { display: block; font-weight: 600; font-size: 14.5px; }
  .row small { display: block; color: var(--ink-3); font-size: 12.5px; line-height: 1.45; }
  .switch { position: relative; width: 44px; height: 26px; flex: 0 0 auto; }
  .switch input { position: absolute; inset: 0; opacity: 0; margin: 0; cursor: pointer; }
  .switch span {
    position: absolute; inset: 0; border-radius: 13px; background: #2a3b43; transition: background 140ms ease;
  }
  .switch span::after {
    content: ""; position: absolute; top: 3px; left: 3px; width: 20px; height: 20px; border-radius: 50%;
    background: #fff; box-shadow: 0 1px 3px rgba(0,0,0,.4); transition: transform 140ms cubic-bezier(.2,0,0,1);
  }
  .switch input:checked + span { background: var(--brand); }
  .switch input:checked + span::after { transform: translateX(18px); }
  .actions { display: flex; align-items: center; gap: 14px; }
  .btn {
    appearance: none; border: 0; cursor: pointer; font: inherit; font-weight: 600;
    padding: 11px 22px; border-radius: 10px; background: var(--brand); color: var(--brand-ink);
    transition: transform 80ms ease, filter 120ms ease;
  }
  .btn:hover { filter: brightness(1.06); }
  .btn:active { transform: translateY(1px); }
  .btn:focus-visible { outline: 2px solid #fff; outline-offset: 2px; }
  .note { color: var(--ink-3); font-size: 12.5px; }
  .bar { position: absolute; bottom: 0; left: 0; height: 2px; width: 100%; background: rgba(60,170,130,.14); }
  @keyframes rise { from { opacity: 0; transform: translateY(10px) scale(.99); } to { opacity: 1; transform: none; } }
  @media (prefers-reduced-motion: reduce) { .card { animation: none; } }
  @media (max-width: 480px) { .card { padding: 28px 20px 22px; } .row { grid-template-columns: 36px 1fr auto; } }
</style></head>
<body>
  <div class="stage">
    <form class="card" id="f">
      <img class="mark" src="` + markDataURI + `" alt="" />
      <p class="eyebrow">Before you start</p>
      <h1>What ` + appName + ` may use on this computer</h1>
      <p class="lede">Nothing here is used until you are in a call or somebody writes to you. You can change any of it later from the ` + appName + ` icon in the notification area.</p>
      <div class="rows">
        <label class="row">
          <span class="ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7.5A1.5 1.5 0 014.5 6h9A1.5 1.5 0 0115 7.5v9A1.5 1.5 0 0113.5 18h-9A1.5 1.5 0 013 16.5z M15 10.5l6-3.5v10l-6-3.5z"/></svg></span>
          <span><b>Camera</b><small>For video calls and meetings. Off between calls.</small></span>
          <span class="switch"><input type="checkbox" id="camera" /><span></span></span>
        </label>
        <label class="row">
          <span class="ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M12 15a3 3 0 003-3V6a3 3 0 00-6 0v6a3 3 0 003 3z M19 11a7 7 0 01-14 0 M12 18v3"/></svg></span>
          <span><b>Microphone</b><small>For calls. Muted until you unmute.</small></span>
          <span class="switch"><input type="checkbox" id="microphone" /><span></span></span>
        </label>
        <label class="row">
          <span class="ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M6 8a6 6 0 0112 0v4.3l1.7 2.7H4.3L6 12.3z M10 19a2 2 0 004 0"/></svg></span>
          <span><b>Notifications</b><small>Incoming calls and new messages while the window is closed or behind other work.</small></span>
          <span class="switch"><input type="checkbox" id="notifications" /><span></span></span>
        </label>
      </div>
      <div class="actions">
        <button class="btn" type="submit" id="go">Continue</button>
        <span class="note">Asked once. Change it any time.</span>
      </div>
    </form>
    <div class="bar"></div>
  </div>
  <script>
  (function () {
    var cur = ` + string(cur) + `;
    ['camera', 'microphone', 'notifications'].forEach(function (k) { document.getElementById(k).checked = !!cur[k]; });
    document.getElementById('f').addEventListener('submit', function (e) {
      e.preventDefault();
      var go = document.getElementById('go');
      go.disabled = true; go.textContent = 'Starting…';
      var v = function (k) { return document.getElementById(k).checked; };
      if (typeof window.aetherPermissions === 'function') {
        window.aetherPermissions(v('camera'), v('microphone'), v('notifications'));
      }
    });
  })();
  </script>
</body></html>`
}
