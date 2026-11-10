# Resume here — Teams parity work

## Start here (handoff, 2026-11-09 evening)

Production plan, Phase 1 (deploy to an Ubuntu server in Docker, for the
user's org and the public) is **complete** as of 2026-11-09: two-step
sign-in `0b95751`, SSO `148fbe1`, sign-up guards `8dd0ab9`, capacity
`057d739`, and the Windows installer/updates `ad9fe53` (CI green).
Gates run on the last commit: Windows `-race` suite, Linux test stage
(`docker build --target test`), image build (`aether:dev`, version
`2026.1109.1206` stamped from the build time, `/app/desktop/latest.json`
present), the opt-in live update test, and the real-supervisor swap
(ARCHITECTURE 6by, last paragraphs). **Phase 1 leaves one decision only
the owner can make: the LICENSE file. Ask; never pick one.** Then Phase 2
(below, item 4) and the security phase. Earlier Phase 1 history: `ea217c2`
(part 1), CI fixes `636de82` `c49a908` `a11109f` `608b7a0`, `48cedfa`
(email-verified sign-up). Check the latest CI run on main first (a newer
push cancels the older run):
`curl -s "https://api.github.com/repos/Ambujkumar909/Aether/actions/runs?per_page=1" | grep -E '"(head_sha|status|conclusion)"'`;
failing steps report as annotations at `.../check-runs/<id>/annotations`,
because job logs need a login).

**Next, in order:**
1. **Two-step sign-in (TOTP).** DONE 2026-11-09 (ARCHITECTURE 6by; the
   notes below are kept for the record). Was: done and tested, not wired:
   `identity/totp.go` (RFC 6238 vectors, replay-proof, +-1 step),
   `internal/platform/seal` (AES-256-GCM at rest bound to the account id,
   HMAC "pepper" for recovery codes, key file created atomically). Written
   but NOT yet tested: `internal/platform/qr/qr.go` (QR encoder, byte mode,
   level M, versions 1-10, SVG; compiles). Test it against a real decoder:
   render PNGs from Go and decode them with zbar in a throwaway container
   (`docker run --rm -v <dir>:/w alpine sh -c "apk add --no-cache zbar >/dev/null && zbarimg -q --raw /w/x.png"`),
   for texts that land in each version 1-10 -- no decoder exists on the
   machine (no cv2/pyzbar; Chrome has no BarcodeDetector on Windows). Then:
   migration (secret sealed, last_step, recovery codes as seal.MAC), a login
   second step (ticket, 5 tries, counts toward lockout), setup/enable/disable
   endpoints (password re-check, revoke other sessions on enable), admin
   reset for a lost phone, audit events, the console Account card and an
   auth-card "code" view; e2e with a TOTP computed in the harness.
2. SSO/OIDC: DONE 2026-11-09 (ARCHITECTURE 6by). Sign-up guards (ceiling, block list, mail check, counters): DONE 2026-11-09. Capacity test against the container: DONE 2026-11-09 (50 over WebRTC holds; 100 does not -- the §6bu dispatch wall; see 6by). Windows installer and updates: DONE 2026-11-09 (ARCHITECTURE 6by, "The Windows app installs and updates itself"; deploy/README "Windows app"). **Next: the project's LICENSE (ask the owner -- never pick one)**, then Phase 2. Optional, not done: a "Get the Windows app" link on the sign-in screen (the server would advertise `desktop: true` in `/api/identity/config` when `-desktop-dir` is set). Phase 4's first item is now concrete: a bounded send queue per receiver, and the load client on another machine. (Was: the remaining abuse controls for public sign-up (a global sign-up
   rate, disposable-domain policy); capacity test; Windows installer and
   auto-update (exe resources done: `tools/winres.sh`, needs MinGW windres);
   the project's own LICENSE (the owner's choice -- ask).
3. **Open findings (ARCHITECTURE 6by):** the video harness fails about half
   its runs (a sender's bandwidth estimate that starts starved; predates
   today, A/B measured -- next: compare the SFU's transport-cc feedback in a
   passing and a failing run); Chrome rarely wedges a newcomer's subscriber
   under DevTools emulation (joinrace retries that exact signature once).
4. Phase 2 is under way: the calendar (week view, recurring meetings, who is
   free) is DONE 2026-11-10 (ARCHITECTURE 6bz; `tools/e2e/calendar.e2e.mjs`,
   34 checks; layout harness covers the week grid and the schedule dialog).
   **Next in Phase 2: web push and a service worker**, then Aether's own
   sound set, meeting recap, keyboard shortcuts, out of office and
   notification settings, server-side speaker selection, data export and
   deletion. Then the security phase (threat model, pentest), Phase 4.

**Running things (changed today):** both runners start Mailpit (a real
SMTP server, `tools/e2e/mailpit.sh`) and turn email on; the native runner
also creates an admin (recording needs it). With mail on, every harness
signs up through the emailed link (`signUpVerified`/`mailedToken` in
`meet.lib.mjs`). Signup is still 3/hour per process: run `console` first so
A/B exist (`video` and `call` only read saved accounts).

    & tools/e2e/.stress/browser-run.ps1 -Fresh -Names console,signup,reset,messaging -Out tools/e2e/.stress/b.txt
    & tools/e2e/.stress/docker-run.ps1 -Fresh -PublicIP <host ip> -Names console,reset -Out tools/e2e/.stress/d.txt
    docker build --target test .        # Linux vet + race suite
    bash tools/e2e/acme.e2e.sh          # Pebble; also runs in WSL Ubuntu (wsl -e bash -c ...)

**Gotchas found today:** Python `open(p,'w')` on Windows writes CRLF -- a CRLF
shell script cannot be parsed by bash on Linux (exit 2); `.gitattributes`
pins LF for *.sh, *.yml and the Dockerfile, but edit with binary-mode Python
or the Edit tool. In Bash-tool heredocs, a backslash-n inside Python strings often
arrives as a real newline and breaks Go/JS string literals: check with
`gofmt`/`node --check` after every scripted edit. Installing outside tools
(`go install`) is blocked by the permission classifier; use what is installed.


Paused 2 Nov 2026; batch 2 finished the same day. This file is the hand-off: what is done, what is half
done (with the exact state of every file), what comes next, and how to run
everything. Read it with `CLAUDE.md` (the build rules) and
`docs/TEAMS_PARITY.md` (the feature matrix).

## The goal

Build every Teams feature into Aether, in an order of Claude's choosing, and
make the meeting screen laid out exactly like Teams ("ditto"), with every
screen adapting to every device. Copy layout, controls, menus, wording and
behaviour; never Microsoft's logo, icon artwork, sounds or the "Teams" name.
Aether keeps its own visual system (one teal accent, slate neutrals, radii
up to 12px). No subagents. Each feature gets the full CLAUDE.md treatment:
tests proven to fail (mutation), `-race`, browser e2e, layout harness at 390
/ 820 / 1280 with screenshots looked at, tshark measurement, a section in
`docs/ARCHITECTURE.md` and a row in `docs/TEAMS_PARITY.md`.

## Where the evidence comes from

`tools/teams-evidence.mjs` reads the installed Teams package (never runs it).
Teams auto-updates, so find the current folder first:

    powershell -c "(Get-AppxPackage -Name MSTeams).InstallLocation"
    node tools/teams-evidence.mjs extract "<that folder>" <outdir>     # ~10 s
    node tools/teams-evidence.mjs strings  <outdir> calling 'spotlight' [limit]
    node tools/teams-evidence.mjs settings <outdir> '^numberOfAllowedSpotlights '
    node tools/teams-evidence.mjs grep "<that folder>" 'participantsNumLargeGrid' 300

Versions read so far: 26276.515.5267.3634 (29 Oct; 2,095 modules, 64,379
strings, 11,829 settings) and 26288.2004.5269.3420 (2 Nov; 2,101 / 62,182 /
11,927). Never put a leading `.{0,N}` in a `grep` pattern: it backtracks on
minified lines and takes minutes; pass a context length instead.

## Done and verified

| Feature | Where | Proof |
| --- | --- | --- |
| Evidence tool | `tools/teams-evidence.mjs` | reproduces the numbers on two versions |
| Spotlight (max 7, exit, stop all, no pin while lit) | `room/spotlight.go`, `OpSpotlight` 0x11f, `web/src/spotlight.js` | room + gateway + client tests, 10 mutations caught, `spotlight.e2e.mjs` 28/28, tshark (ARCHITECTURE §6ay) |
| End meeting for everyone | `room.End`, `OpEndMeeting` 0x120 | same; e2e found and fixed the mic left on after end |
| Teams meeting layout | `web/index.html` (shell `.meet`), CSS block at the end of `styles.css`, `web/src/gallery.js` | layout harness passes at all widths; screenshots looked at; spotlight e2e passes on it |

The Teams layout in detail, so nobody rebuilds it:
- Top bar: clock (now ticks; it never did before) + title | Chat · People(count) · Raise/Lower · React · View · More | Camera · Mic ▾ · Share | Leave ▾ (red; the arrow shows only to hosts and holds "End meeting", `#hc-end`).
- View menu: Gallery (default, cap 9 + "+N more") · Speaker · Large gallery (from 10 people, up to 49) · Focus on content (only while presenting) · Don't show chat bubbles · Full screen. Choice kept in `localStorage aether.layout`.
- More menu: Turn on/off live captions (`#btn-cc`), Audio settings, Meeting info (`#btn-invite`), Pop out (`#btn-pip`).
- Gallery: `bestGrid` sizes tiles to the largest 16:9 that fits; your own video floats bottom-right; pin, a presentation or a spotlight turn it into Speaker; tiles have a "..." menu (Pin for me, Spotlight for everyone / Stop / Exit spotlight); no click-to-pin.
- Chat bubbles: 4000 ms, 300 ms fade (Teams' `chatBubblesTimeout`, `chatBubbleFadeDuration`), from room chat and the kept meeting chat.
- Chat and People are toggles opening a right panel (overlay below 1024 px, full-height sheet on a phone). The meeting opens with the panel closed, as Teams does.
- Phone: the bar splits; clock and panel icons at the top, Camera/Mic/Share/Leave in a bottom dock.
- All ids the code and harnesses used were kept (`btn-mic`, `rail-chat`, `rail-people`, `strip`, `chip-who` ...). `reportViewport` uses `featuredFor()` so the SFU is told exactly which tiles are drawn.

## Batch 2 — done: People panel, roles, Meeting options, hands, removal

Finished 2 Nov 2026 (ARCHITECTURE §6az, TEAMS_PARITY rows). Server: `room/roles.go`,
`OpSetRole` 0x121 (with an organizer bit), `OpRemove` 0x122, `OpLowerHands` 0x123,
"Allow reactions" as an inverted bit in `OpPermissions`, presenters exempt from
attendee controls, host handover now announced. Client: People panel rebuilt
(`renderPeople`, `openPersonMenu`, `popMenu`, `openMeetingOptions`, `wireHostControls`
in `web/src/app.js`), Meeting options under More, `state.myRole`, `state.handOrder`.
Proof: 10 server mutations caught; `tools/e2e/roles.e2e.mjs` 33/33 (fails with the
handover fix removed); `spotlight.e2e.mjs` rewritten on `tools/e2e/meet.lib.mjs`;
layout harness with person-menu / meeting-options / view-menu / leave-menu screens;
tshark in §6az.

## Batch 3 — done: Download attendance list

ARCHITECTURE §6ba. `GET /api/scheduling/rooms/{room}/attendance`, `room.HostSession`, index `0047_scheduling_attendance_room_idx`; security cases in `roles.e2e.mjs`.

## Batch 4 — done: "5 minutes left in your meeting."

ARCHITECTURE §6bb; the 60-minute cap is N/A by decision.

## Batch 5 — done: breakout rooms

ARCHITECTURE §6bc. Server `room/breakout.go`, `signal/breakout.go`; client `web/src/breakout.js` + Rooms panel in `app.js`; `tools/e2e/breakout.e2e.mjs`.

## Batch 6 — done: "Disable mic / camera" for one attendee

ARCHITECTURE §6bd (`OpBlock` 0x124); also fixed presenters being muted by their own client.

## Batch 7 — done: co-organizers chosen when scheduling

ARCHITECTURE §6be; also the regressions the older harnesses caught after the rebuild, and the schedule form's layout. All e2e harnesses pass.

## Batch 8 — done: Q&A

ARCHITECTURE §6bf; `scheduling/qna.go`, `OpQnA` 0x125, Q&A panel in `app.js`, `tools/e2e/qna.e2e.mjs`.

## Batch 9 — done: Call health

ARCHITECTURE §6bg; `web/src/health.js`, `rtc.js healthSample`, `tools/e2e/health.e2e.mjs`.

## Batch 10 — done: meeting notes

ARCHITECTURE §6bh; `scheduling/notes.go`, `OpNotes` 0x126, Notes panel in `app.js`, `tools/e2e/notes.e2e.mjs`.

## Batch 11 — done: whiteboard

ARCHITECTURE §6bi; `room/whiteboard.go`, `signal/whiteboard.go`, `OpWhiteboard` 0x127, `web/src/whiteboard.js`, board in `app.js`, `tools/e2e/whiteboard.e2e.mjs`. Also: layout harness checks meeting-bar overlap; Q&A/Notes/Rooms move under More below 1024 px; `Conn.flush` write-ordering bug fixed (§6bi).

## Batch 12 — done: recording, transcription, live captions

ARCHITECTURE §6bj; `room/recording.go`, `signal/recording.go` (`OpRecording` 0x102; `OpCaption` retired), `scheduling/transcript.go` (tables 0058-0059), `web/src/recording.js`, captions/consent/transcript in `app.js`, `tools/e2e/recording.e2e.mjs`.
Also fixed: the one-to-one call chat showed the wrong log (`renderChatTo` now redraws; the call peer is chosen before the roster), and the SFU test port helper. Older harnesses rerun and passing: whiteboard, call, controls, notes, qna, mute, share, live, messaging.
The recording e2e needs an administrator: a fresh server started with `-admin-email/-admin-password`, the same in `AETHER_ADMIN_EMAIL/AETHER_ADMIN_PASSWORD`. "Recording allowed" is off by default.

## Batch 13 — done: video effects

ARCHITECTURE §6bk; `web/src/effects.js` (+ `effects.test.mjs`), `web/vendor/mediapipe` (Apache-2.0), effects panel / pre-join sheet / `camTrackFor` in `app.js`, `tools/e2e/effects.e2e.mjs`, `tools/e2e/measure-effects.mjs`. `AETHER_E2E_GPU=1` runs the harness with the GPU on.

## Batch 14 — done: constant-quality camera on the socket path

ARCHITECTURE §6bl; VP9 at quantizer 32 (camera) / 24 (screen) in `wsmedia.js` (`pickVideoConfig`), codec on spare flag bit 1 (`wire.js`), receivers switch decoder at a key frame. `tools/e2e/codec-study.mjs` is the study. The §6bk wire finding was corrected (A's port, not B's).

## Batch 15 — done: effects measured properly; Together mode

ARCHITECTURE §6bn (a WebGL compositor built, measured as no gain, removed; §6bk's cost table corrected; `tools/e2e/measure-fx-cost.mjs`) and §6bo (Together mode: `OpTogether` 0x128, `room/together.go`, `signal/together.go`, `web/src/together.js`, scene in `app.js`, `tools/e2e/together.e2e.mjs`). The effects and Together harnesses use a drawn head-and-shoulders camera the segmenter recognises.

## Batch 16 — done: teams as containers above channels

ARCHITECTURE §6bp; migrations 0060-0066, `messaging/teams.go` + `teams_handler.go` + `teams_test.go`, team rules in `channels.go`, Teams surface in `chat.js` (`renderTeamsSurface`), rail button in `index.html`, `tools/e2e/teams.e2e.mjs`, Teams screens in the layout harness.

## Batch 17 — done: the activity feed; coalescing on the chat socket

ARCHITECTURE §6bq; migration 0067, `messaging/activity.go` (+ `activity_test.go`), `OpActivity` 0x129, hooks in `service.go` / `compose.go` / `teams.go` / `repository.go`, chat-socket coalescing in `hub.go` (`writePump`, `TestWriterCoalesces`), Activity surface at the end of `chat.js`, `tools/e2e/activity.e2e.mjs`, `tools/e2e/measure-activity.mjs`. Gotchas: `.unread` is the global badge class (do not reuse it for row state); raw socket clients must read every frame that interns strings, or their dictionary falls behind.

## Batch 18 — done: stress and scale (signalling, chat, teams, activity, meeting controls)

ARCHITECTURE §6br; `tools/e2e/stress.mjs` (`--n`, `--msgs`, `--ui`, `--bin`), `tools/e2e/chatframes.mjs` (shared chat decoder). Fixes, each with a failing-first test: per-person allowances on relayed meeting frames (`room/allowance.go`), unchanged state dropped, argon2id slots min(cores, 8), media routers dropped with their rooms (`Reap(minAge, gone)`), `-trust-proxy` honoured on the meeting socket, upright Together scene + readable names, Go runtime in `/api/telemetry`. A partial index on hidden members measured 6× worse under load and was removed.

## Batch 19 — done: media under stress (socket path)

ARCHITECTURE §6bs; media phase in `tools/e2e/stress.mjs` (`--no-media`, `--media-seconds`). Socket cameras follow each viewer's viewport (`room/sockview.go`, participant ids in `reportViewport`), a lagging receiver drops whole frames until a key frame instead of being disconnected (`Conn.EmitMedia`, 2 MB media budget), and one sender's media has a 25 Mbit/s / 6 MB allowance. 50 people: 4.13 to 1.00 Mbit/s each, 206 to 49.7 Mbit/s out; a 24 MB flood: 2 GB to 352 MB.

## Batch 20 — done: every other meeting feature under load; ordered pushes; shared media payloads

ARCHITECTURE §6bt; the "controls" phase of `tools/e2e/stress.mjs` (Q&A, notes, captions, host controls, breakouts, polls, files, search). `pushMessage` serialises a message's pushes with a dirty re-run (`pushorder_test.go`: monotonic tallies under 100 concurrent votes, and a gated interleaving); media payloads are shared across receivers (`wire.EncodeBatchParts`, `Conn.EmitMedia(payload)`), and a grown write buffer is released.

## The plan to production (agreed 2026-11-07)

Target: a Docker container on an Ubuntu server, for the user's organization and
the public (open sign-up, like Google Meet). The user buys the domain after the
build is done. Outside paid services (email provider, phone carrier, signing
certificate) are skipped for now; build so they plug in later. Every feature is
tested rigorously before the next starts; security is hardened throughout and
penetration-tested once the build is complete. Standing hard rule: every Teams
feature, but everything seen or heard is Aether's own.

- **Phase 0, close-out** (2026-11-07): read-time unread for large
  conversations; MediaPipe telemetry neutralised (`vendor.test.mjs`, harness
  `offOrigin`); GTCRN removed; Go 1.26.10, x/crypto 0.58, x/net 0.62
  (govulncheck: 0 reachable); the fan-out timing test made load-proof; every
  ORDER BY given a total tiebreak (a notes-order bug found by it); stale docs
  fixed; the root `aether-desktop.exe` rebuilt with `-trimpath` (ARCHITECTURE
  §6bx).
- **Phase 1, production foundation.** Done (ARCHITECTURE §6by): the Linux
  race run (in the image's test stage), Dockerfile and compose (distroless,
  read-only, no capabilities), env configuration, ACME (Pebble end to end),
  backups with a restore drill, the open-relay fix, post-quantum TLS, three
  slow-link client bugs (`joinrace`), Prometheus metrics and alert rules, CI,
  email (any SMTP), password reset, the login-CSRF fix (415 for non-JSON
  bodies), third-party notices (`tools/notices.sh`, CI-checked), Windows exe
  icon and version resources (`tools/winres.sh`). Remaining: two-step sign-in
  (TOTP and recovery codes; `totp.go` and `internal/platform/seal` are written
  and tested, not wired), then SSO/OIDC; abuse controls for public sign-up;
  capacity test; Windows installer and updates; the project's own LICENSE
  (the owner's choice).
  Open findings to fix (ARCHITECTURE 6by): the video harness fails about half
  its runs (a sender's bandwidth estimate that starts starved; predates this
  batch, measured), and Chrome rarely wedges a newcomer's subscriber under
  DevTools emulation (joinrace retries that signature once). Email
  verification on sign-up is done (6by).
- **Phase 2, what an organization needs to switch:** calendar day/week/month,
  recurring meetings, RSVP, scheduling assistant (DONE 2026-11-10,
  ARCHITECTURE 6bz; not done: editing one occurrence's time or title); web
  push and a service worker; Aether's own sound set; meeting recap; keyboard
  shortcuts; out of office and notification settings; server-side speaker
  selection; data export and deletion.
- **Security phase:** threat model, hardening review, penetration test of the
  finished build.
- **Phase 3, full parity** (after launch) and **Phase 4, 50,000 users**
  (Postgres or replicated SQLite, multi-node media, distributed load tests).

## When the whole project is finished

- **Restore the Windows Firewall prompts** the user asked to be turned off while work was in progress (2026-11-03): in an elevated PowerShell, `Set-NetFirewallProfile -All -NotifyOnListen True`. It was True before (the Windows default).

## How to run things

    # unit tests
    cd engine && go test -race ./... && go vet ./...
    cd web && npm test

    # test servers (never touch engine/aether.db; use a throwaway db)
    cd engine && go build -o <scratch>/aether-test.exe ./cmd/aether
    <scratch>/aether-test.exe -addr :9443 -tls -db <scratch>/e2e.db -web ../web -media-port 8811
    <scratch>/aether-test.exe -addr 127.0.0.1:8444 -db <scratch>/measure.db -web ../web -media-port 8802 -relay=false

    # the whole browser battery, natively or against the hardened container
    # (both start Mailpit, a real SMTP server, and turn email on)
    & tools/e2e/.stress/browser-run.ps1 -Fresh -Names console,layout,reset -Out tools/e2e/.stress/browser.txt
    & tools/e2e/.stress/docker-run.ps1 -Fresh -PublicIP <host ip> -Names console,layout -Out tools/e2e/.stress/docker-browser.txt
    bash tools/e2e/acme.e2e.sh          # automatic certificates against Pebble

    # browser e2e and layout (web is served from disk: no rebuild for JS/CSS)
    node tools/e2e/spotlight.e2e.mjs https://localhost:9443   # also roles, breakout, endnotice, coorg, call, controls, share, mute, audio, video, live, livestatus, messaging
    # sign-in is rate-limited (5/min, burst 10, in memory): restart the throwaway server between harness runs
    node tools/e2e/layout.e2e.mjs https://localhost:9443 [--only phone]   # screenshots in tools/e2e/.shots

    # tshark (plaintext server, IPv4 loopback: 4 B loopback + 20 IP + 20 TCP)
    "C:/Program Files/Wireshark/tshark.exe" -i '\Device\NPF_Loopback' -f "tcp port 8444" -w meet.pcapng -a duration:22 &
    node tools/e2e/measure-meeting.mjs
    tshark -r meet.pcapng -Y websocket -T fields -E separator='|' -e frame.time_relative -e tcp.srcport -e frame.len -e tcp.len -e websocket.payload_length

    # mutation check: each (file, old, new) applied alone, tests run, restored
    cd engine && python ../tools/mutate.py ./internal/signal/ 'TestName' <file> '<old>' '<new>' ...

## Gotchas found on the way

- The Bash tool fails on heredocs containing some quote mixes ("unexpected EOF"): write the content with the Write tool, then `cat`/`python` it in.
- `cd` into `C:\Program Files\WindowsApps` is denied; reading files by full path works.
- `until(page, fn)` in meet.lib returns FALSE on timeout, it does not throw: a harness that waits for home and then calls the API sees a confusing 401 when the sign-in itself failed (a 429 from the login limiter: 5/min, burst 10 per address, shared with the second-step endpoint). Assert the outcome after a wait, as twostep.e2e.mjs signIn does, and budget sign-ins per harness. Sign-UP is 3/hour per address by default; the runners pass `-signup-per-address 20` (a harness that must create its own accounts because an earlier suite failed would otherwise hit it). The runners also wait for the fake identity provider to listen before starting the server: started together, the server found no provider and exited, and the whole CI browser job fell over from that one race.
- Windows reserves random port ranges at boot (Hyper-V/WSL): `netsh interface ipv4 show excludedportrange protocol=udp`. When one overlaps the relay range, `docker run` fails to publish ("forbidden by its access permissions"), the container never appears healthy, and media never connects. `docker-run.ps1 -RelayPorts <a free range>` publishes another; this is a test-machine problem only (Linux has no such reservation).
- The layout harness checks overflow, tap size and meeting-bar overlap only; still look at the screenshots for everything else.
- `go test -race ./...` runs packages in parallel; the 100-person fan-out test can miss real time from CPU starvation alone. If it fails only there, run it alone (and the suite with `-p 1`) before suspecting code.
- Signup is limited to 3 an hour per address (in memory): a fresh server for an e2e needing four new accounts means a restart halfway. `accounts()` now saves each account as soon as it is made.
- Appending to a doc in Python: read it FIRST, then open for writing. `open(p,'w').write(open(p).read()+x)` truncates before reading; it wiped ARCHITECTURE.md once (restored from git, commit 4aade70).
- Heredocs with mixed quotes still fail in the Bash tool: write the text with Write, then append with python.
- `measure-meeting.mjs` writes client WebSocket frames itself; frames over 125 B need the extended length (fixed; a 200-point stroke broke it).
- Older CSS layers force `display` on some elements and beat the `hidden` attribute; inside the meeting bar `.meet-bar [hidden]` is `!important` for that reason.
