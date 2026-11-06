# Aether — build rules

Standing rules for every change in this repository. These are not aspirations;
a change that does not satisfy them is not finished.

## 1. Measure every feature at the bit level, when it is built

Every feature that puts bytes on a wire gets a real measurement before it is
called done. Not an estimate, not a calculation from the spec — a capture.

- Capture with **tshark** on the real connection, not a simulation.
- Account for **every layer**: Ethernet/loopback framing, IP (note that
  loopback here is IPv6, so headers are 40 bytes, not 20), TCP or UDP, TLS
  records, WebSocket framing, RTP/SRTP headers and auth tags, and only then
  the payload.
- Report **payload efficiency** — useful bytes over total bytes on the wire —
  alongside the raw totals. A protocol that is dense per frame but rides one
  frame per packet is not efficient, and that distinction has already been
  measured here: 27.1% payload efficiency at one frame per message against
  48.0% when coalesced. Quote both, never the flattering one alone.
- Compare against the honest baseline (JSON over the same transport, or the
  competitor behaviour being replaced), and state what the comparison assumes.

## 2. No wasted packets

- Coalesce. A dense encoding does not produce a dense connection on its own;
  per-packet overhead dominates small payloads.
- Send nothing when nothing changed. Idle must cost zero, not "a little".
- Never pay for a header twice. If a value is already implied by the frame,
  the connection, or an earlier field, it does not go on the wire again —
  unless correctness requires it, in which case say so explicitly at the
  definition. `internal/rc` is the deliberate exception: input events carry
  absolute coordinates because a misplaced click is irreversible, and that
  trade is documented where it is made.
- Bound every queue. An unbounded buffer is a memory leak with extra steps.

## 3. Quality is never traded for bits

There is no bitrate ceiling. Optimise the average, not the peak: a static
screen should cost almost nothing and a burst of real motion should be allowed
to spend what it needs. Under pressure, drop whole frames rather than degrade
quality — that is only safe because frames are self-contained, which is itself
a rule (see 5).

## 4. Test rigorously, and prove the test can fail

- Run the full suite under `-race` before calling anything done.
- A concurrency or teardown test that has never failed against the broken code
  is not evidence. Reproduce the bug first, then fix it. This has already
  mattered twice here: a dictionary-desync test passed against genuinely
  broken code because every string it sent was unique, so no frame ever
  referenced an index.
- Every close path is tested: double close, concurrent close, close racing a
  write, close of something never started, send after close, and goroutine
  count returning to a settled baseline.
- Use real sockets, not fakes, when testing socket behaviour. A fake cannot
  get close wrong.

## 5. Never introduce cross-frame state unless recovery from its loss is cheap

Learned expensively in a sibling project, where a delta codec 24× cheaper per
frame on paper spent 13–21 MB repainting one screen after a single drop.
Recovery being the most expensive operation means loss makes congestion worse
exactly when it is already bad.

## 6. Record the measurement

Findings go in `docs/ARCHITECTURE.md` with the numbers that produced them,
including the ones that were disappointing. An architecture document that
implies completeness is a lie.

## 7. Speed, accuracy, security — none negotiable

Assume 50,000 people on one deployment. Every feature is judged against that.

- **Speed.** Every search and every list the interface draws is backed by an
  index, never a scan; a keystroke's query must be microseconds-to-single-
  milliseconds at that scale, measured in a scale test that seeds the real row
  counts (see `scale_test.go` in messaging and identity). No N+1 — a list is a
  bounded number of queries however long it is. Nothing the client draws per
  keystroke may wait on the network to be usable: local results first, the
  server's answer merged when it lands, stale answers dropped.
- **Accuracy.** What is shown is what is stored. Optimistic updates are
  confirmed by the push and reconciled, never left to drift; ordering has a
  total tiebreak; counts come from the source of truth. A push must carry
  everything the view draws (the "push tail" rule) or it is a regression.
- **Security.** Every endpoint checks membership/ownership server-side before
  it does anything — before accepting an upload's bytes, not after. Every
  new endpoint gets a stranger, an anonymous and a CSRF case in the end-to-end
  harness (`tools/e2e/messaging.e2e.mjs`). Bytes a browser could execute are
  never served inline from this origin. Inputs are bounded (size, count,
  length) and the bound is tested one over the line.

A change that makes something faster but less accurate, or more convenient
but less secure, is not finished.

## 8. Every screen adapts to every device — strictly

The client is one codebase for a 360px phone, a tablet, a laptop, a 4K
monitor and the Windows shell. Nothing ships that only works at the width it
was designed on.

- **Mobile-first.** Layouts collapse to one column below ~720px: the chat
  list and the thread are two screens with a back control, dialogs become
  full-height sheets, the rail becomes a bottom bar or a drawer. No
  horizontal scroll of the page, ever; only tables, code and diagrams scroll
  sideways, inside their own container.
- **Touch.** Every control is at least 44x44 CSS px on touch devices; hover-
  only affordances (the message action bar, menus that open on hover) have a
  tap equivalent (long-press or a visible button).
- **Fluid, not fixed.** Relative units, `clamp()` for type, `min()`/`max()`
  for widths, `100dvh` not `100vh`, safe-area insets on notched phones,
  `overscroll-behavior: none` where the page is an app surface.
- **Verified at three widths before it is done**: 390x844 (phone), 820x1180
  (tablet) and 1280+ (desktop), with the layout harness
  (`tools/e2e/layout.e2e.mjs`), which drives the installed Chrome headless,
  signs in, walks the main screens and fails on any element wider than the
  viewport, any page-level horizontal overflow, or any tap target under
  44px. Screenshots from the run are looked at, not only the numbers.
- **The Windows shell is a device too**: maximised is the work area exactly
  (frame off screen, taskbar visible), the window has a floor size the
  layout can hold, and DPI scaling is honoured everywhere.

"Works on desktop" is not done. A screen that is unusable on a phone is a
bug of the same severity as one that is unusable on a laptop.

## Knowledge graph — route before you read

`graphify-out/graph.json` maps this repo: every function, type, file and doc
heading, with call/import edges and named communities (AST-extracted by
graphify, no LLM). It is kept current automatically: the SessionStart hook
syncs it and injects the last checkpoint; the Stop hook re-indexes changed
files in the background (`tools/kg/kg_sync.py`, run in PowerShell with
graphify's interpreter, `graphify-out/.graphify_python`).

Before opening files to find where something lives, route (≈200 ms, ≈450 tokens):

    & (gc graphify-out\.graphify_python) tools\kg\kg_sync.py route "<words or symbol>"

It prints the files to read first with the symbols and line numbers that
matched, then the files connected by calls/imports. Read only those lines.
Deeper questions: `graphify explain "<symbol>"`, `graphify query "<question>"`
(BFS, ~2k tokens), `graphify affected <node-id>` (what depends on it).

Limits, measured: Go calls through a receiver (`s.pushMessage(...)`) are
often not resolved, so `affected` under-reports for Go methods — grep
callers before changing a shared method. Docs carry heading nodes only.
The graph is a map; read the real lines before editing.
