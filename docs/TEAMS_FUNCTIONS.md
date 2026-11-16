# Teams, function by function

Read on 15 Nov 2026 from the installed desktop package
**MSTeams 26304.1201.5310.7985 (x64)**, without running it. This is the
inventory behind `docs/TEAMS_PARITY.md`: that file says what Aether has built;
this one lists what Teams ships, area by area, so nothing is missed by not
knowing it exists.

Status words are the parity matrix's: **Built**, **Partial** (gap named),
**Not built**, **N/A** (needs Microsoft's ecosystem, a phone carrier or room
hardware), and **Never** (a paid plan or upsell; the owner's rule). A function
is listed under what it does, never under how Teams draws it: the hard rule
stands that everything seen or heard in Aether is its own.

## How it was read, and what that can and cannot show

Two readers, both static:

- `tools/teams-evidence.mjs` (ours): decompresses the web chunks in memory
  and pattern-matches them.
- REA (the reverse-engineering server): the native layer. Export and import
  tables of the executables, read from the PE headers.

      powershell -c "(Get-AppxPackage -Name MSTeams).InstallLocation"
      node tools/teams-evidence.mjs extract "<package>" <out>
      node tools/teams-evidence.mjs labels  <out>            # count per area
      node tools/teams-evidence.mjs labels  <out> settings   # one area's commands
      node tools/teams-evidence.mjs strings <out> people 'out of office'

| What | Count |
| --- | --- |
| Web chunks in the manifest | 7,056 (6,960 files on disk, 207 MB gzipped) |
| English code after unpacking | 3,133 files, 341 MB (3,825 locale copies skipped) |
| Feature modules (chunk names, locales folded) | 2,101 |
| English UI strings | 62,182 in 102 namespaces (37,117 distinct) |
| **Labels** (strings short enough to be a button, menu item or heading) | **17,403** |
| Compiled default settings | 11,927 |
| Native binaries | 3 executables, 51 DLLs |
| Shipped sounds | 24 MP3 files |

**Observed** (in the package): every module name, string, setting, export and
import quoted here. **Inferred**: that a label is a function a person can
invoke. Translation keys are hashed (`kateb0cyh`), so a label is the nearest
thing to a function name the package has; grouping labels into functions is a
reading, done by hand below. **Not shown by any of this**: which functions a
given tenant or licence turns on, anything the server does, and any behaviour
at run time. Nothing was executed and no traffic was captured.

**The code graph, and where it stopped.** REA was also asked for a static
graph of the whole unpacked client (modules, calls, data flow; parsed as
text, nothing run). At its default budget it parsed 151 of 3,120 files
(3,145,906 syntax nodes, 3,297 webpack modules) and then met its 300-second
per-file deadline on the largest chunk, a single 28 MB file. The same call
narrowed to five chunks (the two per-conversation notification dialogs, the
recommendation dialog, the notifications entry and the presence service)
finished: 38,241 nodes, 33 modules, 698 call relations. So a whole-client
graph is hours of budget and is not what this inventory rests on; a graph of
one feature's chunks is seconds and is the way to read a behaviour closely.
Identifiers read that way are quoted in the rows below
(`missedActivityEmailDelayOptions`, `MentionsAndReplies`).

The label filter drops sentences, so a function that exists only as a
sentence ("Send replies only during a time period") is found by `strings`,
not `labels`. The areas below were read with both.

## The native layer (REA)

| Finding | Evidence |
| --- | --- |
| The app is a native host around a WebView2 page, the same shape as `cmd/aether-desktop` | `ms-teams.exe` (37.9 MB, sha256 `c7460dde…`) delay-loads `WebView2Loader.dll`; imports 64 DLLs / 1,237 functions, delay-loads 29 / 258 |
| The media engine is native, bridged into the page | `SlimCoreWebview2.dll` (20.4 MB) exports 112 symbols: 13 `SlimCoreWV2::` bridge functions (`Register`, `RegisterTrusted`, `SetStreamRendererFactory`, `SetHostServiceFactory`) and 97 `napi_*` functions |
| Codecs and capture are a separate library | `RtmCodecs.dll` exports 211 symbols: `MLE*`/`MLD*` (encoder, decoder, `MLDIsHWDecoderResourceAvailable`), `RtcPalSetDirectMftHevcEnabled`, MJPEG camera decode, `RtmCodecsVscaCreateFecUtility`, `RtmCodecsVscaCreateSourceRequestUtility`, `RtmCodecsGetPacsiInfo`, `RtcPalVideoSourceIsScreenScraper`, virtual video sources, per-frame latency steps |
| Headset buttons are read directly | `HID.DLL`, 26 functions, delay-loaded ("Sync device buttons" in the UI) |
| Speech recognition can run on the device | `Microsoft.CognitiveServices.Speech.extension.embedded.sr*.dll` and an ONNX runtime |
| Local store, network probes, crash capture | `sqlite3.dll` (35), `wlanapi` (5), `WINHTTP` (19), `wer.dll`, `dbghelp.dll` |
| Sounds are recorded files | `Audio/`: 24 MP3 files, among them ring, ringring, meetup_ring, screenshare_ring, teams_notification and 13 named tones (beep, bop, bounce, bubbles, dripdrop, eureka, flutter, highscore, nextlevel, plink, remix, ripple, spacetime, wishgranted, wobble) |

What this means for Aether: Teams carries its own encoder, forward error
correction and receiver-driven source requests in native code. Aether's
choice is the browser's codecs behind an SFU that never sends what cannot be
seen (ARCHITECTURE 3, 6bv), and synthesised sounds (6bz). Nothing here
argues for a native media engine; the receiver-driven request is the idea
worth having and it is already built as viewport allocation.

## 1. Meetings and calls (`calling`: 7,168 strings, 3,325 labels)

The largest area by far. Read in thirteen groups.

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| **Joining.** Pre-join with device check, join with audio off, lobby, admit / deny / admit all, lock the meeting, rejoin | Built | |
| Join with a numeric meeting ID and passcode; "Verify you're a real person" (captcha) before an anonymous join | Not built | Aether joins by link; the passcode is the link. A challenge for anonymous joiners belongs to the security phase |
| Lobby chat; "Send message to lobby participants" | Not built | |
| Transfer a call to this device; "Active on another device"; leave on all my devices; add a room; companion mode | Not built | One account on several devices already sees its messages live; a call does not move |
| **Devices and audio.** Choose microphone, speaker, camera; noise suppression; echo cancellation; automatic gain | Built | RNNoise, the leveler, the Audio panel (6ar-6aw) |
| Noise suppression levels (auto / high / low), voice isolation, music mode / high fidelity, stereo, spatial audio | Not built | Suppression is on or off |
| Make a test call; test speaker; "Teams can't hear you" checks | Partial | Pre-join meter only; no echo-back test call |
| Headset call buttons (HID), secondary ringer, choose a ringtone | Not built | Ringtone choice is cheap: the cues are scores (sounds.js) |
| **Video.** Background blur, portrait blur, image backgrounds, add your own, soft focus, brightness, mirror | Built | 6bk |
| Green screen, generated backgrounds, frames / decorate, avatars, auto framing, eye contact | Not built | Avatars and generation need outside services |
| Turn off incoming video; hide a participant's video for me | Not built | Viewport allocation already stops unseen video; a manual switch is small |
| **Views.** Gallery, Speaker, Large gallery, Together mode with scenes, Focus on content, Full screen, pin for me, pop out | Built | 6az, 6bo |
| Gallery at top / on the right, gallery size, prioritise video; assign seats in Together mode; zoom and pan shared content | Not built | |
| Presenter modes over shared content (standout, side by side, reporter, cameo) | Not built | |
| **Sharing.** Screen, window, tab; whiteboard; one presenter at a time | Built | 6ax, 6bi |
| Include computer sound | Not built | `getDisplayMedia` is asked for video only |
| Give control / take control of a shared screen | Partial | Engine built and measured (`internal/rc`, `modules/control`, opcodes `OpControlInput`/`OpControlState`, ARCHITECTURE 4c) in the Windows path; not offered in the browser meeting bar |
| Pause sharing; several people sharing at once; share a portion of the screen; content from camera | Not built | |
| PowerPoint Live, Excel Live, collaborative annotation over a share | Not built | Annotation could reuse the whiteboard stroke path |
| **People and roles.** Organizer, co-organizer, presenter, attendee; make presenter / attendee; mute all; disable mic / camera for one or all; remove; raised hands in order; lower all; spotlight (7); end for everyone | Built | 6ay-6bd |
| Hide attendee names; view-only attendees past capacity; edit display name in the meeting | Not built | |
| Sign-language view, signer role | Not built | |
| **Reactions, hands, meeting chat, chat bubbles** | Built | 6av |
| **Recording and words.** Record (local), transcribe with consent, live captions, download .vtt / .txt, banner and audit | Built | 6bj |
| Record to the cloud; record automatically; caption font, colour, height, position; translated captions; spoken-language choice; profanity filter; .docx / .pdf transcript; delete transcript | Not built | Caption styling and "record automatically" need no outside service |
| CART captioner, real-time text (RTT), teletypewriter mode | Not built | Accessibility; RTT is a text channel over the meeting socket |
| **Meeting tools.** Q&A, notes (agenda, notes, tasks), breakout rooms, attendance list, call health, "5 minutes left" | Built | 6ba-6bh |
| Meeting timer ("Set a timer", agenda timer) | Not built | |
| Watermark, end-to-end encryption with a verification code, sensitivity labels | Not built | E2EE is honest only for one-to-one calls on WebRTC; the socket path is hop-by-hop by design (gateway.go says so to the user) |
| Rate your meeting; quality feedback | Not built | |
| **Language and AI.** Human interpreters on a language channel | Not built | An audio lane per language is within reach of the mute-lanes work |
| AI interpreter, Copilot, Facilitator, intelligent recap, speaker coach, voice and face profiles, synthetic-speech detection | Not built | Each needs a model service; out of scope for a self-hosted build |
| **Telephony inside the product.** Hold, transfer, consult then transfer, merge, park, forward, also ring, call groups, delegates, voicemail and greetings, speed dial, call history, missed calls | Partial | Ringing, accept / decline and the call chat are built; a missed call is shown in the thread. Hold, transfer, forwarding, history as a list: not built, and none of them needs a carrier |
| PSTN dial-in / dial-out, dial pad, caller ID, emergency calling and location, spam call filter, call queues, auto attendants, shared lines, barge / whisper / monitor | N/A | Carrier |
| **Events production.** Producer view, bring on screen / take off, green room, RTMP in and out, NDI, run of show | Not built | With webinars and town halls (section 9) |
| **Rooms and devices.** Teams Rooms, room remote, proximity join, Front Row, cast, PTZ, content camera, hot desking, bookable desks | N/A | Hardware |

## 2. Chat (`messaging`, `files`, `compose`, `chatList`, `simpleCollab`, `funstuff`: about 13,000 strings)

`files` is misnamed: it is the shell's shared bundle and repeats most of the
chat, settings and status strings.

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| One-to-one, group and self chat; formatting; reactions; quoted replies; edit with history; delete; undo; forward; mentions and @everyone; read receipts; typing; pin and save; important / urgent; scheduled send; polls; praise; files, images, clips, voice messages; search in a chat and across chats; favourite, mute, hide, mark unread; block; filters; pop out | Built | 6w-6ai |
| **Remind me about this message** (set reminder, view reminders, reminder due in the feed) | Not built | Small: the scheduled-send scheduler already wakes on a time |
| **Report this message / this person** (harassment, spam, and so on), with the report going to an administrator | Not built | Needed for a public deployment; belongs with the security phase |
| Chat requests from people you have never talked to (accept, block, "preview their messages safely") | Partial | Aether rings only between people who share a chat and exposes one exact address; there is no request inbox |
| Sections / folders in the chat list; move to section; list density (comfy, compact); time stamps on or off | Not built | |
| Code snippet with a language; tables; headings; text and highlight colour; horizontal rule | Partial | Inline code and code blocks only |
| Link previews (and "Remove URL preview"); copy link to a message | Partial | Copy link exists; previews not built. A preview is a server-side fetch: it must be bounded and never follow private addresses |
| Custom emoji ("Add your own emoji", "Your org's emoji"); skin tone | Not built | |
| GIF search, stickers, memes | Not built | Outside catalogue; by decision |
| Translate a message; auto-translate; immersive reader; suggested replies; Copilot rewrite and tone | Not built | Model service |
| Loop components | Not built | Collaborative editing subsystem |
| Threads inside chats (follow, unfollow, "New Thread"), convert a chat to a channel, move messages to a channel | Not built | Channel threads are built |
| Share a contact card; share to email | Not built | |
| Export your chat history | Not built | Queued in Phase 2 as data export |
| "is out of office and may not respond" above the compose box | Built | 6ca; direct chats. A status note is not shown there, and a group does not say a member is away |
| SMS chat | N/A | Carrier |

## 3. Teams and channels (`teamsAndChannels`, `channels`, `peopleTargeting`, `communities`)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Team as a container; owners and members; public and private; join code; General channel; standard and private channels; threads with replies; pin; topic, picture, rename; announcements; delete | Built | 6bp, 6y, 6ai |
| **Channel notification level** (all new posts / mentions and replies / off), per channel | Partial | Mute per conversation; the account-wide kinds of 6cb apply to channels as groups. No middle level per channel |
| **Channel moderation**: who can start a post, who can reply ("You and moderators can reply") | Not built | |
| Tags: create and manage, @tag reaches its members, tag-based chat | Not built | Server already expands @everyone; a tag is a named subset |
| Join requests and approval; pending members | Not built | |
| Archive and restore a team; hide / show channels; channel info pane | Not built | |
| Post with a subject; post in several channels at once; announcement banner colour | Partial | Announcements built; subject and cross-post not |
| Member permission matrix (create channels, delete messages, add tabs) | Partial | Owner / member only |
| Channel meetings ("Meet now" in a channel), channel calendar | Not built | |
| Tabs, connectors, incoming webhooks, email to a channel | Not built | No app platform by decision; an incoming webhook alone is small |
| Shared channels, org-wide teams, guests in teams | N/A / Not built | Single deployment |
| Communities (the consumer product: nicknames, guidelines, featured) | N/A | "Communities is retiring soon" is in the package itself |

## 4. Calendar and scheduling (`calendar`, `recurrence`, `meetingOptions`, `meetForWork`)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Month, week and day; schedule with invitees, categories, co-organizers; recurring series; cancel one or all; accept / decline; who is free; meeting options | Built | 6bz |
| Tentative ("Maybe"); respond to one occurrence | Not built | An answer is to the series today |
| Edit one occurrence's time or title | Not built | Recorded in the parity matrix |
| All-day events; time zone shown per attendee; work week view; agenda view | Not built | The organiser's zone is kept; display is the viewer's |
| Forward a meeting; cancellation note; "I'm running late" | Not built | |
| Required and optional attendees; location / room | Not built | |
| Meeting templates; record automatically; allow forwarding | Not built | |
| Working hours ("Outside your working hours") | Not built | Feeds quiet hours and free/busy |
| Meeting started notification; meeting chat notifications until I join | Not built | |
| Calendar file for other calendars (.ics) | Not built | Small; no outside service |
| Google and Outlook calendar sync, Bookings | N/A | External calendars |

## 5. Activity, notifications and settings (`activity`, `toastNotifications`, `settings`: 404 strings, 212 labels)

This is the area the next queue item covers, so it is listed in full.

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Feed of mentions, replies, reactions, team adds; unread badge; filters; mark all read | Built | 6bq |
| Missed calls and voicemail in the feed; delete from feed; mark one unread | Partial | Missed calls are in the thread, not the feed |
| Do not disturb; quiet hours; a sound level; push while closed, per browser | Built | 6bz |
| **Per-kind choice**: chat messages, personal @mentions, channel and team mentions, everyone mentions, replies, likes and reactions — each "banner and feed", "only show in feed" or off | Built | 6cb: direct, group, mention, reply, reaction as alert / quiet / off. Everyone and channel mentions are not separate kinds |
| **Show message preview** on or off | Built | 6cb; the words are withheld on the server, so a lock screen never has them |
| **Meeting started**, **meeting chat** (on / until I join or send / off) | Not built | Aether sends no "meeting started" notice to configure |
| **Missed activity emails**: as soon as possible, every 10 minutes, hourly, every 8 hours, daily, off (`missedActivityEmailDelayOptions`, default `"Hourly"`) | Built | 6cc. Off (the default), every 10 minutes, hourly, every 8 hours, daily; one plain-text email a period about what is still unread. "As soon as possible" is not offered: the push notification is that channel |
| **Per conversation**: all activity, mentions and replies, or muted (`MentionsAndReplies`, `Mute`), with separate switches for everyone, channel / team and tag mentions | Partial | Mute per conversation is built; the middle level is not |
| Test notification | Not built | |
| Notify me when a person becomes available ("Manage status notifications") | Not built | |
| Priority access: people who may reach you through do not disturb | Not built | Urgent messages already pass |
| Pause notifications until a time | Partial | Do not disturb with an expiry covers it |
| Reply or react from the notification itself | Not built | A service-worker action; needs care with CSRF |
| Appearance: light, dark, high contrast, follow the system; accent | Partial | Follows the system; no manual switch |
| App language, date and time format, 24-hour clock | Not built | The client is English only |
| Read receipts on or off; blocked contacts list | Partial | Block list exists; receipts are always on |
| Auto-start, open in background, efficiency mode, download location | Partial | The Windows shell starts and updates itself; no settings page for it |
| Customise keyboard shortcuts; shortcuts from other apps | Not built | One table (shortcuts.js); remapping is a second column |
| Spell-check languages, markdown on or off, Enter sends or starts a line | Not built | The browser's spell-check is used |

## 6. People, presence and profile (`people`, `presence`, `livePersonaEditor`, `contacts`, `places`)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Available, busy, do not disturb, away, appear offline; a note with an expiry; automatic in a call / presenting / away; last seen; profile card | Built | 6ag, 6an-6ap |
| **Out of office**: a message, optionally only during a time period, shown on the profile and to anyone who writes | Built | 6ca. No automatic reply and no separate message for outsiders, by decision |
| Status duration ("Reset status after"); "Be right back" | Partial | The note expires; the picked status does not have its own timer in the UI |
| "Show when people message me" for a status note | Not built | The out of office line is shown that way; a status note is not |
| Profile picture; edit display name | Partial | Letters and colour come from the person (6n); no photo upload |
| Contacts: favourites, groups, speed dial; sync from Google, Outlook, the phone | Partial / N/A | Directory and people search are built; contact groups are not; sync is external |
| Work location (office, remote, a building); nearby | Not built | |
| Organisation chart | Not built | Needs a manager field |

## 7. Search (`search`, `autosuggest`, `unifiedSearch`)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Search messages across chats and inside one; people search | Built | 6ab, 6m; 7.5 ms p95 over 200k |
| Filters: from a person, date range, has attachment, mentions me, team or channel | Not built | Each is an indexed column already |
| File search; meeting search; recent searches | Not built | |
| Slash commands (`/goto`, call someone, set status) | Not built | |

## 8. After the meeting (`meetingRecap`, `meetingAnalytics`, `coachReport`, `podcast`)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Recap page: who came and for how long, notes, transcript, downloads, meeting chat | Built | 6bz |
| Attendance and engagement report: first join, last leave, role, reactions, hands, Q&A counts | Partial | Join, leave and duration; no engagement counts |
| Recording playback with chapters and speakers | Not built | Recording is local to the recorder |
| AI summary, tasks, audio recap, speaker coach | Not built | Model service |
| Share the recap; request access | Partial | Attendees and invitees can read it; no sharing beyond them |

## 9. Events (`virtualEvents`, `webinar`, `broadcast`, `broadcastScheduling`, `immersiveContent`: about 3,600 strings)

| Teams function | Aether | Gap or note |
| --- | --- | --- |
| Webinar: registration form with custom questions, capacity and waitlist, approval, confirmation and reminder emails, presenter bios, theme | Not built | Phase 3; registration is the part worth having first |
| Town hall / live event: producer, queue and live, attendee view, event Q&A, embed, RTMP | Not built | |
| Immersive 3D spaces | N/A | |

## 10. Platform and the rest

| Area (namespace) | What Teams does there | Aether |
| --- | --- | --- |
| Apps (`extensibility`, 2,290 strings) | Store, tabs, bots, message extensions, connectors, workflows, adaptive cards, meeting apps | Not built; no app platform by decision |
| Breakout rooms (`breakoutRooms`) | Built. Not yet: let people choose their room, import assignments from CSV, move people while rooms are open | Partial |
| Call queues (`queues`), personal attendant | Queue dashboards, opt-in, auto attendant flows | N/A (carrier) |
| Accounts (`auth`, `nova`) | Several accounts and organisations side by side, switch org, join without an account, external team invitations | Partial: sign-up, two-step, SSO, guest join by link; one account per browser |
| Compliance (`InformationProtection*` modules, sensitivity labels, "Message violates organization policy", safe links, threat detail) | Labels, data-loss prevention, link scanning | Not built; the audit log is the compliance surface |
| Desktop (`desktop`) | Tray menu with status, auto-start, repair, collect logs, update and restart | Partial: tray, badge, toasts, self-install and server-fed updates; no log collection or status from the tray |
| Feedback and growth (`survey`, `feedback`, `whatsNew`, `esp`, `growth`, `campaignNotifications`) | Surveys, what's new, coach marks, product campaigns | Not built; none wanted |
| Paid plans (`premiumBenefits`, upsell, trials, credits) | Plans, trials, licence requests | **Never** |
| Education, guardians, HoloLens, Places, cast | Class teams, parents, mixed reality, desk booking | N/A |

## What this changes in the queue

Nothing in the agreed order (`docs/RESUME.md`). It makes three things
concrete:

1. **The next item has a specification.** Out of office is a message with an
   optional time period, shown on the profile and above the compose box of
   anyone who writes. Notification settings are a choice per kind (banner and
   feed, feed only, off), message preview, meeting started, meeting chat,
   missed-activity email frequency, and a per-channel level.
2. **Two gaps matter for a public deployment more than their size suggests**:
   reporting a message or a person to an administrator, and a challenge
   before an anonymous join. Both belong to the security phase.
3. **The cheap wins** that need no outside service, for Phase 3: message
   reminders, tentative RSVP, a calendar file, the meeting timer, caption
   styling, record automatically, include computer sound, channel moderation,
   tags, search filters, hold and transfer, ringtone choice.

## Honest limits of this inventory

- It is one build on one machine. Teams updates itself; four builds have
  been read so far (26276, 26288, 26304 twice). Re-run `extract` before
  quoting a number.
- A label proves a string ships, not that the function is on. Thousands of
  strings sit behind settings (`enableOOFSettings = !0` is on by default;
  many others are `!1`).
- The grouping into functions is by hand. The rows above cover every
  namespace over 40 strings; 30 smaller ones (avatar, links, lightbox and
  the like) are UI furniture and are not listed.
- Aether's status in each row was checked against `docs/TEAMS_PARITY.md`
  and by searching the code for the function on the day of writing; a row
  marked Partial names what was found.
