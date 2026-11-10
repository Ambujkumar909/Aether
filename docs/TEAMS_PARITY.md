# Aether against Microsoft Teams — feature parity matrix

Compiled 21 Sept 2026 from Microsoft's own pages: the Teams service
description, the small-business setup guide, the meetings-and-events feature
comparison, and the limits-and-specifications page (all fetched, listed at
the end). Status is what is built and proven here, not what is planned.

**Work in progress:** the Teams parity build is paused mid-batch; what is
done, what is half done and what comes next is in `docs/RESUME.md`.

**Package evidence (added 29 Oct 2026).** Every row is now also checked
against the shipped Teams desktop package (MSTeams 26276.515.5267.3634,
x64), read without executing anything:

    node tools/teams-evidence.mjs extract "C:/Program Files/WindowsApps/MSTeams_<version>_x64__8wekyb3d8bbwe" <out>
    node tools/teams-evidence.mjs strings  <out> calling 'spotlight'
    node tools/teams-evidence.mjs settings <out> '^numberOfAllowedSpotlights '
    node tools/teams-evidence.mjs grep     <package> 'maxBreakoutRoomCount:[^,]*'

It yields 2,095 feature modules, 64,379 English UI strings in 102 namespaces
and 11,829 default settings. A rule quoted in a row below in "quotes" is a
string from the package; a number in `code` is a setting from it.

Legend: **Built** = shipped, unit-tested, in the e2e harness where it has an
endpoint, and walked by the layout harness where it has a screen. **Partial**
= a usable subset, gap named. **Not built** = deliberately not, reason given.
**N/A** = not applicable to a self-hosted product without Microsoft's
ecosystem.

## Chat

| Feature | Status | Notes |
| --- | --- | --- |
| 1:1 chat, group chat, notes to self | Built | Groups auto-named; 40+ members do not ship member lists |
| Message formatting (bold, italic, strike, code, code blocks, links) | Built | Safe markdown subset, toolbar with pin |
| Reactions (pill, who reacted, toggle, only your own removable) | Built | Push carries reactions; harness proves nobody removes another's |
| Replies with quote | Built | Quote refreshed on read; cross-conversation quote refused |
| Edit, edit history | Built | `messaging_message_edits`; member-only history |
| Delete for me / for everyone; undo send | Built | Tombstone for everyone |
| Forward (with files) | Built | All-or-nothing on membership |
| Mentions: @name, @everyone/@all/@channel | Built | Server expands `*` to members (≤200); counted per member |
| Read receipts (1:1 seen/sent; groups "read by N" with list) | Built | From read cursors; cursor only moves while looking |
| Typing indicator | Built | Small conversations only |
| Presence (online/offline) | Built | Peers only |
| Availability status + note + expiry; Do not disturb | Built | `OpStatus`; DND silences all but urgent |
| Automatic "In a call" / "Presenting" while in a meeting | Built | From the room registry, pushed on join/leave/share; not settable by hand; the calendar sets nothing |
| Automatic Away after 5 idle minutes; every device idle | Built | Client idle watch → `OpIdle`; server folds devices; a picked Busy/DND/Away stands |
| Last seen (offline, or idle-away) | Built | Stamped on disconnect and on going idle; on the status read, the status push and the departure frame |
| Appear offline | Built | Manual; hides arrival, departure and last seen; even a live call reads offline |
| Offline grace (no flicker on reconnect) | Built | 10 s after the last socket closes before anyone is told |
| Status glyphs (tick, bar, clock, handset, screen, ring) | Built | One SVG set on every avatar: chat, header, members, meeting people, directory, picker |
| Profile card from a name or avatar | Built | Chat header, message author, members list, meeting people; message / audio / video actions |
| Typing indicator (multi-typer, self-expiring, server-coalesced) | Built | 6 B relayed frame; one per 3 s while typing; stop on empty/pause/send; list-row preview |
| Same account on several devices sees its own messages live | Built | Device id per tab; pushed to the other devices, never echoed to the sender |
| Echo cancellation required on the microphone; warning when a device cannot | Built | `web/src/echo.js`; exact constraints, fallback, toast |
| Same-device echo advisory (two clients in one meeting from one address) | Built | `OpSameDevice` at join; the one echo no server can remove |
| Adaptive receive jitter buffer on the socket media path | Built | `web/src/jitter.js`; glass-to-glass 348 → 170 ms measured |
| Instant unmute (silenced live track, no renegotiation) | Built | 96 ms on WebRTC, 181 ms on the socket |
| Automatic WebRTC for browsers without WebCodecs (Safari, iOS, Firefox) | Built | `chooseTransport` |
| Muted participants cost the room nothing (socket path) | Built | disabled track sends no frames; receive clock treats the gap as a pause |
| Active-speaker audio selection at the server | Not built | relay every unmuted sender; 100 talking = 3.2 Mbit/s per recipient, measured |
| gzip for pages, scripts and JSON; 304 revalidation for unchanged files | Built | first load 645 → 210 KB; unchanged files 0 B body |
| On-demand keyframes on the socket path (periodic every 10 s only as fallback) | Built | `OpKeyframeReq` to a socket publisher, deduplicated per 300 ms |
| Meeting reactions (10, click picker, rising animation, nothing stored) | Built | `tools/e2e/call.e2e.mjs` |
| One-to-one call: kept conversation and a call chat that ends with the call, on both sides | Built | selector labels say which is which |
| Quiet hours | Built | Per device |
| Pin a message; save/bookmark messages | Built | |
| Priority (important, urgent) | Built | |
| Scheduled send | Built | Delivered by scheduler; author-only until then |
| Praise cards, announcements, polls | Built | Polls: live tally on the push, anonymous option |
| Files, images, video clips, voice messages | Built | MIME sniffed, inline allowlist, membership before bytes |
| Shared files / links / photos tabs | Built | |
| Search across chats and within a chat | Built | Trigram FTS on plain text; 7.5 ms p95 over 200k |
| Favourites, mute, hide, mark unread, delete for me | Built | |
| Block a person | Built | Both directions quiet; groups untouched |
| Filters (unread, meeting chats, channels, groups, favourites, muted) | Built | |
| Open chat in new window | Built | Web; bare popup in the shell |
| Desktop notifications, taskbar/tray badge, ring flash | Built | Windows toasts from the shell; toast rendering not observed headless |
| Activity feed: mentions, replies, reactions, team adds; unread badge; filters; mark all read | Built | §6bq; grouped reactions and day sections, pushed live; missed calls and file shares not in the feed yet |
| Translate, immersive reader, AI rewrite/Copilot | Not built | Needs an external model service; out of scope for a self-hosted build |
| GIF/sticker galleries | Not built | External catalogue (Giphy); emoji picker and images cover it |
| Loop components / live shared canvases | Not built | Large collaborative-editing subsystem |
| Text prediction | Not built | Suggested replies exist for questions |

## Teams and channels

| Feature | Status | Notes |
| --- | --- | --- |
| Standard (public) channels | Built | Browse, join, leave, owners |
| Private channels | Built | Invisible; members add |
| Shared channels (cross-tenant) | N/A | Single deployment; external accounts sign up directly |
| Threaded posts and replies | Built | Roots feed; one level deep; summaries |
| Channel name rules and uniqueness | Built | Unique partial index |
| Channel picture, topic, rename, pin | Built | |
| Add members with history option | Built | none / last N days / all |
| Delete channel (owner) | Built | Files removed with it |
| Teams as a container above channels: owners/members, public/private, join codes, General, standard and private channels | Built | §6bp; `messaging/teams.go`, Teams in the rail |
| Org-wide teams; tags; tabs/apps; guests; shared channels; archiving | Not built | Teams' enterprise layer; no app platform |
| Limits (Teams: 250 in a group chat, 1,000 channels, 100 KB post) | Built (own) | Body 4,000 chars, 10 files, 100 MB, 20 mentions/200 for everyone — each bound tested one over |

## Meetings and calls

| Feature | Status | Notes |
| --- | --- | --- |
| Video/audio meetings, 1:1 and group calls, ringing | Built | SFU; rings only between people who share a chat |
| Pre-join lobby (camera/mic check), lobby admission | Built | |
| Screen sharing, hand raise, reactions, captions | Built | |
| Meeting chat (broadcast) and persistent meeting chat at 3+ | Built | Group created automatically |
| Invite people during a meeting; invite links | Built | |
| Mute all, roles/permissions, host controls | Built | |
| Spotlight for everyone | Built | Hosts only, `numberOfAllowedSpotlights = 7`, "Exit spotlight", "Stop all spotlights", "Can't pin a participant that is spotlighted"; ARCHITECTURE §6ay |
| End meeting for everyone | Built | "You'll end the meeting for everyone."; lobby told "ended"; lock/spotlight/permissions reset; §6ay |
| Teams meeting layout: top bar, View / More / Leave menus | Built | Chat · People · Raise · React · View · More · Camera · Mic · Share · Leave ▾; phone splits into top bar + bottom dock; §6az |
| Views: Gallery, Speaker, Large gallery, Focus on content | Built | Gallery 9 + "+N more" (published, no package constant); Large gallery 49 from `minParticipantsInLargeGrid = 10`; self view floats |
| Chat bubbles | Built | `chatBubblesTimeout = 4000`, `chatBubbleFadeDuration = 300`; "Don't show chat bubbles" |
| Roles: organizer, co-organizer, presenter, attendee | Built | "Make a presenter" / "Make an attendee"; presenters exempt from attendee controls; organizer not changeable; §6az |
| People panel (Share invite, search, Waiting in lobby, Raised hands, In this meeting, Mute all) | Built | Teams' wording; person "..." menu |
| Raised hands in order; Lower all hands | Built | ranks to `raiseHandsOrderedRankLimit = 20` (client arrival order; late joiners see roster order) |
| Remove from meeting | Built | "You've been removed from this meeting"; they may rejoin, as in Teams |
| Meeting options (lobby, who can present, mic/camera for attendees, chat, reactions) | Built | "Allow reactions" is a new server-enforced control |
| Co-organizers chosen when scheduling | Built | From the invitees, at most 10, arrive with the role; same-organization check not built; §6be |
| Download attendance list | Built | Teams' Summary + Participants columns; no email column (directory rule); organizers and co-organizers present; §6ba |
| "5 minutes left in your meeting." | Built | Notice before a scheduled end; nothing ends; §6bb |
| 60-minute meeting time limit ("This meeting will end in 15 minutes") | N/A | Teams free-tier cap; Aether has none by decision (§6j, §6bb) |
| "Disable mic / camera" for one attendee | Built | Attendees only; by account across rejoin; lifted by "Make a presenter"; §6bd |
| Pop-out / picture-in-picture | Built | |
| Meeting notes | Built | Agenda / Notes / Follow-up tasks, organizer adds, everyone edits; item versions refuse a conflicting edit; §6bh. Character-level co-editing of one item not built |
| Recording / transcription | Built | More > Record and transcribe: Start/Stop recording (made on the recorder's computer, Teams' local-recording model; WebM), Start/Stop transcription, Show transcript, Download .vtt/.txt; organizers and presenters only; "Recording allowed" deployment ceiling with Teams' refusal wording; banner and red dot; audit log; each speaker asked before their speech becomes text; §6bj. Cloud recording linked in chat, .docx download, spoken-language choice, translated captions: not built |
| Breakout rooms | Built | Create (≤50, suggested 4 a room), Automatically/Manually, Assign, Shuffle, Rename (≤35), time limit, auto-move, return, Open/Close, announcement, Delete, managers join; ticketed entry; §6bc. Moving people between rooms while open: not built (close, reassign, reopen) |
| Whiteboard | Built | Share > Whiteboard on the main stage; pen, highlighter, 7 inks, 3 widths, eraser, sticky notes, Undo, Clear, "Turn off whiteboard"; live preview while drawing; late arrivals given the board; Aether's own board (Teams embeds Microsoft Whiteboard); §6bi. Kept after the meeting: not built |
| PowerPoint Live | Not built | |
| Q&A in meetings | Built | Ask (anonymous optional), Like, Reply; review before publishing, Publish/Dismiss, Pin, Mark as answered, Close Q&A; stored by room; §6bf |
| Call health | Built | Network / Audio / Video / Screen Sharing / devices, Teams' rows; WebRTC from getStats, WebSocket path says what it cannot measure; §6bg |
| Webinars, town halls, registration | Not built | Events tier |
| Audio conferencing / PSTN / voicemail / call queues | N/A | Telephony carrier integration |
| Custom backgrounds, video effects | Built | Camera > Video effects and pre-join "Background filters": None, Standard blur, Portrait blur, backgrounds, Add new (kept on the device, never uploaded), Soft focus, Adjust brightness, Mirror my video; segmentation on the device (MediaPipe); a background asked to be hidden is never sent unblurred; §6bk. Animated backgrounds, avatars: not built |
| Together mode | Built | View > Together mode; organizer applies it for everyone and picks a scene (Auditorium, Café, Conference room); everyone cut out by their own 64x36 outline (sent only when it changes) and seated in one scene; §6bo. Spatial audio, "only for you": not built |
| Meeting themes | Not built | |
| Live streaming (RTMP/SRT/NDI), eCDN | Not built | |

## Calendar and scheduling

| Feature | Status | Notes |
| --- | --- | --- |
| Schedule meetings, categories, invitations | Built | |
| Calendar: month grid, day agenda, week grid | Built | Day/Week switch, Today; the week scrolls sideways inside its own box on a phone |
| Recurring meetings | Built | Daily, weekly (chosen days), monthly, every N, until a date; the organiser's zone is kept across clock changes; one occurrence or the series can be cancelled (ARCHITECTURE 6bz) |
| RSVP (accept, decline) | Built | An answer is to the series; a moved meeting resets answers |
| Scheduling assistant | Built | "Who is free": busy bars for the people invited, spans only, and "Find the next free time" over five working days |
| Edit one occurrence of a series | Not yet | Cancel one, yes; a different time or title for one occurrence, not yet |
| Bookings, Outlook/Google add-ins | N/A | External calendars |

## Files

| Feature | Status | Notes |
| --- | --- | --- |
| Upload/download with membership checks, inline images/video/audio | Built | `nosniff`, server-named blobs |
| Co-authoring, versioning, SharePoint storage | N/A | |

## Administration and security

| Feature | Status | Notes |
| --- | --- | --- |
| Users, roles, policies, audit log | Built | Hash-chained audit log; console |
| Directory scope: contacts, organisation domains, everyone | Built | Public providers refused as org domains |
| Rate limits, CSRF, session cookies, argon2id | Built | Every endpoint has stranger/anon/CSRF harness cases |
| SSO (Google/Microsoft), guest access, external tenants | Not built | User asked for SSO last |
| Compliance holds, eDiscovery, sensitivity labels, DLP | Not built | |

## Platforms

| Feature | Status | Notes |
| --- | --- | --- |
| Web (Chrome/Edge), Windows app (WebView2) | Built | App: tray, badge, notifications, exact maximise, permissions once |
| Phone and tablet layouts | Built | Layout harness at 390, 820, 1280 and the shell's 940/1024; 44px touch targets; safe-area insets; keyboard resizes content |
| Native iOS/Android/macOS apps | Not built | The web client is the mobile client |

## Sources

- https://learn.microsoft.com/en-us/office365/servicedescriptions/teams-service-description
- https://learn.microsoft.com/en-us/microsoftteams/deploy-small-business
- https://learn.microsoft.com/en-us/microsoftteams/meetings-events-feature-comparison
- https://learn.microsoft.com/en-us/microsoftteams/limits-specifications-teams
- https://support.microsoft.com/en-us/office/chat-in-microsoft-teams-f3a917cb-1a83-42b2-a097-0678298703bb
