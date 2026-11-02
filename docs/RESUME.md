# Resume here — Teams parity work

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

## Next, in order

1. Video effects/background blur, together mode.
2. Beyond meetings: teams as containers above channels, activity feed, calendar views + scheduling assistant, remaining chat items (see TEAMS_PARITY "Not built" rows).

## How to run things

    # unit tests
    cd engine && go test -race ./... && go vet ./...
    cd web && npm test

    # test servers (never touch engine/aether.db; use a throwaway db)
    cd engine && go build -o <scratch>/aether-test.exe ./cmd/aether
    <scratch>/aether-test.exe -addr :9443 -tls -db <scratch>/e2e.db -web ../web -media-port 8811
    <scratch>/aether-test.exe -addr 127.0.0.1:8444 -db <scratch>/measure.db -web ../web -media-port 8802 -relay=false

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
- The layout harness checks overflow, tap size and meeting-bar overlap only; still look at the screenshots for everything else.
- `go test -race ./...` runs packages in parallel; the 100-person fan-out test can miss real time from CPU starvation alone. If it fails only there, run it alone (and the suite with `-p 1`) before suspecting code.
- Signup is limited to 3 an hour per address (in memory): a fresh server for an e2e needing four new accounts means a restart halfway. `accounts()` now saves each account as soon as it is made.
- Heredocs with mixed quotes still fail in the Bash tool: write the text with Write, then append with python.
- `measure-meeting.mjs` writes client WebSocket frames itself; frames over 125 B need the extended length (fixed; a 200-point stroke broke it).
- Older CSS layers force `display` on some elements and beat the `hidden` attribute; inside the meeting bar `.meet-bar [hidden]` is `!important` for that reason.
