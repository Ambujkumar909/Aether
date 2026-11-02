package main

import "encoding/json"

// The first frame.
//
// WebView2 shows nothing until a page commits. On a cold start that is the
// runtime initialising, then a TLS handshake, then the client's modules
// arriving -- and for all of it the window is an empty rectangle. An empty
// rectangle is what a web view looks like; it is not what an application looks
// like, and it is the whole reason the app "feels like a browser opening a
// page" rather than like a program starting.
//
// So the window is painted before anything is fetched. The mark is the one
// from the sign-in panel, on the same ground, so the splash and the first real
// screen are continuous rather than two different products handing over.
//
// Inline and self-contained on purpose: this has to render before the network
// has been touched, so it can reference nothing.

// splashHTML is built rather than declared, so the embedded artwork can be
// interpolated into it.
//
// It knows the server's address for one reason: it is a history entry, and
// if anything ever brings the view back to it -- a gesture the shell failed
// to swallow, a runtime that ignores the setting -- it must not sit there as
// a logo with no title bar. Reached by going back, it forwards to the app
// at once, replacing itself so the entry is gone for good.
func splashHTML(server string) string {
	target, _ := json.Marshal(server)
	return `<!doctype html>
<html><head><meta charset="utf-8">
<style>
  :root { color-scheme: dark; }
  html, body {
    height: 100%; margin: 0;
    background: #0e1a20;
    overflow: hidden;
    font-family: -apple-system, 'Segoe UI Variable Text', 'Segoe UI', system-ui, sans-serif;
    -webkit-user-select: none; user-select: none;
    cursor: default;
  }
  .stage { height: 100%; display: grid; place-items: center; position: relative; }

  /* A single wide, very low pool of brand light behind the mark. Not a
     gradient wash: one soft source, so the glass has something to sit in. */
  .stage::before {
    content: ""; position: absolute; inset: 0;
    background: radial-gradient(60% 50% at 50% 44%,
      rgba(60, 170, 130, .16), transparent 70%);
  }

  .lockup {
    position: relative;
    display: grid; justify-items: center;
    animation: rise 720ms cubic-bezier(.2,0,0,1) both;
  }
  .mark {
    width: 168px; height: auto;
    filter: drop-shadow(0 20px 40px rgba(0,0,0,.5));
    animation: breathe 3.6s ease-in-out 720ms infinite;
  }
  .word { width: 150px; height: auto; margin-top: 26px; opacity: .96; }
  .tag  { width: 132px; height: auto; margin-top: 11px; opacity: .5; }

  /* A hairline that fills, not a spinner. A spinner says "this may take a
     while"; this says "it is already happening". */
  .bar { position: absolute; bottom: 0; left: 0; height: 2px; width: 100%;
         background: rgba(60,170,130,.14); }
  .bar::after {
    content: ""; display: block; height: 100%; width: 38%;
    background: linear-gradient(90deg, transparent, #3fbf94, transparent);
    animation: sweep 1250ms cubic-bezier(.4,0,.2,1) infinite;
  }

  @keyframes rise {
    from { opacity: 0; transform: translateY(10px) scale(.984); }
    to   { opacity: 1; transform: none; }
  }
  @keyframes breathe {
    0%, 100% { transform: none; }
    50%      { transform: translateY(-4px); }
  }
  @keyframes sweep {
    0%   { transform: translateX(-110%); }
    100% { transform: translateX(370%); }
  }
  @media (prefers-reduced-motion: reduce) {
    .lockup, .mark { animation: none; }
    .bar::after { animation: none; width: 100%; opacity: .45; }
  }
</style></head>
<body>
  <div class="stage">
    <div class="lockup">
      <img class="mark" src="` + markDataURI + `" alt="" />
      <img class="word" src="` + wordmarkDataURI + `" alt="Aether" />
      <img class="tag"  src="` + taglineDataURI + `" alt="" />
    </div>
    <div class="bar"></div>
  </div>
  <script>
  (function () {
    var app = ` + string(target) + `;
    if (!app) return;
    var nav = performance.getEntriesByType ? performance.getEntriesByType('navigation')[0] : null;
    if (nav && nav.type === 'back_forward') location.replace(app);
    addEventListener('pageshow', function (e) { if (e.persisted) location.replace(app); });
  })();
  </script>
</body></html>`
}

// SplashHoldMS is how long the splash stays before the real client is
// fetched.
//
// Short, and not a fake delay: the window is already painted, and this only
// gives the compositor a frame or two to show it before a navigation begins.
// Long enough to be seen, short enough that nobody is waiting on it.
const SplashHoldMS = 620
