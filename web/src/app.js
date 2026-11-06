import {
  Op,
  Role,
  Writer,
  Reader,
  newDictPair,
  encodeBatch,
  splitBatch,
  encodeState,
  decodeState,
  encodeSpeakers,
  decodeSpeakers,
  encodePing,
  decodeRoster,
  decodeWelcome,
  encodeSDP,
  decodeSDP,
  encodeICE,
  decodeICE,
  decodeSlots,
  encodeViewport,
  encodeKeyframeReq,
  encodeMediaFrame,
  decodeMediaFrame,
  encodeBwe,
  RoleHost,
} from './wire.js';
import { MediaPlane } from './rtc.js';
import {
  session, login, logout, resume, signup, serverConfig,
  telemetry, scheduling, ApiError,
} from './api.js';
import {
  connectChatSocket,
  renderChatSurface,
  renderTeamsSurface,
  renderActivitySurface,
  refresh as refreshChat,
  frequentContacts,
  directWith,
  sendDirect,
  openChatWith,
  openSelfChat,
  matchConversations,
  openConversationById,
  openConversationAt,
} from './chat.js';
import { initConsole, openConsole, toast } from './console.js';
import { openPiP, closePiP, pipActive, pipSupported, markAutoPiP, autoPiPAvailable } from './pip.js';
import { revocations, chatBlocked } from './permissions.js';
import { decideMediaMode, chooseTransport } from './transport.js';
import { meetingPhase, isUpcoming, phaseLabel, endNoticeDelay } from './meeting.js';
import {
  rankPeople, mergePeople, highlight, createSearch, matchScore, NO_MATCH,
} from './peoplesearch.js';
import { initials, avatarAttrs, avatarHTML, toneOf } from './avatar.js';
import { attachPicker, isOnlyEmoji } from './emoji.js';
import { attachTitleBar, windowButtonsHTML, inDesktopShell } from './titlebar.js';
import {
  createRecogniser,
  captionsSupported,
  speechOnDevice,
  visibleLines,
} from './captions.js';
import { directory, messaging, qna, notes, transcript as transcriptApi } from './api.js';
import { EffectsPipeline, PRESETS as FX_PRESETS, MAX_IMAGES as FX_MAX_IMAGES, loadSettings as loadFx, saveSettings as saveFx, normalize as normalizeFx, needsPipeline as fxNeedsPipeline, listImages as fxListImages, addImage as fxAddImage, removeImage as fxRemoveImage, loadSegmenter } from './effects.js';
import { quantize as togQuantize, encodeMask as togEncode, MASK_W as TOG_W, MASK_H as TOG_H, MAX_MASK_BYTES as TOG_MAX, encodeSet as encodeTogSet, encodeOutline as encodeTogOutline, decodeTogether, seats as togSeats, SCENES as TOG_SCENES, PORTRAIT as TOG_PORTRAIT } from './together.js';
import { encodeSet as encodeRecSet, encodeCaption, decodeRecording, banner as recBanner, elapsed as recElapsed, pickMime, ChunkSink, fileName as recFileName, WHAT as REC } from './recording.js';
import { WSMediaSender, WSMediaReceiver, wsMediaSupported } from './wsmedia.js';
import { sendRing, answerRing, myStatus, statusLabel, isDND, quietHours, setQuietHours, inQuietHours, myPresenceDot, isAutoStatus, personPresence } from './chat.js';
import { presenceIcon } from './presence.js';
import { acquireMic } from './echo.js';
import { Denoiser, denoiseWanted, setDenoiseWanted, denoiseSupported } from './denoise.js';
import { plainText } from './format.js';
import { MAX_SPOTLIGHTS, spotlightChange, pickSpotlit } from './spotlight.js';
import { VIEWS, LARGE_GALLERY_MIN_PEOPLE, tileCap, bestGrid, galleryOrder } from './gallery.js';
import { health, show as showHealth } from './health.js';
import { BOARD_W, BOARD_H, MAX_POINTS, W, TOOL, PALETTE, INK, NOTE_COLORS, toBoard, encodeOpen, encodeStroke, encodeSimple, encodeNote, decodeWhiteboard } from './whiteboard.js';
import { K, MAX_ROOMS, MAX_NAME, MAX_ANNOUNCEMENT, suggestRooms, autoAssign, shuffle, remaining, decodeBreakout, encodeSet, encodeCmd, encodeAnnounce } from './breakout.js';

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

const $ = (id) => document.getElementById(id);

const esc = (s) =>
  String(s ?? '').replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]
  );

const pad2 = (n) => String(n).padStart(2, '0');

function clockHM(d) {
  return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

function dayLabel(d) {
  const today = new Date();
  const t = new Date(today.getFullYear(), today.getMonth(), today.getDate());
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const diff = Math.round((x - t) / 86400000);
  if (diff === 0) return 'Today';
  if (diff === 1) return 'Tomorrow';
  if (diff === -1) return 'Yesterday';
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

function elapsed(sinceMs) {
  const s = Math.max(0, Math.floor((Date.now() - sinceMs) / 1000));
  return `${pad2(Math.floor(s / 3600))}:${pad2(Math.floor((s % 3600) / 60))}:${pad2(s % 60)}`;
}

function readPref(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : v === 'true';
  } catch {
    return fallback;
  }
}

function writePref(key, value) {
  try {
    localStorage.setItem(key, String(value));
  } catch {
    // Private browsing. The preference simply does not persist.
  }
}

// ---------------------------------------------------------------------------
// Theme
// ---------------------------------------------------------------------------

const THEME_KEY = 'aether.theme';

function applyTheme(t) {
  if (t === 'system') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', t);

  const dark =
    t === 'dark' ||
    (t !== 'light' && matchMedia('(prefers-color-scheme: dark)').matches);

  // The icon names the mode you are in, not the one a click would take you
  // to. Both conventions exist; this is the one nobody has to have explained.
  document
    .querySelectorAll('.theme-icon')
    .forEach((u) => u.setAttribute('href', dark ? '#i-moon' : '#i-sun'));
}

function initTheme() {
  let t = 'system';
  try {
    t = localStorage.getItem(THEME_KEY) || 'system';
  } catch {
    // Storage disabled. The interface must work anyway.
  }
  applyTheme(t);

  const toggle = () => {
    const dark =
      document.documentElement.getAttribute('data-theme') === 'dark' ||
      (!document.documentElement.hasAttribute('data-theme') &&
        matchMedia('(prefers-color-scheme: dark)').matches);
    const next = dark ? 'light' : 'dark';
    applyTheme(next);
    writePref(THEME_KEY, next);
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch {}
  };
  toggleTheme = toggle;
  for (const id of ['btn-theme', 'btn-theme-auth', 'btn-theme-console']) {
    $(id)?.addEventListener('click', toggle);
  }
}

// Assigned by the theme setup above, so every caller switches the theme the
// same way rather than by synthesising a click on some button.
let toggleTheme = () => {};

// ---------------------------------------------------------------------------
// Client-side byte ledger
//
// The server keeps its own books; this one measures what the browser actually
// put on and took off the socket, so the panel can show both sides rather
// than asking the user to trust one of them.
// ---------------------------------------------------------------------------

const meter = {
  txBytes: 0,
  txMessages: 0,
  txFrames: 0,
  rxBytes: 0,
  rxMessages: 0,
  rxFrames: 0,
  jsonEquivalent: 0,
};

// Measured in engine/internal/wire/wire_test.go against the real shape a
// shipping competitor uses.
const JSON_BYTES_PER_EVENT = 172;
const WS_FRAMING_CLIENT = 2 + 4 + 22; // header + mask + TLS 1.3 record

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  ws: null,
  me: null,
  room: null,
  title: '',
  joinedAt: 0,
  dicts: newDictPair(),
  people: new Map(),
  localStream: null,
  screenStream: null,
  mic: true,
  cam: true,
  hand: false,
  sharing: false,
  // trackId of our own screen, announced to everyone in the state frame so
  // receivers can tell the presentation from the camera.
  screenTrackId: '',
  // trackId -> MediaStream, one per remote video track.
  videoStreams: new Map(),
  // WebRTC media slots (engine sfu/slots.go): the server re-points a fixed
  // pool of incoming tracks at whatever this receiver has on screen.
  // slotTracks: slot id -> MediaStreamTrack; slotMap: slot id -> what the
  // server says it carries; slotApplied: what is registered for it now.
  slotTracks: new Map(),
  slotMap: new Map(),
  slotApplied: new Map(),
  // Chosen input devices, remembered across pre-join adjustments.
  camDeviceId: '',
  micDeviceId: '',
  speakerId: '', // chosen output device for every remote voice ('' = system default)
  denoiser: null, // the microphone's noise suppressor graph (denoise.js), one at a time
  // People knocking, when this client is a host.
  waiting: [],
  // Whether this client is hosting, and what attendees may currently do.
  // Both come from the server; neither is decided here.
  isHost: false,

  // Media over the WebSocket: the fallback when no WebRTC path forms.
  // wsSender encodes our own tracks; wsReceivers is one decoder per remote
  // publisher, keyed by their id. wsMediaOn is the mode switch.
  wsMediaOn: false,
  wsSender: null,
  wsReceivers: new Map(),
  // publisher id -> the canvas track id carrying their WebSocket screen share
  wsScreenTracks: new Map(),
  // The room of the current 1:1 call, if this meeting is one. See joinMeeting.
  callRoom: null,

  // Who the chat panel is writing to: 'everyone' -- the meeting's own
  // broadcast, which ends with the meeting -- or an account id, which is the
  // permanent conversation with that person, the same one in the sidebar.
  //
  // Two different things behind one panel, deliberately. A remark to the room
  // belongs to the room and should not follow anybody home; a message to a
  // person is a message to a person, and losing it because a call ended is
  // the behaviour every other product gets wrong.
  chatTo: 'everyone',
  // Set when the person chose a chat target themselves. The automatic
  // choice (a two-person call opens on the two of you) must not undo a
  // deliberate switch to the call chat on the next roster redraw -- it did,
  // and a message meant to vanish with the call landed in the permanent
  // conversation.
  chatToPicked: false,
  // Set when chatTo is the automatic two-person choice, so it can be undone
  // when the meeting stops being two people (renderChatTo).
  chatToAuto: false,
  // account id -> { convId, messages, unread }
  dms: new Map(),
  // Messages said to the room while the panel was on a private thread.
  roomUnread: 0,
  // The meeting's own conversation, once the room has three or more people:
  // what "Everyone" means from then on, and what outlives the meeting.
  meetingConv: null, // { convId, messages } | null
  meetingConvTries: 0,
  perms: { mic: true, camera: true, share: true, chat: true, reactions: true },
  // Your meeting role (wire Role): attendee, presenter or host. Presenters
  // and hosts are not bound by the attendee controls.
  myRole: 1,
  // A host's "Disable mic" / "Disable camera" on you.
  blocked: { mic: false, cam: false },
  // Raised hands in the order they went up: id -> sequence. Teams numbers
  // them, up to `raiseHandsOrderedRankLimit = 20`.
  handOrder: new Map(),
  handSeq: 0,
  captions: false,
  // participant id -> { name, text, at }. What the caption bar is showing.
  captionLines: new Map(),
  recogniser: null,
  // Recording, transcription, captions (recording.js): the room's state as
  // the server last said it; this meeting session (the room's epoch); this
  // speaker's consent to have their speech turned into text (null: not yet
  // asked); the utterance being spoken; the recording made on this machine.
  // Video effects (effects.js): the chosen effects, kept on this device; the
  // running pipeline, if any; your own background images, decoded.
  fx: loadFx(),
  fxPipe: null,
  fxStatus: 'none',
  fxImages: new Map(),
  // Together mode (together.js): the room's state, each person's latest
  // outline, and this machine's own.
  together: { on: false, scene: 0, by: '' },
  outlines: new Map(),
  myOutline: null,
  rec: { recording: false, transcribing: false, captions: false, recBy: '', recSince: 0, trBy: '', trSince: 0 },
  session: 0,
  speechConsent: null,
  speechLocal: false,
  utt: 1,
  recorder: null,
  recPending: null,
  recBannerHidden: false,
  // Reactions in flight, so a burst of them does not stack into a wall.
  reactionAt: 0,
  outbox: [],
  outTimer: null,
  audioCtx: null,
  analyser: null,
  lastEnergySent: 0,
  media: null,
  remote: new Map(),
  trackOwner: new Map(),
  pinned: null,
  // Who the host has put on everybody's stage, as the server last sent it.
  spotlights: [],
  // Teams' View menu: 'gallery' (its default), 'speaker', 'large', 'focus'.
  layout: (() => { try { return localStorage.getItem('aether.layout') || 'gallery'; } catch { return 'gallery'; } })(),
  // Chat bubbles over the meeting while the chat is closed; on by default, as in Teams.
  bubbles: (() => { try { return localStorage.getItem('aether.bubbles') !== '0'; } catch { return true; } })(),
  sideView: '',
  // Breakout rooms: the managers' view of them (STATE), and, inside a room,
  // what it is (INFO). ticket is what this connection presents to get in.
  breakouts: null,
  bo: null,
  ticket: '',
  autoJoin: false,
  boWarned: false,
  lastViewport: '',
  chat: [],
  meetings: [],
  calDate: new Date(),
  calMonth: new Date(),
  catsOff: new Set(),
  homeTab: 'meet',
  // Mirroring the camera self-view is on by default because an unmirrored
  // reflection of yourself reads as wrong to almost everyone. It is a
  // preference rather than a rule, because "almost" is not "all".
  mirrorSelf: readPref('aether.mirror', true),
};

// ---------------------------------------------------------------------------
// Link readout
//
// The number in the header: what this connection is carrying right now, up
// and down, and over what. Measured from the same counters the wire
// measurements in the architecture document use -- bytes actually handed to
// the socket, framing included -- so it is a reading, not an estimate. Once a
// second, because the eye cannot follow faster and the number should not
// flicker. Also shows queued bytes when the socket is backed up, which is the
// only honest way to say "your link is the bottleneck".
// ---------------------------------------------------------------------------

let rateTimer = null;
function startRateReadout(ws) {
  clearInterval(rateTimer);
  let lastTx = meter.txBytes;
  let lastRx = meter.rxBytes;
  let lastAt = performance.now();
  const fmt = (bps) =>
    bps >= 1e6 ? `${(bps / 1e6).toFixed(1)} Mbps` : `${Math.round(bps / 1e3)} kbps`;
  rateTimer = setInterval(() => {
    const el = $('rate-label');
    if (!el) return;
    const now = performance.now();
    const dt = Math.max(0.001, (now - lastAt) / 1000);
    const up = ((meter.txBytes - lastTx) * 8) / dt;
    const down = ((meter.rxBytes - lastRx) * 8) / dt;
    lastTx = meter.txBytes;
    lastRx = meter.rxBytes;
    lastAt = now;
    const path = state.wsMediaOn ? 'WS' : 'RTC';
    const queued = ws.bufferedAmount;
    const backlog = queued > 64 * 1024 ? ` · ${Math.round(queued / 1024)} KB queued` : '';
    el.textContent = `↑ ${fmt(up)}  ↓ ${fmt(down)} · ${path}${backlog}`;
    el.title = 'Bytes on the wire this second, framing included. WS = media over the WebSocket.';
  }, 1000);
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// Fifteen milliseconds is below the threshold at which a UI state change
// reads as anything other than instant, and it lets a burst of frames --
// which is what every user action actually produces -- ride one WebSocket
// frame and one TLS record instead of three or four.
const COALESCE_MS = 15;

function send(encode) {
  const w = new Writer(state.dicts.tx);
  encode(w);
  state.outbox.push(w.bytes());
  meter.jsonEquivalent += JSON_BYTES_PER_EVENT + WS_FRAMING_CLIENT;
  if (!state.outTimer) state.outTimer = setTimeout(flush, COALESCE_MS);
}

function flush() {
  state.outTimer = null;
  if (!state.outbox.length || state.ws?.readyState !== WebSocket.OPEN) {
    state.outbox = [];
    return;
  }
  const frames = state.outbox;
  state.outbox = [];
  const payload = encodeBatch(frames);
  state.ws.send(payload);
  meter.txBytes += payload.length + WS_FRAMING_CLIENT;
  meter.txMessages += 1;
  meter.txFrames += frames.length;
}

function connect(room, name) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(
    `${proto}://${location.host}/rtc?room=${encodeURIComponent(room)}&name=${encodeURIComponent(name)}${
      state.ticket ? `&bt=${encodeURIComponent(state.ticket)}` : ''}`
  );
  ws.binaryType = 'arraybuffer';
  state.ws = ws;

  ws.onopen = () => {
    setLink('ok');
    // A one byte heartbeat. Not a JSON object with a timestamp.
    setInterval(() => {
      if (ws.readyState === WebSocket.OPEN) send(encodePing);
    }, 5000);
    startRateReadout(ws);
  };

  ws.onmessage = (ev) => {
    const bytes = new Uint8Array(ev.data);
    meter.rxBytes += bytes.length + 2 + 22; // server frames are unmasked
    meter.rxMessages += 1;

    const r = new Reader(bytes, state.dicts.rx);
    const h = r.header();
    if (!h) return;
    if (h.op === Op.Batch) {
      splitBatch(r, (f) => {
        const fr = new Reader(f, state.dicts.rx);
        const fh = fr.header();
        if (fh) {
          meter.rxFrames += 1;
          handle(fh, fr);
        }
      });
    } else {
      meter.rxFrames += 1;
      handle(h, r);
    }
  };

  ws.onclose = () => setLink('bad');
  ws.onerror = () => setLink('warn');
}

function setLink(kind) {
  const d = $('link-dot');
  if (d) d.className = 'dot' + (kind === 'ok' ? '' : ' ' + kind);
}

// ---------------------------------------------------------------------------
// Protocol
// ---------------------------------------------------------------------------

function handle(h, r) {
  switch (h.op) {
    case Op.Welcome: {
      const w = decodeWelcome(r);
      state.me = w.id;
      state.room = w.room;
      state.session = Number(w.epoch);
      break;
    }
    case Op.SameDevice: {
      // Somebody in this meeting connected from this same address: almost
      // always a second window or the app on this very machine. Each side
      // plays the other's copy of a voice into the same room, and neither
      // echo canceller knows the other's output -- the one echo no server
      // can remove. Said once per meeting, with the way out.
      const others = Number(r.uvarint());
      if (!state.sameDeviceTold) {
        state.sameDeviceTold = true;
        toast(
          others > 1
            ? `${others} other participants are joined from this same device or network. To avoid hearing yourself, mute all but one microphone, or use headphones.`
            : 'Someone else is in this meeting from this same device or network. To avoid hearing yourself, mute one of the microphones, or use headphones.',
          'bad'
        );
        try {
          document.documentElement.dataset.sameDevice = String(others);
        } catch {}
      }
      break;
    }
    case Op.KeyframeReq: {
      // Somebody watching our socket stream needs a keyframe.
      r.str();
      state.wsSender?.keyframe();
      break;
    }
    case Op.Roster: {
      const e = decodeRoster(r);
      if (!e.present) {
        const leaverName = state.people.get(e.id)?.name;
        // Writing to somebody who left falls back to the room. Their
        // conversation is still there in the sidebar; it is just no longer
        // one of the people in front of you.
        if (state.chatTo && state.chatTo === state.people.get(e.id)?.userId) {
          state.chatTo = 'everyone';
          state.chatToPicked = false;
          state.chatToAuto = false;
        }
        state.people.delete(e.id);
        renderChatTo();
        // Their fallback decoder, if they were arriving over the WebSocket.
        dropWSMediaFor(e.id);
        state.remote.delete(e.id);
        // A 1:1 call with nobody left on the other end is over. Say so and
        // leave, rather than leaving the remaining person in an empty room
        // wondering whether the other one is coming back.
        const othersLeft = [...state.people.values()].filter((p) => p.id !== state.me).length;
        if (state.callRoom === state.room && othersLeft === 0) {
          toast(`Call ended${leaverName ? ` — ${leaverName} left` : ''}.`);
          setTimeout(() => leaveMeeting(), 1500);
        }
        // Their audio element must go with them, or it holds a dead stream
        // for the life of the meeting.
        syncAudioSinks();
      } else if (!state.people.has(e.id)) {
        togSent = null; // an arrival has nobody's outline yet
        state.people.set(e.id, {
          id: e.id,
          name: e.name,
          role: e.role,
          organizer: !!e.organizer,
          userId: e.userId || '',
          state: {},
          energy: 0,
        });
        renderChatTo();
        ensureMeetingChat();
        // Somebody new can only decode from a keyframe. Force one now rather
        // than making them wait out the 2 s cadence.
        state.wsSender?.keyframe();
      }
      // Hosting is the server's decision, arriving in the roster. Deciding it
      // here -- first to join, say -- would disagree with the server the
      // moment anything reassigned it.
      if (e.id === state.me) {
        state.myRole = e.role;
        if (e.role >= RoleHost) state.isHost = true;
      }
      renderStage();
      renderPeople();
      break;
    }
    case Op.StateDelta: {
      const s = decodeState(r);
      const p = state.people.get(s.participant);
      if (p) {
        // Sharing has stopped: drop the screen track we knew about. The
        // delta does not carry screenTrack when sharing is false, so
        // without this the old id lingers and the still-open (muted) screen
        // track is reclassified as their camera -- a frozen screen shown as
        // their face. Same for the WebSocket-carried screen.
        const wasSharing = !!p.state?.sharing;
        if (wasSharing && !s.sharing) {
          const dead = p.state.screenTrack;
          if (dead) {
            state.videoStreams.delete(dead);
            state.trackOwner.delete(dead);
          }
          dropWSScreenFor(p.id);
          if (state.pinned === screenIdFor(p.id)) state.pinned = null;
        }
        if (s.handRaised && !p.state?.handRaised) state.handOrder.set(p.id, ++state.handSeq);
        if (!s.handRaised) state.handOrder.delete(p.id);
        p.state = s;
        renderStage();
        renderPeople();
      }
      break;
    }
    case Op.ScreenShare: {
      // The presenter changed (the server holds one presenter per room;
      // room/presenter.go). Whoever was presenting is not any more: drop
      // their screen from the stage now rather than waiting for their own
      // state update, and if it was us, stop capturing.
      const stopped = r.str();
      const by = r.str();
      if (r.err) return;
      const byName = by === state.me ? 'You' : state.people.get(by)?.name || 'Someone';
      const q = state.people.get(stopped);
      if (q?.state?.sharing) {
        const dead = q.state.screenTrack;
        if (dead) {
          state.videoStreams.delete(dead);
          state.trackOwner.delete(dead);
        }
        dropWSScreenFor(stopped);
        if (state.pinned === screenIdFor(stopped)) state.pinned = null;
        q.state = { ...q.state, sharing: false, screenTrack: '' };
      }
      if (stopped === state.me && state.sharing) {
        queueMedia(stopShare);
        toast(`${byName} started presenting, so your presentation stopped.`);
      }
      renderStage();
      renderPeople();
      break;
    }
    case Op.LobbyStatus: {
      const status = r.str();
      const message = r.str();
      if (r.err) return;
      showLobby(status, message);
      break;
    }
    case Op.LobbyList: {
      const n = Number(r.uvarint());
      const list = [];
      // Bounded against what the frame could possibly contain, so a bad
      // length cannot spin the browser allocating an enormous array.
      for (let i = 0; i < n && i < 512; i++) {
        const id = r.str();
        const name = r.str();
        const authenticated = r.bool();
        r.bits(7);
        if (r.err) return;
        list.push({ id, name, authenticated });
      }
      if (r.err) return;
      renderAdmitList(list);
      break;
    }
    case Op.Permissions: {
      const mic = r.bool();
      const camera = r.bool();
      const share = r.bool();
      const chat = r.bool();
      // Inverted on the wire: an old zero means reactions are allowed.
      const reactions = !r.bool();
      r.bits(3);
      if (r.err) return;
      state.perms = { mic, camera, share, chat, reactions };
      enforcePermissions();
      syncControls();
      renderPeople();
      break;
    }
    case Op.MuteAll: {
      // The server has muted us. Applied rather than requested: it arrives
      // only for people the host actually muted, and the server has already
      // stopped forwarding.
      if (state.mic) {
        state.mic = false;
        applyMicEnabled();
        queueMic(reconcileMic);
        toast('The host muted everyone.');
      }
      syncControls();
      renderPeople();
      break;
    }
    case Op.MeetingPolicy: {
      const policy = r.str();
      const locked = r.bool();
      r.bits(7);
      if (r.err) return;
      state.policy = policy;
      state.locked = locked;
      syncControls();
      renderPeople();
      break;
    }
    case Op.Spotlight: {
      // Always the whole list. Every id is read even past any sane count:
      // each string may teach the receive dictionary, and skipping one would
      // misread every later reference to it.
      const n = Number(r.uvarint());
      const ids = [];
      for (let i = 0; i < n && i < 512 && !r.err; i++) ids.push(r.str());
      if (r.err) return;
      const nameOf = (id) => state.people.get(id)?.name ?? 'Someone';
      const { notices, added } = spotlightChange(state.spotlights, ids, state.me, nameOf);
      state.spotlights = ids;
      // A spotlight is "pin for everyone": a new one replaces your own pin so
      // you see what the host wants seen. You can pin someone else after.
      if (added) state.pinned = null;
      for (const msg of notices) toast(msg);
      renderStage();
      renderPeople();
      reportViewport(true);
      break;
    }
    case Op.EndMeeting:
      meetingEnded();
      break;
    case Op.Whiteboard: {
      const msg = decodeWhiteboard(r);
      if (!r.err && msg) onWhiteboard(msg);
      break;
    }
    case Op.Notes: {
      const body = r.blob();
      if (!r.err) onNotes(body);
      break;
    }
    case Op.QnA: {
      const body = r.blob();
      if (!r.err) onQnA(body);
      break;
    }
    case Op.Breakout: {
      const msg = decodeBreakout(r);
      if (!r.err && msg) onBreakout(msg);
      break;
    }
    case Op.SetRole: {
      const id = r.str();
      const role = r.bits(2);
      const organizer = r.bits(1) === 1;
      r.bits(5);
      if (r.err) return;
      const p = state.people.get(id);
      if (p) {
        p.role = role;
        p.organizer = organizer;
        if (role >= Role.Moderator) p.blocked = { mic: false, cam: false };
      }
      if (id === state.me && role >= Role.Moderator) state.blocked = { mic: false, cam: false };
      if (id === state.me) {
        const was = state.myRole;
        state.myRole = role;
        // Hosting follows the role: this is what lets the person a leaving
        // host handed over to actually use the controls.
        state.isHost = role >= RoleHost;
        if (was !== role) toast(role === Role.Member ? 'Your role has been changed to attendee' : 'Your role has been changed');
        enforcePermissions(true);
      }
      syncControls();
      renderPeople();
      renderStage();
      break;
    }
    case Op.Remove:
      meetingEnded('removed');
      break;
    case Op.Block: {
      const id = r.str();
      const mic = r.bool();
      const cam = r.bool();
      r.bits(6);
      if (r.err) return;
      const p = state.people.get(id);
      if (p) p.blocked = { mic, cam };
      if (id === state.me) {
        const was = state.blocked;
        state.blocked = { mic, cam };
        // Teams' words, to the person it happened to.
        if (mic !== was.mic) toast(mic ? 'Your mic has been disabled' : "Your mic has been enabled. Unmute if you'd like to speak.");
        if (cam !== was.cam) toast(cam ? 'Your camera has been disabled' : 'Your camera has been enabled');
        enforcePermissions();
      }
      syncControls();
      renderPeople();
      break;
    }
    case Op.LowerHands:
      if (state.hand) {
        state.hand = false;
        pushState();
        toast('Your hand was lowered');
        syncControls();
        renderPeople();
      }
      break;
    case Op.Speakers: {
      const list = decodeSpeakers(r);
      for (const p of state.people.values()) {
        p.energy = 0;
        p.rank = -1;
      }
      list.forEach((s, i) => {
        const p = state.people.get(s.participant);
        if (p) {
          p.energy = s.energy;
          p.rank = i;
        }
      });
      renderStage();
      break;
    }
    case Op.Chat: {
      const who = r.str();
      const body = new TextDecoder().decode(r.blob());
      addChat(who, body);
      break;
    }
    case Op.Together: {
      const m = decodeTogether(r);
      if (m?.kind === 'state') onTogetherState(m);
      else if (m?.kind === 'mask') {
        state.outlines.set(m.from, m.bits);
        // How many arrived, for the live checks (a still person sends few).
        document.documentElement.dataset.togOutlines = String(Number(document.documentElement.dataset.togOutlines || 0) + 1);
      }
      break;
    }
    case Op.Recording: {
      const m = decodeRecording(r);
      if (m?.kind === 'state') onRecState(m);
      else if (m?.kind === 'caption') onCaption(m);
      else if (m?.kind === 'refused') onRecRefused(m);
      break;
    }
    case Op.Reaction: {
      const who = r.str();
      const emoji = r.str();
      if (r.err) break;
      showReaction(who, emoji);
      break;
    }
    case Op.MediaFrame: {
      const f = decodeMediaFrame(r);
      if (r.err) return;
      onWSMediaFrame(f);
      break;
    }
    case Op.Sdp:
      state.media?.handleSDP(decodeSDP(r)).catch((e) => console.warn('sdp:', e.message));
      break;
    case Op.Ice:
      state.media?.handleICE(decodeICE(r));
      break;
    case Op.Slots: {
      const entries = decodeSlots(r);
      if (r.err) return;
      const live = new Set();
      for (const e of entries) {
        live.add(e.slot);
        state.slotMap.set(e.slot, e);
        applySlot(e.slot);
      }
      for (const id of [...state.slotMap.keys()]) {
        if (!live.has(id)) {
          state.slotMap.delete(id);
          applySlot(id);
        }
      }
      renderStage();
      renderPeople();
      syncAudioSinks();
      reportViewport(true);
      break;
    }
    default:
      // Unknown opcodes are ignored. A client talking to a newer server
      // degrades rather than disconnecting.
      break;
  }
}

function pushState() {
  send((w) =>
    encodeState(w, {
      participant: state.me ?? '',
      micMuted: !state.mic,
      camOff: !state.cam,
      sharing: state.sharing,
      screenTrack: state.screenTrackId,
      handRaised: state.hand,
      speaking: false,
      recording: false,
      role: Role.Member,
    })
  );
  syncSpeech();
}

// ---------------------------------------------------------------------------
// Media
// ---------------------------------------------------------------------------

// The microphone as published: the device's track through the neural noise
// suppressor (denoise.js) when it is wanted and the platform has it; the
// device's track itself otherwise. One suppressor at a time -- a new
// microphone replaces the old one's graph and stops the old device. What
// was got is recorded on <html data-denoise> for the harness.
async function micTrackFor(raw) {
  state.denoiser?.stop();
  state.denoiser = null;
  if (!denoiseSupported()) {
    document.documentElement.dataset.denoise = 'off';
    document.documentElement.dataset.leveler = 'off';
    return raw;
  }
  const suppress = denoiseWanted();
  try {
    const d = await Denoiser.wrap(raw, { suppress });
    state.denoiser = d;
    document.documentElement.dataset.denoise = suppress ? (d.simd ? 'simd' : 'on') : 'off';
    document.documentElement.dataset.leveler = 'on';
    return d.track;
  } catch (e) {
    // Strong failed (a policy blocking WebAssembly, an old engine): try the
    // leveler alone before giving up on processing altogether.
    if (suppress) {
      try {
        const d = await Denoiser.wrap(raw, { suppress: false });
        state.denoiser = d;
        document.documentElement.dataset.denoise = 'failed';
        document.documentElement.dataset.leveler = 'on';
        console.warn('noise suppression unavailable, leveling only:', e?.message);
        return d.track;
      } catch {}
    }
    console.warn('voice processing unavailable, using the microphone as is:', e?.message);
    document.documentElement.dataset.denoise = 'failed';
    document.documentElement.dataset.leveler = 'off';
    return raw;
  }
}

async function startMedia() {
  // Audio and video are acquired SEPARATELY, and this is the whole point of
  // the function.
  //
  // getUserMedia({audio, video}) is one promise over two devices: if either
  // one fails, the whole call rejects and you get neither. A camera already
  // in use by another application is enough -- and on a laptop that is the
  // normal state of affairs, not an edge case. The symptom is that the
  // microphone stops working for a reason that has nothing to do with the
  // microphone, and because nothing is then published, screen sharing
  // silently does nothing either.
  //
  // Two calls, two failures, two independent outcomes.
  state.localStream = new MediaStream();

  try {
    // Echo cancellation as a requirement, best effort as a fallback, and a
    // word to the person when the device cannot cancel: see echo.js.
    const { track: raw, report } = await acquireMic((c) => navigator.mediaDevices.getUserMedia(c), state.micDeviceId || '');
    noteMicProcessing(report);
    if (raw) state.localStream.addTrack(await micTrackFor(raw));
  } catch (err) {
    console.warn('microphone unavailable:', err.name, err.message);
    state.mic = false;
    toast('Microphone unavailable. Others will not hear you.', 'bad');
  }

  try {
    const v = await navigator.mediaDevices.getUserMedia({
      video: { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30 } },
    });
    for (const t of v.getVideoTracks()) state.localStream.addTrack(await camTrackFor(t));
  } catch (err) {
    console.warn('camera unavailable:', err.name, err.message);
    state.cam = false;
    $('stage-msg').textContent = 'Camera unavailable — you can still be heard, and share your screen';
  }

  if (!state.localStream.getTracks().length) {
    state.localStream = null;
    $('stage-msg').textContent =
      'No camera or microphone available — you can still watch, chat and share your screen';
    return;
  }

  startLevelMeter();
  renderStage();
}

// The level meter runs locally and reports a quantised energy to the server.
// Doing it here rather than server-side is cheaper and more accurate, and it
// is the only approach that still works when the media is end-to-end
// encrypted and the server genuinely cannot listen.
// What the microphone's processing turned out to be, for the person (a
// toast once per session when echo cancellation is off) and for the live
// check (a data attribute on the document).
let micProcessingToldOnce = false;
function noteMicProcessing(report) {
  try {
    document.documentElement.dataset.micAec = report.ok ? 'on' : 'off';
    if (report.off.length) document.documentElement.dataset.micOff = report.off.join(',');
    else delete document.documentElement.dataset.micOff;
  } catch {}
  if (!report.ok) {
    console.warn('microphone processing:', report.message);
    if (!micProcessingToldOnce) {
      micProcessingToldOnce = true;
      toast(report.message, 'bad');
    }
  }
}

function startLevelMeter() {
  const track = state.localStream?.getAudioTracks()[0];
  if (!track) return;
  state.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  const src = state.audioCtx.createMediaStreamSource(state.localStream);
  const analyser = state.audioCtx.createAnalyser();
  analyser.fftSize = 512;
  analyser.smoothingTimeConstant = 0.6;
  src.connect(analyser);
  state.analyser = analyser;

  const data = new Uint8Array(analyser.frequencyBinCount);
  const tick = () => {
    analyser.getByteTimeDomainData(data);
    let sum = 0;
    for (let i = 0; i < data.length; i++) {
      const v = (data[i] - 128) / 128;
      sum += v * v;
    }
    const energy = Math.min(1, Math.sqrt(sum / data.length) * 3.2);

    // Report only on a material change, and never faster than 4 Hz. A level
    // meter that streams a float at 60 Hz is a surprisingly common way to
    // spend a hundred kilobits per second saying almost nothing.
    const now = performance.now();
    if (
      state.mic &&
      now - state.lastEnergySent > 250 &&
      Math.abs(energy - (state.lastEnergy ?? 0)) > 0.05
    ) {
      state.lastEnergySent = now;
      state.lastEnergy = energy;
      send((w) => encodeSpeakers(w, [{ participant: state.me ?? '', energy }]));
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

// Remote audio is played by dedicated <audio> elements, one per participant,
// and never by the video tiles.
//
// Decoupling them is not tidiness, it is the difference between hearing the
// meeting and not. The video elements exist to show pictures, and their
// layout changes constantly: a tile is on stage, then in the strip, then
// scrolled out of view, then hidden entirely because somebody else started
// talking. If audio rides those elements it follows the same fate, and the
// symptoms are exactly what they look like -- everyone in the strip is
// inaudible because strip tiles are muted, and the one person on stage is
// audible only while they stay there.
//
// Worse, when the stage shows YOU, an unmuted stage element plays your own
// microphone back at you. That is a feedback loop, and it is also why the
// element must be muted rather than merely quiet: Chrome refuses to autoplay
// unmuted media without a user gesture, so an unmuted <video autoplay> can
// silently never start at all, which reads as "the video is broken".
//
// So: every <video> in this application is muted, always. Audio is played
// here, for every remote participant, regardless of what the layout is doing.
function syncAudioSinks() {
  const host = $('audio-sinks');
  if (!host) return;

  const wanted = new Set();
  for (const [publisher, stream] of state.remote) {
    // Never play your own audio back to yourself.
    if (publisher === state.me) continue;
    if (!stream.getAudioTracks().length) continue;
    wanted.add(publisher);

    let el = host.querySelector(`audio[data-id="${CSS.escape(publisher)}"]`);
    if (!el) {
      el = document.createElement('audio');
      el.dataset.id = publisher;
      el.autoplay = true;
      host.appendChild(el);
    }
    if (el.srcObject !== stream) el.srcObject = stream;
    applySpeaker(el);
    // Autoplay can still be refused if the user has not interacted with the
    // page yet. Joining a meeting is itself a gesture, so this normally
    // succeeds; when it does not, the rejection is reported rather than
    // swallowed, because silent audio with no explanation is the worst
    // possible failure here.
    el.play?.().catch((err) => {
      console.warn('audio playback blocked for', publisher, err?.name);
      toast('Click anywhere to enable audio.', 'bad');
    });
  }

  for (const el of host.querySelectorAll('audio')) {
    if (!wanted.has(el.dataset.id)) {
      el.srcObject = null;
      el.remove();
    }
  }
}

// The stream id every slot track carries (engine sfu.SlotStream).
const SLOT_STREAM = 'slots';

// Register what a slot carries, as if it were that publisher's own track:
// video under the published track id (so screen detection, viewport reports
// and keyframe requests name the same track the server does), audio as the
// publisher's stream for the audio sinks. A slot re-pointed elsewhere first
// drops what it was registered as. Needs both the track and its map entry,
// which can arrive in either order.
function applySlot(slotId) {
  const track = state.slotTracks.get(slotId);
  const want = state.slotMap.get(slotId);
  const had = state.slotApplied.get(slotId);
  const same = had && track && want && want.publisher && had.track === want.track;
  if (had && !same) {
    state.trackOwner.delete(had.track);
    if (had.kind === 'video') state.videoStreams.delete(had.track);
    else if (state.remote.get(had.publisher) === had.stream) state.remote.delete(had.publisher);
    state.slotApplied.delete(slotId);
  }
  if (same || !track || !want || !want.publisher) return;
  const stream = new MediaStream([track]);
  state.trackOwner.set(want.track, want.publisher);
  if (track.kind === 'video') state.videoStreams.set(want.track, stream);
  else state.remote.set(want.publisher, stream);
  state.slotApplied.set(slotId, { publisher: want.publisher, track: want.track, kind: track.kind, stream });
}

async function startMediaPlane() {
  state.media = new MediaPlane(
    {
      sendSDP: (dir, isOffer, body) => send((w) => encodeSDP(w, { dir, isOffer, body })),
      sendICE: (dir, candidate) => send((w) => encodeICE(w, { dir, candidate })),
    },
    {
      onRemoteTrack: (track, stream, publisher) => {
        if (!stream) return;
        // A slot: what it carries is named by the slot map, not its stream.
        if (stream.id === SLOT_STREAM) {
          state.slotTracks.set(track.id, track);
          track.onended = () => {
            state.slotTracks.delete(track.id);
            applySlot(track.id);
            renderStage();
            renderPeople();
            syncAudioSinks();
          };
          applySlot(track.id);
          renderStage();
          renderPeople();
          syncAudioSinks();
          reportViewport(true);
          return;
        }
        state.remote.set(publisher, stream);
        state.trackOwner.set(track.id, publisher);

        // A publisher who is presenting sends two video tracks, and the
        // server puts both in one MediaStream because they share a stream
        // id. Rendering them apart needs one stream per track, so each video
        // track gets its own wrapper here.
        if (track.kind === 'video') {
          state.videoStreams.set(track.id, new MediaStream([track]));
        }

        track.onended = () => {
          state.trackOwner.delete(track.id);
          state.videoStreams.delete(track.id);
          renderStage();
          renderPeople(); // presentations are people-list entries too
          syncAudioSinks();
        };
        renderStage();
        renderPeople(); // presentations are people-list entries too
        syncAudioSinks();
        reportViewport(true);
      },
      onState: (which, st) => {
        if (st === 'failed' || st === 'disconnected') setLink('warn');
        else if (which === 'subscriber' && st === 'connected') setLink('ok');
        // A terminal failure on either leg: stop waiting for WebRTC.
        if (st === 'failed') enableWSMedia(`${which} ICE failed`);
      },
      // The subscriber connection stalled and cannot restart itself (the
      // server offers it). Asking for keyframes on return of service, plus a
      // viewport refresh, prompts the server to resume sending.
      onSubscriberStalled: () => {
        reportViewport(true);
        requestKeyframes();
      },
    }
  );
  if (chooseTransport(location.search, wsMediaSupported()) === 'ws') {
    // The reliable path, from the start. The WebRTC plane above exists so
    // that every `state.media?.` reference stays valid, but it is never
    // started: no ICE, no relay allocation, no publisher that can fail.
    // A browser without WebCodecs (Safari, iOS, Firefox) does not get here:
    // it takes the WebRTC path below rather than a toast and no media.
    await enableWSMedia('default');
    return;
  }

  await state.media.start(await iceServers());
  // Publish whatever was acquired, even if that is audio alone. Waiting for a
  // complete set is how a working microphone ends up silent because a camera
  // was busy.
  if (state.localStream?.getTracks().length) {
    await state.media.publish(state.localStream);
  }

  // WebRTC mode: arm the automatic fallback. Checked against live state at
  // fire time, so a connection that comes up in the meantime cancels it.
  {
    const startedAt = Date.now();
    const connected = (pc) =>
      pc && (pc.iceConnectionState === 'connected' || pc.iceConnectionState === 'completed');
    const check = () => {
      if (state.wsMediaOn) return;
      const sub = state.media?.sub;
      const pub = state.media?.pub;

      // Decisive: the publisher failed outright.
      if (pub && pub.iceConnectionState === 'failed') {
        enableWSMedia('publisher ICE failed');
        return;
      }
      // We are publishing something and the publisher still has not connected
      // after the window: do not wait the ~30 s it takes ICE to say "failed".
      // A working network connects the publisher in well under a second, so
      // this never fires on one that works.
      const publishing = !!state.localStream?.getTracks().length;
      if (publishing && pub && !connected(pub) && Date.now() - startedAt >= WS_MEDIA_FALLBACK_MS) {
        enableWSMedia(`publisher not connected after ${WS_MEDIA_FALLBACK_MS} ms`);
        return;
      }
      // We were offered a subscription and it will not connect: peers cannot
      // reach us directly.
      if (sub?.remoteDescription && !connected(sub) && Date.now() - startedAt >= WS_MEDIA_FALLBACK_MS) {
        enableWSMedia('subscriber offered but not connecting');
        return;
      }
      setTimeout(check, 1500);
    };
    setTimeout(check, WS_MEDIA_FALLBACK_MS);
  }
}

// ---------------------------------------------------------------------------
// Media over the WebSocket
//
// The guaranteed path. See wsmedia.js for why it exists and how it works. Here
// is only the wiring: when to switch it on, and how a decoded remote stream
// is handed to the same rendering that WebRTC streams use, so tiles, stage,
// mirroring and audio sinks need no knowledge of which path fed them.
// ---------------------------------------------------------------------------

// How long the WebRTC subscriber may sit unconnected after join before we
// stop waiting for a network that is not going to deliver. ICE's own failure
// timeout is longer than a person will wait; on the campus network it never
// fired at all -- connections sat in "checking" until the participant left.
const WS_MEDIA_FALLBACK_MS = 6000;

// Which transport carries our media (transport.js has the decision and its
// history).
//
//   'webrtc' -- the default. RTP over UDP with Opus in-band FEC and the
//               browser's concealment: a lost packet is one 20 ms frame
//               reconstructed, not a stall. The socket fallback below is
//               armed for networks that do not carry UDP.
//
//   'ws'     -- opt in with ?media=ws (or the old ?wsmedia=1). Media rides
//               the signalling WebSocket from the first frame: no ICE, no
//               detection window, and every stall of the TCP stream is a
//               hole in the voice. Measured against WebRTC on a speech
//               sample under Wi-Fi-like stalls; see ARCHITECTURE.md.
function mediaMode() {
  return decideMediaMode(location.search);
}

async function enableWSMedia(reason) {
  if (state.wsMediaOn) return;
  if (!wsMediaSupported()) {
    toast('This browser cannot use the fallback media path; the connection may not carry video.');
    return;
  }
  state.wsMediaOn = true;
  console.info('media: WebSocket media on:', reason);
  if (reason !== 'default') toast('Weak connection — switching to the reliable path.');

  // Stop the WebRTC publisher cold. Left running, it keeps trying and its
  // half-dead senders feed the far side a torn stream alongside the socket
  // frames -- which is exactly the audio tearing that was reported.
  state.media?.abandonPublisher();

  // The sender reads socket pressure directly: bytes accepted by ws.send()
  // and not yet handed to the network. That is the buffer that grew without
  // bound; now it is the signal that stops it growing.
  state.wsSender = new WSMediaSender(send, encodeMediaFrame, () => state.ws?.bufferedAmount ?? 0);
  if (state.localStream?.getTracks().length) {
    try {
      await state.wsSender.start(state.localStream);
      // Which camera encoder this browser got, for the live checks.
      document.documentElement.dataset.wsCodec = state.wsSender.codec ?? '';
    } catch (e) {
      console.warn('wsmedia sender:', e.message);
    }
  }
  // Already presenting when the switch happened: carry the screen too.
  const st = state.screenStream?.getVideoTracks()[0];
  if (state.sharing && st) {
    state.wsSender.startScreen(st).catch((e) => console.warn('ws screen:', e.message));
  }
}

function disableWSMedia() {
  state.wsMediaOn = false;
  state.wsSender?.stop();
  state.wsSender = null;
  for (const rx of state.wsReceivers.values()) rx.close();
  state.wsReceivers.clear();
}

// A remote frame arrived. Create the publisher's receiver on first sight and
// register its stream exactly as onRemoteTrack registers a WebRTC one.
function onWSMediaFrame(f) {
  let rx = state.wsReceivers.get(f.publisher);
  if (!rx) {
    if (!wsMediaSupported()) return;
    rx = new WSMediaReceiver();
    state.wsReceivers.set(f.publisher, rx);
    // This receiver needs a keyframe: ask the publisher through the server.
    // The "track" named is the publisher's participant id, which the server
    // recognises as a socket publisher when no SFU track has that id.
    rx.onNeedKey = () => send((w) => encodeKeyframeReq(w, f.publisher));

    // When this publisher starts presenting over the socket, their screen
    // arrives as a second stream. Register it the way a WebRTC screen track
    // is: under its own id, owned by them, and remembered as their screen so
    // the tile builder shows "X is presenting" rather than a second camera.
    rx.onScreenStarted = (screenStream) => {
      const t = screenStream.getVideoTracks()[0];
      if (!t) return;
      state.trackOwner.set(t.id, f.publisher);
      state.videoStreams.set(t.id, new MediaStream([t]));
      state.wsScreenTracks.set(f.publisher, t.id);
      renderStage();
      renderPeople(); // presentations are people-list entries too
      reportViewport(true);
    };

    // Register as a remote stream. Mirrors onRemoteTrack: the stream under
    // the publisher, each video track under its own id so the presenter /
    // camera split keeps working, and the owner map so viewport reporting
    // and keyframe requests know whose tile this is.
    const stream = rx.stream;
    state.remote.set(f.publisher, stream);
    for (const t of stream.getTracks()) {
      state.trackOwner.set(t.id, f.publisher);
      if (t.kind === 'video') state.videoStreams.set(t.id, new MediaStream([t]));
    }
    renderStage();
    renderPeople(); // presentations are people-list entries too
    syncAudioSinks();
    reportViewport(true);
  }
  rx.push(f);
}

// A publisher stopped presenting over the socket: drop just their screen.
function dropWSScreenFor(publisher) {
  const tid = state.wsScreenTracks.get(publisher);
  if (tid) {
    state.videoStreams.delete(tid);
    state.trackOwner.delete(tid);
    state.wsScreenTracks.delete(publisher);
  }
  state.wsReceivers.get(publisher)?.stopScreen();
}

// A publisher left: drop their decoder and their registered tracks.
function dropWSMediaFor(publisher) {
  dropWSScreenFor(publisher);
  const rx = state.wsReceivers.get(publisher);
  if (!rx) return;
  for (const t of rx.stream.getTracks()) {
    state.trackOwner.delete(t.id);
    state.videoStreams.delete(t.id);
  }
  state.remote.delete(publisher);
  rx.close();
  state.wsReceivers.delete(publisher);
  renderStage();
  syncAudioSinks();
}

// ICE servers.
//
// A public STUN server is only useful for discovering a public address, which
// is meaningless when both ends are on the same machine or the same LAN. Worse
// than useless, in fact: if the host has no route to the internet, gathering
// waits on a server that will never answer, and the connection feels slow for
// a reason that has nothing to do with the connection.
async function iceServers() {
  // The relay's TURN servers, minted fresh by the server on each config
  // fetch. This is the fallback path that carries media when the network
  // blocks UDP -- the case that made calls fail on the campus network. The
  // browser still prefers a direct UDP pair and only relays when it must, so
  // adding these costs nothing on a network that already works.
  try {
    const cfg = await serverConfig();
    if (Array.isArray(cfg.iceServers) && cfg.iceServers.length) {
      state.hasRelay = true;
      return cfg.iceServers;
    }
  } catch {
    // fall through to the local heuristic
  }

  // No relay configured. A public STUN server is only useful for discovering
  // a public address, meaningless on a LAN, so a private host offers nothing.
  state.hasRelay = false;
  const h = location.hostname;
  const local =
    h === 'localhost' ||
    h === '127.0.0.1' ||
    h === '::1' ||
    /^10\./.test(h) ||
    /^192\.168\./.test(h) ||
    /^172\.(1[6-9]|2\d|3[01])\./.test(h);
  return local ? [] : [{ urls: 'stun:stun.l.google.com:19302' }];
}

// Screen sharing is split in two, and the split is forced by the browser.
//
// getDisplayMedia must be called from within the user gesture that asked for
// it; browsers refuse a picker opened from a deferred continuation. So the
// picker runs directly in the click handler, and only the peer connection
// work -- which is what actually races -- goes through the media queue.
// Whoever else is presenting now, or null.
function otherPresenter() {
  for (const p of state.people.values()) {
    if (p.id !== state.me && p.state?.sharing) return p;
  }
  return null;
}

// "X is presenting. Present instead?" -- the question Google Meet asks.
// Resolves true to take over, false to leave them presenting.
function confirmTakeover(name) {
  return confirmAction({
    title: `${name} is presenting`,
    body: `Only one person can present at a time. If you present now, ${name}'s presentation stops.`,
    yes: 'Present instead',
  });
}

// A yes/no question in the meeting's dialog. Resolves true on yes. Escape,
// Cancel, or a dialog already open all resolve false.
function confirmAction({ title, body, yes, danger = false }) {
  return new Promise((resolve) => {
    const host = $('dialog');
    if (!host || !host.classList.contains('hidden')) return resolve(false);
    host.dataset.takeover = '1';
    host.innerHTML = `
      <div class="dialog-card takeover-card" role="alertdialog" aria-labelledby="takeover-title" aria-describedby="takeover-body">
        <h2 id="takeover-title">${esc(title)}</h2>
        <p id="takeover-body">${esc(body)}</p>
        <div class="dialog-actions">
          <span class="spacer"></span>
          <button type="button" class="btn btn-quiet" data-no>Cancel</button>
          <button type="button" class="btn ${danger ? 'btn-danger' : 'btn-primary'}" data-yes>${esc(yes)}</button>
        </div>
      </div>`;
    host.classList.remove('hidden');
    const done = (v) => {
      host.classList.add('hidden');
      host.innerHTML = '';
      delete host.dataset.takeover;
      removeEventListener('keydown', onKey, true);
      resolve(v);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') done(false);
    };
    addEventListener('keydown', onKey, true);
    host.querySelector('[data-yes]').onclick = () => done(true);
    host.querySelector('[data-no]').onclick = () => done(false);
    host.querySelector('[data-yes]').focus();
  });
}

async function toggleScreen() {
  if (state.sharing) {
    queueMedia(stopShare);
    return;
  }
  const other = otherPresenter();
  if (other && !(await confirmTakeover(other.name || 'Someone'))) return;

  let stream;
  try {
    // Video only. System audio would carry this machine's loudspeaker --
    // the meeting itself -- back to everyone as an echo of their own voices,
    // and the share path publishes no audio track in any case, so asking
    // for it only put a checkbox in the picker that did nothing.
    stream = await navigator.mediaDevices.getDisplayMedia({
      video: { frameRate: { ideal: 30 } },
      audio: false,
    });
  } catch (err) {
    // NotAllowedError is the user dismissing the picker, which is not a
    // fault. Anything else is, and swallowing it is how a broken feature
    // looks like an unresponsive button.
    if (err?.name !== 'NotAllowedError') {
      console.error('screen share failed:', err?.name, err?.message);
      toast('Could not start screen sharing: ' + (err?.message ?? 'unknown error'), 'bad');
    }
    return;
  }
  queueMedia(() => startShare(stream));
}

async function startShare(stream) {
  const track = stream.getVideoTracks()[0];
  if (!track) return;

  // Another share may have been started and stopped while this one was in the
  // picker. Discard this stream rather than clobbering the current state.
  if (state.sharing) {
    stream.getTracks().forEach((t) => t.stop());
    return;
  }

  state.screenStream = stream;
  state.sharing = true;
  state.screenTrackId = track.id;

  // A second track, not a replacement. The camera keeps running and keeps its
  // own tile, so presenting no longer makes the presenter vanish.
  await state.media?.startScreen(track);
  // And over the socket, when that is the path carrying us.
  if (state.wsMediaOn && state.wsSender) {
    state.wsSender.startScreen(track).catch((e) => console.warn('ws screen:', e.message));
  }

  // The browser's own "stop sharing" bar ends the track without telling us,
  // so this is the only reliable notification that sharing has finished.
  track.onended = () => queueMedia(stopShare);

  pushState();
  renderStage();
  renderPeople(); // presentations are people-list entries too
  syncControls();
}

async function stopShare() {
  if (!state.sharing && !state.screenStream) return;
  state.screenStream?.getTracks().forEach((t) => t.stop());
  state.screenStream = null;
  state.sharing = false;
  state.screenTrackId = '';

  // Only the screen sender stops. The camera was never touched, so there is
  // nothing to hand back -- but the camera intent may have changed while the
  // share was running, so reconcile it.
  await state.media?.stopScreen();
  state.wsSender?.stopScreen();
  await reconcileCamera();

  if (state.pinned && state.pinned.endsWith(SCREEN_SUFFIX)) state.pinned = null;

  pushState();
  renderStage();
  renderPeople(); // presentations are people-list entries too
  syncControls();
}

// ---------------------------------------------------------------------------
// Stage: one large feed, everyone else on the strip
// ---------------------------------------------------------------------------

// A presentation is modelled as its own participant, with an id derived from
// the presenter's. That is what lets it be pinned, unpinned, put on stage and
// laid out exactly like anybody else, without every renderer needing to know
// what a screen share is.
const SCREEN_SUFFIX = '\u0000screen';
const screenIdFor = (id) => id + SCREEN_SUFFIX;
const isScreenId = (id) => typeof id === 'string' && id.endsWith(SCREEN_SUFFIX);
const ownerOfScreenId = (id) => id.slice(0, -SCREEN_SUFFIX.length);

function participants() {
  const out = [];
  if (state.me) {
    out.push({
      id: state.me,
      name: 'You',
      self: true,
      stream: state.localStream,
      st: {
        micMuted: !state.mic,
        camOff: !state.cam,
        handRaised: state.hand,
        sharing: state.sharing,
      },
      energy: state.lastEnergy ?? 0,
      rank: -1,
    });
    if (state.sharing && state.screenStream) {
      out.push({
        id: screenIdFor(state.me),
        name: 'Your presentation',
        self: true,
        presentation: true,
        stream: state.screenStream,
        st: {},
        energy: 0,
        rank: -1,
      });
    }
  }
  for (const p of state.people.values()) {
    if (p.id === state.me) continue;
    const st = p.state ?? {};

    // Their camera is whichever video track is not the one they named as
    // their screen.
    let camera = null;
    let screen = null;
    for (const [trackId, owner] of state.trackOwner) {
      if (owner !== p.id) continue;
      const ms = state.videoStreams.get(trackId);
      if (!ms) continue;
      const vt = ms.getVideoTracks()[0];
      if (!vt || vt.readyState !== 'live') continue;
      const isScreen =
        (st.sharing && st.screenTrack && trackId === st.screenTrack) ||
        state.wsScreenTracks.get(p.id) === trackId;
      if (isScreen) screen = ms;
      else camera = ms;
    }

    out.push({
      id: p.id,
      name: p.name,
      self: false,
      stream: camera ?? state.remote.get(p.id) ?? null,
      st,
      energy: p.energy ?? 0,
      rank: p.rank ?? -1,
    });

    if (screen) {
      out.push({
        id: screenIdFor(p.id),
        name: `${p.name} is presenting`,
        self: false,
        presentation: true,
        stream: screen,
        st: {},
        energy: 0,
        rank: -1,
      });
    }
  }
  return out;
}

// Who occupies the stage. Explicit intent first, then a screen share, then
// whoever is speaking, then anybody with a picture, then yourself.
function pickFeatured(list) {
  if (state.pinned) {
    const p = list.find((x) => x.id === state.pinned);
    if (p) return p;
  }

  // A screen share outranks the active speaker, and this ordering is the
  // whole reason somebody shares in the first place: the share is the subject
  // of the conversation, and the person talking is talking ABOUT it. Ranking
  // the speaker higher put a colleague's shared screen in a thumbnail while
  // their face filled the stage, which reads as sharing being broken.
  // Any presentation, including your own if nobody else is presenting. It is
  // its own entry now, so this is a straight lookup rather than a guess about
  // whose camera has been swapped.
  const presentation =
    list.find((x) => x.presentation && !x.self) ?? list.find((x) => x.presentation);
  if (presentation) return presentation;

  // The host's spotlight, for everyone. After a presentation, as in Teams,
  // where shared content holds the stage and the spotlighted sit beside it.
  const lit = pickSpotlit(list, state.spotlights);
  if (lit) return lit;

  const speaking = list
    .filter((x) => !x.self && x.rank >= 0)
    .sort((a, b) => a.rank - b.rank)[0];
  if (speaking) return speaking;
  const anyRemote = list.find((x) => !x.self && x.stream);
  if (anyRemote) return anyRemote;
  return list[0] ?? null;
}

const liveVideo = (p) =>
  !!p?.stream && !!p.stream.getVideoTracks?.().some((t) => t.readyState === 'live') && !p.st?.camOff;

// What the stage draws, from the chosen view and what is happening in it:
//   'grid'    -- Gallery / Large gallery: everyone in tiles, you floating.
//   'speaker' -- one feed large, the rest on a strip. Gallery turns into this
//                while something has been put on the stage -- your pin, a
//                presentation, the host's spotlight -- as Teams does.
//   'focus'   -- Focus on content: the presentation alone, no strip.
function stageMode(list) {
  if (wb.open) return 'speaker';
  const presenting = list.some((x) => x.presentation);
  if (state.layout === 'focus' && presenting) return 'focus';
  if (state.layout === 'speaker') return 'speaker';
  const pinned = !!state.pinned && list.some((x) => x.id === state.pinned);
  if (pinned || presenting || pickSpotlit(list, state.spotlights)) return 'speaker';
  return 'grid';
}

// Who holds the large feed, or null when the gallery is showing. Used by the
// renderer and by reportViewport, so what is reported is what is drawn.
function featuredFor(list) {
  return stageMode(list) === 'grid' ? null : pickFeatured(list);
}

function renderStage() {
  const list = participants();
  const mode = stageMode(list);
  const featured = mode === 'grid' ? null : pickFeatured(list);
  const stage = $('stage');
  stage.dataset.mode = mode;
  stage.dataset.layout = state.layout;

  const vidBox = $('stage-video');
  const empty = $('stage-empty');
  const video = vidBox.querySelector('video');
  const hasVideo = !!featured && liveVideo(featured);

  if (mode === 'grid') {
    video.srcObject = null;
    vidBox.classList.add('hidden');
    empty.classList.add('hidden');
  } else if (hasVideo) {
    if (video.srcObject !== featured.stream) video.srcObject = featured.stream;
    // Mirror a camera self-view, never a screen share. Sharing a screen and
    // seeing your own text backwards is the kind of defect that makes a
    // product feel unfinished.
    vidBox.classList.toggle('mirror', featured.self && !featured.presentation && state.mirrorSelf);
    vidBox.classList.remove('hidden');
    empty.classList.add('hidden');
  } else {
    video.srcObject = null;
    vidBox.classList.add('hidden');
    empty.classList.remove('hidden');
    $('stage-avatar').textContent = featured ? initials(faceOf(featured)) : '?';
    if (!$('stage-msg').textContent.startsWith('Camera unavailable')) {
      $('stage-msg').textContent = featured
        ? `${featured.name} has their camera off`
        : 'Waiting for others to join';
    }
  }

  $('chip-who').textContent = featured?.name ?? '—';
  $('chip-who').dataset.owner = featured?.id ?? '';
  $('chip-role').textContent =
    state.pinned === featured?.id ? 'Pinned'
      : state.spotlights.includes(featured?.id) ? 'Spotlighted'
      : featured?.rank === 0 ? 'Speaking' : 'On stage';

  // Who gets a tile, and in what order.
  const strip = $('strip');
  let tiles = [];
  let more = 0;
  if (mode === 'grid') {
    const others = galleryOrder(list.map((p) => ({ ...p, hasVideo: liveVideo(p) })));
    const cap = tileCap(state.layout);
    if (others.length > cap) {
      tiles = others.slice(0, cap - 1);
      more = others.length - tiles.length;
    } else tiles = others;
    const me = list.find((p) => p.self && !p.presentation);
    // Alone, you are the gallery; with company, you float in the corner.
    if (me) tiles = tiles.length ? [...tiles, { ...me, float: true }] : [me];
  } else if (mode === 'speaker') {
    tiles = list.filter((p) => p.id !== featured?.id);
  }

  const wanted = new Set();
  for (const p of tiles) {
    wanted.add(p.id);
    let tile = strip.querySelector(`.strip-tile[data-id="${CSS.escape(p.id)}"]`);
    if (!tile) {
      tile = document.createElement('div');
      tile.className = 'strip-tile';
      tile.dataset.id = p.id;
      tile.innerHTML = `
        <video autoplay playsinline muted></video>
        <div class="strip-ph"><span class="avatar sm"></span></div>
        <span class="strip-label">
          <span class="strip-mic"><svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-mic"/></svg></span>
          <span class="strip-name"></span>
          <svg class="strip-flag flag-hand" width="13" height="13" viewBox="0 0 24 24" aria-label="Hand raised"><use href="#i-hand"/></svg>
          <svg class="strip-flag flag-spot" width="13" height="13" viewBox="0 0 24 24" aria-label="Spotlighted"><use href="#i-spotlight"/></svg>
        </span>
        <button type="button" class="tile-more" aria-label="More options" aria-haspopup="menu">
          <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-more"/></svg>
        </button>`;
      tile.querySelector('.tile-more').onclick = (e) => {
        e.stopPropagation();
        openPersonMenu(tile.dataset.id, e.currentTarget);
      };
      strip.appendChild(tile);
    }
    paintTile(tile, p);
  }

  // "+N more": the people past the cap, one tap from the full list.
  let moreTile = strip.querySelector('.strip-more');
  if (more > 0) {
    if (!moreTile) {
      moreTile = document.createElement('button');
      moreTile.type = 'button';
      moreTile.className = 'strip-tile strip-more';
      moreTile.dataset.id = '\u0000more';
      moreTile.onclick = () => setSide('people');
      strip.appendChild(moreTile);
    }
    moreTile.textContent = `+${more} more`;
    wanted.add('\u0000more');
  }
  for (const t of [...strip.children]) {
    if (!wanted.has(t.dataset.id)) t.remove();
  }

  if (mode === 'grid') layoutGrid(tiles.filter((p) => !p.float).length + (more > 0 ? 1 : 0));

  // Prefer somebody else as the floated tile: a window of your own face is
  // not what anyone wants to watch while they work in another tab.
  markAutoPiP(hasVideo ? video : strip.querySelector('.strip-tile:not(.self-float) video') ?? strip.querySelector('video'));
}

// Whose face a tile shows: the person, never the words on screen. Your own
// tile is labelled "You", and an avatar made from that label read "Y".
function faceOf(p) {
  if (p?.self) return { userId: session.user?.id, name: session.user?.displayName || '', email: session.user?.email || '' };
  return { userId: p?.userId, name: p?.name ?? '' };
}

function paintTile(tile, p) {
  const v = tile.querySelector('video');
  const ph = tile.querySelector('.strip-ph');
  if (liveVideo(p)) {
    if (v.srcObject !== p.stream) v.srcObject = p.stream;
    v.classList.remove('hidden');
    ph.classList.add('hidden');
  } else {
    if (v.srcObject) v.srcObject = null;
    v.classList.add('hidden');
    ph.classList.remove('hidden');
    ph.querySelector('.avatar').textContent = initials(faceOf(p));
  }
  const spotlit = state.spotlights.includes(p.id);
  tile.classList.toggle('self-float', !!p.float);
  tile.classList.toggle('presentation', !!p.presentation);
  tile.classList.toggle('mirror', p.self && !p.presentation && state.mirrorSelf);
  tile.classList.toggle('speaking', p.energy > 0.12);
  tile.classList.toggle('pinned', state.pinned === p.id);
  tile.classList.toggle('spotlit', spotlit);
  tile.classList.toggle('hand', !!p.st?.handRaised);
  // The spotlighted lead the strip, beside whoever holds the stage.
  tile.style.order = spotlit ? '-1' : '';
  tile.querySelector('.strip-name').textContent = p.name;
  const mic = tile.querySelector('.strip-mic');
  mic.classList.toggle('muted', !!p.st?.micMuted);
  mic.querySelector('use').setAttribute('href', p.st?.micMuted ? '#i-mic-off' : '#i-mic');
  mic.hidden = !!p.presentation;
}

// Size the gallery so every tile is as large as the stage allows at 16:9
// (gallery.js bestGrid), rather than letting a grid stretch them.
function layoutGrid(n) {
  const strip = $('strip');
  const box = $('stage').getBoundingClientRect();
  const pad = 12;
  const g = bestGrid(Math.max(1, n), box.width - pad * 2, box.height - pad * 2, 8);
  strip.style.setProperty('--cols', g.cols);
  strip.style.setProperty('--tile-w', g.w + 'px');
  strip.style.setProperty('--tile-h', g.h + 'px');
}

// The People panel, laid out and worded as Teams' (the strings are from its
// package): "Share invite", "Search for participants", "Waiting in lobby",
// "Raised hands", "In this meeting (N)" with "Mute all" and a "..." for the
// meeting, then one row per person with their role and a "..." of their own.
//
// The top -- invite, add people, search -- is built once and kept, so typing
// in the search is not interrupted by the re-render every roster change
// triggers; only the list below it is rebuilt.

const RAISED_RANK_LIMIT = 20; // Teams' raiseHandsOrderedRankLimit

function roleLabel(p) {
  const who = state.people.get(p.id);
  const role = p.self ? state.myRole : who?.role ?? Role.Member;
  const organizer = p.self ? !!state.people.get(state.me)?.organizer : !!who?.organizer;
  if (organizer) return 'Organizer';
  if (role >= RoleHost) return 'Co-organizer';
  if (role === Role.Moderator) return 'Presenter';
  return 'Attendee';
}

function blockedNote(p) {
  const b = p.self ? state.blocked : state.people.get(p.id)?.blocked;
  if (!b || (!b.mic && !b.cam)) return '';
  return b.mic && b.cam ? ' · Mic and camera disabled' : b.mic ? ' · Mic disabled' : ' · Camera disabled';
}

function renderPeople() {
  const box = $('view-people');
  if (!box) return;
  const list = participants();
  const people = list.filter((p) => !p.presentation);
  const count = $('mb-people-count');
  if (count) count.textContent = String(people.length);

  if (!box.querySelector('#pp-list')) {
    box.innerHTML = `
      <div class="pp-top">
        <button type="button" class="btn btn-quiet sm pp-invite" id="pp-invite">
          <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-person-add"/></svg> Share invite
        </button>
        ${addPeopleHTML()}
        <input class="input pp-search" id="pp-search" type="search" placeholder="Search for participants" autocomplete="off" spellcheck="false" />
      </div>
      <div id="pp-list"></div>`;
    $('pp-invite').onclick = () => inviteDialog();
    $('pp-search').oninput = (e) => {
      state.peopleFilter = e.target.value.trim().toLowerCase();
      renderPeople();
    };
    wireAddPeople();
  }

  const filter = state.peopleFilter ?? '';
  const shown = (p) => !filter || (p.name ?? '').toLowerCase().includes(filter);
  const host = state.isHost;

  // Raised hands, in the order they went up.
  const raised = people
    .filter((p) => (p.self ? state.hand : p.st?.handRaised))
    .sort((a, b) => (state.handOrder.get(a.id) ?? 1e9) - (state.handOrder.get(b.id) ?? 1e9));
  const rank = new Map(raised.map((p, i) => [p.id, i + 1]));

  const row = (p) => {
    const acct = state.people.get(p.id)?.userId;
    const avatar = acct
      ? `<button class="who-btn av-wrap" data-person="${esc(acct)}" data-person-name="${esc(p.name)}" aria-label="About ${esc(p.name)}">`
      : '<span class="av-wrap">';
    const presentation = !!p.presentation;
    const n = rank.get(p.id);
    return `<div class="person" data-pid="${esc(p.id)}">
      ${avatar}<span ${avatarAttrs(p)}>${esc(initials(p))}</span>${
        presentation ? '' : presenceIcon(p.st?.sharing ? 'presenting' : 'incall')
      }${acct ? '</button>' : '</span>'}
      <span class="person-name">${esc(p.name)}
        <span class="person-sub">${presentation ? 'Presenting' : esc(roleLabel(p)) + (p.self ? ' · You' : p.rank === 0 ? ' · Speaking' : '') + blockedNote(p)}</span>
      </span>
      <span class="person-tags">
        ${state.spotlights.includes(p.id) ? '<svg width="15" height="15" viewBox="0 0 24 24" class="is-spotlit" aria-label="Spotlighted"><use href="#i-spotlight"/></svg>' : ''}
        ${n ? `<span class="hand-rank" aria-label="Hand raised, ${n}${n === 1 ? 'st' : n === 2 ? 'nd' : n === 3 ? 'rd' : 'th'}"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-hand"/></svg>${n <= RAISED_RANK_LIMIT ? n : ''}</span>` : ''}
        ${presentation ? '' : `<svg width="15" height="15" viewBox="0 0 24 24" class="${(p.self ? !state.mic : p.st?.micMuted) ? 'is-muted' : ''}"><use href="#${(p.self ? !state.mic : p.st?.micMuted) ? 'i-mic-off' : 'i-mic'}"/></svg>`}
        <button type="button" class="person-more" data-person-menu="${esc(p.id)}" aria-label="More options for ${esc(p.name)}" aria-haspopup="menu">
          <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-more"/></svg>
        </button>
      </span>
    </div>`;
  };

  const section = (title, actions, rows) => `<section class="pp-section">
      <div class="pp-head"><strong>${title}</strong><span class="spacer"></span>${actions}</div>
      ${rows}
    </section>`;

  let html = waitingListHTML();
  if (raised.length) {
    html += section(
      `Raised hands (${raised.length})`,
      host ? '<button type="button" class="btn btn-quiet sm" id="pp-lower-all">Lower all hands</button>' : '',
      raised.filter(shown).map(row).join('')
    );
  }
  html += section(
    `In this meeting (${people.length})`,
    host
      ? `<button type="button" class="btn btn-quiet sm" id="hc-mute-all">Mute all</button>
         <button type="button" class="icon-btn pp-meeting-more" id="pp-meeting-more" aria-label="Meeting actions" aria-haspopup="menu">
           <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-more"/></svg></button>`
      : '',
    list.filter(shown).map(row).join('') || '<div class="empty">Nobody by that name.</div>'
  );
  $('pp-list').innerHTML = html;
  wireHostControls();
  document.querySelectorAll('[data-person-menu]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      openPersonMenu(b.dataset.personMenu, b);
    };
  });
}

function sendSpotlight(id, on) {
  send((w) => {
    w.header(Op.Spotlight);
    w.str(id);
    w.bool(on);
    w.bits(0, 7);
  });
}

function sendPerms(next) {
  send((w) => {
    w.header(Op.Permissions);
    w.bool(next.mic);
    w.bool(next.camera);
    w.bool(next.share);
    w.bool(next.chat);
    w.bool(!next.reactions);
    w.bits(0, 3);
  });
}

function sendPolicy(policy, locked) {
  send((w) => {
    w.header(Op.MeetingPolicy);
    w.str(policy);
    w.bool(locked);
    w.bits(0, 7);
  });
}

function sendRole(id, role) {
  send((w) => {
    w.header(Op.SetRole);
    w.str(id);
    w.bits(role, 2);
    w.bits(0, 6);
  });
}

// "You'll end the meeting for everyone." Stop what this tab is sending at
// once -- a camera light that stays on after the meeting ended is the defect
// people notice -- then say so. The server closes the socket. Also used when
// a host removes you ("removed").
function meetingEnded(status = 'ended') {
  state.ended = true;
  stopLocalRecording();
  state.recogniser?.stop();
  state.recogniser = null;
  for (const s of [state.localStream, state.screenStream]) s?.getTracks().forEach((t) => t.stop());
  // The published microphone is the suppressor's output; the device track
  // behind it is ended only by the suppressor itself.
  state.denoiser?.stop();
  state.denoiser = null;
  showLobby(status);
}

// A small menu at an anchor, closed by a pick, a click elsewhere or Escape.
function popMenu(anchor, items, onPick) {
  document.getElementById('tile-menu')?.remove();
  if (!items.length) return;
  const menu = document.createElement('div');
  menu.id = 'tile-menu';
  menu.className = 'mb-pop tile-menu';
  menu.setAttribute('role', 'menu');
  menu.innerHTML = items
    .map(([k, label, danger]) => `<button role="menuitem" data-k="${k}"${danger ? ' class="danger"' : ''}>${esc(label)}</button>`)
    .join('');
  document.body.appendChild(menu);
  const r = anchor.getBoundingClientRect();
  const w = menu.offsetWidth, h = menu.offsetHeight;
  menu.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.right - w)) + 'px';
  menu.style.top = (r.bottom + h + 8 < innerHeight ? r.bottom + 4 : Math.max(8, r.top - h - 4)) + 'px';
  const close = () => {
    menu.remove();
    removeEventListener('click', outside, true);
    removeEventListener('keydown', onKey, true);
  };
  const outside = (e) => { if (!menu.contains(e.target)) close(); };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  setTimeout(() => {
    addEventListener('click', outside, true);
    addEventListener('keydown', onKey, true);
  });
  menu.onclick = (e) => {
    const k = e.target.closest('[data-k]')?.dataset.k;
    if (!k) return;
    close();
    onPick(k);
  };
  menu.querySelector('button')?.focus();
}

// The "..." on a person, in the People panel or on their tile. What it offers
// is what the server will accept from you: Pin for me to anyone; the
// spotlight, roles and removal to hosts, never on the organizer or yourself
// where Teams does not offer them either.
function openPersonMenu(id, anchor) {
  const p = participants().find((x) => x.id === id);
  if (!p) return;
  const who = state.people.get(id);
  const lit = state.spotlights.includes(id);
  const organizer = !!who?.organizer;
  const role = p.self ? state.myRole : who?.role ?? Role.Member;
  const items = [state.pinned === id ? ['unpin', 'Unpin'] : ['pin', 'Pin for me']];
  if (!p.presentation) {
    if (state.isHost) items.push(lit ? ['unspot', 'Stop spotlighting'] : ['spot', 'Spotlight for everyone']);
    else if (p.self && lit) items.push(['unspot', 'Exit spotlight']);
    if (state.isHost && !p.self && !organizer) {
      if (role !== Role.Moderator) items.push(['presenter', 'Make a presenter']);
      if (role !== Role.Member) items.push(['attendee', 'Make an attendee']);
      if (role < Role.Moderator) {
        const b = who?.blocked ?? { mic: false, cam: false };
        items.push(b.mic ? ['allowmic', 'Allow mic'] : ['blockmic', 'Disable mic']);
        items.push(b.cam ? ['allowcam', 'Allow camera'] : ['blockcam', 'Disable camera']);
      }
      items.push(['remove', 'Remove from meeting', true]);
    }
  }
  popMenu(anchor, items, async (k) => {
    if (k === 'pin') {
      if (lit) return toast("Can't pin a participant that is spotlighted");
      state.pinned = id;
    } else if (k === 'unpin') {
      state.pinned = null;
    } else if (k === 'spot') {
      if (state.spotlights.length >= MAX_SPOTLIGHTS) return toast('No more people can be spotlighted', 'bad');
      if (await confirmAction({ title: 'Spotlight for everyone?', body: `${p.self ? 'Your' : `${p.name}'s`} video will be on the main stage for everyone in the meeting.`, yes: 'Spotlight for everyone' }))
        sendSpotlight(id, true);
      return;
    } else if (k === 'unspot') {
      const own = p.self && !state.isHost;
      if (await confirmAction(own
        ? { title: 'Exit spotlight?', body: 'Your video will no longer be on the main stage for everyone.', yes: 'Exit spotlight' }
        : { title: 'Stop spotlighting this video?', body: 'It will no longer be on the main stage for everyone.', yes: 'Stop spotlighting' }))
        sendSpotlight(id, false);
      return;
    } else if (k === 'blockmic' || k === 'allowmic' || k === 'blockcam' || k === 'allowcam') {
      const b = who?.blocked ?? { mic: false, cam: false };
      const mic = k === 'blockmic' ? true : k === 'allowmic' ? false : b.mic;
      const cam = k === 'blockcam' ? true : k === 'allowcam' ? false : b.cam;
      return send((w) => { w.header(Op.Block); w.str(id); w.bool(mic); w.bool(cam); w.bits(0, 6); });
    } else if (k === 'presenter') {
      return sendRole(id, Role.Moderator);
    } else if (k === 'attendee') {
      return sendRole(id, Role.Member);
    } else if (k === 'remove') {
      if (await confirmAction({ title: 'Remove from meeting?', body: `${p.name} will be removed from the meeting. They can try to rejoin.`, yes: 'Remove', danger: true }))
        send((w) => { w.header(Op.Remove); w.str(id); });
      return;
    }
    renderStage();
    reportViewport(true);
  });
}

// Meeting options: what attendees may do and who waits, in Teams' words.
// Applies on Save; the server decides and echoes it back to everybody.
function openMeetingOptions() {
  if (!state.isHost) return;
  const host = $('dialog');
  if (!host || !host.classList.contains('hidden')) return;
  const p = state.perms;
  const policy = state.policy || 'trusted';
  const toggle = (id, label, on) => `<label class="mo-row"><span>${label}</span>
      <input type="checkbox" class="mo-switch" id="${id}" ${on ? 'checked' : ''} /></label>`;
  const select = (id, label, value, opts) => `<label class="mo-row"><span>${label}</span>
      <select class="input" id="${id}">${opts.map(([v, t]) => `<option value="${v}"${v === value ? ' selected' : ''}>${esc(t)}</option>`).join('')}</select></label>`;
  host.dataset.takeover = '1';
  host.innerHTML = `
    <div class="dialog-card mo-card" role="dialog" aria-labelledby="mo-title">
      <h2 id="mo-title">Meeting options</h2>
      <h3>Security</h3>
      ${select('mo-lobby', 'Who can bypass the lobby?', policy, [
        ['open', 'Everyone'],
        ['trusted', 'People in my org'],
        ['knock', 'Only organizers and co-organizers'],
      ])}
      <p class="mo-note">People who were invited always come straight in.</p>
      ${select('mo-present', 'Who can present?', p.share ? 'everyone' : 'organizers', [
        ['everyone', 'Everyone'],
        ['organizers', 'Only organizers, co-organizers and presenters'],
      ])}
      <h3>Audio &amp; video</h3>
      ${toggle('mo-mic', 'Allow mic for attendees?', p.mic)}
      ${toggle('mo-cam', 'Allow camera for attendees?', p.camera)}
      <h3>Engagement</h3>
      ${select('mo-chat', 'Meeting chat', p.chat ? 'on' : 'off', [['on', 'On'], ['off', 'Off']])}
      ${toggle('mo-react', 'Allow reactions', p.reactions !== false)}
      <div class="dialog-actions">
        <span class="spacer"></span>
        <button type="button" class="btn btn-quiet" data-no>Cancel</button>
        <button type="button" class="btn btn-primary" data-yes>Save</button>
      </div>
    </div>`;
  host.classList.remove('hidden');
  const done = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
    delete host.dataset.takeover;
    removeEventListener('keydown', onKey, true);
  };
  const onKey = (e) => { if (e.key === 'Escape') done(); };
  addEventListener('keydown', onKey, true);
  host.querySelector('[data-no]').onclick = done;
  host.querySelector('[data-yes]').onclick = () => {
    sendPerms({
      mic: $('mo-mic').checked,
      camera: $('mo-cam').checked,
      share: $('mo-present').value === 'everyone',
      chat: $('mo-chat').value === 'on',
      reactions: $('mo-react').checked,
    });
    const lobby = $('mo-lobby').value;
    if (lobby !== policy) sendPolicy(lobby, !!state.locked);
    done();
    toast('Meeting options saved.');
  };
  $('mo-lobby').focus();
}

// "Download attendance list": fetched first, so a refusal is a message rather
// than a downloaded error page; then saved under the meeting's name.
async function downloadAttendance() {
  try {
    const res = await fetch(`/api/scheduling/rooms/${encodeURIComponent(state.room)}/attendance`, { credentials: 'same-origin' });
    if (!res.ok) throw new Error(res.status === 403 ? 'Only organizers and co-organizers can download the attendance list.' : `Could not download the attendance list (${res.status}).`);
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement('a');
    a.href = url;
    a.download = `${(state.title || 'Meeting').replace(/[\\/:*?"<>|]+/g, ' ').trim()} - Attendance list.csv`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  } catch (e) {
    toast(e.message, 'bad');
  }
}

function wireHostControls() {
  if (!state.isHost) return;
  wireWaitingList();
  const muteAll = $('hc-mute-all');
  if (muteAll) {
    muteAll.onclick = () => {
      send((w) => w.header(Op.MuteAll));
      toast('Everyone has been muted.');
    };
  }
  const lower = $('pp-lower-all');
  if (lower) {
    lower.onclick = async () => {
      if (await confirmAction({ title: 'Lower all hands?', body: 'Every raised hand in the meeting will be lowered.', yes: 'Lower all hands' }))
        send((w) => w.header(Op.LowerHands));
    };
  }
  const more = $('pp-meeting-more');
  if (more) {
    more.onclick = (e) => {
      e.stopPropagation();
      const items = [
        state.locked ? ['unlock', 'Unlock the meeting'] : ['lock', 'Lock the meeting'],
        ['options', 'Manage permissions'],
        ['attendance', 'Download attendance list'],
      ];
      if (state.spotlights.length) items.push(['stopspots', 'Stop all spotlights']);
      popMenu(more, items, async (k) => {
        if (k === 'lock' && state.breakouts?.open) toast("You can't lock the meeting while breakout rooms are open.", 'bad');
        else if (k === 'lock' || k === 'unlock') sendPolicy(state.policy || 'trusted', k === 'lock');
        else if (k === 'options') openMeetingOptions();
        else if (k === 'attendance') downloadAttendance();
        else if (k === 'stopspots' && await confirmAction({ title: 'Stop spotlighting all videos?', body: 'Nobody will be on the main stage for everyone.', yes: 'Stop all spotlights' }))
          sendSpotlight('', false);
      });
    };
  }
}


// ---------------------------------------------------------------------------
// Viewport reporting
//
// The single most valuable thing the client tells the server. Every tile
// reports the size it is actually painted at, in device pixels, and whether
// it is on screen. A tile drawn 104 pixels wide then receives a stream sized
// for 104 pixels instead of one sized for 640 and thrown away.
// ---------------------------------------------------------------------------

function reportViewport(force = false) {
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) return;

  const dpr = window.devicePixelRatio || 1;
  const stageEl = $('stage-video');
  const stageOwner = $('chip-who')?.dataset.owner;
  const tiles = [];

  const featuredNow = featuredFor(participants());

  for (const [trackId, owner] of state.trackOwner) {
    // A presentation is laid out under its own id, not the presenter's, so
    // the element and the pin state must both be looked up that way.
    // Reporting a pinned presentation against the presenter's id told the
    // allocator that nothing pinned was on screen, and it would then starve
    // the one tile the viewer was actually looking at.
    const who = state.people.get(owner);
    const isScreen =
      !!who?.state?.sharing && who.state.screenTrack === trackId;
    const tileId = isScreen ? screenIdFor(owner) : owner;

    let el = document.querySelector(`.strip-tile[data-id="${CSS.escape(tileId)}"]`);
    if (!el && stageOwner === tileId) el = stageEl;
    if (!el) {
      // On stage but not in the strip: measure the stage.
      el = featuredNow && featuredNow.id === tileId ? stageEl : null;
    }
    if (!el) {
      tiles.push({ track: trackId, width: 0, height: 0, visible: false, pinned: false });
      continue;
    }
    const rect = el.getBoundingClientRect();
    // Off screen, in a collapsed panel, or in a background tab all mean the
    // same thing to the allocator: send nothing.
    //
    // A picture-in-picture window is the exception, and it was reported as a
    // bug: the tab is hidden, so every tile was declared invisible and the
    // server stopped sending, which froze the floating window on its last
    // frame at the one moment the feature exists for. While PiP is up the
    // geometry alone decides. That keeps slightly more alive than PiP is
    // actually painting, because the tiles it shows are not distinguishable
    // from here -- a few tiles' worth of bandwidth against a feature that
    // otherwise does not work.
    const backgrounded = document.hidden && !pipActive();
    const onScreen =
      !backgrounded &&
      rect.bottom > 0 &&
      rect.top < window.innerHeight &&
      rect.right > 0 &&
      rect.left < window.innerWidth &&
      rect.width > 0;
    tiles.push({
      track: trackId,
      width: Math.round(rect.width * dpr),
      height: Math.round(rect.height * dpr),
      visible: onScreen,
      pinned: state.pinned === tileId,
    });
  }
  // Each person's tile, by participant id: on the socket media path the
  // server sends a camera only to those who have it on screen (the track ids
  // above are this browser's own there, unknown to the server). The same
  // on-screen rule as for tracks.
  for (const p of participants()) {
    if (p.self || p.presentation || p.id === state.me) continue;
    let el = document.querySelector(`.strip-tile[data-id="${CSS.escape(p.id)}"]`);
    if (!el && (stageOwner === p.id || featuredNow?.id === p.id)) el = stageEl;
    const rect = el?.getBoundingClientRect();
    const onScreen =
      !!rect &&
      !(document.hidden && !pipActive()) &&
      rect.bottom > 0 &&
      rect.top < window.innerHeight &&
      rect.right > 0 &&
      rect.left < window.innerWidth &&
      rect.width > 0;
    tiles.push({
      track: p.id,
      width: onScreen ? Math.round(rect.width * dpr) : 0,
      height: onScreen ? Math.round(rect.height * dpr) : 0,
      visible: onScreen,
      pinned: state.pinned === p.id,
    });
  }
  if (!tiles.length) return;

  // Only report on change. A resize observer that fires sixty times a second
  // during a window drag should not produce sixty frames saying the same
  // thing by the time they arrive.
  const sig = JSON.stringify(tiles);
  if (!force && sig === state.lastViewport) return;
  state.lastViewport = sig;
  send((w) => encodeViewport(w, tiles));
}

// Ask every publisher we are watching for a fresh keyframe.
//
// Cheap to send and cheap to ignore: the SFU deduplicates these, so a room
// where everybody returns to the tab at once costs one PLI per publisher
// rather than one per watcher per publisher.
function requestKeyframes() {
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) return;
  for (const trackId of state.trackOwner.keys()) {
    send((w) => encodeKeyframeReq(w, trackId));
  }
}

async function reportBwe() {
  const st = await state.media?.readStats();
  if (!st || !st.estimateKbps) return;
  const last = state.lastBwe ?? 0;
  if (Math.abs(st.estimateKbps - last) < Math.max(50, last * 0.1)) return;
  state.lastBwe = st.estimateKbps;
  send((w) => encodeBwe(w, st));
}

// ---------------------------------------------------------------------------
// Chat and people
// ---------------------------------------------------------------------------

function addChat(whoId, text) {
  state.chat.push({ who: whoId, text, at: Date.now() });
  if (whoId !== state.me) showBubble(state.people.get(whoId)?.name ?? 'Someone', text);
  // Said to the room while you are writing to one person: not lost, unread.
  // The selector marks it, the way the sidebar marks a conversation.
  if (state.chatTo !== 'everyone' && whoId !== state.me) {
    state.roomUnread = (state.roomUnread ?? 0) + 1;
    renderChatTo();
  }
  renderChat();
}

// The panel writes either to the room or to one person. This is the list of
// choices, rebuilt whenever the roster changes.
//
// Only people with an account appear: a private message needs somewhere to
// live after the meeting, and a guest has nowhere.
// ---------------------------------------------------------------------------
// Live captions
//
// Recognised by the browser of the person speaking and sent as text: no
// service is paid by the minute, and a sentence costs a few dozen bytes where
// the audio it came from costs kilobytes a second. On the device where the
// browser can; otherwise by the browser's own online service, which is why
// each speaker is asked first (captions.js).
// ---------------------------------------------------------------------------

// Captions are Teams': "Turn on live captions" shows what everyone says, not
// only what people who turned captions on themselves say. So turning them on
// tells the room (OpRecording, "my captions"), and every speaker's browser
// recognises its own speech while anybody wants captions or the meeting is
// transcribed -- once that speaker has agreed (speechWanted, below).
function toggleCaptions() {
  state.captions = !state.captions;
  $('caption').classList.toggle('hidden', !state.captions);
  if (!state.captions) state.captionLines.clear();
  send((w) => encodeRecSet(w, REC.captions, state.captions));
  renderCaptions();
  syncControls();
}

// Speech becomes text on this machine only while somebody wants it, this
// speaker has said yes, and their microphone is on: muted is not speaking.
function speechWanted() {
  return (state.rec.captions || state.rec.transcribing) && state.mic && !state.ended && captionsSupported();
}

function syncSpeech() {
  const want = speechWanted() && state.speechConsent === true;
  if (want && !state.recogniser) {
    state.recogniser = createRecogniser(
      (text, isFinal) => sendCaption(text, isFinal),
      (err) => toast(err === 'not-allowed' ? 'Captions need microphone access.' : 'Captions are unavailable in this browser.', 'bad'),
      { local: state.speechLocal }
    );
    state.recogniser?.start();
  } else if (!want && state.recogniser) {
    state.recogniser.stop();
    state.recogniser = null;
  }
  renderRecBanner();
}

function sendCaption(text, isFinal) {
  if (!text || !state.ws || state.ws.readyState !== WebSocket.OPEN) return;
  const utt = state.utt;
  send((w) => encodeCaption(w, isFinal, utt, text));
  // Your own words, without waiting for them to come back (they do not).
  noteCaption(state.me, text);
  if (isFinal) {
    state.utt++;
    if (state.rec.transcribing) addTranscriptLine(state.people.get(state.me)?.name ?? 'You', text, Date.now());
  }
}

function onCaption(m) {
  noteCaption(m.speaker, m.text);
  if (m.final && state.rec.transcribing) addTranscriptLine(state.people.get(m.speaker)?.name ?? 'Someone', m.text, Date.now());
}

// The server said no, to this person only. Teams' words.
function onRecRefused(m) {
  if (m.what === REC.recording && state.recPending) {
    clearTimeout(state.recPending.timer);
    state.recPending.writable?.abort?.();
    state.recPending = null;
  }
  if (m.reason === 'deployment') {
    state.recDisabled = true;
    toast('Recording not started. Your IT admin has disabled Recording.', 'bad');
  } else {
    toast(m.what === REC.recording ? 'Only meeting organizers and presenters can control recording' : 'Only meeting organizers and presenters can control transcription', 'bad');
  }
  syncControls();
}

// The room's recording state changed (or this is the arrival's first word).
function onRecState(m) {
  const was = state.rec;
  // The first word an arrival hears is the state as it is, not news: the
  // banner says it; a toast that someone "started" it would be wrong.
  const first = !state.recSeen;
  state.recSeen = true;
  state.rec = m;
  const started = (m.recording && !was.recording) || (m.transcribing && !was.transcribing);
  if (started) state.recBannerHidden = false;
  if (m.recording && !was.recording && state.recPending) {
    // The server agreed to the recording this machine asked for: make it.
    const pending = state.recPending;
    state.recPending = null;
    clearTimeout(pending.timer);
    startLocalRecording(pending.writable);
  } else if (m.recording && !was.recording && !first) {
    toast(`${m.recBy || 'Someone'} started recording.`);
  }
  if (!m.recording && was.recording) {
    if (state.recorder) stopLocalRecording();
    else toast('Recording has stopped');
  }
  if (m.transcribing && !was.transcribing) {
    if (!first) toast(`${m.trBy || 'Someone'} started transcription`);
    transcriptState.lines = [];
    transcriptState.loaded = false;
    if (state.sideView === 'transcript') loadTranscript();
  }
  // Asked once a meeting, the first time someone wants this speaker's words.
  if (speechWanted() && state.speechConsent === null && !state.speechLocalChecked) {
    state.speechLocalChecked = true;
    speechOnDevice().then((local) => {
      state.speechLocal = local;
      renderRecBanner();
    });
  }
  renderRecBanner();
  syncSpeech();
  syncControls();
}

// The strip across the top of the stage: who is being recorded, and, for a
// speaker not yet asked, whether their speech may become text.
function renderRecBanner() {
  const el = $('rec-banner');
  if (!el) return;
  const words = recBanner(state.rec);
  const ask = speechWanted() && state.speechConsent === null;
  const showWords = words && !state.recBannerHidden;
  if (!showWords && !ask) {
    el.classList.add('hidden');
    el.innerHTML = '';
    return;
  }
  const how = state.speechLocal ? 'on this device' : "using your browser's online speech service";
  el.classList.remove('hidden');
  el.innerHTML = `
    ${showWords ? `<div class="rb-row"><span class="rb-dot" aria-hidden="true"></span><span class="rb-text">${esc(words)} By staying in the meeting, you consent to being included.</span><button type="button" class="icon-btn rb-close" data-close aria-label="Dismiss"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg></button></div>` : ''}
    ${ask ? `<div class="rb-row rb-ask"><span class="rb-text">${showWords ? '' : state.rec.transcribing ? 'This meeting is being transcribed. ' : 'Live captions are on. '}Turn your speech into text, ${how}?</span><span class="rb-btns"><button type="button" class="btn sm" data-speech="no">Not now</button><button type="button" class="btn sm btn-primary" data-speech="yes">Allow</button></span></div>` : ''}`;
  el.querySelector('[data-close]')?.addEventListener('click', () => {
    state.recBannerHidden = true;
    renderRecBanner();
  });
  el.querySelectorAll('[data-speech]').forEach((b) =>
    b.addEventListener('click', () => {
      state.speechConsent = b.dataset.speech === 'yes';
      syncSpeech();
      syncControls();
      if (state.sideView === 'transcript') renderTranscript();
    })
  );
}

// Record and transcribe, for organizers and presenters.
async function toggleRecording() {
  if (state.rec.recording) {
    send((w) => encodeRecSet(w, REC.recording, false));
    return;
  }
  if (state.recPending) return;
  // "It'll be saved to their computer." Where, is asked now, while the
  // click still counts as one: a file to stream into, so an hour of meeting
  // never has to fit in memory. Without that, memory, bounded.
  let writable = null;
  if (window.showSaveFilePicker) {
    try {
      const handle = await window.showSaveFilePicker({ suggestedName: recFileName(new Date(), 'video/webm'), types: [{ description: 'Video', accept: { 'video/webm': ['.webm'] } }] });
      writable = await handle.createWritable();
    } catch (e) {
      if (e?.name === 'AbortError') return; // they cancelled: no recording
      writable = null;
    }
  }
  state.recPending = {
    writable,
    timer: setTimeout(() => {
      state.recPending?.writable?.abort?.();
      state.recPending = null;
      toast("Couldn't start recording.", 'bad');
    }, 5000),
  };
  send((w) => encodeRecSet(w, REC.recording, true));
}

function toggleTranscription() {
  send((w) => encodeRecSet(w, REC.transcription, !state.rec.transcribing));
}

// The recording itself: what this screen shows of the stage, drawn into a
// canvas, and everyone's audio mixed, encoded by the browser.
function startLocalRecording(writable) {
  const mime = pickMime((m) => window.MediaRecorder?.isTypeSupported?.(m));
  const canvas = document.createElement('canvas');
  canvas.width = 1920;
  canvas.height = 1080;
  const g = canvas.getContext('2d');
  const ac = new AudioContext();
  const mix = ac.createMediaStreamDestination();
  const sources = new Map(); // track id -> source node
  const syncAudio = () => {
    const tracks = [];
    for (const [id, stream] of state.remote) if (id !== state.me) tracks.push(...stream.getAudioTracks());
    const mine = state.denoiser?.track ?? state.localStream?.getAudioTracks?.()[0];
    if (mine) tracks.push(mine);
    const live = new Set(tracks.map((t) => t.id));
    for (const t of tracks) {
      if (sources.has(t.id) || t.readyState !== 'live') continue;
      const src = ac.createMediaStreamSource(new MediaStream([t]));
      src.connect(mix);
      sources.set(t.id, src);
    }
    for (const [id, src] of sources) if (!live.has(id)) { src.disconnect(); sources.delete(id); }
  };
  syncAudio();
  const draw = () => drawComposite(g, canvas.width, canvas.height);
  draw();
  const stream = new MediaStream([...canvas.captureStream(30).getVideoTracks(), ...mix.stream.getAudioTracks()]);
  let mr;
  try {
    // A target, not a ceiling: the encoder spends less on a still screen.
    mr = new MediaRecorder(stream, { ...(mime ? { mimeType: mime } : {}), videoBitsPerSecond: 8_000_000 });
  } catch {
    toast("Couldn't start recording in this browser.", 'bad');
    send((w) => encodeRecSet(w, REC.recording, false));
    return;
  }
  const sink = writable ? null : new ChunkSink();
  let writing = Promise.resolve();
  const rec = { mr, mime: mr.mimeType || mime || 'video/webm', started: new Date(), capped: false, done: null };
  mr.ondataavailable = (e) => {
    if (!e.data?.size) return;
    if (writable) writing = writing.then(() => writable.write(e.data));
    else if (!sink.push(e.data) && !rec.capped) {
      rec.capped = true;
      toast('The recording reached the size this browser can hold, so it was stopped and saved.', 'bad');
      send((w) => encodeRecSet(w, REC.recording, false));
      stopLocalRecording();
    }
  };
  rec.done = new Promise((resolve) => {
    mr.onstop = async () => {
      clearInterval(rec.drawTimer);
      clearInterval(rec.audioTimer);
      ac.close().catch(() => {});
      stream.getTracks().forEach((t) => t.stop());
      if (writable) {
        try {
          await writing;
          await writable.close();
          toast('Recording saved to your computer.');
        } catch {
          toast("Couldn't save the recording.", 'bad');
        }
      } else {
        const blob = new Blob(sink.chunks, { type: rec.mime });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = recFileName(rec.started, rec.mime);
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 60_000);
        toast('Recording saved to your computer.');
      }
      resolve();
    };
  });
  rec.drawTimer = setInterval(draw, 1000 / 30);
  rec.audioTimer = setInterval(syncAudio, 1000);
  mr.start(1000);
  state.recorder = rec;
  toast("Recording has started. It'll be saved to your computer.");
}

// Stops and saves; resolves once the file is written or handed over.
function stopLocalRecording() {
  const rec = state.recorder;
  if (!rec) return Promise.resolve();
  state.recorder = null;
  if (rec.mr.state !== 'inactive') rec.mr.stop();
  return rec.done;
}

// One frame of the composite: the stage as this screen lays it out, scaled
// into 16:9, tiles, video, the whiteboard and names.
function drawComposite(g, W, H) {
  const stage = $('stage');
  g.fillStyle = '#0b0f14';
  g.fillRect(0, 0, W, H);
  if (!stage) return;
  const sr = stage.getBoundingClientRect();
  if (!sr.width || !sr.height) return;
  const k = Math.min(W / sr.width, H / sr.height);
  const ox = (W - sr.width * k) / 2, oy = (H - sr.height * k) / 2;
  const place = (r) => [ox + (r.left - sr.left) * k, oy + (r.top - sr.top) * k, r.width * k, r.height * k];
  for (const t of stage.querySelectorAll('.strip-tile, .stage-video:not(.hidden)')) {
    const r = t.getBoundingClientRect();
    if (!r.width) continue;
    const [x, y, w, h] = place(r);
    g.fillStyle = '#1b2330';
    g.fillRect(x, y, w, h);
  }
  for (const v of stage.querySelectorAll('video')) {
    if (v.readyState < 2 || !v.videoWidth) continue;
    const r = v.getBoundingClientRect();
    if (!r.width || !r.height) continue;
    const [x, y, w, h] = place(r);
    const fit = getComputedStyle(v).objectFit;
    const vr = v.videoWidth / v.videoHeight, br = w / h;
    if (fit === 'cover') {
      // Crop the source to the box's shape.
      let sw = v.videoWidth, sh = v.videoHeight;
      if (vr > br) sw = sh * br; else sh = sw / br;
      g.drawImage(v, (v.videoWidth - sw) / 2, (v.videoHeight - sh) / 2, sw, sh, x, y, w, h);
    } else {
      let dw = w, dh = h;
      if (vr > br) dh = w / vr; else dw = h * vr;
      g.drawImage(v, x + (w - dw) / 2, y + (h - dh) / 2, dw, dh);
    }
  }
  const wb = $('wb-canvas');
  if (wb && !$('wb').classList.contains('hidden')) {
    const r = wb.getBoundingClientRect();
    if (r.width) {
      const [x, y, w, h] = place(r);
      g.drawImage(wb, x, y, w, h);
    }
  }
  g.font = `${Math.round(14 * k)}px system-ui, sans-serif`;
  g.textBaseline = 'middle';
  for (const n of stage.querySelectorAll('.strip-name, .chip-name')) {
    const r = n.getBoundingClientRect();
    if (!r.width || !n.textContent) continue;
    const [x, y, , h] = place(r);
    const tw = g.measureText(n.textContent).width;
    g.fillStyle = 'rgba(0,0,0,0.55)';
    g.fillRect(x - 6, y, tw + 12, h);
    g.fillStyle = '#fff';
    g.fillText(n.textContent, x, y + h / 2);
  }
}

// ---------------------------------------------------------------------------
// Video effects (effects.js)
//
// The camera's track passes through camTrackFor wherever it is acquired, as
// the microphone passes through micTrackFor. With no effect it is returned as
// it is: no pipeline, no cost. With one, the published track is the
// pipeline's canvas, and stopping that track stops the pipeline and the
// camera behind it -- so every place that turns the camera off (and turns its
// light off) needs no knowledge of effects.
// ---------------------------------------------------------------------------

async function loadFxImages() {
  const want = state.fx.background === 'image' && state.fx.image.startsWith('img-') ? state.fx.image : '';
  if (!want || state.fxImages.has(want)) return;
  const rec = (await fxListImages()).find((r) => r.id === want);
  if (rec) state.fxImages.set(want, await createImageBitmap(rec.blob));
}

async function camTrackFor(raw) {
  if (!raw || !fxNeedsPipeline(state.fx)) {
    markFx();
    return raw;
  }
  await loadFxImages().catch(() => {});
  state.fxStatus = 'none';
  const pipe = new EffectsPipeline(raw, state.fx, { images: state.fxImages, onStatus: onFxStatus });
  const out = pipe.track;
  const stopOut = out.stop.bind(out);
  // Releasing hands the camera back without stopping it ("None" chosen).
  out.release = () => {
    stopOut();
    pipe.stop();
    if (state.fxPipe === pipe) state.fxPipe = null;
    markFx();
    return raw;
  };
  out.stop = () => {
    out.release();
    raw.stop();
  };
  state.fxPipe = pipe;
  markFx();
  return out;
}

// What is running, for the person's panel and for the live checks:
// data-fx="off" | "<background>:<segmenter status>".
function markFx() {
  document.documentElement.dataset.fx = state.fxPipe ? `${state.fx.background}:${state.fxStatus}` : 'off';
}

function onFxStatus(status) {
  state.fxStatus = status;
  markFx();
  if (status === 'failed') toast("Background effects aren't available on this device. Your background stays blurred.", 'bad');
  if (state.sideView === 'effects') renderEffects($('view-effects'));
}

// Swap the published camera track: the stream, the WebRTC sender, the socket
// encoder, the pre-join preview. While a screen is shared the sender carries
// the screen; the camera is picked up from the stream when sharing stops.
async function publishCam(oldT, newT) {
  const ls = state.localStream;
  if (!ls) return;
  ls.removeTrack(oldT);
  ls.addTrack(newT);
  if (!state.sharing && state.room && state.media) await state.media.ensureVideo(newT);
  if (!state.sharing && state.wsMediaOn && state.wsSender) await state.wsSender.setVideoTrack(newT);
  const pv = $('pj-video');
  if (pv && pv.srcObject === ls) pv.srcObject = ls;
  renderStage();
}

async function applyEffects(next) {
  state.fx = normalizeFx(next);
  saveFx(state.fx);
  await loadFxImages().catch(() => {});
  const cur = state.localStream?.getVideoTracks()[0];
  if (cur && cur.readyState === 'live') {
    const pipe = state.fxPipe;
    if (pipe && cur === pipe.track) {
      if (fxNeedsPipeline(state.fx)) pipe.set(state.fx);
      else await publishCam(cur, cur.release());
    } else if (fxNeedsPipeline(state.fx)) {
      await publishCam(cur, await camTrackFor(cur));
    }
  }
  markFx();
  if (state.sideView === 'effects') renderEffects($('view-effects'));
  if ($('fx-sheet')) renderEffects($('fx-sheet-body'));
}

// The picker, in the meeting's side panel and in the pre-join's sheet.
async function renderEffects(host) {
  if (!host) return;
  const fx = state.fx;
  const mine = await fxListImages();
  const tile = (id, label, inner, on, extra = '') =>
    `<button type="button" class="fx-tile${on ? ' on' : ''}" data-fx="${id}" aria-pressed="${on}" title="${esc(label)}">${inner}<span class="fx-tile-label">${esc(label)}</span></button>${extra}`;
  const bg = fx.background;
  const status =
    fx.background === 'none' ? '' : state.fxStatus === 'loading' ? 'Getting background effects ready… your background is blurred until then.' : state.fxStatus === 'failed' ? "Background effects aren't available on this device. Your background stays blurred." : '';
  host.innerHTML = `
    <div class="fx-body">
      <div class="fx-preview${state.mirrorSelf ? ' mirror' : ''}"><video muted autoplay playsinline></video><span class="fx-preview-off">Camera is off</span></div>
      ${status ? `<p class="fx-status">${esc(status)}</p>` : ''}
      <h3 class="fx-h">Background effects</h3>
      <div class="fx-grid" role="group" aria-label="Background effects">
        ${tile('none', 'None', '<span class="fx-ico fx-none"></span>', bg === 'none')}
        ${tile('blur', 'Standard blur', '<span class="fx-ico fx-blur"></span>', bg === 'blur')}
        ${tile('portrait', 'Portrait blur', '<span class="fx-ico fx-portrait"></span>', bg === 'portrait')}
        ${FX_PRESETS.map((p) => tile(`preset:${p.id}`, p.label, `<span class="fx-ico" style="background:${p.color}"></span>`, bg === 'image' && fx.image === p.id)).join('')}
        ${mine
          .map((r) => {
            const url = URL.createObjectURL(r.blob);
            return `<span class="fx-mine">${tile(`image:${r.id}`, 'Your image', `<img class="fx-ico" alt="" src="${url}">`, bg === 'image' && fx.image === r.id)}<button type="button" class="fx-remove" data-remove="${r.id}" aria-label="Remove this image">&times;</button></span>`;
          })
          .join('')}
        ${mine.length < FX_MAX_IMAGES ? `<label class="fx-tile fx-add" title="Add new"><input type="file" accept="image/png,image/jpeg,image/webp" hidden data-add><span class="fx-ico fx-plus" aria-hidden="true">+</span><span class="fx-tile-label">Add new</span></label>` : ''}
      </div>
      <p class="fx-note">Your images stay on this device. They are never uploaded.</p>
      <h3 class="fx-h">Filters</h3>
      <label class="fx-switch"><input type="checkbox" data-filter="softFocus"${fx.softFocus ? ' checked' : ''}><span>Soft focus</span></label>
      <label class="fx-switch"><input type="checkbox" data-filter="brightness"${fx.brightness ? ' checked' : ''}><span>Adjust brightness</span></label>
      <h3 class="fx-h">Your view</h3>
      <label class="fx-switch"><input type="checkbox" data-mirror${state.mirrorSelf ? ' checked' : ''}><span>Mirror my video</span></label>
      <p class="fx-note">Only you see yourself mirrored. Others always see you the right way round.</p>
    </div>`;
  const pv = host.querySelector('.fx-preview video');
  const cam = state.localStream?.getVideoTracks()[0];
  if (cam && cam.readyState === 'live') pv.srcObject = new MediaStream([cam]);
  host.querySelector('.fx-preview').classList.toggle('off', !(cam && cam.readyState === 'live'));
  host.querySelectorAll('[data-fx]').forEach((b) =>
    b.addEventListener('click', () => {
      const v = b.dataset.fx;
      if (v.startsWith('preset:')) applyEffects({ ...state.fx, background: 'image', image: v.slice(7) });
      else if (v.startsWith('image:')) applyEffects({ ...state.fx, background: 'image', image: v.slice(6) });
      else applyEffects({ ...state.fx, background: v, image: '' });
    })
  );
  host.querySelectorAll('[data-filter]').forEach((c) =>
    c.addEventListener('change', () => applyEffects({ ...state.fx, [c.dataset.filter]: c.checked }))
  );
  host.querySelector('[data-mirror]')?.addEventListener('change', (e) => {
    state.mirrorSelf = e.target.checked;
    writePref('aether.mirror', state.mirrorSelf);
    host.querySelector('.fx-preview').classList.toggle('mirror', state.mirrorSelf);
    $('pj-video')?.classList.toggle('unmirrored', !state.mirrorSelf);
    renderStage();
  });
  host.querySelector('[data-add]')?.addEventListener('change', async (e) => {
    const f = e.target.files?.[0];
    if (!f) return;
    try {
      const rec = await fxAddImage(f);
      await applyEffects({ ...state.fx, background: 'image', image: rec.id });
    } catch (err) {
      toast(err?.message ?? 'Could not add that image.', 'bad');
    }
  });
  host.querySelectorAll('[data-remove]').forEach((b) =>
    b.addEventListener('click', async () => {
      const id = b.dataset.remove;
      await fxRemoveImage(id);
      state.fxImages.get(id)?.close?.();
      state.fxImages.delete(id);
      if (state.fx.image === id) await applyEffects({ ...state.fx, background: 'none', image: '' });
      else renderEffects(host);
    })
  );
}

// The pre-join's "Background filters": the same picker, as a sheet.
function openEffectsSheet() {
  const host = $('dialog');
  if (!host || !host.classList.contains('hidden')) return;
  host.innerHTML = `
    <div class="dialog-card fx-sheet" id="fx-sheet" role="dialog" aria-labelledby="fx-sheet-title">
      <h2 id="fx-sheet-title">Background filters</h2>
      <div id="fx-sheet-body"></div>
      <div class="dialog-actions"><span class="spacer"></span><button type="button" class="btn btn-primary" data-yes>Done</button></div>
    </div>`;
  host.classList.remove('hidden');
  renderEffects($('fx-sheet-body'));
  host.querySelector('[data-yes]').onclick = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
  };
}

// ---------------------------------------------------------------------------
// Together mode (together.js)
//
// The organizer applies it for everyone. Each client then sends the outline
// of the person in its own picture -- only when it changes, at most ten times
// a second -- and draws everyone, cut out by their outlines, in one scene.
// ---------------------------------------------------------------------------

let togTimer = null;
let togRaf = 0;
let togSent = null; // the last outline sent, as its bytes' string
let togSeg = null; // { video, seg } when segmenting the raw camera ourselves
const togVideos = new Map(); // stream id -> hidden <video>, for drawing

function onTogetherState(m) {
  const was = state.together.on;
  state.together = { on: m.on, scene: m.scene, by: m.by };
  if (m.on && !was) toast(`${m.by || 'The organizer'} selected together mode for everyone`);
  if (!m.on && was) toast('Together mode has stopped');
  syncTogether();
}

function syncTogether() {
  const on = state.together.on;
  const cv = $('together');
  if (cv) cv.hidden = !on;
  if (on && !togTimer) {
    togSent = null;
    togTimer = setInterval(sendOutline, 100);
  } else if (!on && togTimer) {
    clearInterval(togTimer);
    togTimer = null;
    state.outlines.clear();
    state.myOutline = null;
    if (togSeg) {
      togSeg.video.remove();
      togSeg = null;
    }
  }
  if (on && !togRaf) togRaf = requestAnimationFrame(drawTogether);
  syncViewMenu();
}

// The outline of the person in this machine's picture: from the effects
// pipeline's mask when one is running, else from a segmentation of the
// camera made here, for this.
async function myConfidence() {
  const pipe = state.fxPipe;
  if (pipe?.mask && pipe.maskCanvas?.width) return { conf: pipe.mask, w: pipe.maskCanvas.width, h: pipe.maskCanvas.height };
  const cam = state.localStream?.getVideoTracks()[0];
  if (!cam || cam.readyState !== 'live' || !state.cam) return null;
  if (!togSeg) {
    const video = document.createElement('video');
    video.muted = true;
    video.playsInline = true;
    Object.assign(video.style, { position: 'fixed', left: '-10000px', width: '2px', height: '2px', opacity: '0' });
    document.body.appendChild(video);
    togSeg = { video, seg: null, track: null };
    loadSegmenter().then((s) => togSeg && (togSeg.seg = s), () => {});
  }
  if (togSeg.track !== cam) {
    togSeg.track = cam;
    togSeg.video.srcObject = new MediaStream([cam]);
    togSeg.video.play().catch(() => {});
  }
  const v = togSeg.video;
  if (!togSeg.seg || v.readyState < 2) return null;
  const res = togSeg.seg.segmentForVideo(v, performance.now());
  const m = res.confidenceMasks?.[0];
  const out = m ? { conf: m.getAsFloat32Array(), w: m.width, h: m.height } : null;
  res.close?.();
  return out;
}

let togBusy = false;
async function sendOutline() {
  if (togBusy || !state.together.on) return;
  togBusy = true;
  try {
    const c = state.cam ? await myConfidence() : null;
    if (!c) {
      state.myOutline = null;
      return;
    }
    const bits = togQuantize(c.conf, c.w, c.h, state.myOutline);
    state.myOutline = bits;
    const rle = togEncode(bits);
    const key = rle.join(',');
    // Nothing changed, nothing sent: a person sitting still costs nothing.
    if (key === togSent || rle.length > TOG_MAX) return;
    togSent = key;
    send((w) => encodeTogOutline(w, rle));
  } finally {
    togBusy = false;
  }
}

// The scene, flat, in the app's neutrals: a wall, and in front of each row a
// desk the people sit behind.
const TOG_LOOK = [
  { wall: '#1b2330', floor: '#141a22', tier: '#202a37', desk: '#2b3440', edge: '#3a4654' }, // auditorium
  { wall: '#2e2822', floor: '#211c18', tier: '#2a231d', desk: '#4a3b2e', edge: '#5c4a3a' }, // café
  { wall: '#1d2a30', floor: '#151e22', tier: '#1a252b', desk: '#33444c', edge: '#41565f' }, // conference room
];

function togVideoFor(stream) {
  if (!stream) return null;
  let v = togVideos.get(stream.id);
  if (!v) {
    v = document.createElement('video');
    v.muted = true;
    v.playsInline = true;
    v.autoplay = true;
    Object.assign(v.style, { position: 'fixed', left: '-10000px', width: '2px', height: '2px', opacity: '0' });
    v.srcObject = stream;
    document.body.appendChild(v);
    v.play().catch(() => {});
    togVideos.set(stream.id, v);
  }
  return v;
}

const togMask = Object.assign(document.createElement('canvas'), { width: TOG_W, height: TOG_H });
const togMaskCtx = togMask.getContext('2d');
const togMaskData = togMaskCtx.createImageData(TOG_W, TOG_H);
const togPerson = document.createElement('canvas');

function drawTogether() {
  togRaf = 0;
  if (!state.together.on) {
    for (const v of togVideos.values()) v.remove();
    togVideos.clear();
    return;
  }
  togRaf = requestAnimationFrame(drawTogether);
  const cv = $('together');
  const r = cv.getBoundingClientRect();
  const dpr = Math.min(2, devicePixelRatio || 1);
  const W = Math.max(1, Math.round(r.width * dpr)), H = Math.max(1, Math.round(r.height * dpr));
  if (cv.width !== W || cv.height !== H) {
    cv.width = W;
    cv.height = H;
  }
  const g = cv.getContext('2d');
  const look = TOG_LOOK[state.together.scene] ?? TOG_LOOK[0];
  g.fillStyle = look.floor;
  g.fillRect(0, 0, W, H);
  // The scene at 16:9, centred; on a screen held upright, at 3:4 with fewer
  // people to a row (together.js, PORTRAIT).
  const portrait = H > W;
  const ar = portrait ? TOG_PORTRAIT : 16 / 9;
  const sw = Math.min(W, H * ar), sh = sw / ar, ox = (W - sw) / 2, oy = (H - sh) / 2;
  g.fillStyle = look.wall;
  g.fillRect(ox, oy, sw, sh * 0.7);
  const people = participants().filter((p) => !p.presentation);
  const st = togSeats(state.together.scene, people.length, portrait);
  const rows = new Map();
  people.forEach((p, i) => {
    const s = st[i];
    if (!rows.has(s.row)) rows.set(s.row, []);
    rows.get(s.row).push({ p, s });
  });
  // Back rows first, so the front ones sit in front of them. Each row stands
  // on its own tier, a little lighter than the wall: the room's depth.
  for (const row of [...rows.keys()].sort((a, b) => b - a)) {
    const list = rows.get(row);
    {
      const s0 = list[0].s;
      const ty = oy + (s0.y + s0.h * 0.9) * sh;
      g.fillStyle = look.tier;
      g.fillRect(ox, ty, sw, Math.max(4, s0.h * sh * 0.5));
    }
    for (const { p, s } of list) {
      const x = ox + s.x * sw, y = oy + s.y * sh, w = s.w * sw, h = s.h * sh;
      const video = p.stream && liveVideo(p) ? togVideoFor(p.stream) : null;
      const bits = p.self ? state.myOutline : state.outlines.get(p.id);
      if (video && video.readyState >= 2 && video.videoWidth) {
        const pw = Math.round(w), ph = Math.round((w * 9) / 16);
        if (togPerson.width !== pw || togPerson.height !== ph) {
          togPerson.width = pw;
          togPerson.height = ph;
        }
        const pc = togPerson.getContext('2d');
        pc.globalCompositeOperation = 'copy';
        pc.drawImage(video, 0, 0, pw, ph);
        if (bits) {
          const d = togMaskData.data;
          for (let i = 0, j = 3; i < bits.length; i++, j += 4) d[j] = bits[i] ? 255 : 0;
          togMaskCtx.putImageData(togMaskData, 0, 0);
          pc.globalCompositeOperation = 'destination-in';
          pc.imageSmoothingEnabled = true;
          pc.filter = `blur(${Math.max(1, pw / 160)}px)`;
          pc.drawImage(togMask, 0, 0, pw, ph);
          pc.filter = 'none';
          g.drawImage(togPerson, x, y + h - ph);
        } else {
          // No outline yet: the picture in a rounded frame.
          g.save();
          g.beginPath();
          g.roundRect(x, y + h - ph, pw, ph, Math.min(12, pw / 12));
          g.clip();
          g.drawImage(togPerson, x, y + h - ph);
          g.restore();
        }
      } else {
        const rad = Math.min(w, h) * 0.24;
        g.fillStyle = '#244a46';
        g.beginPath();
        g.arc(x + w / 2, y + h - rad * 1.6, rad, 0, Math.PI * 2);
        g.fill();
        g.fillStyle = '#9fd8cf';
        g.font = `600 ${Math.round(rad * 0.8)}px system-ui, sans-serif`;
        g.textAlign = 'center';
        g.textBaseline = 'middle';
        g.fillText(initials(faceOf(p)), x + w / 2, y + h - rad * 1.6);
      }
    }
    // The desk this row sits behind, and the names on it.
    const first = list[0].s, last = list[list.length - 1].s;
    const dx = ox + (first.x - first.w * 0.06) * sw, dw = (last.x + last.w * 1.06 - first.x + first.w * 0.06) * sw;
    const dy = oy + (first.y + first.h * 0.86) * sh, dh = Math.max(6, first.h * sh * 0.16);
    g.fillStyle = look.desk;
    g.beginPath();
    g.roundRect(dx, dy, dw, dh, Math.min(10, dh / 2));
    g.fill();
    g.fillStyle = look.edge;
    g.fillRect(dx, dy, dw, Math.max(2, dh * 0.12));
    g.fillStyle = '#e7ecef';
    // Names: never squeezed to fit. The whole name if it fits the seat at a
    // readable size, else the first name, else the initials.
    const px = Math.max(11 * dpr, Math.round(dh * 0.42));
    g.font = `500 ${px}px system-ui, sans-serif`;
    g.textAlign = 'center';
    g.textBaseline = 'middle';
    for (const { p, s } of list) {
      const room = s.w * sw * 1.08;
      const full = p.self ? 'You' : p.name || '';
      const label = [full, full.split(/\s+/)[0], initials(faceOf(p))].find((t) => g.measureText(t).width <= room) ?? '';
      g.fillText(label, ox + (s.x + s.w / 2) * sw, dy + dh * 0.58);
    }
  }
}

// View > Together mode. The organizer chooses it, and the scene, for everyone.
async function openTogether() {
  const mayApply = state.isHost;
  if (!mayApply) {
    toast(state.together.on ? 'Only the organizer can change the scene when Together Mode is selected for everyone.' : 'Only the organizer can choose Together mode for everyone.');
    return;
  }
  const on = state.together.on;
  const v = await formDialog(
    on ? 'Change scene' : 'Together mode',
    `<p class="mo-note">Everyone in the meeting will see each other in one scene. The scene will change for everyone.</p>
     <div class="mo-row"><span>Scene</span><select class="input" name="scene">${TOG_SCENES.map((s) => `<option value="${s.id}"${s.id === state.together.scene ? ' selected' : ''}>${esc(s.name)}</option>`).join('')}</select></div>
     ${on ? '<label class="mo-row"><span>Stop Together mode for everyone</span><input type="checkbox" name="stop"></label>' : ''}`,
    on ? 'Apply' : 'Start for everyone'
  );
  if (!v) return;
  const stop = v.stop === 'on' || v.stop === true;
  send((w) => encodeTogSet(w, !stop, stop ? 0 : Number(v.scene) || 0));
}

// The transcript panel: the session's lines, live while it is transcribed.
const transcriptState = { lines: [], loaded: false, loading: false, error: '' };

async function loadTranscript() {
  if (transcriptState.loading || !state.room) return;
  transcriptState.loading = true;
  try {
    const res = await transcriptApi.get(state.room, state.session);
    // Lines that arrived live while this was loading are already in the
    // server's answer; the answer wins.
    transcriptState.lines = (res.lines ?? []).map((l) => ({ name: l.name, text: l.text, at: Date.parse(l.at) }));
    transcriptState.error = '';
    transcriptState.loaded = true;
  } catch (e) {
    transcriptState.error = 'Transcript is unavailable';
  }
  transcriptState.loading = false;
  renderTranscript();
}

function addTranscriptLine(name, text, at) {
  if (!transcriptState.loaded) return; // the load will bring it
  transcriptState.lines.push({ name, text, at });
  if (transcriptState.lines.length > 20000) transcriptState.lines.shift();
  if (state.sideView === 'transcript') renderTranscript(true);
}

function renderTranscript(append = false) {
  const view = $('view-transcript');
  if (!view) return;
  const rec = state.rec;
  const start = rec.trSince || state.session;
  const time = (at) => recElapsed(at - state.session);
  const lineHTML = (l) => `<li class="tr-line"><div class="tr-meta"><b>${esc(l.name)}</b><span>${time(l.at)}</span></div><p>${esc(l.text)}</p></li>`;
  if (append && view.querySelector('.tr-list')) {
    const list = view.querySelector('.tr-list');
    const stick = list.scrollTop + list.clientHeight >= list.scrollHeight - 8;
    list.querySelector('.tr-empty')?.remove();
    list.insertAdjacentHTML('beforeend', lineHTML(transcriptState.lines[transcriptState.lines.length - 1]));
    if (stick) list.scrollTop = list.scrollHeight;
    return;
  }
  const base = `/api/scheduling/rooms/${encodeURIComponent(state.room)}/transcript?session=${state.session}`;
  const mayControl = state.isHost || state.myRole >= Role.Moderator;
  const status = rec.transcribing ? `Transcription started by ${esc(rec.trBy || 'someone')}` : transcriptState.lines.length ? 'Transcription has stopped' : 'No meeting transcript';
  const mine =
    state.speechConsent === true ? 'Your speech is included.' : state.speechConsent === false ? 'Your speech is not included.' : '';
  view.innerHTML = `
    <div class="tr-head">
      <p class="tr-status">${status}${start && rec.transcribing ? ` · ${recElapsed(Date.now() - start)}` : ''}</p>
      <div class="tr-actions">
        ${mayControl ? `<button type="button" class="btn sm ${rec.transcribing ? '' : 'btn-primary'}" id="tr-toggle">${rec.transcribing ? 'Stop transcription' : 'Start transcription'}</button>` : ''}
        <a class="btn sm" id="tr-vtt" href="${base}&format=vtt" download="transcript.vtt">Download .vtt</a>
        <a class="btn sm" id="tr-txt" href="${base}&format=txt" download="transcript.txt">Download .txt</a>
      </div>
      ${mine && rec.transcribing ? `<p class="tr-mine">${mine} <button type="button" class="link" id="tr-consent">${state.speechConsent ? 'Stop including it' : 'Include it'}</button></p>` : ''}
    </div>
    <ol class="tr-list" aria-live="polite">${
      transcriptState.error
        ? `<li class="tr-empty">${transcriptState.error}</li>`
        : transcriptState.lines.length
        ? transcriptState.lines.map(lineHTML).join('')
        : `<li class="tr-empty">${rec.transcribing ? 'Wait a moment... Transcription will be available shortly.' : 'No meeting transcript'}</li>`
    }</ol>`;
  $('tr-toggle')?.addEventListener('click', toggleTranscription);
  $('tr-consent')?.addEventListener('click', () => {
    state.speechConsent = !state.speechConsent;
    syncSpeech();
    renderTranscript();
  });
  const list = view.querySelector('.tr-list');
  list.scrollTop = list.scrollHeight;
}

function noteCaption(who, text) {
  const name = who === state.me ? 'You' : state.people.get(who)?.name ?? 'Someone';
  if (!text) state.captionLines.delete(who);
  else state.captionLines.set(who, { name, text, at: Date.now() });
  renderCaptions();
}

function renderCaptions() {
  const bar = $('caption');
  if (!bar || !state.captions) return;
  const lines = visibleLines(state.captionLines);
  if (!lines.length) {
    bar.textContent = !captionsSupported() ? "This browser cannot recognise speech. Other people's captions still appear here." : state.recogniser?.running ? 'Listening…' : '';
    return;
  }
  bar.innerHTML = lines
    .map(
      (l) =>
        `<div class="caption-line"><b>${esc(l.name)}</b> ${esc(l.text)}</div>`
    )
    .join('');
}

// Lines age out on their own, so a sentence nobody followed does not sit on
// the video for the rest of the meeting.
setInterval(() => {
  if (state.captions && state.captionLines.size) renderCaptions();
}, 1000);

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// Ten, in two rows of five: the six that were here, plus the ones people
// reached for and did not find (sad, fire, thanks, eyes).
const REACTIONS = ['\u{1F44D}', '\u{2764}\u{FE0F}', '\u{1F602}', '\u{1F62E}', '\u{1F622}', '\u{1F44F}', '\u{1F389}', '\u{1F525}', '\u{1F64F}', '\u{1F440}'];

function sendReaction(emoji) {
  // One every half second. A held-down key should not become a frame storm,
  // and nobody can read them faster than that anyway.
  const now = Date.now();
  if (now - state.reactionAt < 500) return;
  state.reactionAt = now;
  send((w) => {
    w.header(Op.Reaction);
    w.str(emoji);
  });
}

// A reaction floats up over the stage and removes itself. It is never stored:
// a gesture during a moment is not a record, and a log of them is neither
// wanted nor kept.
function showReaction(who, emoji) {
  const stage = document.querySelector('.stage') ?? document.body;
  const el = document.createElement('div');
  el.className = 'reaction-float';
  const name = who === state.me ? 'You' : state.people.get(who)?.name ?? '';
  el.innerHTML = `<span class="reaction-emoji">${esc(emoji)}</span>${
    name ? `<span class="reaction-who">${esc(name)}</span>` : ''
  }`;
  el.style.left = 12 + Math.random() * 60 + '%';
  el.style.setProperty('--drift', Math.round(-30 + Math.random() * 60) + 'px');
  stage.appendChild(el);
  setTimeout(() => el.remove(), 2600);
}

// The other person in a one-to-one call, if this tab placed or accepted it.
function callPeerFor(room) {
  try {
    return sessionStorage.getItem('aether.callWith.' + room) || '';
  } catch {
    return '';
  }
}

function pickChat(to) {
  state.chatTo = to;
  state.chatToPicked = true;
  state.chatToAuto = false;
  if (to === 'everyone') {
    state.roomUnread = 0;
    if (state.meetingConv?.convId) messaging.markRead(state.meetingConv.convId).catch(() => {});
  }
  const dm = state.dms.get(to);
  if (dm) dm.unread = 0;
  renderChatTo();
  renderChat();
}

function renderChatTo() {
  const sel = $('chat-to');
  if (!sel) return;
  const others = [...state.people.values()].filter(
    (p) => p.id !== state.me && p.userId
  );
  const chatBefore = state.chatTo;

  // Two signed-in people in a room ARE a conversation. Whether they got here
  // by a ring or by a link, the panel opens on the two of them -- the kept
  // thread with its history -- unless the person has chosen otherwise. The
  // other choice, the call chat, is still there and ends with the call.
  const oneToOne = others.length === 1 && !state.meetingConv?.convId;
  if (oneToOne && !state.chatToPicked && state.chatTo === 'everyone') {
    state.chatTo = others[0].userId;
    state.chatToAuto = true;
  }
  // ...and stop being one when a third person arrives. The automatic choice
  // used to stay, so in a three-person meeting the panel still wrote to one
  // of them privately -- a message meant for the room went to a person, and
  // the other's messages never showed. What the person chose stays chosen.
  if (!oneToOne && state.chatToAuto && !state.chatToPicked) {
    state.chatTo = 'everyone';
    state.chatToAuto = false;
  }
  if (others.some((p) => p.userId === state.callPeer)) state.callPeerSeen = true;
  const awaitingPeer = state.chatTo === state.callPeer && !state.callPeerSeen;
  if (state.chatTo !== 'everyone' && !awaitingPeer && !others.some((p) => p.userId === state.chatTo)) {
    state.chatTo = 'everyone';
    state.chatToPicked = false;
  }

  // One-to-one: two tabs above the messages -- your conversation, saved,
  // and this call's chat, gone when it ends -- instead of a dropdown in the
  // composer. A group keeps the picker: there, the choice is a list.
  const tabs = $('chat-tabs');
  if (tabs) {
    tabs.hidden = !oneToOne;
    if (oneToOne) {
      const p = others[0];
      const dmUnread = state.dms.get(p.userId)?.unread;
      const tab = (to, label, sub, unread) =>
        `<button type="button" role="tab" class="chat-tab" data-chat="${esc(to)}" aria-selected="${state.chatTo === to}">` +
        `<span class="chat-tab-label">${label}${unread ? '<span class="chat-tab-dot" aria-label="unread"></span>' : ''}</span>` +
        `<span class="chat-tab-sub">${sub}</span></button>`;
      tabs.innerHTML =
        tab(p.userId, `Chat with ${esc(p.name)}`, 'Saved', dmUnread) +
        tab('everyone', 'This call only', 'Deleted after the call', state.roomUnread);
    }
  }
  sel.hidden = others.length === 0 || oneToOne;
  const everyoneLabel = state.meetingConv?.convId ? 'Meeting chat' : oneToOne ? 'Call chat · not kept' : 'Everyone';
  sel.innerHTML =
    `<option value="everyone">${everyoneLabel}${state.roomUnread ? ' •' : ''}</option>` +
    others
      .map(
        (p) =>
          `<option value="${esc(p.userId)}">${esc(p.name)}${oneToOne ? ' · kept' : ''}${
            state.dms.get(p.userId)?.unread ? ' •' : ''
          }</option>`
      )
      .join('');
  sel.value = state.chatTo;

  const title = $('side-title');
  if (title && $('view-chat')?.classList.contains('active')) {
    const who = others.find((p) => p.userId === state.chatTo);
    title.textContent = who ? who.name : state.meetingConv?.convId ? 'Meeting chat' : oneToOne ? 'Call chat' : 'Group chat';
  }
  const input = $('chat-input');
  if (input) {
    const who = others.find((p) => p.userId === state.chatTo);
    input.placeholder = who
      ? `Message ${who.name}… (kept after the call)`
      : state.meetingConv?.convId
        ? 'Message the meeting… (kept after it ends)'
        : oneToOne
          ? 'Message here… (ends with the call)'
          : 'Message everyone…';
  }
  // Which chat is chosen changed under the log (the roster arrived, or the
  // other person left): draw the chat now chosen. Without this the tab said
  // "Chat with A" over the call chat's empty log until something else
  // redrew it -- call.e2e.mjs caught it, 2 runs in 3.
  if (state.chatTo !== chatBefore) renderChat();
}

// Three or more signed-in people make the room a meeting, and the server
// gives it a conversation the moment the third arrives. The roster and that
// creation race by a few milliseconds, so a miss is retried briefly.
async function ensureMeetingChat() {
  if (!state.room || state.meetingConv?.convId || state.meetingConv?.loading) return;
  const accounts = new Set([...state.people.values()].map((p) => p.userId).filter(Boolean));
  if (session.user?.id) accounts.add(session.user.id);
  if (accounts.size < 3) return;
  state.meetingConv = { convId: '', messages: [], loading: true };
  try {
    const conv = await messaging.meetingChat(state.room);
    const res = await messaging.messages(conv.id);
    state.meetingConv = { convId: conv.id, messages: res.messages ?? [], display: conv.display };
    state.meetingConvTries = 0;
    if (state.chatTo === 'everyone') {
      renderChat();
      if ($('view-chat')?.classList.contains('active')) messaging.markRead(conv.id).catch(() => {});
    }
    renderChatTo();
  } catch (e) {
    state.meetingConv = null;
    if (e instanceof ApiError && e.status === 404 && state.meetingConvTries < 5) {
      state.meetingConvTries++;
      setTimeout(ensureMeetingChat, 800 * state.meetingConvTries);
    }
  }
}

// One renderer for a persistent thread inside the meeting panel, used by
// the direct conversations and the meeting chat alike.
function renderPersistent(log, messages, emptyText) {
  if (!messages.length) {
    log.innerHTML = `<div class="empty">${emptyText}</div>`;
    return;
  }
  log.innerHTML = messages
    .map((m) => {
      if (m.system) {
        return `<div class="sys-row"><span>${esc(m.senderId === session.user?.id ? 'You' : m.senderName)} ${esc(m.body)}</span></div>`;
      }
      const mine = m.senderId === session.user?.id;
      const name = mine ? 'You' : m.senderName || 'Them';
      const face = { userId: m.senderId, name: m.senderName };
      return `<div class="msg ${mine ? 'mine' : ''}">
        <span ${avatarAttrs(face)}>${esc(initials(face))}</span>
        <div class="msg-body">
          <div class="msg-who">${esc(name)} · ${esc(clockHM(new Date(m.createdAt)))}</div>
          ${m.deleted ? '<div class="bubble gone">Message deleted</div>' : `<div class="bubble${isOnlyEmoji(m.body) ? ' jumbo' : ''}">${esc(m.body)}</div>`}
        </div>
      </div>`;
    })
    .join('');
  log.parentElement.scrollTop = log.parentElement.scrollHeight;
}

function renderChat() {
  const log = $('chat-log');
  if (!log) return;

  if (state.chatTo !== 'everyone') {
    renderDirect(log);
    return;
  }
  if (state.meetingConv?.convId) {
    renderPersistent(log, state.meetingConv.messages,
      'This is the meeting\'s own chat. It stays in your chat list after the meeting ends.');
    return;
  }

  if (!state.chat.length) {
    const two = [...state.people.values()].filter((p) => p.id !== state.me && p.userId).length === 1;
    log.innerHTML = `<div class="empty">${
      two
        ? 'Messages here are deleted when the call ends. Your saved conversation is the other tab.'
        : 'Messages here go to everyone in the meeting, and end with it.'
    }</div>`;
    return;
  }
  log.innerHTML = state.chat
    .map((m) => {
      const mine = m.who === state.me;
      const p = state.people.get(m.who);
      const name = mine ? 'You' : p?.name ?? 'Someone';
      // Keyed on the account, so somebody wears the same face in the meeting
      // panel as in the chat sidebar.
      const who = mine
        ? { userId: session.user?.id, name: session.user?.displayName, email: session.user?.email }
        : { userId: p?.userId, name: p?.name };
      return `<div class="msg ${mine ? 'mine' : ''}">
        <span ${avatarAttrs(who)}>${esc(initials(who))}</span>
        <div class="msg-body">
          <div class="msg-who">${esc(name)} · ${esc(clockHM(new Date(m.at)))}</div>
          <div class="bubble${isOnlyEmoji(m.text) ? ' jumbo' : ''}">${esc(m.text)}</div>
        </div>
      </div>`;
    })
    .join('');
  log.parentElement.scrollTop = log.parentElement.scrollHeight;
}

// The permanent conversation, drawn inside the meeting. Same messages as the
// sidebar thread, because it is the same conversation: opened by account id,
// stored on the server, still there tomorrow.
function renderDirect(log) {
  const dm = state.dms.get(state.chatTo);
  const who = [...state.people.values()].find((p) => p.userId === state.chatTo);
  if (!dm) {
    log.innerHTML = `<div class="empty">Opening your conversation…</div>`;
    loadDirect(state.chatTo);
    return;
  }
  if (!dm.messages.length) {
    log.innerHTML = `<div class="empty">No messages with ${esc(
      who?.name ?? 'them'
    )} yet. This conversation is saved in your chats.</div>`;
    return;
  }
  log.innerHTML = dm.messages
    .map((m) => {
      const mine = m.senderId === session.user?.id;
      const name = mine ? 'You' : m.senderName || who?.name || 'Them';
      const face = { userId: m.senderId, name: m.senderName };
      return `<div class="msg ${mine ? 'mine' : ''}">
        <span ${avatarAttrs(face)}>${esc(initials(face))}</span>
        <div class="msg-body">
          <div class="msg-who">${esc(name)} · ${esc(
            clockHM(new Date(m.createdAt))
          )}</div>
          <div class="bubble${isOnlyEmoji(m.body) ? ' jumbo' : ''}">${esc(m.body)}</div>
        </div>
      </div>`;
    })
    .join('');
  log.parentElement.scrollTop = log.parentElement.scrollHeight;
}

// Fetched once per person per meeting, then kept current by the chat push
// socket -- the same socket the sidebar uses. No polling: an idle
// conversation costs nothing, which is the rule the whole media path is
// built on.
async function loadDirect(userId) {
  if (!userId || state.dms.has(userId)) return;
  state.dms.set(userId, { convId: '', messages: [], unread: 0, loading: true });
  try {
    const { conv, messages } = await directWith(userId);
    state.dms.set(userId, { convId: conv.id, messages, unread: 0 });
  } catch {
    state.dms.delete(userId);
    const log = $('chat-log');
    if (log && state.chatTo === userId) {
      log.innerHTML = `<div class="empty">Could not open that conversation.</div>`;
    }
    return;
  }
  if (state.chatTo === userId) renderChat();
  renderChatTo();
}

// A message pushed into one of these conversations while the meeting is open.
addEventListener('aether:dm', (e) => {
  const m = e.detail;
  const mc = state.meetingConv;
  if (mc?.convId && mc.convId === m.conversationId) {
    const at = mc.messages.findIndex((x) => x.id === m.id);
    if (at >= 0) mc.messages[at] = m;
    else mc.messages.push(m);
    if (at < 0 && !m.system && m.senderId !== session.user?.id) showBubble(m.senderName || 'Someone', plainText(m.body ?? ''));
    const viewing = state.chatTo === 'everyone' && $('view-chat')?.classList.contains('active');
    if (viewing) {
      renderChat();
      if (!m.system && m.senderId !== session.user?.id) messaging.markRead(mc.convId).catch(() => {});
    } else if (!m.system && m.senderId !== session.user?.id) {
      state.roomUnread = (state.roomUnread ?? 0) + 1;
    }
    renderChatTo();
    return;
  }
  for (const [userId, dm] of state.dms) {
    if (dm.convId !== m.conversationId) continue;
    const at = dm.messages.findIndex((x) => x.id === m.id);
    if (at >= 0) dm.messages[at] = m;
    else dm.messages.push(m);
    if (state.chatTo === userId) renderChat();
    else if (m.senderId !== session.user?.id) dm.unread = (dm.unread ?? 0) + 1;
    renderChatTo();
    return;
  }
});







// Bring somebody into THIS meeting by ringing them, the way Teams' "Add
// people" does. The list is who you actually talk to -- the direct
// conversations -- minus anyone already here. Ringing sends the current room,
// so accepting drops them straight in.
function addPeopleHTML() {
  if (!state.room) return '';
  return `<details class="add-people" id="add-people">
    <summary><svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-people"/></svg> Add people to this meeting</summary>
    <input id="add-search" class="input add-search" placeholder="Search by name, or type a full email address"
           autocomplete="off" spellcheck="false" />
    <div class="add-people-list" id="add-results"></div>
  </details>`;
}

// Who is already in the room, so the list never offers to ring somebody
// standing in front of you. Matched on account where there is one, and on
// name for a guest, who has no account to match.
function alreadyHere() {
  const ids = new Set();
  const names = new Set();
  for (const p of state.people.values()) {
    if (p.userId) ids.add(p.userId);
    names.add((p.name ?? '').toLowerCase());
  }
  return { ids, names };
}

function paintAddResults(people, q) {
  const box = $('add-results');
  if (!box) return;
  const { ids, names } = alreadyHere();
  const rows = people.filter(
    (c) => !ids.has(c.userId) && !names.has((c.name ?? '').toLowerCase())
  );
  if (!rows.length) {
    box.innerHTML = `<div class="empty">${
      q
        ? 'Nobody by that name. To reach someone new, type their full email address.'
        : 'Start typing a name to search.'
    }</div>`;
    return;
  }
  box.innerHTML = rows
    .map(
      (c) => `<div class="add-row">
        <span ${avatarAttrs(c, 'sm')}>${esc(initials(c))}</span>
        <span class="add-name">${highlight(c.name, q)}<small>${highlight(c.email, q)}</small></span>
        <button class="btn btn-primary sm" data-ring-in="${esc(c.userId)}" data-ring-name="${esc(c.name)}">Ring</button>
      </div>`
    )
    .join('');
  wireRingButtons();
}

let addSearcher = null;

function wireAddPeople() {
  const input = $('add-search');
  if (!input) return;

  // The participants panel re-renders on every roster change, which throws
  // this input away and builds a new one. Rebuild the searcher with it, and
  // abandon whatever the old one had in flight.
  addSearcher?.cancel();
  addSearcher = createSearch(
    async (q) => {
      const local = rankPeople(
        frequentContacts(200, session.user?.id).map((c) => ({
          userId: c.userId,
          name: c.name,
          email: c.email,
        })),
        q,
        10
      );
      if (!q) return local;
      let remote = [];
      try {
        remote = ((await directory.search(q)).people ?? []).map((p) => ({
          userId: p.id,
          name: p.name,
          email: p.email,
        }));
      } catch {
        // Local results still stand.
      }
      return mergePeople(local, rankPeople(remote, q, 15)).slice(0, 15);
    },
    (q, people) => paintAddResults(people, q)
  );

  input.oninput = (e) => addSearcher.input(e.target.value.trim());
  addSearcher.input('', { immediate: true });
}

function wireRingButtons() {
  document.querySelectorAll('[data-ring-in]').forEach((b) => {
    b.onclick = () => {
      if (sendRing(b.dataset.ringIn, state.room)) {
        toast(`Ringing ${b.dataset.ringName}…`);
        b.disabled = true;
        b.textContent = 'Ringing';
      } else {
        toast('You are offline; could not ring.', 'bad');
      }
    };
  });
}

// The people knocking, shown inside the participants list itself.
//
// Placed here, not in a floating panel, because a request to be let in is a
// fact about the roster: it belongs next to the roster. Only a host is sent
// the list at all, so this is empty for everybody else.
function waitingListHTML() {
  if (!state.isHost || !state.waiting || !state.waiting.length) return '';
  const rows = state.waiting.map((w) => `
    <div class="admit-row" data-waiter="${esc(w.id)}">
      <span class="who"><b>${esc(w.name || 'Guest')}</b>
        <small>${w.authenticated ? 'Signed in' : 'Guest — not signed in'}</small></span>
      <button class="btn btn-primary sm" data-admit="${esc(w.id)}">Admit</button>
      <button class="btn btn-quiet sm" data-deny="${esc(w.id)}">Deny</button>
    </div>`).join('');
  return `<div class="waiting-block">
    <div class="waiting-head"><strong>Waiting in lobby (${state.waiting.length})</strong>
      <button class="btn btn-primary sm" id="admit-all-inline">Admit all</button></div>
    ${rows}
  </div>`;
}

function wireWaitingList() {
  document.querySelectorAll('[data-admit]').forEach((b) => {
    b.onclick = () => decideLobby(b.dataset.admit, true);
  });
  document.querySelectorAll('[data-deny]').forEach((b) => {
    b.onclick = () => decideLobby(b.dataset.deny, false);
  });
  const all = $('admit-all-inline');
  if (all) all.onclick = () => (state.waiting || []).forEach((w) => decideLobby(w.id, true));
}


// ---------------------------------------------------------------------------
// Controls
// ---------------------------------------------------------------------------

// Stop doing what the host has just forbidden.
//
// Greying a button out is not enforcement; it only describes what the button
// will do next time. Somebody already sending when the host turned that
// capability off went on sending: the server stopped forwarding it, so nobody
// else saw or heard anything, but their own preview kept playing and their
// uplink kept carrying a stream with no audience. Every signal they had said
// it was still working. This is what makes the host's switch true for people
// who were mid-sentence when it was flipped.
//
// The host is exempt, exactly as they are on the server -- a host caught by
// their own switch would have no way to reach the switch again.
// quiet: the caller has already said why (a role change), so no second
// message replaces the first.
function enforcePermissions(quiet = false) {
  // Exempt: hosts and presenters, exactly as on the server (room.May). A
  // presenter used to be muted here by "Allow mic for attendees" off, though
  // the server would have carried their voice.
  const exempt = state.isHost || state.myRole >= Role.Moderator;
  // A host's "Disable mic / camera" on you counts as that control being off.
  const perms = { ...state.perms, mic: state.perms.mic && !state.blocked.mic, camera: state.perms.camera && !state.blocked.cam };
  const stop = revocations(
    perms,
    { mic: state.mic, camera: state.cam, share: state.sharing },
    exempt
  );
  if (!stop.mic && !stop.camera && !stop.share) return;

  if (stop.mic) {
    state.mic = false;
    applyMicEnabled();
    queueMic(reconcileMic);
    if (!quiet && !state.blocked.mic) toast('The host has turned off microphones for attendees.');
  }
  if (stop.camera) {
    state.cam = false;
    queueMedia(reconcileCamera);
    if (!quiet && !state.blocked.cam) toast('The host has turned off video for attendees.');
  }
  if (stop.share) {
    queueMedia(stopShare);
    if (!quiet) toast('The host has turned off screen sharing for attendees.');
  }
  pushState();
  renderStage();
}

function syncControls() {
  const set = (id, cls, on) => $(id)?.classList.toggle(cls, on);
  set('btn-mic', 'off', !state.mic);
  set('btn-cam', 'off', !state.cam);
  set('btn-screen', 'on', state.sharing);

  // Grey out what the host has turned off. Disabled rather than hidden: a
  // button that vanishes leaves somebody wondering where it went, while one
  // that is visibly unavailable answers the question. The server refuses
  // these regardless of what the button does.
  const gate = (id, allowed, why) => {
    const el = $(id);
    if (!el) return;
    const blocked = !state.isHost && state.myRole < Role.Moderator && !allowed;
    el.disabled = blocked;
    el.classList.toggle('is-blocked', blocked);
    el.title = blocked ? why : '';
  };
  gate('btn-mic', state.perms.mic && !state.blocked.mic, state.blocked.mic ? 'Your mic has been disabled' : 'The host has turned off attendee microphones');
  gate('btn-cam', state.perms.camera && !state.blocked.cam, state.blocked.cam ? 'Your camera has been disabled' : 'The host has turned off attendee video');
  gate('btn-screen', state.perms.share, 'The host has turned off attendee screen sharing');
  const reactBtn = $('btn-react')?.querySelector(':scope > button');
  if (reactBtn) {
    const off = !state.isHost && state.myRole < Role.Moderator && !state.perms.reactions;
    reactBtn.disabled = off;
    reactBtn.title = off ? 'Reactions are turned off for attendees' : '';
    $('btn-react').classList.toggle('is-blocked', off);
  }

  // Chat is refused by the server when it is off, which without this looked
  // like messages vanishing: the composer accepted the text, cleared itself,
  // and nothing ever appeared. Saying so in the box is the whole fix.
  const chatOff = chatBlocked(state.perms, state.isHost);
  const ci = $('chat-input');
  if (ci) {
    ci.disabled = chatOff;
    ci.placeholder = chatOff ? 'The host has turned off chat' : 'Message everyone';
  }
  const cs = $('chat-send');
  if (cs) cs.disabled = chatOff;
  set('btn-hand', 'on', state.hand);
  set('btn-cc', 'on', state.captions);
  // Teams' words: Raise / Lower, and the captions item says what it will do.
  if ($('hand-label')) $('hand-label').textContent = state.hand ? 'Lower' : 'Raise';
  if ($('cc-label')) $('cc-label').textContent = state.captions ? 'Turn off live captions' : 'Turn on live captions';
  // Record and transcribe: organizers and presenters (the server decides).
  const mayRecord = state.isHost || state.myRole >= Role.Moderator;
  if ($('more-rec')) {
    $('more-rec').hidden = !mayRecord;
    // Not disabled after a refusal: an admin may turn recording on while the
    // meeting runs, and asking again costs one frame.
    $('more-rec').innerHTML = state.rec.recording ? 'Stop recording' : state.recDisabled ? 'Start recording<small>Your IT admin has disabled Recording</small>' : 'Start recording';
  }
  if ($('more-tr')) {
    $('more-tr').hidden = !mayRecord;
    $('more-tr').textContent = state.rec.transcribing ? 'Stop transcription' : 'Start transcription';
  }
  if ($('more-rec-head')) $('more-rec-head').hidden = !mayRecord;
  // The red dot and clock beside the meeting's own, while it is recorded.
  const live = $('rec-live');
  if (live) live.hidden = !state.rec.recording;
  // "End meeting" sits behind Leave's arrow, for whoever can end it.
  if ($('more-options')) $('more-options').hidden = !state.isHost;
  // Rooms: for hosts, and for a manager who has walked into a room.
  if ($('btn-rooms')) $('btn-rooms').hidden = !(state.isHost || state.breakouts);
  if ($('more-rooms')) $('more-rooms').hidden = !(state.isHost || state.breakouts);
  const caret = $('btn-leave-more');
  if (caret) caret.hidden = !state.isHost;
  $('leave-menu')?.classList.toggle('has-caret', !!state.isHost);
  $('mic-icon')?.setAttribute('href', state.mic ? '#i-mic' : '#i-mic-off');
  $('cam-icon')?.setAttribute('href', state.cam ? '#i-cam' : '#i-cam-off');

  const pip = $('btn-pip');
  if (pip) {
    pip.hidden = !pipSupported();
    pip.classList.toggle('on', pipActive());
    pip.title = autoPiPAvailable()
      ? 'Also pops out automatically when you switch tabs'
      : 'Install the app for automatic pop-out';
  }
}

function setSide(view) {
  document.querySelectorAll('.seg button').forEach((b) =>
    b.classList.toggle('active', b.dataset.view === view)
  );
  document.querySelectorAll('.side-view').forEach((v) =>
    v.classList.toggle('active', v.id === `view-${view}`)
  );
  $('composer').classList.toggle('hidden', view !== 'chat');
  $('side-title').textContent = { chat: 'Meeting chat', rooms: 'Breakout rooms', qna: 'Q&A', health: 'Call health', notes: 'Meeting notes', transcript: 'Transcript', effects: 'Video effects' }[view] ?? 'People';
  if (view === 'effects') renderEffects($('view-effects'));
  if (view === 'transcript') {
    if (transcriptState.loaded) renderTranscript();
    else loadTranscript();
  }
  if (view === 'notes') {
    if (notesState.loaded) renderNotes();
    else loadNotes();
  }
  // Call health samples only while it is the panel showing.
  if (view === 'health') {
    if (!healthTimer) {
      sampleHealth();
      healthTimer = setInterval(sampleHealth, 1000);
    }
  } else stopHealth();
  if (view === 'rooms') renderRooms();
  if (view === 'qna') {
    $('qna-dot').hidden = true;
    $('more-dot').hidden = true;
    if (qnaState.loaded) renderQnA();
    else loadQnA();
  }
  state.sideView = view;
  if (view === 'chat') {
    renderChatTo();
    renderChat();
    clearBubbles(); // the messages they announced are on screen now
  }

  for (const [id, v] of [
    ['rail-chat', 'chat'],
    ['rail-people', 'people'],
    ['btn-rooms', 'rooms'],
    ['btn-qna', 'qna'],
    ['btn-notes', 'notes'],
  ]) {
    $(id)?.classList.toggle('active', v === view);
    $(id)?.setAttribute('aria-pressed', String(v === view));
  }
  $('shell').classList.remove('panel-closed');
  renderStage(); // the stage just got narrower
  reportViewport(true);
}

function closeSide() {
  stopHealth();
  $('shell').classList.add('panel-closed');
  state.sideView = '';
  for (const id of ['rail-chat', 'rail-people', 'btn-rooms', 'btn-qna', 'btn-notes']) {
    $(id)?.classList.remove('active');
    $(id)?.setAttribute('aria-pressed', 'false');
  }
  renderStage();
  reportViewport(true);
}

// ---------------------------------------------------------------------------
// The bar's menus: View, More, and Leave's "End meeting".
// ---------------------------------------------------------------------------

// One open menu at a time, closed by a pick, by a click elsewhere and by
// Escape -- the same rules as the reaction picker.
function wireMenu(btnId, popId, onOpen) {
  const btn = $(btnId);
  const pop = $(popId);
  if (!btn || !pop) return () => {};
  const setOpen = (on) => {
    pop.hidden = !on;
    btn.setAttribute('aria-expanded', String(on));
    btn.classList.toggle('on', on);
    if (on) {
      onOpen?.();
      keepOnScreen(pop);
    }
  };
  btn.onclick = (e) => {
    e.stopPropagation();
    const opening = pop.hidden;
    document.querySelectorAll('.mb-pop').forEach((p) => p !== pop && (p.hidden = true));
    setOpen(opening);
  };
  pop.addEventListener('click', (e) => {
    if (e.target.closest('button')) setOpen(false);
  });
  document.addEventListener('click', (e) => {
    if (!pop.hidden && !pop.contains(e.target) && !btn.contains(e.target)) setOpen(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !pop.hidden) setOpen(false);
  });
  return setOpen;
}

function wireMeetingMenus() {
  wireMenu('btn-view', 'view-pop', syncViewMenu);
  $('view-together').onclick = () => openTogether();
  document.querySelectorAll('#view-pop [data-layout]').forEach((b) => {
    b.onclick = () => {
      if (b.disabled) return;
      setLayout(b.dataset.layout);
    };
  });
  $('view-bubbles').onclick = () => {
    state.bubbles = !state.bubbles;
    try { localStorage.setItem('aether.bubbles', state.bubbles ? '1' : '0'); } catch {}
    if (!state.bubbles) clearBubbles();
  };
  $('view-full').onclick = () => {
    if (document.fullscreenElement) document.exitFullscreen?.().catch(() => {});
    else document.documentElement.requestFullscreen?.().catch(() => toast('Full screen is not available here.', 'bad'));
  };

  wireMenu('btn-more', 'more-pop');
  wireMenu('btn-share-more', 'share-pop');
  wireWhiteboard();
  $('more-options').onclick = () => openMeetingOptions();
  $('more-health').onclick = () => openHealth();
  $('more-qna').onclick = () => setSide('qna');
  $('more-notes').onclick = () => setSide('notes');
  $('more-rec').onclick = () => {
    $('more-pop').hidden = true;
    toggleRecording();
  };
  $('more-tr').onclick = () => {
    $('more-pop').hidden = true;
    toggleTranscription();
  };
  $('more-effects').onclick = () => {
    $('more-pop').hidden = true;
    setSide('effects');
  };
  wireMenu('btn-cam-more', 'cam-pop');
  $('cam-effects').onclick = () => {
    $('cam-pop').hidden = true;
    setSide('effects');
  };
  $('more-transcript').onclick = () => {
    $('more-pop').hidden = true;
    setSide('transcript');
  };
  $('more-rooms').onclick = () => setSide('rooms');
  // "Audio settings" in More opens the same panel as the microphone's arrow.
  $('more-audio').onclick = (e) => {
    e.stopPropagation();
    $('more-pop').hidden = true;
    $('btn-audio').click();
  };

  // Leave: the button leaves; its arrow, shown to hosts, offers "End meeting".
  wireMenu('btn-leave-more', 'leave-pop');
  $('leave-only').onclick = leaveMeeting;
  $('hc-end').onclick = async () => {
    if (!state.isHost) return;
    if (await confirmAction({ title: 'End the meeting?', body: "You'll end the meeting for everyone.", yes: 'End meeting', danger: true }))
      send((w) => w.header(Op.EndMeeting));
  };
}

// ---------------------------------------------------------------------------
// Layout: Gallery, Speaker, Large gallery, Focus on content
// ---------------------------------------------------------------------------

function realPeopleCount() {
  return participants().filter((p) => !p.presentation).length;
}

function syncViewMenu() {
  const n = realPeopleCount();
  const presenting = participants().some((p) => p.presentation);
  for (const b of document.querySelectorAll('#view-pop [data-layout]')) {
    const l = b.dataset.layout;
    // Teams offers these only when they can show something: large gallery
    // from `minParticipantsInLargeGrid` people, focus only while content is
    // being shared.
    const off = (l === 'large' && n < LARGE_GALLERY_MIN_PEOPLE) || (l === 'focus' && !presenting);
    b.disabled = off;
    b.setAttribute('aria-checked', String(state.layout === l));
  }
  $('view-bubbles').setAttribute('aria-checked', String(!state.bubbles));
  $('view-together')?.setAttribute('aria-checked', String(state.together.on));
  $('view-full').textContent = document.fullscreenElement ? 'Exit full screen' : 'Full screen';
}

function setLayout(layout) {
  if (!VIEWS.includes(layout)) return;
  state.layout = layout;
  try { localStorage.setItem('aether.layout', layout); } catch {}
  renderStage();
  reportViewport(true);
}

// ---------------------------------------------------------------------------
// Chat bubbles: a message that arrives while the chat is closed is shown over
// the meeting for a moment (`chatBubblesTimeout = 4000`, faded over
// `chatBubbleFadeDuration = 300`), unless "Don't show chat bubbles" is on.
// ---------------------------------------------------------------------------

const BUBBLE_MS = 4000;
const BUBBLE_FADE_MS = 300;
const BUBBLE_MAX = 3;

function showBubble(name, text) {
  if (!state.bubbles || state.sideView === 'chat' && !$('shell').classList.contains('panel-closed')) return;
  const box = $('bubbles');
  if (!box) return;
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'bubble';
  b.innerHTML = `<b>${esc(name)}</b><span>${esc(text.length > 140 ? text.slice(0, 139) + '…' : text)}</span>`;
  b.onclick = () => setSide('chat');
  box.appendChild(b);
  while (box.children.length > BUBBLE_MAX) box.firstElementChild.remove();
  setTimeout(() => {
    b.classList.add('fading');
    setTimeout(() => b.remove(), BUBBLE_FADE_MS);
  }, BUBBLE_MS);
}

function clearBubbles() {
  const box = $('bubbles');
  if (box) box.innerHTML = '';
}

async function togglePiP() {
  if (pipActive()) {
    await closePiP();
    syncControls();
    return;
  }
  const ok = await openPiP({
    getTiles: () =>
      participants().map((p) => ({
        id: p.id,
        name: p.name,
        self: p.self,
        stream: p.stream,
        speaking: p.energy > 0.12,
        mirror: p.self && !p.presentation && state.mirrorSelf,
      })),
    getState: () => ({ mic: state.mic, cam: state.cam }),
    onToggleMic: () => $('btn-mic').click(),
    onToggleCam: () => $('btn-cam').click(),
    onClose: () => syncControls(),
  });
  if (!ok) toast('This browser will not open a floating window right now.', 'bad');
  syncControls();
}

function inviteLink(room) {
  const u = new URL(location.origin);
  u.searchParams.set('room', room);
  return u.toString();
}

async function copyInvite(room) {
  const link = inviteLink(room);
  try {
    await navigator.clipboard.writeText(link);
    toast('Invite link copied.');
  } catch {
    // A copy button that silently does nothing is worse than no button.
    window.prompt('Copy this invite link', link);
  }
}

// Media toggles are RECONCILED, not executed.
//
// A click used to run its own effect directly, which meant N rapid clicks ran
// N overlapping async operations, each acquiring or releasing hardware and
// renegotiating a peer connection, all racing each other. Toggling quickly
// left the camera in whichever state the slowest one happened to finish in,
// and frequently wedged signalling entirely.
//
// Instead the click records INTENT and returns immediately, so the interface
// responds instantly, and a serialised reconciler makes reality match the
// intent afterwards. Because each reconciler reads the current intent when it
// runs rather than when it was queued, ten rapid clicks collapse into at most
// one or two real operations and always settle on what the user last asked
// for. It cannot end up inverted, and it cannot overlap itself.
let mediaOp = Promise.resolve();
// The microphone has its own lane. It used to share queueMedia with the
// camera, and a camera acquisition that hung -- a device another program
// held, a permission prompt nobody answered -- held every mute behind it:
// the button went grey and the voice kept going out. Measured (mute.e2e.mjs
// scenario 7) before this: B kept hearing A for the whole hold. The switch
// itself, track.enabled, is set synchronously in the handler and here; the
// queue only carries the parts that must await (opening a microphone that
// was never opened, attaching it to a sender).
let micOp = Promise.resolve();
function queueMic(fn) {
  micOp = micOp.then(fn, fn).catch((err) => {
    console.error('microphone operation failed:', err?.name, err?.message);
  });
  return micOp;
}

// Every audio track we hold follows the switch, not just the first: a stale
// second track (a device switched in the pre-join) must not keep speaking.
function applyMicEnabled() {
  for (const t of state.localStream?.getAudioTracks() ?? []) t.enabled = state.mic;
}

function queueMedia(fn) {
  mediaOp = mediaOp.then(fn, fn).catch((err) => {
    console.error('media operation failed:', err?.name, err?.message);
  });
  return mediaOp;
}

async function reconcileCamera() {
  // While sharing, the outbound video sender carries the screen. The camera
  // intent is remembered and applied when sharing stops.
  if (state.sharing) return;
  const track = state.localStream?.getVideoTracks()[0];
  const live = !!track && track.readyState === 'live';
  if (state.cam && !live) await restartCamera();
  else if (!state.cam && live) await stopCamera();
}

async function reconcileMic() {
  applyMicEnabled();
  let track = state.localStream?.getAudioTracks().find((t) => t.readyState === 'live') ?? null;
  // Unmuting with no live microphone opens one. A person who joined with
  // the microphone off in the pre-join had no track at all, and the old
  // "if (!track) return" meant the unmute button changed colour and did
  // nothing else, for the rest of the meeting (mute.e2e.mjs scenario 5).
  if (state.mic && !track) {
    track = await restartMic();
    if (!track) return;
  }
  if (!track) return;
  // Mute is silence on a live track, on both transports. It used to swap
  // the track out of the WebRTC sender and back; the far side then had to
  // resynchronise a stream that had stopped, and "unmute to being heard"
  // measured up to a second (58 / 540 / 1002 ms) against 80 ms for the
  // transport itself. A disabled track sends silence frames, and Opus
  // spends almost nothing on them. The sender is only touched when it has
  // no track yet -- a microphone that appeared after publish.
  track.enabled = state.mic;
  if (state.mic && state.media && !state.media.sending('audio')) {
    await state.media.ensureAudio(track);
  }
  // Over the socket, muting is silence on the same encoder (a disabled track
  // yields silent frames), but a track that appeared after start() -- or
  // replaced the one it started on -- must be attached or it is never
  // encoded at all.
  if (state.wsMediaOn && state.wsSender && state.wsSender.audioTrack !== track) {
    await state.wsSender.setAudioTrack(track);
  }
  renderPeople();
}

// ---------------------------------------------------------------------------
// In-call audio controls: microphone, speaker, noise cancellation, level.
//
// Everything the pre-join offers, without leaving the call. A microphone or
// noise-cancellation change is a live swap: the new track is built first,
// handed to whichever sender is running (WebRTC replaceTrack, or the socket
// encoder), and only then is the old one stopped -- so there is no moment
// with no microphone and never a moment with two. The swap follows the mute
// switch: a muted person who changes microphone stays muted.
// ---------------------------------------------------------------------------

// A popover centred over its button stays inside the viewport: measured
// after it opens, shifted by the overflow plus an 8 px margin. Only
// horizontal -- the bar is at the bottom and they open upwards.
function keepOnScreen(pop) {
  pop.style.setProperty('--nudge', '0px');
  const r = pop.getBoundingClientRect();
  if (!r.width || getComputedStyle(pop).position === 'fixed') return;
  const m = 8;
  let dx = 0;
  if (r.left < m) dx = m - r.left;
  else if (r.right > innerWidth - m) dx = innerWidth - m - r.right;
  if (dx) pop.style.setProperty('--nudge', Math.round(dx) + 'px');
}

function applySpeaker(el) {
  if (!state.speakerId || typeof el.setSinkId !== 'function') return;
  if (el.sinkId === state.speakerId) return;
  el.setSinkId(state.speakerId).catch((e) => console.warn('speaker:', e?.name, e?.message));
}

async function switchMic() {
  const oldDenoiser = state.denoiser;
  const oldTracks = state.localStream?.getAudioTracks() ?? [];
  state.denoiser = null; // micTrackFor must not stop the running one yet
  let raw;
  try {
    ({ track: raw } = await acquireMic((c) => navigator.mediaDevices.getUserMedia(c), state.micDeviceId || ''));
  } catch (err) {
    state.denoiser = oldDenoiser;
    toast('Could not open that microphone. Check that nothing else is using it.', 'bad');
    console.warn('switch microphone:', err?.name, err?.message);
    return;
  }
  if (!raw) {
    state.denoiser = oldDenoiser;
    return;
  }
  const track = await micTrackFor(raw);
  track.enabled = state.mic;
  if (!state.localStream) state.localStream = new MediaStream();
  state.localStream.addTrack(track);
  // Hand it to the running sender before the old track goes.
  if (state.media?.senders?.has('audio')) await state.media.ensureAudio(track);
  if (state.wsMediaOn && state.wsSender) await state.wsSender.setAudioTrack(track);
  oldDenoiser?.stop();
  for (const t of oldTracks) {
    t.stop();
    state.localStream.removeTrack(t);
  }
  applyMicEnabled();
  renderAudioPanel();
}

async function renderAudioPanel() {
  const pop = $('audio-pop');
  if (!pop) return;
  let devices = [];
  try {
    devices = await navigator.mediaDevices.enumerateDevices();
  } catch {}
  const opts = (kind, chosen, dflt) => {
    const list = devices.filter((d) => d.kind === kind);
    if (!list.length) return `<option value="">${dflt}</option>`;
    return list
      .map((d, i) => `<option value="${esc(d.deviceId)}"${d.deviceId === chosen ? ' selected' : ''}>${esc(d.label || `${dflt} ${i + 1}`)}</option>`)
      .join('');
  };
  const live = state.localStream?.getAudioTracks().find((t) => t.readyState === 'live');
  const micNow = state.micDeviceId || live?.getSettings?.().deviceId || '';
  $('ap-mic').innerHTML = opts('audioinput', micNow, 'Microphone');
  const canSpeaker = typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
  $('ap-speaker-row').hidden = !canSpeaker;
  if (canSpeaker) $('ap-speaker').innerHTML = opts('audiooutput', state.speakerId || 'default', 'Speaker');
  const strongOk = denoiseSupported();
  const mode = denoiseWanted() && strongOk ? 'strong' : 'standard';
  for (const r of pop.querySelectorAll('input[name="ap-nc"]')) r.checked = r.value === mode;
  $('ap-nc-strong').disabled = !strongOk;
  const got = document.documentElement.dataset.denoise;
  $('ap-nc-note').textContent =
    mode === 'strong' && got === 'failed'
      ? 'Strong noise cancellation could not start here; the standard one is on.'
      : mode === 'strong'
        ? 'Removes fans, typing and room noise. Adds a little delay.'
        : 'The browser\'s own cancellation. Lowest delay; lets more noise through.';
}

let apMeter = null;
function startAudioMeter() {
  stopAudioMeter();
  const track = state.localStream?.getAudioTracks().find((t) => t.readyState === 'live');
  const bar = $('ap-level');
  if (!track || !bar) return;
  try {
    const ctx = new AudioContext();
    const src = ctx.createMediaStreamSource(new MediaStream([track]));
    const an = ctx.createAnalyser();
    an.fftSize = 512;
    src.connect(an);
    const buf = new Float32Array(an.fftSize);
    const tick = () => {
      an.getFloatTimeDomainData(buf);
      let s = 0;
      for (const v of buf) s += v * v;
      const rms = Math.sqrt(s / buf.length);
      // Muted reads as muted, not as a meter that still moves.
      const pct = state.mic ? Math.min(100, Math.round(Math.sqrt(rms) * 180)) : 0;
      bar.style.width = pct + '%';
      apMeter.raf = requestAnimationFrame(tick);
    };
    apMeter = { ctx, raf: requestAnimationFrame(tick) };
  } catch {}
}
function stopAudioMeter() {
  if (!apMeter) return;
  cancelAnimationFrame(apMeter.raf);
  apMeter.ctx.close().catch(() => {});
  apMeter = null;
}

function wireAudioPanel() {
  const menu = $('audio-menu');
  const trigger = $('btn-audio');
  const pop = $('audio-pop');
  if (!menu || !trigger || !pop) return;
  const setOpen = async (on) => {
    menu.classList.toggle('open', on);
    trigger.setAttribute('aria-expanded', String(on));
    if (on) {
      keepOnScreen(pop);
      await renderAudioPanel();
      startAudioMeter();
    } else stopAudioMeter();
  };
  trigger.onclick = (e) => {
    e.stopPropagation();
    setOpen(!menu.classList.contains('open'));
  };
  pop.addEventListener('click', (e) => e.stopPropagation());
  $('ap-close')?.addEventListener('click', () => setOpen(false));
  document.addEventListener('click', (e) => {
    if (menu.classList.contains('open') && !menu.contains(e.target)) setOpen(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && menu.classList.contains('open')) setOpen(false);
  });
  $('ap-mic').onchange = (e) => {
    state.micDeviceId = e.target.value;
    queueMic(async () => {
      await switchMic();
      startAudioMeter();
    });
  };
  $('ap-speaker').onchange = (e) => {
    state.speakerId = e.target.value;
    for (const el of document.querySelectorAll('#audio-sinks audio')) applySpeaker(el);
  };
  for (const r of pop.querySelectorAll('input[name="ap-nc"]')) {
    r.onchange = () => {
      if (!r.checked) return;
      setDenoiseWanted(r.value === 'strong');
      queueMic(async () => {
        await switchMic();
        startAudioMeter();
      });
    };
  }
  navigator.mediaDevices?.addEventListener?.('devicechange', () => {
    if (menu.classList.contains('open')) renderAudioPanel();
  });
}

/**
 * Open the microphone during a meeting: it was off in the pre-join, or it
 * was lost. The previous audio tracks, live or ended, are stopped and
 * dropped so exactly one microphone speaks for this person.
 */
async function restartMic() {
  try {
    const { track: raw, report } = await acquireMic((c) => navigator.mediaDevices.getUserMedia(c), state.micDeviceId || '');
    if (!raw) return null;
    noteMicProcessing(report);
    if (!state.localStream) state.localStream = new MediaStream();
    for (const old of state.localStream.getAudioTracks()) {
      old.stop();
      state.localStream.removeTrack(old);
    }
    const track = await micTrackFor(raw);
    state.localStream.addTrack(track);
    track.enabled = state.mic;
    return track;
  } catch (err) {
    console.warn('microphone unavailable:', err?.name, err?.message);
    state.mic = false;
    pushState();
    syncControls();
    renderPeople();
    toast('Could not start the microphone. Check that nothing else is using it.', 'bad');
    return null;
  }
}

/**
 * Turn the camera off completely, including the hardware.
 *
 * The track is stopped rather than merely detached, so the camera indicator
 * light goes out. That is a trust property rather than a bandwidth one: a
 * user who turns their camera off and watches the light stay on has been told
 * something is still watching them, and no amount of reassurance in the UI
 * outweighs a lit LED. Re-acquisition costs a few hundred milliseconds when
 * they turn it back on, which is the right side of that trade.
 */
async function stopCamera() {
  await state.media?.setSending('video', false);
  if (state.wsMediaOn && state.wsSender) await state.wsSender.setVideoTrack(null);
  const t = state.localStream?.getVideoTracks()[0];
  if (t) {
    t.stop();
    state.localStream?.removeTrack(t);
  }
  // Repaint for the same reason the restart does: the stage was drawn before
  // this finished, when the track was still live.
  renderStage();
}

/** Re-acquire the camera and resume sending on the existing transceiver. */
async function restartCamera() {
  try {
    const fresh = await navigator.mediaDevices.getUserMedia({
      video: state.videoConstraints ?? true,
    });
    const camRaw = fresh.getVideoTracks()[0];
    if (!camRaw) return;
    const t = await camTrackFor(camRaw);
    if (!state.localStream) state.localStream = new MediaStream();
    state.localStream.addTrack(t);
    // ensureVideo covers the case where there was never a camera to begin
    // with, so no video sender exists yet.
    await state.media?.ensureVideo(t);
    // And the socket path, which otherwise keeps encoding the stopped track.
    if (state.wsMediaOn && state.wsSender) await state.wsSender.setVideoTrack(t);

    // Re-render, and let renderStage own the attachment.
    //
    // This used to assign srcObject here, to $('stage-video') -- which is the
    // wrapping div, not the video element inside it. Assigning srcObject to a
    // div is not an error in JavaScript; it sets an expando property that
    // nothing reads. The camera came back, the track was published, other
    // people could see it, and the local preview stayed black with nothing in
    // any log to say why.
    //
    // The click handler already re-rendered once, before this finished, when
    // there was no track to show yet. Acquiring a device is asynchronous, so
    // whatever paints has to paint again afterwards.
    renderStage();
  } catch (err) {
    // Losing the camera is recoverable and must not take the meeting with
    // it: the user stays in the call with video off and is told why.
    state.cam = false;
    toast('Could not start the camera. Check that nothing else is using it.');
  }
}

function wireMeeting() {
  wireAudioPanel();
  // Muting must actually stop the bytes. See RTC.setSending for why
  // `track.enabled = false` does not: it sends black frames and silence,
  // which the SFU then forwards to everybody.
  // The microphone track is kept alive while muted rather than stopped, so
  // unmuting is instant. Conversation depends on that: half a second of device
  // acquisition between pressing unmute and being heard is the difference
  // between answering a question and talking over the answer.
  $('btn-mic').onclick = () => {
    state.mic = !state.mic;
    // The switch first, synchronously: nothing below, and nothing the
    // camera is doing, can delay a mute.
    applyMicEnabled();
    pushState();
    syncControls();
    renderStage();
    renderPeople();
    queueMic(reconcileMic);
  };

  $('btn-cam').onclick = () => {
    state.cam = !state.cam;
    pushState();
    syncControls();
    renderStage();
    queueMedia(reconcileCamera);
  };
  $('btn-screen').onclick = toggleScreen;
  $('btn-hand').onclick = () => {
    state.hand = !state.hand;
    if (state.hand) state.handOrder.set(state.me, ++state.handSeq);
    else state.handOrder.delete(state.me);
    pushState();
    syncControls();
    renderPeople();
  };
  $('btn-cc').onclick = () => toggleCaptions();

  // The reaction picker. Built here rather than in the markup so the set can
  // change without touching two files.
  // It opens on a click (not on hover: hover does not exist on a phone and
  // a picker that appears when the pointer passes is one that vanishes
  // when it moves), closes on a pick, on a click anywhere else, and on
  // Escape. One reaction per pick.
  const menu = $('btn-react');
  const pop = menu?.querySelector('.react-pop');
  const trigger = menu?.querySelector(':scope > button');
  if (menu && pop && trigger) {
    const setOpen = (on) => {
      menu.classList.toggle('open', on);
      trigger.setAttribute('aria-expanded', String(on));
      if (on) keepOnScreen(pop);
    };
    pop.innerHTML = REACTIONS.map(
      (e) => `<button type="button" data-react="${e}" title="Send ${e}" aria-label="React ${e}">${e}</button>`
    ).join('');
    pop.querySelectorAll('[data-react]').forEach((b) => {
      b.onclick = (e) => {
        e.stopPropagation();
        sendReaction(b.dataset.react);
        setOpen(false);
      };
    });
    trigger.onclick = (e) => {
      e.stopPropagation();
      setOpen(!menu.classList.contains('open'));
    };
    document.addEventListener('click', (e) => {
      if (!menu.contains(e.target)) setOpen(false);
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') setOpen(false);
    });
  }
  $('btn-pip').onclick = togglePiP;
  $('btn-leave').onclick = leaveMeeting;
  $('btn-back').onclick = leaveMeeting;
  $('btn-invite').onclick = () => inviteDialog();

  // Chat and People are toggles, as in Teams: the second press closes the
  // panel and gives the people the room back.
  const panelToggle = (view) => () => {
    const shell = $('shell');
    if (!shell.classList.contains('panel-closed') && state.sideView === view) closeSide();
    else setSide(view);
  };
  $('rail-chat').onclick = panelToggle('chat');
  $('rail-people').onclick = panelToggle('people');
  $('btn-rooms').onclick = panelToggle('rooms');
  $('btn-qna').onclick = panelToggle('qna');
  $('btn-notes').onclick = panelToggle('notes');
  $('side-close').onclick = closeSide;
  document.querySelectorAll('.seg button').forEach((b) => {
    b.onclick = () => setSide(b.dataset.view);
  });
  wireMeetingMenus();

  const sendChat = async () => {
    const input = $('chat-input');
    const text = input.value.trim();
    if (!text) return;
    input.value = '';

    // To the meeting's conversation, when the room is a meeting: kept.
    if (state.chatTo === 'everyone' && state.meetingConv?.convId) {
      const mc = state.meetingConv;
      try {
        const m = await sendDirect(mc.convId, text);
        if (m?.id && !mc.messages.some((x) => x.id === m.id)) mc.messages.push(m);
        renderChat();
      } catch (e) {
        toast(e?.message || 'Could not send that message.', 'bad');
        input.value = text;
      }
      return;
    }

    // To the room: the meeting's own broadcast, over the meeting socket, and
    // it ends with the meeting.
    if (state.chatTo === 'everyone') {
      const body = new TextEncoder().encode(text);
      send((w) => {
        w.header(Op.Chat);
        w.blob(body);
      });
      return;
    }

    // To one person: their permanent conversation, over the chat API, the
    // same thread as the sidebar. It is still there when the call ends.
    const dm = state.dms.get(state.chatTo);
    if (!dm?.convId) {
      toast('That conversation is still opening.', 'bad');
      input.value = text;
      return;
    }
    try {
      const m = await sendDirect(dm.convId, text);
      // The push socket echoes it back, but not to the sender's own meeting
      // tab reliably enough to wait for: show it now, and let the echo
      // replace it by id rather than duplicate it.
      if (m?.id && !dm.messages.some((x) => x.id === m.id)) dm.messages.push(m);
      renderChat();
    } catch (e) {
      toast(e?.message || 'Could not send that message.', 'bad');
      input.value = text;
    }
  };
  $('chat-send').onclick = sendChat;
  $('chat-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') sendChat();
  });
  attachPicker($('chat-emoji'), $('chat-emoji-panel'), $('chat-input'));
  $('chat-to').addEventListener('change', (e) => pickChat(e.target.value));
  $('chat-tabs')?.addEventListener('click', (e) => {
    const b = e.target.closest('[data-chat]');
    if (b) pickChat(b.dataset.chat);
  });

  $('volume').addEventListener('input', (e) => {
    const v = Number(e.target.value) / 100;
    document.querySelectorAll('video').forEach((el) => {
      if (!el.muted) el.volume = v;
    });
  });

  // Muting is the most time-critical action in any meeting and should never
  // require finding a button.
  addEventListener('keydown', (e) => {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    if ($('app').classList.contains('hidden')) return;
    if (e.key === 'm' || e.key === 'M') $('btn-mic').click();
    if (e.key === 'v' || e.key === 'V') $('btn-cam').click();
    if (e.key === 'p' || e.key === 'P') togglePiP();
  });

  addEventListener('resize', () => {
    renderStage();
    reportViewport();
  });
  addEventListener('visibilitychange', () => {
    reportViewport(true);
    if (document.hidden) {
      if (autoPiPAvailable() && !pipActive()) {
        markAutoPiP(document.querySelector('#stage-video video'));
      }
      return;
    }
    // Coming back. Reporting the tiles visible again restarts the streams,
    // but the forwarder holds every packet until a keyframe arrives, and an
    // encoder nobody has asked sends its next one whenever it happens to --
    // seconds later, or not until something else asks. Without this, coming
    // back to the tab showed black squares for long enough to read as the
    // video having broken rather than as it returning.
    requestKeyframes();
  });
}

// ---------------------------------------------------------------------------
// Calling
//
// A ring, the way Teams does it: pick somebody you have a conversation with,
// and every device they are signed in on rings -- a dialog in the app and a
// system notification if the tab is in the background. Accepting opens the
// room; declining tells the caller. Thirty seconds unanswered is a missed
// call, and both sides are told that too, so nobody waits on a ring that
// nobody is going to answer.
// ---------------------------------------------------------------------------

const RING_TIMEOUT_MS = 30000;
let ringTone = null;
let ringTimer = null;
let outgoingRing = null; // { targetId, targetName, room }

function startRingTone() {
  try {
    const ctx = new AudioContext();
    const gain = ctx.createGain();
    gain.gain.value = 0.0001;
    gain.connect(ctx.destination);
    const osc = ctx.createOscillator();
    osc.type = 'sine';
    osc.frequency.value = 440;
    osc.connect(gain);
    osc.start();
    // A classic ring cadence: 1 s on, 2 s off.
    const tick = () => {
      const t = ctx.currentTime;
      gain.gain.setValueAtTime(0.0001, t);
      gain.gain.exponentialRampToValueAtTime(0.25, t + 0.05);
      gain.gain.setValueAtTime(0.25, t + 1.0);
      gain.gain.exponentialRampToValueAtTime(0.0001, t + 1.05);
    };
    tick();
    const iv = setInterval(tick, 3000);
    ringTone = { ctx, osc, iv };
    ctx.resume().catch(() => {});
  } catch {
    ringTone = null;
  }
}

function stopRingTone() {
  if (!ringTone) return;
  clearInterval(ringTone.iv);
  try {
    ringTone.osc.stop();
  } catch {
    /* already stopped */
  }
  ringTone.ctx.close().catch(() => {});
  ringTone = null;
}

function closeCallDialog() {
  stopRingTone();
  clearTimeout(ringTimer);
  ringTimer = null;
  const host = $('dialog');
  if (host && host.dataset.call === '1') {
    host.classList.add('hidden');
    host.innerHTML = '';
    delete host.dataset.call;
  }
}

// Outgoing: create a room, go there, and ring them.
async function placeCall(targetId, targetName, audioOnly = false) {
  const room = 'meet-' + Math.random().toString(36).slice(2, 8);
  // An audio call starts with the camera off on this end; the callee is told
  // via the ring's room suffix so their pre-join defaults the same way.
  if (audioOnly) state.cam = false;
  if (!sendRing(targetId, audioOnly ? room + '?audio=1' : room)) {
    toast('You are offline; the call could not be placed.', 'bad');
    return;
  }
  outgoingRing = { targetId, targetName, room };
  state.callRoom = room;
  try {
    sessionStorage.setItem('aether.callWith.' + room, targetId);
  } catch {}
  toast(`Calling ${targetName}…`);
  ringTimer = setTimeout(() => {
    if (outgoingRing?.room === room) {
      toast(`${targetName} did not answer.`);
      outgoingRing = null;
    }
  }, RING_TIMEOUT_MS);
  joinMeeting(room, `Call with ${targetName}`);
}

// Incoming: ring, notify, and ask.
function incomingCall({ callerId, callerName, room: rawRoom }) {
  // The caller may have asked for an audio call: the room carries a hint we
  // strip here and pass along on accept, so both sides start camera-off.
  const audioOnly = rawRoom.endsWith('?audio=1');
  const room = audioOnly ? rawRoom.slice(0, -'?audio=1'.length) : rawRoom;
  // Already in a meeting or already ringing: decline politely rather than
  // stack dialogs.
  if (state.ws?.readyState === WebSocket.OPEN && !$('view-meeting')?.classList.contains('hidden')) {
    answerRing(callerId, room, false);
    return;
  }
  const host = $('dialog');
  // Any dialog already open -- a call, a schedule form, an invite -- means the
  // person is mid-task. Decline politely rather than clobbering it; the caller
  // is told, and can try again.
  if (!host || !host.classList.contains('hidden')) {
    answerRing(callerId, room, false);
    return;
  }
  host.dataset.call = '1';
  host.innerHTML = `
    <div class="dialog-card call-card" role="alertdialog" aria-live="assertive">
      <span ${avatarAttrs(callerName, 'lg')}>${esc(initials(callerName))}</span>
      <h3>${esc(callerName)}</h3>
      <p>is calling you</p>
      <div class="call-actions">
        <button class="btn btn-quiet" data-decline>Decline</button>
        <button class="btn btn-primary" data-accept>
          <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-cam"/></svg> Accept
        </button>
      </div>
    </div>`;
  host.classList.remove('hidden');
  startRingTone();

  // A system notification, for a tab that is not in front. Permission is
  // asked for here, on a moment the person can see why.
  if ('Notification' in window && typeof window.aetherRinging !== 'function') {
    const show = () => {
      try {
        const n = new Notification(`${callerName} is calling`, {
          body: 'Open Aether to answer.',
          tag: 'aether-ring-' + room,
        });
        n.onclick = () => {
          window.focus();
          n.close();
        };
      } catch {
        /* blocked */
      }
    };
    if (Notification.permission === 'granted') show();
    else if (Notification.permission !== 'denied') Notification.requestPermission().then((p) => p === 'granted' && show());
  }

  const finish = (accepted) => {
    answerRing(callerId, room, accepted);
    closeCallDialog();
    if (accepted) {
      // Who this call is with, for the meeting page to open your
      // conversation at once (see callPeerFor). This tab's storage, keyed by
      // the room: not the link, which anybody could craft.
      try {
        sessionStorage.setItem('aether.callWith.' + room, callerId);
      } catch {}
      location.href = '/?room=' + encodeURIComponent(room) + '&call=1' + (audioOnly ? '&audio=1' : '');
    }
  };
  host.querySelector('[data-accept]').onclick = () => finish(true);
  host.querySelector('[data-decline]').onclick = () => finish(false);
  ringTimer = setTimeout(() => {
    toast(`Missed call from ${callerName}.`);
    finish(false);
  }, RING_TIMEOUT_MS);
}

function wireCalling() {
  addEventListener('aether:call', (e) =>
    placeCall(e.detail.targetId, e.detail.targetName, !!e.detail.audioOnly)
  );
  addEventListener('aether:ring', (e) => {
    // In the desktop shell the window may be hidden in the tray: a call is
    // the one thing that brings it back unasked, with the taskbar button
    // flashing and a Windows notification if it was out of sight.
    // Do not disturb: the call still shows in the window, quietly -- no
    // flash, no toast, no window pulled to the front.
    if (!isDND() && !inQuietHours()) {
      if (typeof window.aetherRinging === 'function') window.aetherRinging(e.detail.callerName || 'Somebody');
      if (typeof window.aetherShowWindow === 'function') window.aetherShowWindow();
    }
    incomingCall(e.detail);
  });
  addEventListener('aether:ring-answer', (e) => {
    const { calleeName, room, accepted } = e.detail;
    if (outgoingRing?.room !== room) return;
    clearTimeout(ringTimer);
    if (!accepted) toast(`${calleeName} declined the call.`);
    outgoingRing = null;
  });
}
wireCalling();
// A people picker that found nobody can point an admin at the directory
// setting; the console lives in this module, the picker does not.
document.addEventListener('aether:open-console', (e) => openConsole(e.detail || 'policy'));
// My own status changed (set here, on another device, or by a call):
// the dot on my avatar in the bar follows.
addEventListener('aether:my-status', () => {
  const wrap = document.getElementById('top-avatar')?.closest('.av-wrap');
  if (wrap) {
    wrap.querySelector('.presence')?.remove();
    wrap.insertAdjacentHTML('beforeend', myPresenceDot());
  }
});

// ---------------------------------------------------------------------------
// Profile menu
//
// Every account action -- who you are, your settings, administration, sign
// out -- hangs off the avatar, which is where people look for it. It opens on
// hover because that is what was asked for, and ALSO on click, because hover
// does not exist on a touch screen and a menu that cannot be opened on a
// phone is not a menu.
// ---------------------------------------------------------------------------

let pmEl = null;
let pmCloseTimer = null;

// Availability, at the top of the profile menu: the four states, a note,
// and how long it lasts. Do not disturb silences everything but urgent.
function statusSection() {
  const st = myStatus();
  // The picker offers the four a person chooses. In a call and Presenting
  // come from the rooms and are shown, not offered; a calendar meeting
  // shows as "In a meeting" the same way.
  const optLabel = (k) => (k === 'offline' ? 'Appear offline' : statusLabel(k));
  const opt = (k) => `<button type="button" class="st-opt${st.status === k && !st.auto ? ' active' : ''}" data-status="${k}" role="menuitemradio" aria-checked="${st.status === k && !st.auto}">
      ${presenceIcon(k, { title: optLabel(k), online: k !== 'offline' })}${optLabel(k)}</button>`;
  const autoLine = st.auto
    ? `<div class="st-auto">${presenceIcon(st.status, { title: statusLabel(st.status) })}${esc(st.note)} <small>automatic</small></div>`
    : '';
  return `<div class="pm-status">
    ${autoLine}
    <div class="st-opts">${['available', 'busy', 'dnd', 'away', 'offline'].map(opt).join('')}</div>
    ${quietRow()}
    <div class="st-note-row">
      <input id="st-note" class="input" placeholder="Set a status message" maxlength="120" value="${esc(st.note || '')}" autocomplete="off" />
      <select id="st-until" class="input" aria-label="Clear after">
        <option value="">Keep</option>
        <option value="30">30 min</option>
        <option value="60">1 hour</option>
        <option value="240">4 hours</option>
        <option value="today">Today</option>
      </select>
    </div>
  </div>`;
}
function quietRow() {
  const q = quietHours();
  return `<div class="quiet-row${q.on ? ' on' : ''}">
    <label class="quiet-main">
      <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-moon"/></svg>
      <span class="quiet-label">Quiet hours <small>${q.on ? `${esc(q.from)} – ${esc(q.to)}` : 'off'}</small></span>
      <span class="switch"><input type="checkbox" id="q-on" ${q.on ? 'checked' : ''} aria-label="Quiet hours" /><span></span></span>
    </label>
    <div class="quiet-times"${q.on ? '' : ' hidden'}>
      <input type="time" id="q-from" class="input tiny" value="${esc(q.from)}" aria-label="From" />
      <span class="q-dash">to</span>
      <input type="time" id="q-to" class="input tiny" value="${esc(q.to)}" aria-label="To" />
    </div>
  </div>`;
}
function wireQuietRow(el) {
  const save = () => {
    const q = { on: !!el.querySelector('#q-on')?.checked, from: el.querySelector('#q-from')?.value || '22:00', to: el.querySelector('#q-to')?.value || '08:00' };
    setQuietHours(q);
    el.querySelector('.quiet-row')?.classList.toggle('on', q.on);
    const times = el.querySelector('.quiet-times');
    if (times) times.hidden = !q.on;
    const lab = el.querySelector('.quiet-label small');
    if (lab) lab.textContent = q.on ? `${q.from} – ${q.to}` : 'off';
  };
  for (const id of ['q-on', 'q-from', 'q-to']) {
    const i = el.querySelector('#' + id);
    i?.addEventListener('click', (e) => e.stopPropagation());
    i?.addEventListener('change', save);
  }
}
function wireStatusSection(el) {
  wireQuietRow(el);
  const untilValue = () => {
    const v = el.querySelector('#st-until')?.value;
    if (!v) return null;
    if (v === 'today') {
      const d = new Date();
      d.setHours(23, 59, 0, 0);
      return d.toISOString();
    }
    return new Date(Date.now() + Number(v) * 60_000).toISOString();
  };
  const save = async (status) => {
    const note = el.querySelector('#st-note')?.value.trim() ?? '';
    try {
      await messaging.setStatus(status, note, untilValue());
      el.querySelectorAll('[data-status]').forEach((b) => {
        b.classList.toggle('active', b.dataset.status === status);
        b.setAttribute('aria-checked', String(b.dataset.status === status));
      });
    } catch (e) {
      toast(e.message || 'Could not set your status.', 'bad');
    }
  };
  el.querySelectorAll('[data-status]').forEach((b) => {
    b.addEventListener('click', (e) => {
      e.stopPropagation();
      save(b.dataset.status);
    });
  });
  const note = el.querySelector('#st-note');
  note?.addEventListener('click', (e) => e.stopPropagation());
  note?.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      save(el.querySelector('.st-opt.active')?.dataset.status || 'available');
    }
  });
  el.querySelector('#st-until')?.addEventListener('click', (e) => e.stopPropagation());
  el.querySelector('#st-until')?.addEventListener('change', () => save(el.querySelector('.st-opt.active')?.dataset.status || 'available'));
}

function buildProfileMenu() {
  if (pmEl) return pmEl;
  pmEl = document.createElement('div');
  pmEl.className = 'pop-menu hidden';
  pmEl.setAttribute('role', 'menu');
  document.body.appendChild(pmEl);

  // The menu keeps itself open while the pointer is inside it, and closes a
  // beat after the pointer leaves. The delay is what makes the diagonal move
  // from avatar to menu survivable; without it the menu vanishes the moment
  // the pointer crosses the gap.
  pmEl.addEventListener('mouseenter', () => clearTimeout(pmCloseTimer));
  pmEl.addEventListener('mouseleave', scheduleMenuClose);

  addEventListener('click', (e) => {
    if (pmEl && !pmEl.contains(e.target) && !e.target.closest?.('[data-profile]')) {
      hideProfileMenu();
    }
  });
  addEventListener('keydown', (e) => {
    if (e.key === 'Escape') hideProfileMenu();
  });
  return pmEl;
}

function scheduleMenuClose() {
  clearTimeout(pmCloseTimer);
  pmCloseTimer = setTimeout(hideProfileMenu, 320);
}

function hideProfileMenu() {
  pmEl?.classList.add('hidden');
}

function showProfileMenu(anchor) {
  const u = session.user;
  if (!u) return;
  const el = buildProfileMenu();
  const canAdmin = session.can('user.read') || session.can('audit.read');

  el.innerHTML = `
    <div class="pm-head">
      <span ${avatarAttrs({ userId: u.id, name: u.displayName, email: u.email })}>${esc(
        initials({ userId: u.id, name: u.displayName, email: u.email })
      )}</span>
      <div class="pm-who">
        <b>${esc(u.displayName || u.email)}</b>
        <span>${esc(u.email)}</span>
      </div>
    </div>
    ${statusSection()}
    <button class="pm-item" data-pm="account" role="menuitem">
      <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-people"/></svg>
      Your account
    </button>
    ${
      canAdmin
        ? `<button class="pm-item" data-pm="admin" role="menuitem">
             <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-shield"/></svg>
             Administration
           </button>`
        : ''
    }
    <div class="pm-sep"></div>
    <button class="pm-item danger" data-pm="signout" role="menuitem">
      <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-close"/></svg>
      Sign out
    </button>`;

  el.querySelector('[data-pm="account"]').onclick = () => {
    hideProfileMenu();
    openConsole('account');
  };
  wireStatusSection(el);
  el.querySelector('[data-pm="admin"]')?.addEventListener('click', () => {
    hideProfileMenu();
    openConsole('users');
  });
  el.querySelector('[data-pm="signout"]').onclick = async () => {
    hideProfileMenu();
    await logout();
    location.href = location.origin;
  };

  // Position below the anchor, right edges aligned. The avatar lives at the
  // top right of both the web bar and the app's title bar now, so the menu
  // drops from it the way every account menu in that corner does. Clamped to
  // the viewport so a narrow window never pushes it off the left edge.
  el.classList.remove('hidden');
  const r = anchor.getBoundingClientRect();
  const mw = el.offsetWidth;
  const mh = el.offsetHeight;
  let left = r.right - mw;
  let top = r.bottom + 8;
  if (top + mh > innerHeight - 8) top = Math.max(8, r.top - mh - 8);
  left = Math.max(8, Math.min(left, innerWidth - mw - 8));
  el.style.left = `${left}px`;
  el.style.top = `${top}px`;
}

// attachProfileMenu wires one avatar to the menu: hover opens, click toggles.
function attachProfileMenu(anchor) {
  if (!anchor || anchor.dataset.profile) return;
  anchor.dataset.profile = '1';
  anchor.addEventListener('mouseenter', () => {
    clearTimeout(pmCloseTimer);
    showProfileMenu(anchor);
  });
  anchor.addEventListener('mouseleave', scheduleMenuClose);
  anchor.addEventListener('click', (e) => {
    e.stopPropagation();
    if (pmEl && !pmEl.classList.contains('hidden')) hideProfileMenu();
    else showProfileMenu(anchor);
  });
}

// ---------------------------------------------------------------------------
// Pre-join
// ---------------------------------------------------------------------------

const pj = { stream: null, ctx: null, raf: 0 };

async function openPreJoin() {
  show('prejoin');
  $('pj-title').textContent = state.title || 'Ready to join?';
  $('pj-sub').textContent = 'Check your camera and microphone before you join.';
  $('pj-avatar').textContent = initials(
    session.user?.displayName || session.user?.email || '?'
  );
  $('pj-note').classList.add('hidden');

  await acquirePreview();
  await listDevices();
  syncPreJoinControls();
  startPreviewMeter();
}

// Acquisitions never overlap. Each one stops what the previous one opened,
// so two in flight at once (Join pressed while the preview was still
// opening; a toggle during a device change) would stop each other's tracks
// and hand the meeting an ended microphone -- which is exactly what the
// ring flow did in mute.e2e.mjs: "Input track cannot be ended", and nobody
// heard the caller. One lane; Join waits for it (pj-join below).
async function acquirePreview() {
  const run = () => acquirePreviewNow();
  pj.acquiring = (pj.acquiring ?? Promise.resolve()).then(run, run);
  return pj.acquiring;
}

// Acquired separately, for the same reason startMedia does it: one call over
// two devices means a busy camera also costs you your microphone.
async function acquirePreviewNow() {
  stopPreview();
  // Every re-acquisition (a toggle, a device change) replaced the stream and
  // left the old tracks running: a microphone turned off in the pre-join was
  // still open, its light still on, until the page was left.
  pj.stream?.getTracks().forEach((t) => t.stop());
  pj.stream = null;
  state.denoiser?.stop();
  state.denoiser = null;
  const stream = new MediaStream();
  const notes = [];

  if (state.mic) {
    try {
      // Choosing a specific microphone must not quietly turn the
      // processing off. Naming only the deviceId left echo cancellation,
      // noise suppression and gain control to whatever the browser felt
      // like, so picking a headset could sound worse than not choosing at
      // all -- with nothing in the interface to suggest why. echo.js asks
      // for the processing as a requirement and reports what it got.
      const { track: raw, report } = await acquireMic((c) => navigator.mediaDevices.getUserMedia(c), state.micDeviceId || '');
      noteMicProcessing(report);
      if (raw) stream.addTrack(await micTrackFor(raw));
      if (!report.ok) notes.push(report.message);
    } catch (err) {
      notes.push('Microphone unavailable — others will not hear you.');
      state.mic = false;
    }
  }

  if (state.cam) {
    try {
      const v = await navigator.mediaDevices.getUserMedia({
        video: state.camDeviceId
          ? { deviceId: { exact: state.camDeviceId } }
          : { width: { ideal: 1280 }, height: { ideal: 720 } },
      });
      for (const t of v.getVideoTracks()) stream.addTrack(await camTrackFor(t));
    } catch (err) {
      notes.push('Camera unavailable — you can still be heard and share your screen.');
      state.cam = false;
    }
  }

  pj.stream = stream;
  state.localStream = stream;

  const video = $('pj-video');
  video.srcObject = stream;
  if (notes.length) {
    $('pj-note').textContent = notes.join(' ');
    $('pj-note').classList.remove('hidden');
  }
}

// Device labels are only readable after permission has been granted, which is
// why this runs after acquiring rather than before: called first it returns a
// list of blank names, which is worse than no list at all.
async function listDevices() {
  let devices = [];
  try {
    devices = await navigator.mediaDevices.enumerateDevices();
  } catch {
    return;
  }
  const fill = (sel, kind, chosen) => {
    const el = $(sel);
    el.innerHTML = '';
    const list = devices.filter((d) => d.kind === kind);
    if (!list.length) {
      el.innerHTML = '<option>No device found</option>';
      el.disabled = true;
      return;
    }
    el.disabled = false;
    for (const d of list) {
      const o = document.createElement('option');
      o.value = d.deviceId;
      o.textContent = d.label || `${kind} ${el.length + 1}`;
      if (d.deviceId === chosen) o.selected = true;
      el.appendChild(o);
    }
  };
  fill('pj-cam-select', 'videoinput', state.camDeviceId);
  fill('pj-mic-select', 'audioinput', state.micDeviceId);
}

function syncPreJoinControls() {
  $('pj-mic').classList.toggle('off', !state.mic);
  $('pj-cam').classList.toggle('off', !state.cam);
  const live = state.cam && !!pj.stream?.getVideoTracks().length;
  $('pj-video').classList.toggle('hidden', !live);
  $('pj-video').classList.toggle('unmirrored', !state.mirrorSelf);
  $('pj-off').classList.toggle('hidden', live);
}

// The level meter is the actual microphone test: a toggle proves a switch is
// set, a moving bar proves the device works.
function startPreviewMeter() {
  stopPreviewMeter();
  const track = pj.stream?.getAudioTracks()[0];
  if (!track) return;
  try {
    pj.ctx = new (window.AudioContext || window.webkitAudioContext)();
    const src = pj.ctx.createMediaStreamSource(pj.stream);
    const analyser = pj.ctx.createAnalyser();
    analyser.fftSize = 512;
    analyser.smoothingTimeConstant = 0.65;
    src.connect(analyser);
    const buf = new Uint8Array(analyser.frequencyBinCount);
    const bar = $('pj-level');
    const tick = () => {
      analyser.getByteFrequencyData(buf);
      let sum = 0;
      for (const v of buf) sum += v * v;
      const rms = Math.sqrt(sum / buf.length) / 255;
      bar.style.width = `${Math.min(100, Math.round(rms * 260))}%`;
      pj.raf = requestAnimationFrame(tick);
    };
    pj.raf = requestAnimationFrame(tick);
  } catch {
    // No audio context is survivable; the meter simply stays still.
  }
}

function stopPreviewMeter() {
  if (pj.raf) cancelAnimationFrame(pj.raf);
  pj.raf = 0;
  pj.ctx?.close?.().catch(() => {});
  pj.ctx = null;
}

// Only the meter and the preview element are torn down. The tracks are handed
// to the meeting, because stopping and re-acquiring them would put the camera
// light out and back on again and cost a visible second at the worst moment.
function stopPreview() {
  stopPreviewMeter();
  $('pj-video').srcObject = null;
}

function wirePreJoin() {
  // Noise cancellation: on by default, remembered, re-acquired on change so
  // the level meter shows what will be sent. Hidden where the platform
  // cannot run it (no AudioWorklet or WebAssembly) rather than shown broken.
  const dn = $('pj-denoise');
  if (dn) {
    dn.closest('.field')?.classList.toggle('hidden', !denoiseSupported());
    dn.checked = denoiseWanted();
    dn.onchange = async (e) => {
      setDenoiseWanted(e.target.checked);
      await acquirePreview();
      syncPreJoinControls();
      startPreviewMeter();
    };
  }
  $('pj-mic').onclick = async () => {
    state.mic = !state.mic;
    await acquirePreview();
    syncPreJoinControls();
    startPreviewMeter();
  };
  $('pj-cam').onclick = async () => {
    state.cam = !state.cam;
    await acquirePreview();
    syncPreJoinControls();
    startPreviewMeter();
  };
  $('pj-cam-select').onchange = async (e) => {
    state.camDeviceId = e.target.value;
    await acquirePreview();
    syncPreJoinControls();
    startPreviewMeter();
  };
  $('pj-mic-select').onchange = async (e) => {
    state.micDeviceId = e.target.value;
    await acquirePreview();
    syncPreJoinControls();
    startPreviewMeter();
  };
  $('pj-join').onclick = async () => {
    $('pj-join').disabled = true;
    // Whatever the preview is still opening finishes first, so the meeting
    // starts on the tracks the person will actually be heard through.
    try {
      await pj.acquiring;
    } catch {}
    stopPreviewMeter();
    $('pj-video').srcObject = null;
    try {
      await enterMeeting();
    } finally {
      $('pj-join').disabled = false;
    }
  };
  $('pj-fx').onclick = () => openEffectsSheet();
  $('pj-back').onclick = () => {
    stopPreview();
    pj.stream?.getTracks().forEach((t) => t.stop());
    state.denoiser?.stop();
    state.denoiser = null;
    pj.stream = null;
    state.localStream = null;
    show('home');
  };
}

// ---------------------------------------------------------------------------
// The waiting room
// ---------------------------------------------------------------------------

function showLobby(status, message) {
  const cover = $('lobby-cover');
  const spin = $('lobby-spinner');
  const head = $('lobby-head');
  const msg = $('lobby-msg');

  if (status === 'admitted') {
    cover.classList.add('hidden');
    spin.classList.remove('done');
    return;
  }

  cover.classList.remove('hidden');
  spin.classList.toggle('done', status !== 'waiting');
  $('lobby-leave').textContent = status === 'ended' || status === 'removed' ? 'Back to home' : 'Leave';
  switch (status) {
    case 'waiting':
      head.textContent = 'Asking to be let in…';
      msg.textContent = message || 'Someone in the meeting will let you in shortly.';
      break;
    case 'denied':
      head.textContent = 'Not admitted';
      msg.textContent = message || 'You were not let into this meeting.';
      break;
    case 'expired':
      head.textContent = 'Nobody answered';
      msg.textContent = message || 'No one let you in. You can try again later.';
      break;
    case 'locked':
      head.textContent = 'Meeting locked';
      msg.textContent = message || 'The host has locked this meeting.';
      break;
    case 'ended':
      head.textContent = 'This meeting has ended';
      msg.textContent = message || 'The host ended the meeting for everyone.';
      break;
    case 'removed':
      head.textContent = "You've been removed from this meeting";
      msg.textContent = 'Someone just removed you from the meeting. You can try to rejoin.';
      break;
    default:
      head.textContent = 'Cannot join';
      msg.textContent = message || 'You were not admitted to this meeting.';
  }
}

// The host's list of people knocking. Only hosts are ever sent it: the names
// belong to people who are not in the meeting, and showing the room who tried
// to get in and failed is exactly what a waiting room exists to prevent.
function renderAdmitList(waiting) {
  state.waiting = waiting;
  renderPeople(); // the inline list in the participants tab
  const panel = $('admit-panel');
  const list = $('admit-list');

  if (!waiting.length) {
    panel.classList.add('hidden');
    list.innerHTML = '';
    return;
  }
  panel.classList.remove('hidden');
  $('admit-count').textContent =
    waiting.length === 1 ? '1 person waiting' : `${waiting.length} people waiting`;

  list.innerHTML = '';
  for (const w of waiting) {
    const row = document.createElement('div');
    row.className = 'admit-row';
    const who = document.createElement('div');
    who.className = 'who';
    const b = document.createElement('b');
    b.textContent = w.name || 'Guest';
    const small = document.createElement('small');
    small.textContent = w.authenticated ? 'Signed in' : 'Guest — not signed in';
    who.append(b, small);

    const yes = document.createElement('button');
    yes.className = 'btn btn-primary sm';
    yes.textContent = 'Admit';
    yes.onclick = () => decideLobby(w.id, true);

    const no = document.createElement('button');
    no.className = 'btn btn-quiet sm';
    no.textContent = 'Deny';
    no.onclick = () => decideLobby(w.id, false);

    row.append(who, yes, no);
    list.appendChild(row);
  }
}

function decideLobby(id, admit, all = false) {
  send((w) => {
    w.header(Op.LobbyDecide);
    w.str(id ?? '');
    w.bool(admit);
    w.bool(all);
  });
}

function wireLobby() {
  $('lobby-leave').onclick = () => leaveMeeting();
  $('admit-close').onclick = () => $('admit-panel').classList.add('hidden');
  $('admit-all').onclick = () => decideLobby('', true, true);
  $('deny-all').onclick = () => decideLobby('', false, true);
}

// ---------------------------------------------------------------------------
// Screens
// ---------------------------------------------------------------------------

function show(which) {
  for (const id of ['auth', 'home', 'prejoin', 'app']) {
    $(id).classList.toggle('hidden', id !== which);
  }
}

// Joining is two steps now, and the first one is the point.
//
// Nobody should discover which microphone they are on, or that their camera
// was already running, from the reaction of a room full of people. So the
// devices are acquired, previewed and adjusted BEFORE the socket is opened
// and before anything is published -- which also means a person who decides
// against joining never appeared at all.
async function joinMeeting(room, title) {
  state.room = room;
  state.sameDeviceTold = false;
  state.chatToPicked = false;
  state.chatToAuto = false;
  // A one-to-one call: fetch your conversation with them now, while the
  // pre-join is on screen, so the call's chat opens with its history
  // instead of "Opening your conversation..." after you walk in. The
  // meeting is its own page, so the thread the chat screen had is gone.
  const peer = callPeerFor(room);
  // ...and open on it before the roster says they are here: the person you
  // called or answered is known already, so the panel need not wait.
  state.callPeer = peer;
  state.callPeerSeen = false;
  if (peer) {
    loadDirect(peer);
    state.chatTo = peer;
  }
  state.title = title || room;
  // A room reached through a call ends for everyone when the other person
  // hangs up, the way a phone call does. Marked by the caller when placing
  // it and by the callee via ?call=1 on accept.
  const q = new URLSearchParams(location.search);
  if (q.get('call') === '1') state.callRoom = room;
  // An audio call: the camera starts off. The person can turn it on; this is
  // the default the caller asked for, not a restriction.
  if (q.get('audio') === '1') state.cam = false;
  state.chat = [];
  state.meetingConv = null;
  state.meetingConvTries = 0;
  await openPreJoin();
  // Moved here between rooms: the person already chose to be in the meeting.
  if (state.autoJoin) {
    state.autoJoin = false;
    $('pj-join').click();
  }
}

// Keep the machine awake for the length of a meeting. A laptop that nobody
// touches drops into Modern Standby mid-call -- measured: every stream on
// every receiver stalled together for ~25 s at the moment Windows logged
// "The system is entering Modern Standby", in a call where people were only
// listening. The Wake Lock API is what a video player or Meet uses; it
// works in the Windows app's WebView2 too. The lock is released by the
// browser whenever the page is hidden, so it is taken again when the page
// comes back; leaving the meeting reloads the page, which releases it.
let wakeLock = null;
async function holdWake() {
  try {
    if (!('wakeLock' in navigator) || document.visibilityState !== 'visible') return;
    if (wakeLock && !wakeLock.released) return;
    wakeLock = await navigator.wakeLock.request('screen');
    document.documentElement.dataset.wake = 'held';
    wakeLock.addEventListener('release', () => {
      document.documentElement.dataset.wake = 'released';
    });
  } catch (e) {
    // Refused (battery saver, policy): the call works; the machine may sleep.
    document.documentElement.dataset.wake = 'refused';
  }
}
document.addEventListener('visibilitychange', () => {
  if (state.room && !$('app').classList.contains('hidden')) holdWake();
});

async function enterMeeting() {
  const room = state.room;
  state.joinedAt = Date.now();
  holdWake();

  show('app');
  $('room-title').textContent = state.title;
  $('room-pill').textContent = room;
  // Teams opens a meeting on the people, with Chat and People closed.
  closeSide();
  renderChat();

  // Media was already acquired in the pre-join screen, with the toggles the
  // user chose. Only fall back to acquiring it here if that was skipped.
  if (!state.localStream) await startMedia();
  connect(room, session.user.displayName || session.user.email);
  await startMediaPlane();
  await reconcileMic();
  await reconcileCamera();

  if (!state.timers) {
    state.timers = true;
    // The meeting's running time, in the bar, as Teams shows it: m:ss, then
    // h:mm:ss. It read 00:00 for the whole meeting before; nothing set it.
    const clock = () => {
      const t = Math.max(0, Math.floor((Date.now() - state.joinedAt) / 1000));
      const h = Math.floor(t / 3600), m = Math.floor((t % 3600) / 60), sec = String(t % 60).padStart(2, '0');
      $('chip-time').textContent = h ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${String(m).padStart(2, '0')}:${sec}`;
    };
    clock();
    setInterval(clock, 1000);
    // A breakout room's clock, in its banner and in the managers' panel.
    setInterval(() => {
      if (state.bo?.closes) renderBoBanner();
      const left = $('br-left');
      if (left && state.breakouts?.closes) left.textContent = remaining(state.breakouts.closes);
    }, 1000);
    // "5 minutes left in your meeting." for a scheduled meeting. A notice;
    // nothing ends. The calendar may not be loaded when the meeting was
    // joined straight from a link, so it is loaded first.
    (state.meetings?.length ? Promise.resolve() : loadMeetings()).then(() => {
      const m = (state.meetings ?? [])
        .filter((x) => x.room === state.room && x.status !== 'cancelled')
        .sort((a, b) => Math.abs(a.start - Date.now()) - Math.abs(b.start - Date.now()))[0];
      const wait = endNoticeDelay(m?.end);
      if (wait != null) setTimeout(() => { if (!state.ended) toast('5 minutes left in your meeting.'); }, wait);
    }).catch(() => {});
    // The gallery is sized to the stage, so it follows the window.
    addEventListener('resize', () => renderStage());
    setInterval(() => reportViewport(), 500);
    setInterval(reportBwe, 2000);
  }
  syncControls();
}

async function leaveMeeting() {
  // A recording made on this machine is saved before anything is torn down:
  // the reload below would otherwise throw it away.
  if (state.recorder) {
    toast('Saving the recording…');
    await stopLocalRecording();
    await new Promise((r) => setTimeout(r, 800)); // let the download start
  }
  // A reload is the honest way to tear down two peer connections, a
  // WebSocket, a media stream, and an audio context. Anything cleverer here
  // leaks a camera light somebody will notice.
  location.href = location.origin;
}

// ---------------------------------------------------------------------------
// Home
// ---------------------------------------------------------------------------

const CATEGORIES = [
  { id: 'business', label: 'Business', color: '#e8705f' },
  { id: 'design', label: 'Design', color: '#93c5dd' },
  { id: 'internal', label: 'Internal', color: '#6f9e88' },
  { id: 'external', label: 'External', color: '#2e4a62' },
  { id: 'personal', label: 'Personal', color: '#3f6d59' },
];

const CAT_CLASS = {
  business: 'c-coral',
  design: 'c-sky',
  internal: 'c-sage',
  external: 'c-navy',
  personal: 'c-forest',
};

async function loadMeetings() {
  try {
    const from = new Date();
    from.setMonth(from.getMonth() - 2);
    const to = new Date();
    to.setMonth(to.getMonth() + 6);
    const res = await scheduling.list({
      from: from.toISOString(),
      to: to.toISOString(),
    });
    // What actually happened, for "recent". Independent of the calendar:
    // an ad-hoc room appears here and nowhere else.
    try {
      const h = await scheduling.history();
      state.history = (h.history ?? []).map((a) => ({
        ...a,
        joined: new Date(a.joinedAt),
        left: a.leftAt ? new Date(a.leftAt) : null,
      }));
    } catch {
      state.history = state.history ?? [];
    }
    state.meetings = (res.meetings ?? []).map((m) => ({
      ...m,
      start: new Date(m.startsAt),
      end: new Date(m.endsAt),
    }));
  } catch {
    state.meetings = [];
  }
}

// A person's avatar tone, derived from their name so it is the same colour
// everywhere they appear.
//
// Eight muted hues rather than a hash straight to 360 degrees: an unbounded
// hue lands on yellows and limes that cannot carry white text, and picking
// per-person from a curated set is the difference between a palette and a
// paintbox. See .avatar[data-tone] for the tones themselves.
function greeting() {
  const h = new Date().getHours();
  if (h < 5) return 'Good evening';
  if (h < 12) return 'Good morning';
  if (h < 17) return 'Good afternoon';
  return 'Good evening';
}

function renderHome() {
  // Rebuild the surface rather than assuming it survived. Calendar and Chat
  // replace this container, so anything that reads from it must put it back
  // first or it will be reading from whatever was rendered last.
  const u = session.user;
  document.querySelector('.home-main').innerHTML = `
    <header class="home-head">
      <div>
        <p class="home-hello" id="home-hello">${esc(greeting())}, ${esc(
          (u.displayName || u.email).split(/[\s@]/)[0]
        )}</p>
        <h1>Meetings</h1>
      </div>
      <div class="spacer"></div>
      <div class="home-clock">
        <strong id="home-time">—</strong>
        <span class="home-date" id="home-date"></span>
      </div>
    </header>

    <!-- The part of the page that answers the only question somebody opening
         this app actually has: what is next, and can I get into it. -->
    <div class="hero">
      <div class="hero-next" id="hero-next"></div>
      <aside class="hero-today">
        <div class="hero-today-head">
          <h2>Today</h2>
          <span class="count" id="today-count">0</span>
        </div>
        <div class="agenda" id="agenda"></div>
      </aside>
    </div>

    <div class="quick">
      <button class="quick-card primary" id="q-new">
        <span class="quick-icon"><svg width="19" height="19" viewBox="0 0 24 24"><use href="#i-cam" /></svg></span>
        <span class="quick-label">New meeting</span>
        <span class="quick-sub">Start one right now</span>
      </button>
      <button class="quick-card" id="q-join">
        <span class="quick-icon"><svg width="19" height="19" viewBox="0 0 24 24"><use href="#i-arrow-right" /></svg></span>
        <span class="quick-label">Join</span>
        <span class="quick-sub">With a room name or link</span>
      </button>
      <button class="quick-card" id="q-schedule">
        <span class="quick-icon"><svg width="19" height="19" viewBox="0 0 24 24"><use href="#i-calendar" /></svg></span>
        <span class="quick-label">Schedule</span>
        <span class="quick-sub">Pick a time and invite people</span>
      </button>
    </div>

    <!-- Counted from the meetings already loaded, so it costs no request and
         cannot disagree with the lists below it. -->
    <div class="stats" id="stats"></div>

    <div class="home-split">
      <section class="home-section">
        <div class="home-section-head"><h2>Upcoming</h2><span class="count" id="up-count">0</span></div>
        <div class="up-grid" id="up-grid"></div>
      </section>

      <section class="home-section">
        <div class="home-section-head"><h2>People</h2></div>
        <div class="people-strip" id="people-strip"></div>
      </section>
    </div>

    <section class="home-section">
      <div class="home-section-head"><h2>History</h2></div>
      <div class="hist" id="hist"></div>
    </section>`;

  wireHomeActions();
  paintHome();
  startHomeClock();
}

// The clock and the countdown tick together, on one timer.
//
// One interval rather than one per card: a page with a timer behind every
// element spends its afternoon waking up to redraw text that has not changed,
// and on a laptop that is measurable battery.
function startHomeClock() {
  stopHomeClock();
  const tick = () => {
    const el = $('home-time');
    if (!el) return stopHomeClock();
    el.textContent = new Date().toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
    });
    paintCountdown();
  };
  tick();
  state.homeTimer = setInterval(tick, 1000);
}

function stopHomeClock() {
  if (state.homeTimer) clearInterval(state.homeTimer);
  state.homeTimer = 0;
}

// ---------------------------------------------------------------------------
// What the home page is made of
// ---------------------------------------------------------------------------

function nextMeeting() {
  const now = Date.now();
  return (
    state.meetings
      .filter((m) => m.status === 'scheduled' && m.end.getTime() >= now)
      .sort((a, b) => a.start - b.start)[0] ?? null
  );
}

function todaysMeetings() {
  const today = new Date();
  return state.meetings
    .filter((m) => m.status === 'scheduled' && sameDay(m.start, today))
    .sort((a, b) => a.start - b.start);
}

// A week of activity, counted from what is already loaded.
function weekStats() {
  const now = new Date();
  const start = new Date(now);
  start.setDate(now.getDate() - now.getDay());
  start.setHours(0, 0, 0, 0);
  const end = new Date(start);
  end.setDate(start.getDate() + 7);

  const inWeek = state.meetings.filter(
    (m) => m.start >= start && m.start < end && m.status !== 'cancelled'
  );
  let minutes = 0;
  const people = new Set();
  for (const m of inWeek) {
    minutes += Math.max(0, Math.round((m.end - m.start) / 60000));
    for (const i of m.invitees ?? []) if (i.email) people.add(i.email.toLowerCase());
  }
  return { count: inWeek.length, minutes, people: people.size };
}

// Who you actually talk to, taken from chat rather than from invitee lists.
//
// An invitee list answers "who was on this calendar entry", which includes the
// whole team, the room account and whoever was copied in once. It is not the
// same question as "who do I work with", and using it produced a strip full of
// people the user has never spoken to.
function recentPeople(limit = 8) {
  return frequentContacts(limit, session.user?.id ?? '');
}

function humanDuration(min) {
  if (min <= 0) return '0m';
  const h = Math.floor(min / 60);
  const m = min % 60;
  if (!h) return `${m}m`;
  return m ? `${h}h ${m}m` : `${h}h`;
}

// "in 4 minutes", "starting now", "24 minutes ago". Written out rather than a
// bare clock time because the question is never what time it is, it is whether
// to get up from the desk.
// A meeting is running because people are in it, not because the clock says
// so. An hour booked for an hour that runs to ninety minutes is still
// happening; an hour everybody left after ten minutes is over.
function countdownText(target, endsAt, m) {
  const now = Date.now();
  // Live, completed, cancelled and ended are all decided by meeting.js, which
  // is where that judgement is tested. Only a meeting still ahead of us gets
  // a countdown, which is the part that has to be worded.
  if (m) {
    const said = phaseLabel(m, now);
    if (said) return said;
  }
  const diff = target.getTime() - now;
  const mins = Math.round(diff / 60000);
  if (mins <= 0) return 'Starting now';
  if (mins < 60) return `in ${mins} minute${mins === 1 ? '' : 's'}`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `in ${hrs} hour${hrs === 1 ? '' : 's'}`;
  const days = Math.round(hrs / 24);
  return `in ${days} day${days === 1 ? '' : 's'}`;
}

function paintCountdown() {
  const el = document.getElementById('next-countdown');
  if (!el) return;
  const m = state.meetings.find((x) => x.id === el.dataset.id);
  if (!m) return;
  el.textContent = countdownText(m.start, m.end, m);
  const phase = meetingPhase(m);
  el.classList.toggle('live', phase === 'live' || phase === 'now');
}

// One field, two kinds of answer: people you can message or call, and meetings
// on the calendar. Drawn under the field rather than on a page of its own,
// because a search that navigates away has lost the thing you were doing.
// The application bar: the mark, one search field, the account.
//
// It sits directly under the system caption in the same colour, so the two
// read as one band rather than as a thin grey strip with a different-coloured
// strip beneath it. That is the whole reason it looks the way it does; an
// earlier version drew the caption itself, and that came out.
//
// Present from the moment the window is, signed in or not, because it is
// chrome rather than part of a screen.
// Two products, two bars.
//
// The desktop app owns its window: the system caption is removed and this
// fixed strip is drawn in its place, carrying the mark, the name, search, the
// theme toggle, the account and the window buttons. The web is a page in a
// browser tab -- the tab names it, the browser frames it -- so none of that
// applies, and it keeps a bar of its own inside the page (renderWebBar). The
// two are not kept in step on purpose: the web must never grow the app's
// chrome, and the app must never look like a page.
function renderTitleBar() {
  const bar = document.getElementById('titlebar');
  if (!bar) return;
  if (!inDesktopShell()) {
    bar.classList.add('hidden');
    bar.innerHTML = '';
    document.body.classList.remove('has-titlebar');
    renderWebBar();
    return;
  }
  const u = session.user;

  bar.classList.remove('hidden');
  document.body.classList.add('has-titlebar');
  bar.innerHTML = `
    <button class="tb-icon" id="tb-collapse" data-no-drag
            title="${railCollapsed() ? 'Show sidebar' : 'Hide sidebar'}" aria-label="Toggle sidebar"
            aria-pressed="${railCollapsed() ? 'true' : 'false'}">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round">
        <rect x="3" y="4.5" width="18" height="15" rx="2.5"/>
        <path d="M9.5 4.5v15"/>
        <rect class="ico-col" x="3" y="4.5" width="6.5" height="15" rx="2.5" fill="currentColor" stroke="none" opacity=".55"/>
      </svg>
    </button>
    <span class="tb-name">Aether</span>
    ${
      u
        ? `<label class="tb-search">
             <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-search"/></svg>
             <input id="top-find" data-appbar-search type="search"
                    placeholder="Search people, or type an email"
                    autocomplete="off" spellcheck="false" />
           </label>`
        : '<span class="tb-spacer"></span>'
    }

    <div class="tb-right">
      <button class="tb-icon" id="top-theme" title="Switch theme" aria-label="Switch theme">
        <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-sun" class="theme-icon"/></svg>
      </button>
      ${
        u
          ? `<span class="av-wrap"><button ${avatarAttrs(
              { userId: u.id, name: u.displayName, email: u.email },
              'sm'
            )} id="top-avatar" title="${esc(u.displayName || u.email)} · ${esc(statusLabel(myStatus().status))}">${esc(
              initials({ userId: u.id, name: u.displayName, email: u.email })
            )}</button>${myPresenceDot()}</span>`
          : ''
      }
    </div>
    ${windowButtonsHTML()}`;

  attachTitleBar(bar, { onSearch: paintTopFind });
  wireTopFind(bar);
  if (u) attachProfileMenu($('top-avatar'));
  $('top-theme')?.addEventListener('click', toggleTheme);
  applyRailState();
  $('tb-collapse').onclick = () => setRailCollapsed(!railCollapsed());
}

// The sidebar toggle. App only: a browser page has no caption to put it in,
// and its rail is part of the page. The choice is remembered per machine,
// because somebody who hides the rail wants it hidden tomorrow too.
const RAIL_KEY = 'aether.rail';

function railCollapsed() {
  try {
    return localStorage.getItem(RAIL_KEY) === 'hidden';
  } catch {
    return false;
  }
}

function setRailCollapsed(yes) {
  try {
    localStorage.setItem(RAIL_KEY, yes ? 'hidden' : 'shown');
  } catch {
    // No storage: the toggle still works for this session.
  }
  applyRailState(yes);
}

function applyRailState(yes = railCollapsed()) {
  document.body.classList.toggle('rail-collapsed', yes);
  const b = $('tb-collapse');
  if (b) {
    b.title = yes ? 'Show sidebar' : 'Hide sidebar';
    b.setAttribute('aria-pressed', yes ? 'true' : 'false');
  }
}

// The web's bar, at the top of the home page: search, then the theme toggle
// and the account side by side. Drawn once there is somebody to draw it for.
function renderWebBar() {
  const bar = document.getElementById('web-bar');
  if (!bar) return;
  const u = session.user;
  if (!u) {
    bar.innerHTML = '';
    return;
  }
  bar.innerHTML = `
    <label class="top-search">
      <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-search"/></svg>
      <input id="top-find" data-appbar-search type="search"
             placeholder="Search people, or type an email"
             autocomplete="off" spellcheck="false" />
    </label>
    <div class="top-right">
      <button class="icon-btn" id="top-theme" title="Switch theme" aria-label="Switch theme">
        <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-sun" class="theme-icon"/></svg>
      </button>
      <span class="av-wrap"><button ${avatarAttrs(
        { userId: u.id, name: u.displayName, email: u.email },
        'sm'
      )} id="top-avatar" title="${esc(u.displayName || u.email)} · ${esc(statusLabel(myStatus().status))}">${esc(
        initials({ userId: u.id, name: u.displayName, email: u.email })
      )}</button>${myPresenceDot()}</span>
    </div>`;
  // attachTitleBar wires only the search field outside the shell; the drag
  // and window buttons it also knows about do not exist here.
  attachTitleBar(bar, { onSearch: paintTopFind });
  wireTopFind(bar);
  attachProfileMenu($('top-avatar'));
  $('top-theme').addEventListener('click', toggleTheme);
}

// ---------------------------------------------------------------------------
// The title-bar search.
//
// This is where you find people -- not the chats you already have (the chat
// column filters those itself) but anybody: the colleague you have never
// messaged, by name, or an outsider by their full address. It is the one
// place a new conversation starts, so it has to be complete.
//
// Two sources, merged. The people you already talk to are in memory and rank
// on every keystroke, so the list is never empty while a request is out. The
// directory answers for everybody else, inside the rules identity enforces:
// names resolve within the organisation's declared domains, and outside them
// only a complete email address finds anyone. Nothing here widens that.
// ---------------------------------------------------------------------------

function topFindPanel() {
  let panel = document.getElementById('top-find-results');
  if (!panel || !panel.isConnected) {
    panel = document.createElement('div');
    panel.id = 'top-find-results';
    panel.className = 'find-results';
    panel.setAttribute('data-no-drag', '');
    panel.hidden = true;
    document.querySelector('.tb-search, .top-search')?.appendChild(panel);
  }
  return panel;
}

const localFindPeople = () =>
  frequentContacts(200, session.user?.id).map((c) => ({
    id: c.userId,
    userId: c.userId,
    name: c.name,
    email: c.email,
  }));

// The directory's answer arrives after the local one and replaces it. Stale
// answers -- a slow reply to a query you have already typed past -- are
// dropped by createSearch, which is the whole reason it exists.
const topFind = createSearch(
  async (q) => {
    const local = rankPeople(localFindPeople(), q, 6);
    let remote = [];
    try {
      remote = ((await directory.search(q, 12)).people ?? []).map((p) => ({
        ...p,
        userId: p.id,
      }));
    } catch {
      // The local half still stands.
    }
    return mergePeople(local, rankPeople(remote, q, 12)).slice(0, 8);
  },
  (q, people) => drawTopFind(q, people, { pending: false })
);

// Messages: three or more characters, searched as an index lookup on the
// server, drawn as their own group with the conversation they are in.
let lastMsgResults = { q: '', rows: [] };
const topFindMessages = createSearch(
  async (q) => (q.length >= 3 ? (await messaging.search(q, 6)).messages ?? [] : []),
  (q, rows) => {
    lastMsgResults = { q, rows };
    const panel = document.getElementById('top-find-results');
    if (panel && !panel.hidden) drawMessageResults(panel, q, rows);
  }
);

function drawMessageResults(panel, q, rows) {
  panel.querySelector('.find-msgs')?.remove();
  if (!rows.length) return;
  const box = document.createElement('div');
  box.className = 'find-msgs';
  box.innerHTML =
    `<div class="find-group">Messages</div>` +
    rows
      .map((m) => {
        // What was read, not what was typed: the server matched the plain
        // text, so the row shows the plain text the match was found in.
        return `<div class="find-row" role="button" tabindex="0" data-find-msg="${esc(m.id)}" data-find-msg-conv="${esc(m.conversationId)}">
          <span ${avatarAttrs({ userId: m.senderId, name: m.senderName }, 'sm')}>${esc(initials({ userId: m.senderId, name: m.senderName }))}</span>
          <span class="find-main"><span>${highlight(plainText(m.body || '').slice(0, 90), q)}</span><small>${esc(m.senderName)} · ${esc(dayLabel(new Date(m.createdAt)))}</small></span>
        </div>`;
      })
      .join('');
  panel.appendChild(box);
  box.querySelectorAll('[data-find-msg]').forEach((el) => {
    el.onclick = () => {
      const panel2 = topFindPanel();
      panel2.hidden = true;
      const input = document.getElementById('top-find');
      if (input) input.value = '';
      openChatFromFind(() => openConversationAt(el.dataset.findMsgConv, el.dataset.findMsg));
    };
  });
}

// Whether the query is you. Your own account is deliberately absent from
// directory results -- matching yourself while looking for a colleague is
// noise -- but the thread with yourself is the most used one in every product
// that has it, so it appears when it is plainly what you typed.
function queryIsSelf(q) {
  const u = session.user;
  if (!u) return false;
  const n = q.toLowerCase();
  if (['me', 'you', 'myself', 'self', 'notes', 'note'].some((w) => w.startsWith(n))) return true;
  return matchScore({ name: u.displayName, email: u.email }, q) !== NO_MATCH;
}

function paintTopFind(q) {
  const panel = topFindPanel();
  if (!q) {
    topFind.cancel();
    panel.hidden = true;
    panel.innerHTML = '';
    return;
  }
  // Instantly, from memory; the directory's answer follows.
  drawTopFind(q, rankPeople(localFindPeople(), q, 6), { pending: true });
  topFind.input(q);
  topFindMessages.input(q);
}

function drawTopFind(q, people, { pending }) {
  const panel = topFindPanel();
  const needle = q.toLowerCase();
  const chats = matchConversations(q, 5);
  const meetings = (state.meetings ?? [])
    .filter((m) => (m.title ?? '').toLowerCase().includes(needle))
    .slice(0, 4);
  const self = queryIsSelf(q);
  const looksLikeEmail = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(q);

  panel.hidden = false;

  if (!people.length && !meetings.length && !self && !chats.length) {
    panel.innerHTML = pending
      ? `<div class="find-empty">Searching…</div>`
      : `<div class="find-empty">${
          looksLikeEmail
            ? `Nobody with the address <b>${esc(q)}</b> has an account yet.`
            : `Nobody by that name. To reach someone outside your organisation, type their full email address.`
        }</div>`;
    return;
  }

  const me = session.user;
  const personRow = (p) => `<div class="find-row" role="button" tabindex="0"
        data-find-person="${esc(p.userId)}" data-find-name="${esc(p.name)}">
      <span class="av-wrap">
        <span ${avatarAttrs(p, 'sm')}>${esc(initials(p))}</span>
        ${p.online ? personPresence(p.userId) : presenceIcon('offline', { online: false })}
      </span>
      <span class="find-main"><span>${highlight(p.name, q)}</span><small>${highlight(p.email, q)}</small></span>
      <span class="find-acts" data-no-open>
        <button class="find-act" data-find-call="${esc(p.userId)}" data-find-name="${esc(p.name)}"
                data-audio="1" title="Audio call" aria-label="Audio call ${esc(p.name)}">
          <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-mic"/></svg>
        </button>
        <button class="find-act" data-find-call="${esc(p.userId)}" data-find-name="${esc(p.name)}"
                data-audio="0" title="Video call" aria-label="Video call ${esc(p.name)}">
          <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-cam"/></svg>
        </button>
      </span>
    </div>`;

  panel.innerHTML =
    (people.length || self
      ? `<div class="find-group">People</div>` +
        (self && me
          ? `<div class="find-row" role="button" tabindex="0" data-find-self>
              <span ${avatarAttrs({ userId: me.id, name: me.displayName, email: me.email }, 'sm')}>${esc(
                initials({ userId: me.id, name: me.displayName, email: me.email })
              )}</span>
              <span class="find-main"><span>${esc(me.displayName || me.email)}</span><small>Message yourself · notes, links and reminders</small></span>
            </div>`
          : '') +
        people.map(personRow).join('')
      : '') +
    (chats.length
      ? `<div class="find-group">Groups and channels</div>` +
        chats
          .map(
            (c) => `<div class="find-row" role="button" tabindex="0" data-find-conv="${esc(c.id)}">
              <span class="avatar sm ${c.kind === 'channel' ? 'chan' : ''}">${c.kind === 'channel' ? '#' : esc(initials({ name: c.display }))}</span>
              <span class="find-main"><span>${highlight(c.display, q)}</span><small>${c.memberCount || 0} members</small></span>
            </div>`
          )
          .join('')
      : '') +
    (meetings.length
      ? `<div class="find-group">Meetings</div>` +
        meetings
          .map(
            (m) => `<div class="find-row" role="button" tabindex="0" data-find-meeting="${esc(m.id)}">
              <span ${avatarAttrs({ name: m.title }, 'sm')}>${esc(initials({ name: m.title }))}</span>
              <span class="find-main"><span>${highlight(m.title, q)}</span><small>${esc(
                dayLabel(m.start)
              )} · ${esc(clockHM(m.start))}</small></span>
            </div>`
          )
          .join('')
      : '') +
    (pending ? `<div class="find-note">Searching the directory…</div>` : '');
  if (lastMsgResults.q === q && lastMsgResults.rows.length) drawMessageResults(panel, q, lastMsgResults.rows);

  const closeFind = () => {
    topFind.cancel();
    topFindMessages.cancel();
    panel.hidden = true;
    panel.innerHTML = '';
    const input = document.getElementById('top-find');
    if (input) input.value = '';
  };
  const activate = (el, fn) => {
    el.onclick = (e) => {
      if (e.target.closest('[data-no-open]')) return;
      fn();
    };
    el.onkeydown = (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        fn();
      }
    };
  };

  panel.querySelectorAll('[data-find-person]').forEach((el) => {
    activate(el, () => {
      closeFind();
      openChatFromFind(() => openChatWith(el.dataset.findPerson));
    });
  });
  panel.querySelectorAll('[data-find-conv]').forEach((el) => {
    activate(el, () => {
      closeFind();
      openChatFromFind(() => openConversationById(el.dataset.findConv));
    });
  });
  panel.querySelectorAll('[data-find-self]').forEach((el) => {
    activate(el, () => {
      closeFind();
      openChatFromFind(openSelfChat);
    });
  });
  panel.querySelectorAll('[data-find-call]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      closeFind();
      window.dispatchEvent(
        new CustomEvent('aether:call', {
          detail: {
            targetId: b.dataset.findCall,
            targetName: b.dataset.findName,
            audioOnly: b.dataset.audio === '1',
          },
        })
      );
    };
  });
  panel.querySelectorAll('[data-find-meeting]').forEach((el) => {
    activate(el, () => {
      closeFind();
      const m = state.meetings.find((x) => x.id === el.dataset.findMeeting);
      if (m) joinMeeting(m.room, m.title);
    });
  });
}

// Land in the chat column with the chosen thread open. The chat surface is a
// home tab, so a search made from another tab first switches there.
async function openChatFromFind(openIt) {
  if ($('home').classList.contains('hidden')) {
    // In a meeting or on another screen: the thread would be painted under
    // the call. Better to say so than to draw it where nobody can see it.
    toast('Leave the meeting to open a chat.', 'bad');
    return;
  }
  state.homeTab = 'chat';
  document
    .querySelectorAll('[data-home]')
    .forEach((x) => x.classList.toggle('active', x.dataset.home === 'chat'));
  renderChatSurface();
  try {
    await openIt();
  } catch (e) {
    toast(e instanceof ApiError ? e.message : 'Could not open that chat.', 'bad');
  }
}

// The dropdown closes when you click away or press Escape, and comes back
// when the field regains focus with something still typed in it. Bound once,
// on the document, because the bar itself is re-rendered on every session
// change and a listener on a replaced element is a leak with a long fuse.
let topFindWired = false;
function wireTopFind(bar) {
  const input = bar.querySelector('#top-find');
  if (input) {
    input.addEventListener('focus', () => {
      const q = input.value.trim();
      if (q) paintTopFind(q);
    });
    input.addEventListener('keydown', (e) => {
      // Down-arrow into the first result, so the whole thing works without a
      // mouse.
      if (e.key !== 'ArrowDown') return;
      const first = topFindPanel().querySelector('[role="button"]');
      if (first) {
        e.preventDefault();
        first.focus();
      }
    });
  }
  if (topFindWired) return;
  topFindWired = true;
  document.addEventListener('click', (e) => {
    const panel = document.getElementById('top-find-results');
    if (!panel || panel.hidden) return;
    if (panel.contains(e.target) || e.target.closest('.tb-search, .top-search')) return;
    panel.hidden = true;
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    const panel = document.getElementById('top-find-results');
    if (panel) panel.hidden = true;
  });
}

function paintHome() {
  const now = Date.now();
  // Overrunning is not over: a meeting with people still in it stays here
  // however far past its scheduled end it runs. And "completed" is a fact the
  // server records when the last person leaves, before or after the end time,
  // which is what moves a meeting out of this list.
  const upcoming = state.meetings
    .filter((m) => isUpcoming(m, now))
    .sort((a, b) => a.start - b.start)
    .slice(0, 6);
  const past = state.meetings
    .filter((m) => !isUpcoming(m, now))
    .sort((a, b) => b.start - a.start)
    .slice(0, 8);

  paintHero();
  paintAgenda();
  paintStats();
  paintPeople();

  $('up-count').textContent = upcoming.length;
  $('up-grid').innerHTML = upcoming.length
    ? upcoming.map(meetingCard).join('')
    : emptyState(
        'i-calendar',
        'Nothing scheduled',
        'Meetings you schedule or are invited to appear here.',
        'Schedule one'
      );

  // Recent is what you were actually in, most recent first, de-duplicated
  // by room so a meeting you rejoined twice is one entry. Past scheduled
  // meetings you never joined follow, so nothing scheduled disappears.
  const seenRooms = new Set();
  const attended = (state.history ?? []).filter((a) => {
    if (seenRooms.has(a.room)) return false;
    seenRooms.add(a.room);
    return true;
  });
  const dur = (s) => (s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`);
  const attendedRows = attended.map(
    (a) => `<div class="hrow" data-rejoin="${esc(a.room)}" title="Rejoin ${esc(a.room)}">
      <span ${avatarAttrs(a.title, 'sm')}>${esc(initials(a.title))}</span>
      <div class="hrow-main">
        <h4>${esc(a.title)}</h4>
        <span>${esc(a.room)}</span>
      </div>
      <span class="tag tag-member">${a.left ? esc(dur(a.seconds)) : 'in progress'}</span>
      <span class="hrow-when">${esc(dayLabel(a.joined))}, ${esc(clockHM(a.joined))}</span>
    </div>`
  );
  const scheduledRows = past
    .filter((m) => !seenRooms.has(m.room))
    .map(
      (m) => `<div class="hrow">
        <span ${avatarAttrs(m.title, 'sm')}>${esc(initials(m.title))}</span>
        <div class="hrow-main">
          <h4>${esc(m.title)}</h4>
          <span>${esc(m.room)}</span>
        </div>
        <span class="tag ${m.status === 'cancelled' ? 'tag-bad' : 'tag-member'}">${esc(m.status)}</span>
        <span class="hrow-when">${esc(dayLabel(m.start))}, ${esc(clockHM(m.start))}</span>
      </div>`
    );
  // Five. Recent means recent; a longer list is a history page, which this
  // is not.
  const rows = [...attendedRows, ...scheduledRows].slice(0, 5);
  $('hist').innerHTML = rows.length
    ? rows.join('')
    : emptyState('i-home', 'No history yet', 'Meetings you have been in will be listed here.');
  // One click rejoins a room you were in.
  $('hist')
    .querySelectorAll('[data-rejoin]')
    .forEach((el) => {
      el.style.cursor = 'pointer';
      el.onclick = () => {
        location.href = '/?room=' + encodeURIComponent(el.dataset.rejoin);
      };
    });

  const scheduleBtn = document.querySelector('[data-empty-action]');
  if (scheduleBtn) scheduleBtn.onclick = scheduleDialog;

  $('up-grid')
    .querySelectorAll('[data-join]')
    .forEach((b) => {
      b.onclick = (e) => {
        e.stopPropagation();
        const m = state.meetings.find((x) => x.id === b.dataset.join);
        if (m) joinMeeting(m.room, m.title);
      };
    });
}

// An empty state that does something.
//
// "Nothing scheduled" in grey text is a dead end: it tells somebody what is
// absent and leaves them to work out what to do about it. Every empty region
// here names the thing that would fill it and offers the action that does.
function emptyState(icon, title, body, action) {
  return `<div class="empty-rich">
    <span class="empty-icon"><svg width="22" height="22" viewBox="0 0 24 24"><use href="#${icon}" /></svg></span>
    <h4>${esc(title)}</h4>
    <p>${esc(body)}</p>
    ${action ? `<button class="btn btn-primary sm" data-empty-action>${esc(action)}</button>` : ''}
  </div>`;
}

function paintHero() {
  const el = $('hero-next');
  if (!el) return;
  const m = nextMeeting();

  if (!m) {
    el.innerHTML = `<div class="next-empty">
      <span class="empty-icon lg"><svg width="26" height="26" viewBox="0 0 24 24"><use href="#i-cam" /></svg></span>
      <h2>Nothing next</h2>
      <p>Your calendar is clear. Start a meeting now, or schedule one for later.</p>
      <div class="next-actions">
        <button class="btn btn-primary" id="next-start">Start a meeting</button>
        <button class="btn btn-quiet" id="next-schedule">Schedule</button>
      </div>
    </div>`;
    $('next-start').onclick = () =>
      joinMeeting('meet-' + Math.random().toString(36).slice(2, 8), 'New meeting');
    $('next-schedule').onclick = scheduleDialog;
    return;
  }

  const invitees = (m.invitees ?? []).slice(0, 5);
  const more = Math.max(0, (m.invitees ?? []).length - invitees.length);

  el.innerHTML = `
    <div class="next-head">
      <span class="tag tag-member">Next</span>
      <span class="next-when" id="next-countdown" data-id="${esc(m.id)}">—</span>
    </div>
    <h2>${esc(m.title)}</h2>
    <p class="next-meta">
      ${esc(dayLabel(m.start))} · ${esc(clockHM(m.start))} – ${esc(clockHM(m.end))}
      · ${esc(humanDuration(Math.round((m.end - m.start) / 60000)))}
    </p>
    <div class="next-people">
      ${invitees
        .map(
          (i) =>
            `<span ${avatarAttrs({ email: i.email }, 'sm')} title="${esc(i.email)}">${esc(
              initials({ email: i.email })
            )}</span>`
        )
        .join('')}
      ${more ? `<span class="avatar sm more">+${more}</span>` : ''}
      <span class="next-people-label">${
        (m.invitees ?? []).length
          ? `${(m.invitees ?? []).length} invited`
          : 'No one invited yet'
      }</span>
    </div>
    <div class="next-actions">
      <button class="btn btn-primary" id="next-join">Join now</button>
      <button class="btn btn-quiet" id="next-copy">Copy link</button>
    </div>`;

  $('next-join').onclick = () => joinMeeting(m.room, m.title);
  $('next-copy').onclick = async () => {
    const link = `${location.origin}/?room=${encodeURIComponent(m.room)}`;
    try {
      await navigator.clipboard.writeText(link);
      toast('Invite link copied.');
    } catch {
      window.prompt('Copy this invite link', link);
    }
  };
  paintCountdown();
}

// Today, as a timeline rather than a list.
//
// A marker for the current time is the whole point: it turns "here are four
// meetings" into "you are between the second and the third", which is the
// thing somebody glancing at the page is trying to work out.
function paintAgenda() {
  const el = $('agenda');
  if (!el) return;
  const today = todaysMeetings();
  $('today-count').textContent = today.length;

  if (!today.length) {
    el.innerHTML = `<p class="agenda-empty">No meetings scheduled today.</p>`;
    return;
  }

  const now = Date.now();
  let markerPlaced = false;
  const rows = [];
  for (const m of today) {
    if (!markerPlaced && m.start.getTime() > now) {
      rows.push(`<div class="agenda-now"><span></span><em>now</em></div>`);
      markerPlaced = true;
    }
    const live = now >= m.start.getTime() && now < m.end.getTime();
    rows.push(`<button class="agenda-row${live ? ' live' : ''}" data-agenda="${esc(m.id)}">
      <span class="agenda-time">${esc(clockHM(m.start))}</span>
      <span class="agenda-bar"></span>
      <span class="agenda-main">
        <b>${esc(m.title)}</b>
        <small>${esc(humanDuration(Math.round((m.end - m.start) / 60000)))}${
          live ? ' · happening now' : ''
        }</small>
      </span>
    </button>`);
  }
  if (!markerPlaced) rows.push(`<div class="agenda-now past"><span></span><em>now</em></div>`);
  el.innerHTML = rows.join('');

  el.querySelectorAll('[data-agenda]').forEach((b) => {
    b.onclick = () => {
      const m = state.meetings.find((x) => x.id === b.dataset.agenda);
      if (m) joinMeeting(m.room, m.title);
    };
  });
}

function paintStats() {
  const el = $('stats');
  if (!el) return;
  const w = weekStats();
  const tiles = [
    { k: 'This week', v: String(w.count), s: w.count === 1 ? 'meeting' : 'meetings' },
    { k: 'Scheduled time', v: humanDuration(w.minutes), s: 'across the week' },
    { k: 'People', v: String(w.people), s: 'you are meeting' },
    {
      k: 'Today',
      v: String(todaysMeetings().length),
      s: todaysMeetings().length === 1 ? 'meeting' : 'meetings',
    },
  ];
  el.innerHTML = tiles
    .map(
      (t) => `<div class="stat">
        <span class="stat-k">${esc(t.k)}</span>
        <strong class="stat-v">${esc(t.v)}</strong>
        <span class="stat-s">${esc(t.s)}</span>
      </div>`
    )
    .join('');
}

function paintPeople() {
  const el = $('people-strip');
  if (!el) return;
  const people = recentPeople();
  if (!people.length) {
    el.innerHTML = emptyState(
      'i-people',
      'No conversations yet',
      'People you chat with appear here, so their thread is always one click away.'
    );
    return;
  }
  el.innerHTML = people
    .map(
      (p) => `<button class="person-chip" data-person="${esc(p.userId)}"
          title="${esc(p.email || p.name)}">
        <span ${avatarAttrs(p, 'sm')}>${esc(initials(p))}</span>
        <span class="person-chip-name">${esc(p.name)}</span>
        ${p.unread ? `<span class="person-unread">${p.unread}</span>` : ''}
      </button>`
    )
    .join('');

  el.querySelectorAll('[data-person]').forEach((b) => {
    b.onclick = () => {
      // Open the conversation rather than starting a call. These are the
      // people you were last talking to, so the thing you almost always want
      // next is the thread, and a click that dropped somebody into an empty
      // meeting room would be a click nobody meant.
      state.homeTab = 'chat';
      document
        .querySelectorAll('[data-home]')
        .forEach((x) => x.classList.toggle('active', x.dataset.home === 'chat'));
      renderChatSurface();
    };
  });
}

function meetingCard(m) {
  const people = m.invitees ?? [];
  const shown = people.slice(0, 3);
  return `<div class="mcard">
    <div class="mcard-top">
      <span class="stack">${shown
        .map((p) => avatarHTML({ email: p.email }))
        .join('')}</span>
      ${people.length > 3 ? `<span class="stack-more">+${people.length - 3}</span>` : ''}
      <span class="mcard-when">${
        m.live ? 'Happening now' : esc(dayLabel(m.start))
      }</span>
    </div>
    <h3>${esc(m.title)}</h3>
    <p>${esc(m.description || CATEGORIES.find((c) => c.id === m.category)?.label || 'Meeting')}</p>
    <div class="mcard-foot">
      <span class="time-chip">
        <svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-calendar"/></svg>
        ${esc(clockHM(m.start))}
      </span>
      <button class="go" data-join="${esc(m.id)}" title="Join">
        <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-arrow-right"/></svg>
      </button>
    </div>
  </div>`;
}

// ---------------------------------------------------------------------------
// Calendar
// ---------------------------------------------------------------------------

function renderCalendar() {
  const main = document.querySelector('.home-main');
  main.innerHTML = `
    <header class="home-head">
      <div>
        <p class="home-hello">Your schedule</p>
        <h1>Availability</h1>
      </div>
      <div class="spacer"></div>
      <button class="btn btn-primary" id="cal-new">New meeting</button>
    </header>
    <div class="cal">
      <div class="cal-col">
        <h3 id="cal-month-label"></h3>
        <div class="range">
          <div class="range-chip"><span>Selected</span><b id="cal-sel"></b></div>
          <div class="range-chip"><span>Meetings</span><b id="cal-sel-count"></b></div>
        </div>
        <div class="month">
          <div class="month-head">
            <b id="month-name"></b>
            <div class="month-nav">
              <button id="m-prev"><svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-back"/></svg></button>
              <button id="m-next"><svg width="15" height="15" viewBox="0 0 24 24" style="transform:rotate(180deg)"><use href="#i-back"/></svg></button>
            </div>
          </div>
          <div class="dow"><span>Mon</span><span>Tue</span><span>Wed</span><span>Thu</span><span>Fri</span><span>Sat</span><span>Sun</span></div>
          <div class="days" id="days"></div>
        </div>
        <h3 style="margin-top:22px">Categories</h3>
        <div class="cats" id="cats"></div>
      </div>

      <div class="cal-col">
        <div class="day">
          <div class="day-head">
            <button id="d-prev"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-back"/></svg></button>
            <b id="day-label"></b>
            <button id="d-next"><svg width="16" height="16" viewBox="0 0 24 24" style="transform:rotate(180deg)"><use href="#i-back"/></svg></button>
          </div>
          <div class="day-body" id="day-body"></div>
        </div>
      </div>

      <div class="cal-col inv-col">
        <h3>Invitations</h3>
        <div id="inv-list"></div>
      </div>
    </div>`;

  $('cal-new').onclick = scheduleDialog;
  $('m-prev').onclick = () => {
    state.calMonth = new Date(state.calMonth.getFullYear(), state.calMonth.getMonth() - 1, 1);
    paintMonth();
  };
  $('m-next').onclick = () => {
    state.calMonth = new Date(state.calMonth.getFullYear(), state.calMonth.getMonth() + 1, 1);
    paintMonth();
  };
  $('d-prev').onclick = () => {
    state.calDate = new Date(state.calDate.getTime() - 86400000);
    state.calMonth = new Date(state.calDate.getFullYear(), state.calDate.getMonth(), 1);
    paintMonth();
    paintDay();
  };
  $('d-next').onclick = () => {
    state.calDate = new Date(state.calDate.getTime() + 86400000);
    state.calMonth = new Date(state.calDate.getFullYear(), state.calDate.getMonth(), 1);
    paintMonth();
    paintDay();
  };

  paintCats();
  paintMonth();
  paintDay();
  paintInvites();
}

function paintCats() {
  $('cats').innerHTML = CATEGORIES.map(
    (c) => `<button class="cat ${state.catsOff.has(c.id) ? 'off' : ''}" data-cat="${c.id}">
      <i style="background:${c.color}"></i>${esc(c.label)}
    </button>`
  ).join('');
  $('cats')
    .querySelectorAll('[data-cat]')
    .forEach((b) => {
      b.onclick = () => {
        const id = b.dataset.cat;
        if (state.catsOff.has(id)) state.catsOff.delete(id);
        else state.catsOff.add(id);
        paintCats();
        paintMonth();
        paintDay();
      };
    });
}

function visibleMeetings() {
  return state.meetings.filter(
    (m) => m.status === 'scheduled' && !state.catsOff.has(m.category)
  );
}

function sameDay(a, b) {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}

function paintMonth() {
  const m = state.calMonth;
  $('month-name').textContent = m.toLocaleDateString([], { month: 'long', year: 'numeric' });
  $('cal-month-label').textContent = m.toLocaleDateString([], { month: 'long', year: 'numeric' });

  // Monday-first grid.
  const first = new Date(m.getFullYear(), m.getMonth(), 1);
  const offset = (first.getDay() + 6) % 7;
  const start = new Date(first.getTime() - offset * 86400000);
  const today = new Date();
  const list = visibleMeetings();

  let html = '';
  for (let i = 0; i < 42; i++) {
    const d = new Date(start.getTime() + i * 86400000);
    const other = d.getMonth() !== m.getMonth();
    const has = list.some((x) => sameDay(x.start, d));
    html += `<button class="${other ? 'other ' : ''}${sameDay(d, today) ? 'today ' : ''}${
      sameDay(d, state.calDate) ? 'sel ' : ''
    }${has ? 'has' : ''}" data-d="${d.toISOString()}" style="position:relative">${d.getDate()}</button>`;
  }
  $('days').innerHTML = html;
  $('days')
    .querySelectorAll('[data-d]')
    .forEach((b) => {
      b.onclick = () => {
        state.calDate = new Date(b.dataset.d);
        paintMonth();
        paintDay();
      };
    });

  $('cal-sel').textContent = state.calDate.toLocaleDateString([], {
    day: '2-digit',
    month: '2-digit',
    year: '2-digit',
  });
  $('cal-sel-count').textContent = String(
    list.filter((x) => sameDay(x.start, state.calDate)).length
  );
}

function paintDay() {
  $('day-label').textContent = state.calDate.toLocaleDateString([], {
    day: 'numeric',
    month: 'long',
    year: 'numeric',
  });

  const list = visibleMeetings()
    .filter((m) => sameDay(m.start, state.calDate))
    .sort((a, b) => a.start - b.start);

  if (!list.length) {
    $('day-body').innerHTML = `<div class="day-empty">Nothing scheduled for this day.</div>`;
    return;
  }

  // Show the hours the day actually uses, with an hour of air either side.
  const firstH = Math.max(0, Math.min(...list.map((m) => m.start.getHours())) - 1);
  const lastH = Math.min(23, Math.max(...list.map((m) => m.end.getHours())) + 1);

  let html = '';
  for (let h = firstH; h <= lastH; h++) {
    const inHour = list.filter((m) => m.start.getHours() === h);
    html += `<div class="hour">
      <div class="hour-label">${h % 12 === 0 ? 12 : h % 12}:00 ${h < 12 ? 'am' : 'pm'}</div>
      <div class="hour-slot">
        ${inHour
          .map(
            (m) => `<div class="event ${CAT_CLASS[m.category] ?? 'c-sage'}" data-open="${esc(m.id)}">
              <div class="event-top">
                <span>${esc(clockHM(m.start))}</span>
                <span>${m.durationMinutes ?? Math.round((m.end - m.start) / 60000)} min</span>
              </div>
              <div class="event-title">${esc(m.title)}</div>
            </div>`
          )
          .join('')}
      </div>
    </div>`;
  }
  $('day-body').innerHTML = html;
  $('day-body')
    .querySelectorAll('[data-open]')
    .forEach((el) => {
      el.onclick = () => {
        const m = state.meetings.find((x) => x.id === el.dataset.open);
        if (m) joinMeeting(m.room, m.title);
      };
    });
}

function paintInvites() {
  const me = session.user?.email?.toLowerCase();
  const pending = state.meetings
    .filter(
      (m) =>
        m.status === 'scheduled' &&
        m.end.getTime() >= Date.now() &&
        (m.invitees ?? []).some((i) => i.email === me && i.response === 'pending')
    )
    .sort((a, b) => a.start - b.start);

  const cancelled = state.meetings
    .filter((m) => m.status === 'cancelled')
    .sort((a, b) => b.start - a.start)
    .slice(0, 2);

  const card = (m, dead) => `<div class="inv">
    <div class="inv-top">
      <h4>${esc(m.title)}</h4>
      <span class="inv-dur">${m.durationMinutes ?? Math.round((m.end - m.start) / 60000)} m</span>
    </div>
    <div class="inv-meta">
      <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-gauge"/></svg>
      ${esc(clockHM(m.start))} – ${esc(clockHM(m.end))}
    </div>
    <div class="inv-meta">
      <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-calendar"/></svg>
      ${esc(m.start.toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' }))}
    </div>
    ${
      dead
        ? `<div class="inv-dead">Cancelled</div>`
        : `<div class="inv-actions">
             <button class="btn" data-decline="${esc(m.id)}">Decline</button>
             <button class="btn btn-primary" data-accept="${esc(m.id)}">Confirm</button>
           </div>`
    }
  </div>`;

  const html =
    pending.map((m) => card(m, false)).join('') + cancelled.map((m) => card(m, true)).join('');
  $('inv-list').innerHTML = html || `<div class="empty">No invitations waiting.</div>`;

  const respond = async (id, response) => {
    try {
      await scheduling.respond(id, response);
      toast(response === 'accepted' ? 'Confirmed.' : 'Declined.');
      await loadMeetings();
      paintInvites();
      paintMonth();
      paintDay();
    } catch (e) {
      toast(e instanceof ApiError ? e.message : 'Could not respond', 'bad');
    }
  };
  $('inv-list')
    .querySelectorAll('[data-accept]')
    .forEach((b) => (b.onclick = () => respond(b.dataset.accept, 'accepted')));
  $('inv-list')
    .querySelectorAll('[data-decline]')
    .forEach((b) => (b.onclick = () => respond(b.dataset.decline, 'declined')));
}

// ---------------------------------------------------------------------------
// Scheduling dialog
// ---------------------------------------------------------------------------

function localInput(d) {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(
    d.getHours()
  )}:${pad2(d.getMinutes())}`;
}

// Suggestions under the schedule dialog's invite field.
//
// The field stays a list of addresses, because you invite people who have no
// account here yet and an autocomplete that refuses them would be worse than
// no autocomplete. Suggestions complete the one you are typing: the text
// before the last comma is left alone, the fragment after it is what is
// searched, and picking a person replaces just that fragment.
function wireInviteSuggest() {
  const input = $('s-inv');
  const box = $('s-sug');
  if (!input || !box) return;

  // What is being typed right now, as opposed to what is already entered.
  const head = () => {
    const at = input.value.lastIndexOf(',');
    return at < 0 ? '' : input.value.slice(0, at + 1);
  };
  const fragment = () => {
    const at = input.value.lastIndexOf(',');
    return (at < 0 ? input.value : input.value.slice(at + 1)).trim();
  };

  const already = () =>
    new Set(
      input.value
        .split(/[,\s]+/)
        .map((x) => x.trim().toLowerCase())
        .filter(Boolean)
    );

  const paint = (people, q) => {
    const taken = already();
    const rows = people.filter((p) => p.email && !taken.has(p.email.toLowerCase()));
    if (!q || !rows.length) {
      box.hidden = true;
      box.innerHTML = '';
      return;
    }
    box.hidden = false;
    box.innerHTML = rows
      .slice(0, 6)
      .map(
        (p) => `<button type="button" class="suggest-row" data-email="${esc(p.email)}">
          <span ${avatarAttrs(p, 'sm')}>${esc(initials(p))}</span>
          <span class="suggest-main">${highlight(p.name, q)}<small>${highlight(p.email, q)}</small></span>
        </button>`
      )
      .join('');
    box.querySelectorAll('[data-email]').forEach((b) => {
      b.onclick = () => {
        input.value = head() + (head() ? ' ' : '') + b.dataset.email + ', ';
        box.hidden = true;
        input.focus();
      };
    });
  };

  const searcher = createSearch(
    async (q) => {
      if (!q) return [];
      const local = rankPeople(
        frequentContacts(200, session.user?.id).map((c) => ({
          userId: c.userId,
          name: c.name,
          email: c.email,
        })),
        q,
        6
      );
      let remote = [];
      try {
        remote = ((await directory.search(q)).people ?? []).map((p) => ({
          userId: p.id,
          name: p.name,
          email: p.email,
        }));
      } catch {
        // Local suggestions still stand; the field accepts typed addresses
        // regardless, so a failed lookup blocks nothing.
      }
      return mergePeople(local, rankPeople(remote, q, 8));
    },
    (q, people) => paint(people, q)
  );

  input.oninput = () => searcher.input(fragment());
  input.onblur = () => setTimeout(() => (box.hidden = true), 150);
  input.onfocus = () => searcher.input(fragment(), { immediate: true });
}

function scheduleDialog() {
  const start = new Date(Math.ceil(Date.now() / 1800000) * 1800000);
  const end = new Date(start.getTime() + 30 * 60000);

  const host = $('dialog');
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true">
      <h2>Schedule a meeting</h2>
      <label class="field"><span>Title</span>
        <input id="s-title" class="input" placeholder="Weekly design review" /></label>
      <label class="field"><span>Description</span>
        <input id="s-desc" class="input" placeholder="Optional" /></label>
      <div class="sched-times">
        <label class="field"><span>Starts</span>
          <input id="s-start" class="input" type="datetime-local" value="${localInput(start)}" /></label>
        <label class="field"><span>Ends</span>
          <input id="s-end" class="input" type="datetime-local" value="${localInput(end)}" /></label>
      </div>
      <label class="field"><span>Category</span>
        <select id="s-cat" class="input">
          ${CATEGORIES.map((c) => `<option value="${c.id}">${c.label}</option>`).join('')}
        </select></label>
      <label class="field"><span>Invite people</span>
        <input id="s-inv" class="input" placeholder="Type a name, or an email address"
               autocomplete="off" spellcheck="false" />
        <div class="suggest" id="s-sug" hidden></div></label>
      <label class="field"><span>Co-organizers</span>
        <input id="s-coorg" class="input" placeholder="Choose co-organizers from the people invited"
               autocomplete="off" spellcheck="false" /></label>
      <p class="hint">Co-organizers run the meeting with you: they can admit people, change meeting options and end the meeting. Up to 10, from the people invited.</p>
      <p class="hint" id="s-warn"></p>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" type="button" data-confirm>Schedule</button>
      </div>
    </div>`;
  host.classList.remove('hidden');

  const close = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
  };
  host.querySelector('[data-cancel]').onclick = close;
  host.onclick = (e) => {
    if (e.target === host) close();
  };
  wireInviteSuggest();

  // Warn about a clash before it is created rather than after. The server
  // does not refuse an overlap -- people genuinely double-book on purpose --
  // but nobody should do it without being told.
  const checkClash = async () => {
    const s = new Date($('s-start').value);
    const e = new Date($('s-end').value);
    if (isNaN(s) || isNaN(e) || e <= s) return;
    try {
      const res = await scheduling.conflicts(s.toISOString(), e.toISOString());
      const n = (res.conflicts ?? []).length;
      $('s-warn').textContent = n
        ? `Heads up: this overlaps ${n} meeting${n > 1 ? 's' : ''} already in your calendar.`
        : '';
    } catch {
      $('s-warn').textContent = '';
    }
  };
  $('s-start').onchange = checkClash;
  $('s-end').onchange = checkClash;
  checkClash();

  const confirmBtn = host.querySelector('[data-confirm]');
  confirmBtn.onclick = async () => {
    const title = $('s-title').value.trim();
    if (!title) {
      toast('A title is required.', 'bad');
      $('s-title').focus();
      return;
    }
    // An empty or unparsable time falls back to the dialog's own defaults
    // rather than refusing: the person pressed Schedule, and a meeting at
    // the suggested time is what they were shown.
    let s = new Date($('s-start').value);
    let e = new Date($('s-end').value);
    if (isNaN(s)) s = start;
    if (isNaN(e) || e <= s) e = new Date(s.getTime() + 30 * 60000);
    // Guard against a double press while the request is in flight.
    if (confirmBtn.disabled) return;
    confirmBtn.disabled = true;
    confirmBtn.textContent = 'Scheduling…';
    try {
      await scheduling.create({
        title,
        description: $('s-desc').value.trim(),
        category: $('s-cat').value,
        startsAt: s.toISOString(),
        endsAt: e.toISOString(),
        invitees: $('s-inv')
          .value.split(/[,\s]+/)
          .map((x) => x.trim())
          .filter(Boolean),
        coOrganizers: $('s-coorg')
          .value.split(/[,\s]+/)
          .map((x) => x.trim())
          .filter(Boolean),
      });
      toast(`Scheduled: ${title} — ${dayLabel(s)}, ${clockHM(s)}`);
      close();
      await loadMeetings();
      // Repaint both surfaces unconditionally: whichever one the person
      // looks at next must already show it.
      renderHome();
      if (typeof renderCalendar === 'function') renderCalendar();
    } catch (err) {
      // The server's own words, not a generic shrug: "too far ahead" or "end
      // before start" is actionable; "could not schedule" is not.
      const msg = err instanceof ApiError ? err.message : err?.message || 'Could not schedule';
      toast('Could not schedule: ' + msg, 'bad');
      console.error('schedule:', err);
      confirmBtn.disabled = false;
      confirmBtn.textContent = 'Schedule';
    }
  };

  setTimeout(() => $('s-title').focus(), 50);
}

// Inviting somebody, as the several different things that actually means.
//
// A single "copy link" button assumes the next step is a paste into something
// the person already has open. Often it is not: the link has to reach somebody
// on WhatsApp, or go into a calendar invite, or be read out. Each of those is
// a different destination and none of them is served by putting the other
// three behind a menu.
//
// Everything here is share-by-link. The link is not a key -- whoever follows
// it still has to sign in, and still meets the waiting room -- which is what
// makes handing it around safe enough to make easy.
function inviteDialog() {
  const room = state.room ?? '';
  const link = `${location.origin}/?room=${encodeURIComponent(room)}`;
  const title = state.title || room;
  const body = `Join me for ${title}\n${link}`;

  const host = $('dialog');
  host.innerHTML = `
    <div class="dialog-card invite-card" role="dialog" aria-modal="true"
         aria-labelledby="inv-h">
      <div class="invite-top">
        <h2 id="inv-h">Invite people</h2>
        <button class="icon-btn" data-cancel title="Close" aria-label="Close">
          <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-close" /></svg>
        </button>
      </div>

      <label class="field"><span>Meeting link</span>
        <div class="invite-link">
          <input id="inv-link" class="input" readonly value="${esc(link)}" />
          <button class="btn btn-primary" data-copy>Copy</button>
        </div>
      </label>

      <div class="invite-ways">
        <a class="invite-way" data-way="email" href="#" rel="noopener">
          <span class="invite-icon"><svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-mail" /></svg></span>
          <span><b>Email</b><small>Open your mail app</small></span>
        </a>
        <a class="invite-way" data-way="whatsapp" href="#" target="_blank" rel="noopener noreferrer">
          <span class="invite-icon"><svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-chat" /></svg></span>
          <span><b>WhatsApp</b><small>Send in a chat</small></span>
        </a>
        <button class="invite-way" data-way="share">
          <span class="invite-icon"><svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-arrow-right" /></svg></span>
          <span><b>Share…</b><small>Anywhere else on this device</small></span>
        </button>
      </div>

      <p class="hint">Anyone with the link still has to sign in, and may still
        have to be let in — the link is not a key.</p>
    </div>`;

  host.classList.remove('hidden');
  const close = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
    document.removeEventListener('keydown', onKey);
  };
  const onKey = (e) => {
    if (e.key === 'Escape') close();
  };
  document.addEventListener('keydown', onKey);

  host.querySelector('[data-cancel]').onclick = close;
  host.onclick = (e) => {
    if (e.target === host) close();
  };

  host.querySelector('[data-copy]').onclick = async () => {
    try {
      await navigator.clipboard.writeText(link);
      toast('Invite link copied.');
    } catch {
      // Clipboard access can be refused; selecting the text is something the
      // user can still act on, which beats a button that silently fails.
      $('inv-link').select();
      toast('Press Ctrl+C to copy.', 'bad');
    }
  };

  // mailto and the WhatsApp scheme are set as real hrefs rather than opened
  // from script, so the browser treats them as an ordinary navigation the
  // user initiated. A window.open here is what popup blockers eat.
  host.querySelector('[data-way="email"]').href =
    `mailto:?subject=${encodeURIComponent(title)}&body=${encodeURIComponent(body)}`;
  host.querySelector('[data-way="whatsapp"]').href =
    `https://wa.me/?text=${encodeURIComponent(body)}`;

  const shareBtn = host.querySelector('[data-way="share"]');
  if (navigator.share) {
    shareBtn.onclick = async () => {
      try {
        await navigator.share({ title, text: `Join me for ${title}`, url: link });
      } catch {
        // Dismissing the sheet is not a failure.
      }
    };
  } else {
    // Hidden rather than shown-and-broken: an option that does nothing is
    // worse than one that was never offered.
    shareBtn.remove();
  }

  setTimeout(() => host.querySelector('[data-copy]')?.focus(), 50);
}

function joinDialog() {
  const host = $('dialog');
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true">
      <h2>Join a meeting</h2>
      <label class="field"><span>Room name or link</span>
        <input id="j-room" class="input" placeholder="design-review" /></label>
      <p class="hint">Anyone with the link still has to sign in — the link is not a key.</p>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm>Join</button>
      </div>
    </div>`;
  host.classList.remove('hidden');
  const close = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
  };
  host.querySelector('[data-cancel]').onclick = close;
  host.onclick = (e) => {
    if (e.target === host) close();
  };
  const go = () => {
    let v = $('j-room').value.trim();
    if (!v) return;
    // Accept a pasted invite link as readily as a bare room name.
    try {
      const u = new URL(v);
      v = u.searchParams.get('room') || v;
    } catch {
      // Not a URL. It is a room name.
    }
    close();
    joinMeeting(v, v);
  };
  host.querySelector('[data-confirm]').onclick = go;
  $('j-room').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') go();
  });
  setTimeout(() => $('j-room').focus(), 50);
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function enterHome() {
  const u = session.user;
  // Set with its tone alongside the other fixed avatars, above. Keyed on the
  // account, so your own face is the same one everybody else sees for you.
  const who = { userId: u.id, name: u.displayName, email: u.email };
  for (const id of ['pj-avatar']) {
    const el = $(id);
    if (!el) continue;
    el.textContent = initials(who);
    el.dataset.tone = toneOf(who);
  }
  initConsole();

  let lastBadge = -1;
  connectChatSocket((n) => {
    // The taskbar and tray badge in the desktop shell; drawn by the shell,
    // so it shows while the window is hidden.
    if (typeof window.aetherBadge === 'function' && lastBadge !== n) {
      lastBadge = n;
      window.aetherBadge(n);
    }
    const b = $('chat-badge');
    if (!b) return;
    b.textContent = n > 99 ? '99+' : String(n);
    b.classList.toggle('hidden', n === 0);
  });
  show('home');
  renderTitleBar();
  // Both awaited before the first render, because the home page draws from
  // both: meetings for the hero and the agenda, conversations for People.
  // Rendering first and filling in afterwards makes the page visibly reflow
  // under the reader, which is worse than a moment of nothing.
  await Promise.all([loadMeetings(), refreshChat()]);
  // Only if the person is still on it: a rail button pressed while this
  // loaded has already drawn their choice, and must not be painted over.
  if (!state.homeTab || state.homeTab === 'meet') renderHome();

  // A link of the form /?room=weekly-sync joins straight through once the
  // session is established.
  const params = new URLSearchParams(location.search);
  const invited = params.get('room');
  if (invited && (params.has('bt') || params.has('bn'))) {
    state.ticket = params.get('bt') || '';
    state.autoJoin = true;
    try {
      const carry = JSON.parse(sessionStorage.getItem('aether.carry') || 'null');
      if (carry) {
        state.mic = !!carry.mic;
        state.cam = !!carry.cam;
      }
      sessionStorage.removeItem('aether.carry');
    } catch {}
    history.replaceState(null, '', `${location.pathname}?room=${encodeURIComponent(invited)}`);
    joinMeeting(invited, params.get('bn') || invited);
  } else if (invited) joinMeeting(invited, invited);
  // /?chat=<id>&msg=<id>: a link to a message. Opens the thread there.
  if (params.get('chat')) {
    state.homeTab = 'chat';
    document.querySelectorAll('[data-home]').forEach((x) => x.classList.toggle('active', x.dataset.home === 'chat'));
    renderChatSurface();
    openConversationAt(params.get('chat'), params.get('msg') || '').catch((e) => toast(e.message || 'Could not open that chat.', 'bad'));
    history.replaceState(null, '', location.pathname);
  }
  // Ctrl+K: the search, from anywhere. A habit people bring from every other
  // tool of this kind.
  addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
      const f = document.getElementById('top-find');
      if (f) {
        e.preventDefault();
        f.focus();
        f.select();
      }
    }
  });
  try {
    if (localStorage.getItem('aether.compact') === '1') document.body.classList.add('compact-chat');
  } catch {}
}

// Buttons inside .home-main are rebuilt on every render, so their handlers
// are attached per render rather than once at boot.
function wireHomeActions() {
  $('home-date').textContent = new Date().toLocaleDateString([], {
    weekday: 'long',
    day: 'numeric',
    month: 'long',
  });
  $('q-new').onclick = () => {
    const room = 'meet-' + Math.random().toString(36).slice(2, 8);
    joinMeeting(room, 'New meeting');
  };
  $('q-join').onclick = joinDialog;
  $('q-schedule').onclick = scheduleDialog;
}

function wireHome() {
  // The chat surface asks for a meeting rather than importing the join path,
  // which keeps the two features from depending on each other.
  addEventListener('aether:join', (e) => joinMeeting(e.detail.room, e.detail.title));

  renderTitleBar();

  document.querySelectorAll('[data-home]').forEach((b) => {
    b.onclick = () => {
      state.homeTab = b.dataset.home;
      document
        .querySelectorAll('[data-home]')
        .forEach((x) => x.classList.toggle('active', x === b));
      if (state.homeTab === 'calendar') renderCalendar();
      else if (state.homeTab === 'chat') renderChatSurface();
      else if (state.homeTab === 'teams') renderTeamsSurface();
      else if (state.homeTab === 'activity') renderActivitySurface();
      else renderHome();
    };
  });
}

function wireAuth() {
  const form = $('login-form');
  const err = $('li-error');

  form.onsubmit = async (e) => {
    e.preventDefault();
    const btn = $('li-submit');
    btn.disabled = true;
    err.classList.add('hidden');
    try {
      await login($('li-email').value.trim(), $('li-pass').value);
      $('li-pass').value = '';
      await enterHome();
    } catch (e2) {
      // Only an actual rejection from the sign-in endpoint may be reported
      // as bad credentials. Anything else -- a rendering fault after a
      // successful sign-in, say -- would otherwise be shown as "wrong
      // password", which sends the user off to reset a password that was
      // never the problem.
      if (e2 instanceof ApiError && e2.status === 429) {
        err.textContent = 'Too many attempts. Wait a moment and try again.';
      } else if (e2 instanceof ApiError && e2.status === 401) {
        // The server refuses to say whether the account exists, and neither
        // does this. Repeating its single message is the correct behaviour.
        err.textContent = 'Email or password is incorrect.';
      } else {
        console.error('sign-in succeeded but the app failed to start:', e2);
        err.textContent = 'Signed in, but something went wrong loading the app.';
      }
      err.classList.remove('hidden');
    } finally {
      btn.disabled = false;
    }
  };

  $('li-forgot').onclick = () =>
    toast('Password reset is not wired yet — ask an administrator to set a new one.');

  // Which forms exist at all is the server's call, so ask before offering.
  serverConfig().then((cfg) => {
    $('auth-swap').classList.toggle('hidden', !cfg.allowSignup);
    if (cfg.minPasswordLength) {
      $('su-hint').textContent =
        `At least ${cfg.minPasswordLength} characters. Length matters more than symbols.`;
    }
  });

  let creating = false;
  $('swap-btn').onclick = () => {
    creating = !creating;
    $('login-form').classList.toggle('hidden', creating);
    $('signup-form').classList.toggle('hidden', !creating);
    $('li-note').classList.toggle('hidden', creating);
    $('swap-text').textContent = creating ? 'Already have an account?' : 'New here?';
    $('swap-btn').textContent = creating ? 'Sign in' : 'Create an account';
    document.querySelector('.auth-tagline').textContent = creating
      ? 'Create your account'
      : 'Access your account';
    (creating ? $('su-name') : $('li-email')).focus();
  };

  const suErr = $('su-error');
  $('signup-form').onsubmit = async (e) => {
    e.preventDefault();
    const btn = $('su-submit');
    btn.disabled = true;
    suErr.classList.add('hidden');
    try {
      const u = await signup(
        $('su-email').value.trim(),
        $('su-name').value.trim(),
        $('su-pass').value
      );
      $('su-pass').value = '';
      if (!u) {
        // The address was already registered. The server will not confirm
        // that, and neither will this: the message points at sign-in without
        // asserting the account exists.
        suErr.textContent =
          'If that address is available the account was created. Try signing in.';
        suErr.classList.remove('hidden');
        return;
      }
      await enterHome();
    } catch (e2) {
      if (e2 instanceof ApiError && e2.status === 429) {
        suErr.textContent = 'Too many attempts from here. Wait a few minutes.';
      } else if (e2 instanceof ApiError) {
        suErr.textContent = e2.message;
      } else {
        console.error('account created but the app failed to start:', e2);
        suErr.textContent = 'Account created, but something went wrong loading the app.';
      }
      suErr.classList.remove('hidden');
    } finally {
      btn.disabled = false;
    }
  };
}

initTheme();
// The title bar is window chrome, not part of any screen, so it is drawn
// before anything else and lives for as long as the window does.
//
// Rendering it only after sign-in was a real bug and an obvious one in
// hindsight: the shell had already removed the system caption, so the sign-in
// screen had no buttons, no drag handle and no way to move or close the
// window at all.
renderTitleBar();
wireAuth();
wireHome();
wireMeeting();
wirePreJoin();
wireLobby();
syncControls();

// A reload should not force a sign-in. The session cookie is still valid, so
// ask the server who we are before showing a login form nobody needs.
resume().then((u) => {
  if (u) enterHome();
  else show('auth');
});

// ---------------------------------------------------------------------------
// Breakout rooms (engine room/breakout.go, signal/breakout.go; web breakout.js)
// ---------------------------------------------------------------------------

// Going to another room is leaving this one and joining that one, as a page
// load does -- the honest teardown of the peer connections (leaveMeeting).
// The ticket the server minted goes with it; the person's microphone and
// camera choices are carried over so the move does not change them.
function goRoom(room, title, ticket) {
  try {
    sessionStorage.setItem('aether.carry', JSON.stringify({ mic: state.mic, cam: state.cam }));
  } catch {}
  const q = new URLSearchParams({ room, bn: title || room });
  if (ticket) q.set('bt', ticket);
  location.href = `${location.origin}/?${q}`;
}

// The main meeting's title, kept across the move so coming back shows it.
function parentTitle() {
  try {
    return sessionStorage.getItem('aether.bo.parentTitle') || '';
  } catch {
    return '';
  }
}

// "We'll move you ... in N seconds." on the full cover, with a button to go now.
function moveCountdown(head, line, seconds, label, go) {
  const cover = $('lobby-cover');
  cover.classList.remove('hidden');
  $('lobby-spinner').classList.add('done');
  $('lobby-head').textContent = head;
  const btn = $('lobby-leave');
  btn.textContent = label;
  let left = seconds;
  let gone = false;
  const tick = () => {
    $('lobby-msg').textContent = line(left);
    if (left <= 0) finish();
    left--;
  };
  const iv = setInterval(tick, 1000);
  const finish = () => {
    if (gone) return;
    gone = true;
    clearInterval(iv);
    go();
  };
  btn.onclick = finish;
  tick();
}

function onBreakout(msg) {
  if (msg.kind === 'state') {
    state.breakouts = msg.rooms.length ? msg : null;
    syncControls();
    renderRooms();
    return;
  }
  if (msg.kind === 'move') {
    try { sessionStorage.setItem('aether.bo.parentTitle', state.title || ''); } catch {}
    const go = () => goRoom(msg.room, msg.name, msg.ticket);
    if (msg.prompt) {
      confirmAction({ title: 'Join breakout room?', body: `You've been assigned to ${msg.name}.`, yes: 'Join room' }).then((yes) => yes && go());
      return;
    }
    moveCountdown(msg.name, (n) => `We'll move you to your assigned room in ${n} seconds.`, 10, 'Join now', go);
    return;
  }
  if (msg.kind === 'return') {
    moveCountdown(
      msg.expired ? 'The breakout room timer has expired.' : 'Your breakout room is closing.',
      (n) => `We'll move you back to the main meeting in ${n} seconds.`,
      msg.delay,
      'Return now',
      () => goRoom(msg.parent, parentTitle(), msg.ticket)
    );
    return;
  }
  if (msg.kind === 'announce') {
    notice('Announcement', `${msg.from}: ${msg.text}`);
    return;
  }
  if (msg.kind === 'info') {
    state.bo = msg;
    state.boWarned = false;
    renderBoBanner();
    syncControls();
  }
}

// The banner in a breakout room: its name, "Remaining time", "Return".
function renderBoBanner() {
  const el = $('bo-banner');
  if (!el) return;
  const b = state.bo;
  if (!b) {
    el.classList.add('hidden');
    return;
  }
  const left = remaining(b.closes);
  el.classList.remove('hidden');
  el.innerHTML = `<strong>${esc(b.name)}</strong>${left ? `<span class="bo-time">Remaining time ${esc(left)}</span>` : ''}${
    b.allowReturn ? '<button type="button" class="btn btn-quiet sm" id="bo-return">Return</button>' : ''}`;
  const ret = $('bo-return');
  if (ret) ret.onclick = () => goRoom(b.parent, parentTitle(), b.back);
  // "This room will close in 60 seconds", once.
  if (b.closes && !state.boWarned && b.closes * 1000 - Date.now() <= 60000 && b.closes * 1000 > Date.now()) {
    state.boWarned = true;
    toast('This room will close in 60 seconds');
  }
}

// A message with one button.
function notice(title, body) {
  const host = $('dialog');
  if (!host || !host.classList.contains('hidden')) return toast(`${title}: ${body}`);
  host.dataset.takeover = '1';
  host.innerHTML = `
    <div class="dialog-card takeover-card" role="alertdialog" aria-labelledby="takeover-title" aria-describedby="takeover-body">
      <h2 id="takeover-title">${esc(title)}</h2>
      <p id="takeover-body">${esc(body)}</p>
      <div class="dialog-actions"><span class="spacer"></span><button type="button" class="btn btn-primary" data-yes>OK</button></div>
    </div>`;
  host.classList.remove('hidden');
  host.querySelector('[data-yes]').onclick = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
    delete host.dataset.takeover;
  };
  host.querySelector('[data-yes]').focus();
}

// Who can go to a room: the people here who are not hosts.
function assignable() {
  return [...state.people.values()].filter((p) => p.id !== state.me && (p.role ?? 1) < RoleHost);
}

// Sends the rooms. Members are filtered to who is here now: someone who left
// and came back has a new id, and one stale id would make the server refuse
// the whole set.
function sendRooms({ names, members, limitSec, autoMove, allowReturn }) {
  const here = new Set(assignable().map((p) => p.id));
  send((w) => encodeSet(w, {
    names,
    members: members.map((ids) => ids.filter((id) => here.has(id))),
    limitSec,
    autoMove,
    allowReturn,
  }));
}

function currentRooms(over = {}) {
  const b = state.breakouts;
  return {
    names: b.rooms.map((r) => r.name),
    members: b.rooms.map((r) => r.members.map((m) => m.id)),
    limitSec: b.limit,
    autoMove: b.autoMove,
    allowReturn: b.allowReturn,
    ...over,
  };
}

function renderRooms() {
  const box = $('view-rooms');
  if (!box) return;
  const b = state.breakouts;
  const people = assignable();
  if (state.bo && !state.isHost) {
    box.innerHTML = `<p class="br-note">You're in ${esc(state.bo.name)}.</p>`;
    return;
  }
  if (!b) {
    const n = suggestRooms(people.length);
    box.innerHTML = `
      <div class="br-create">
        <h3>Create breakout rooms</h3>
        <label class="mo-row"><span>Rooms</span>
          <select class="input" id="br-count">${Array.from({ length: MAX_ROOMS }, (_, i) => `<option${i + 1 === n ? ' selected' : ''}>${i + 1}</option>`).join('')}</select></label>
        <p class="br-note">You can assign people to rooms yourself, or have them assigned automatically.</p>
        <label class="br-radio"><input type="radio" name="br-mode" value="auto" checked /> <span>Automatically</span></label>
        <label class="br-radio"><input type="radio" name="br-mode" value="manual" /> <span>Manually</span></label>
        <button type="button" class="btn btn-primary" id="br-create">Create rooms</button>
      </div>`;
    $('br-create').onclick = () => {
      const count = Number($('br-count').value);
      const auto = box.querySelector('input[name="br-mode"]:checked')?.value === 'auto';
      sendRooms({
        names: Array.from({ length: count }, (_, i) => `Room ${i + 1}`),
        members: auto ? autoAssign(people.map((p) => p.id), count) : Array.from({ length: count }, () => []),
        limitSec: 0,
        autoMove: true,
        allowReturn: true,
      });
    };
    return;
  }
  const assigned = new Set(b.rooms.flatMap((r) => r.members.map((m) => m.id)));
  const unassigned = people.filter((p) => !assigned.has(p.id));
  const host = state.isHost && !state.bo;
  const actions = host
    ? (b.open
        ? `<button type="button" class="btn btn-primary sm" id="br-close">Close Rooms</button>
           <button type="button" class="btn btn-quiet sm" id="br-announce">Make an announcement</button>`
        : `<button type="button" class="btn btn-primary sm" id="br-open">Open Rooms</button>
           <button type="button" class="btn btn-quiet sm" id="br-assign">Assign participants</button>
           <button type="button" class="btn btn-quiet sm" id="br-shuffle">Shuffle</button>
           <button type="button" class="btn btn-quiet sm" id="br-settings">Rooms settings</button>
           <button type="button" class="btn btn-danger-quiet sm" id="br-delete">Delete rooms</button>`)
    : '';
  box.innerHTML = `
    <div class="br-status">${b.open ? 'Breakout rooms are open' : `${b.rooms.length} breakout rooms`}${
      b.open && b.closes ? ` · Remaining time <span id="br-left">${esc(remaining(b.closes))}</span>` : ''}</div>
    <div class="br-actions">${actions}</div>
    ${b.rooms.map((r, i) => `
      <div class="br-room" data-room="${i}">
        <div class="br-room-head"><strong>${esc(r.name)}</strong>
          <span class="br-count">${b.open ? `${r.present} in room` : `${r.members.length} assigned`}</span>
          ${host && !b.open ? `<button type="button" class="btn btn-quiet sm" data-rename="${i}">Rename</button>` : ''}
          ${b.open ? `<button type="button" class="btn btn-quiet sm" data-join="${i}">Join room</button>` : ''}
        </div>
        <div class="br-members">${r.members.map((m) => esc(m.name)).join(', ') || '<span class="br-note">This room is empty</span>'}</div>
      </div>`).join('')}
    ${unassigned.length && !b.open ? `<div class="br-unassigned"><strong>Unassigned (${unassigned.length})</strong><div>${unassigned.map((p) => esc(p.name)).join(', ')}</div></div>` : ''}`;

  const on = (id, fn) => { const el = $(id); if (el) el.onclick = fn; };
  on('br-open', () => send((w) => encodeCmd(w, K.open)));
  on('br-close', async () => {
    if (await confirmAction({ title: 'Close Rooms?', body: 'Everyone in the rooms will be moved back to the main meeting.', yes: 'Close Rooms' }))
      send((w) => encodeCmd(w, K.close));
  });
  on('br-delete', async () => {
    if (await confirmAction({ title: 'Delete rooms?', body: 'The rooms and their assignments will be removed.', yes: 'Delete', danger: true })) {
      send((w) => encodeCmd(w, K.del));
      toast('Breakout rooms were deleted');
    }
  });
  on('br-shuffle', () => {
    sendRooms(currentRooms({ members: shuffle(people.map((p) => p.id), b.rooms.length) }));
    toast('Everyone has been randomly assigned to new rooms.');
  });
  on('br-announce', announceDialog);
  on('br-settings', roomsSettings);
  on('br-assign', assignDialog);
  box.querySelectorAll('[data-rename]').forEach((el) => {
    el.onclick = () => renameRoom(Number(el.dataset.rename));
  });
  box.querySelectorAll('[data-join]').forEach((el) => {
    el.onclick = () => {
      try { sessionStorage.setItem('aether.bo.parentTitle', state.title || ''); } catch {}
      const r = b.rooms[Number(el.dataset.join)];
      goRoom(r.id, r.name, '');
    };
  });
}

// A small form in the meeting's dialog. fields: [{id, label, html}]; resolves
// with the dialog element on Save, null on Cancel.
function formDialog(title, fieldsHTML, yes = 'Save') {
  return new Promise((resolve) => {
    const host = $('dialog');
    if (!host || !host.classList.contains('hidden')) return resolve(null);
    host.dataset.takeover = '1';
    host.innerHTML = `
      <div class="dialog-card mo-card" role="dialog" aria-labelledby="br-title">
        <h2 id="br-title">${esc(title)}</h2>
        ${fieldsHTML}
        <div class="dialog-actions"><span class="spacer"></span>
          <button type="button" class="btn btn-quiet" data-no>Cancel</button>
          <button type="button" class="btn btn-primary" data-yes>${esc(yes)}</button></div>
      </div>`;
    host.classList.remove('hidden');
    const done = (v) => {
      removeEventListener('keydown', onKey, true);
      resolve(v);
      host.classList.add('hidden');
      host.innerHTML = '';
      delete host.dataset.takeover;
    };
    const onKey = (e) => { if (e.key === 'Escape') done(null); };
    addEventListener('keydown', onKey, true);
    host.querySelector('[data-no]').onclick = () => done(null);
    // Read before the dialog is emptied: resolve with the values now.
    host.querySelector('[data-yes]').onclick = () => {
      const values = {};
      host.querySelectorAll('[name]').forEach((el) => {
        values[el.name] = el.type === 'checkbox' ? el.checked : el.value;
      });
      done(values);
    };
    host.querySelector('input, select, textarea')?.focus();
  });
}

async function renameRoom(i) {
  const b = state.breakouts;
  const v = await formDialog('Rename room', `<label class="mo-row"><span>Room name</span>
    <input class="input" name="name" maxlength="${MAX_NAME}" value="${esc(b.rooms[i].name)}" /></label>
    <p class="mo-note">Room names must be 35 characters or fewer.</p>`);
  const name = v?.name?.trim();
  if (!name) return v && toast('The room name cannot be empty', 'bad');
  const names = b.rooms.map((r) => r.name);
  names[i] = name.slice(0, MAX_NAME);
  sendRooms(currentRooms({ names }));
}

async function roomsSettings() {
  const b = state.breakouts;
  const mins = [0, 5, 10, 15, 20, 30, 45, 60];
  const v = await formDialog('Rooms settings', `
    <label class="mo-row"><span>Set a time limit</span><select class="input" name="limit">${mins.map((m) =>
      `<option value="${m * 60}"${m * 60 === b.limit ? ' selected' : ''}>${m ? `${m} minutes` : 'No time limit'}</option>`).join('')}</select></label>
    <p class="mo-note">Room will close when time expires.</p>
    <label class="mo-row"><span>Automatically move people to rooms</span><input type="checkbox" class="mo-switch" name="auto" ${b.autoMove ? 'checked' : ''} /></label>
    <label class="mo-row"><span>Let people return to the main meeting</span><input type="checkbox" class="mo-switch" name="back" ${b.allowReturn ? 'checked' : ''} /></label>`);
  if (!v) return;
  sendRooms(currentRooms({ limitSec: Number(v.limit), autoMove: v.auto, allowReturn: v.back }));
}

async function assignDialog() {
  const b = state.breakouts;
  const where = new Map(b.rooms.flatMap((r, i) => r.members.map((m) => [m.id, i])));
  const rows = assignable().map((p) => `<label class="mo-row"><span>${esc(p.name)}</span>
    <select class="input" name="${esc(p.id)}"><option value="">Unassigned</option>${b.rooms.map((r, i) =>
      `<option value="${i}"${where.get(p.id) === i ? ' selected' : ''}>${esc(r.name)}</option>`).join('')}</select></label>`).join('');
  const v = await formDialog('Assign participants', rows || '<p class="br-note">No one is available to be assigned right now.</p>');
  if (!v) return;
  const members = b.rooms.map(() => []);
  for (const [id, i] of Object.entries(v)) if (i !== '') members[Number(i)].push(id);
  sendRooms(currentRooms({ members }));
}

async function announceDialog() {
  const v = await formDialog('Make an announcement', `<label class="mo-row br-text"><span>Announcement</span>
    <textarea class="input" name="text" maxlength="${MAX_ANNOUNCEMENT}" rows="3"></textarea></label>`, 'Send');
  const msg = v?.text?.trim();
  if (!msg) return;
  send((w) => encodeAnnounce(w, msg));
  toast('Your announcement has been sent to all breakout rooms.');
}

// ---------------------------------------------------------------------------
// Q&A (engine scheduling/qna.go): questions, likes, replies, moderation.
// The list is read once when the panel opens; after that every change
// arrives whole over the meeting socket (OpQnA). What only this viewer can
// know -- which questions are theirs, which they liked -- is kept here,
// learned from the list read and from their own actions.
// ---------------------------------------------------------------------------

const qnaState = {
  questions: new Map(),
  settings: { moderation: false, anonymous: true, closed: false },
  mine: new Set(),
  liked: new Set(),
  sort: 'recent', // Teams' "Recent" / "Top"
  tab: 'published', // hosts: published | pending | dismissed
  loaded: false,
  replying: '',
};

async function loadQnA() {
  try {
    const res = await qna.list(state.room);
    qnaState.questions.clear();
    for (const q of res.questions ?? []) {
      qnaState.questions.set(q.id, q);
      if (q.mine) qnaState.mine.add(q.id);
      if (q.liked) qnaState.liked.add(q.id);
    }
    qnaState.settings = res.settings ?? qnaState.settings;
    qnaState.loaded = true;
  } catch (e) {
    toast(e.message || 'Could not load Q&A.', 'bad');
  }
  renderQnA();
}

function onQnA(body) {
  let msg;
  try {
    msg = JSON.parse(new TextDecoder().decode(body));
  } catch {
    return;
  }
  if (msg.settings) qnaState.settings = msg.settings;
  if (msg.question) {
    const q = msg.question;
    const had = qnaState.questions.has(q.id);
    qnaState.questions.set(q.id, q);
    // A new question while the panel is closed: a dot on the button.
    const showing = state.sideView === 'qna' && !$('shell').classList.contains('panel-closed');
    if (!had && !showing && !qnaState.mine.has(q.id)) {
      $('qna-dot').hidden = false;
      $('more-dot').hidden = false; // the phone's Q&A lives under More
    }
  }
  if (state.sideView === 'qna') renderQnA();
}

function qnaWhen(iso) {
  const d = new Date(iso);
  return isNaN(d) ? '' : clockHM(d);
}

function renderQnA() {
  const box = $('view-qna');
  if (!box) return;
  if (!qnaState.loaded) {
    box.innerHTML = '<p class="br-note">Loading Q&amp;A…</p>';
    return;
  }
  const set = qnaState.settings;
  const host = state.isHost;
  const all = [...qnaState.questions.values()];
  const counts = { pending: all.filter((q) => q.status === 'pending').length, dismissed: all.filter((q) => q.status === 'dismissed').length };
  const tab = host ? qnaState.tab : 'published';
  let list = all.filter((q) => (tab === 'published' ? q.status === 'published' || (q.status !== 'published' && qnaState.mine.has(q.id)) : q.status === tab));
  list.sort((a, b) =>
    (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0) ||
    (qnaState.sort === 'top' ? b.likes - a.likes : 0) ||
    (qnaState.sort === 'top' ? new Date(a.createdAt) - new Date(b.createdAt) : new Date(b.createdAt) - new Date(a.createdAt)));

  const card = (q) => {
    const liked = qnaState.liked.has(q.id);
    const own = qnaState.mine.has(q.id);
    const status = q.status === 'pending' ? (own && !host ? 'Waiting for the organizer to publish it' : 'Pending') : q.status === 'dismissed' ? 'Dismissed' : '';
    return `<article class="qna-q${q.pinned ? ' pinned' : ''}" data-q="${esc(q.id)}">
      <header><strong>${q.anonymous ? 'Anonymous' : esc(q.author)}</strong><span class="qna-when">${esc(qnaWhen(q.createdAt))}</span>
        ${q.pinned ? '<span class="qna-tag">Pinned</span>' : ''}${q.answered ? '<span class="qna-tag ok">Answered</span>' : ''}${status ? `<span class="qna-tag">${esc(status)}</span>` : ''}
        ${host ? `<button type="button" class="person-more qna-more" data-qmore="${esc(q.id)}" aria-label="Question options"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-more"/></svg></button>` : ''}
      </header>
      <p class="qna-body">${esc(q.body)}</p>
      ${q.status === 'published' ? `<div class="qna-acts">
        <button type="button" class="btn btn-quiet sm qna-like${liked ? ' on' : ''}" data-like="${esc(q.id)}" aria-pressed="${liked}">Like${q.likes ? ` · ${q.likes}` : ''}</button>
        ${set.closed ? '' : `<button type="button" class="btn btn-quiet sm" data-reply="${esc(q.id)}">Reply${q.replies.length ? ` · ${q.replies.length}` : ''}</button>`}
      </div>` : ''}
      ${q.replies.length ? `<div class="qna-replies">${q.replies.map((r) => `<div class="qna-reply"><strong>${r.anonymous ? 'Anonymous' : esc(r.author)}</strong> ${esc(r.body)}</div>`).join('')}</div>` : ''}
      ${qnaState.replying === q.id ? `<div class="qna-replybox"><textarea class="input" id="qna-reply-text" rows="2" maxlength="1000" placeholder="Reply"></textarea>
        <button type="button" class="btn btn-primary sm" data-send-reply="${esc(q.id)}">Reply</button></div>` : ''}
    </article>`;
  };

  box.innerHTML = `
    ${host ? `<div class="qna-tabs" role="tablist">
      ${[['published', 'Published'], ['pending', `Pending${counts.pending ? ` (${counts.pending})` : ''}`], ['dismissed', 'Dismissed']].map(([k, l]) =>
        `<button type="button" role="tab" class="seg-b${tab === k ? ' active' : ''}" aria-selected="${tab === k}" data-qtab="${k}">${l}</button>`).join('')}
      <button type="button" class="btn btn-quiet sm" id="qna-settings">Q&amp;A settings</button></div>` : ''}
    ${set.closed
      ? '<p class="qna-closed">Q&amp;A is closed.</p>'
      : `<div class="qna-compose">
          <textarea class="input" id="qna-text" rows="2" maxlength="1000" placeholder="Ask a question"></textarea>
          <div class="qna-compose-row">
            ${set.anonymous ? '<label class="pj-check qna-anon"><input type="checkbox" id="qna-anon" /> <span>Post anonymously</span></label>' : '<span></span>'}
            <button type="button" class="btn btn-primary sm" id="qna-post">Post</button>
          </div>
          ${set.moderation && !host ? '<p class="br-note">Questions are reviewed before they are published.</p>' : ''}
        </div>`}
    <div class="qna-sort"><button type="button" class="seg-b${qnaState.sort === 'recent' ? ' active' : ''}" data-qsort="recent">Recent</button><button type="button" class="seg-b${qnaState.sort === 'top' ? ' active' : ''}" data-qsort="top">Top</button></div>
    <div class="qna-list">${list.map(card).join('') || `<p class="br-note">${tab === 'published' ? 'No questions yet.' : 'Nothing here.'}</p>`}</div>`;

  const on = (sel, fn) => box.querySelectorAll(sel).forEach((el) => (el.onclick = () => fn(el)));
  on('[data-qtab]', (el) => { qnaState.tab = el.dataset.qtab; renderQnA(); });
  on('[data-qsort]', (el) => { qnaState.sort = el.dataset.qsort; renderQnA(); });
  on('#qna-settings', qnaSettingsDialog);
  on('#qna-post', async () => {
    const text = $('qna-text').value.trim();
    if (!text) return;
    try {
      const q = await qna.ask(state.room, text, !!$('qna-anon')?.checked);
      qnaState.mine.add(q.id);
      qnaState.questions.set(q.id, q);
      $('qna-text').value = '';
      if (q.status === 'pending') toast('Your question will appear once the organizer publishes it.');
      renderQnA();
    } catch (e) {
      toast(e.message || 'Could not post your question.', 'bad');
    }
  });
  on('[data-like]', async (el) => {
    try {
      const { liked } = await qna.like(state.room, el.dataset.like);
      if (liked) qnaState.liked.add(el.dataset.like);
      else qnaState.liked.delete(el.dataset.like);
      renderQnA();
    } catch (e) {
      toast(e.message || 'Could not like that.', 'bad');
    }
  });
  on('[data-reply]', (el) => { qnaState.replying = qnaState.replying === el.dataset.reply ? '' : el.dataset.reply; renderQnA(); $('qna-reply-text')?.focus(); });
  on('[data-send-reply]', async (el) => {
    const text = $('qna-reply-text').value.trim();
    if (!text) return;
    try {
      await qna.reply(state.room, el.dataset.sendReply, text, false);
      qnaState.replying = '';
      renderQnA();
    } catch (e) {
      toast(e.message || 'Could not reply.', 'bad');
    }
  });
  on('[data-qmore]', (el) => {
    const q = qnaState.questions.get(el.dataset.qmore);
    if (!q) return;
    const items = [];
    if (q.status !== 'published') items.push(['publish', 'Publish']);
    if (q.status !== 'dismissed') items.push(['dismiss', 'Dismiss', true]);
    if (q.status === 'published') {
      items.push(q.pinned ? ['unpin', 'Unpin'] : ['pin', 'Pin']);
      items.push(q.answered ? ['unanswered', 'Mark as unanswered'] : ['answered', 'Mark as answered']);
    }
    popMenu(el, items, async (action) => {
      try {
        await qna.moderate(state.room, q.id, action);
      } catch (e) {
        toast(e.message || 'Could not do that.', 'bad');
      }
    });
  });
}

async function qnaSettingsDialog() {
  const s = qnaState.settings;
  const v = await formDialog('Q&A settings', `
    <label class="mo-row"><span>Review questions before they are published</span><input type="checkbox" class="mo-switch" name="moderation" ${s.moderation ? 'checked' : ''} /></label>
    <label class="mo-row"><span>Allow anonymous posts</span><input type="checkbox" class="mo-switch" name="anonymous" ${s.anonymous ? 'checked' : ''} /></label>
    <label class="mo-row"><span>Close Q&amp;A</span><input type="checkbox" class="mo-switch" name="closed" ${s.closed ? 'checked' : ''} /></label>`);
  if (!v) return;
  try {
    await qna.settings(state.room, { moderation: v.moderation, anonymous: v.anonymous, closed: v.closed });
  } catch (e) {
    toast(e.message || 'Could not save Q&A settings.', 'bad');
  }
}

// ---------------------------------------------------------------------------
// Call health (More > "Call health"): refreshed each second while showing,
// and not at all otherwise. Nothing on the wire: it reads what the browser
// and this client already count.
// ---------------------------------------------------------------------------

let healthTimer = null;
let healthPrev = null;
let healthWs = null;

function stopHealth() {
  clearInterval(healthTimer);
  healthTimer = null;
  healthPrev = null;
  healthWs = null;
}

function screenTrackIds() {
  const ids = new Set();
  if (state.screenTrackId) ids.add(state.screenTrackId);
  for (const p of state.people.values()) if (p.state?.screenTrack) ids.add(p.state.screenTrack);
  return ids;
}

async function deviceLabels() {
  // The device, not the noise suppressor's output track that is published.
  const mic = state.denoiser?.raw?.label || state.localStream?.getAudioTracks?.()[0]?.label || '—';
  const cam = state.fxPipe?.raw?.label || state.localStream?.getVideoTracks?.()[0]?.label || '—';
  let speaker = 'Default';
  try {
    if (state.speakerId) {
      const d = (await navigator.mediaDevices.enumerateDevices()).find((x) => x.deviceId === state.speakerId);
      if (d?.label) speaker = d.label;
    }
  } catch {}
  return { mic, speaker, cam };
}

async function sampleHealth() {
  const box = $('view-health');
  if (!box || state.sideView !== 'health') return stopHealth();
  let h;
  let note = '';
  if (state.wsMediaOn || !state.media?.healthSample) {
    // The WebSocket path: the browser keeps no RTP statistics for it. What
    // this client counts -- bytes each way -- is shown; the rest is not
    // invented.
    const now = { at: performance.now(), tx: meter.txBytes, rx: meter.rxBytes };
    const prev = healthWs;
    healthWs = now;
    const ms = prev ? now.at - prev.at : 0;
    h = health(null, { at: now.at, out: {}, in: {} });
    if (ms > 0) {
      h.network.sentBitrate = Math.round(((now.tx - prev.tx) * 8) / ms);
      h.network.receivedBitrate = Math.round(((now.rx - prev.rx) * 8) / ms);
    }
    note = 'This call is carried over the WebSocket path. Audio and video figures are measured on the WebRTC path only.';
  } else {
    const cur = await state.media.healthSample(screenTrackIds());
    h = health(healthPrev, cur);
    healthPrev = cur;
  }
  const d = await deviceLabels();
  const row = (label, field, v) => `<div class="hl-row"><span>${label}</span><b>${esc(showHealth(field, v))}</b></div>`;
  const media = (title, s, video) => `<section class="hl-sec"><h3>${title}</h3>
      ${row('Sent bitrate', 'sentBitrate', s.sentBitrate)}
      ${video ? row('Sent resolution', 'sentResolution', s.sentResolution) + row('Sent frame rate', 'sentFrameRate', s.sentFrameRate) : ''}
      ${row('Received bitrate', 'receivedBitrate', s.receivedBitrate)}
      ${row('Received packet loss', 'receivedPacketLoss', s.receivedPacketLoss)}
      ${video ? row('Received resolution', 'receivedResolution', s.receivedResolution) + row('Received frame rate', 'receivedFrameRate', s.receivedFrameRate) : ''}
    </section>`;
  box.innerHTML = `
    ${note ? `<p class="br-note">${esc(note)}</p>` : ''}
    <section class="hl-sec"><h3>Network</h3>
      ${row('Round trip time', 'roundTripTime', h.network.roundTripTime)}
      ${row('Sent bitrate', 'sentBitrate', h.network.sentBitrate)}
      ${row('Received bitrate', 'receivedBitrate', h.network.receivedBitrate)}
      ${row('Received packet loss', 'receivedPacketLoss', h.network.receivedPacketLoss)}
    </section>
    ${media('Audio', h.audio, false)}
    ${media('Video', h.video, true)}
    ${media('Screen Sharing', h.screen, true)}
    <section class="hl-sec"><h3>Devices</h3>
      <div class="hl-row"><span>Microphone</span><b>${esc(d.mic)}</b></div>
      <div class="hl-row"><span>Speaker</span><b>${esc(d.speaker)}</b></div>
      <div class="hl-row"><span>Camera</span><b>${esc(d.cam)}</b></div>
    </section>`;
}

function openHealth() {
  setSide('health');
}

// ---------------------------------------------------------------------------
// Meeting notes (engine scheduling/notes.go): Agenda, Notes, Follow-up tasks.
// Read once when the panel opens; every change then arrives whole (OpNotes).
// An edit carries the version it was made against; if someone else changed
// the item first, the server refuses it and the panel shows their version.
// ---------------------------------------------------------------------------

const notesState = { created: false, items: new Map(), loaded: false, editing: '' };
const NOTE_SECTIONS = [
  ['agenda', 'Agenda', 'Add an agenda item'],
  ['notes', 'Notes', 'Take notes'],
  ['tasks', 'Follow-up tasks', 'Add a task'],
];

async function loadNotes() {
  try {
    const res = await notes.get(state.room);
    notesState.created = !!res.created;
    notesState.items.clear();
    for (const it of res.items ?? []) notesState.items.set(it.id, it);
    notesState.loaded = true;
  } catch (e) {
    toast(e.message || 'Could not load meeting notes.', 'bad');
  }
  renderNotes();
}

function onNotes(body) {
  let msg;
  try {
    msg = JSON.parse(new TextDecoder().decode(body));
  } catch {
    return;
  }
  if (msg.created) notesState.created = true;
  if (msg.item) {
    if (msg.item.deleted) notesState.items.delete(msg.item.id);
    else notesState.items.set(msg.item.id, msg.item);
  }
  // Re-render, unless it would throw away what this person is typing.
  if (state.sideView === 'notes' && !notesState.editing) renderNotes();
}

function renderNotes() {
  const box = $('view-notes');
  if (!box) return;
  if (!notesState.loaded) {
    box.innerHTML = '<p class="br-note">Loading meeting notes…</p>';
    return;
  }
  if (!notesState.created) {
    box.innerHTML = state.isHost
      ? `<div class="nt-empty"><p>Meeting notes are shared with everyone in the meeting: an agenda, notes, and follow-up tasks.</p>
         <button type="button" class="btn btn-primary" id="nt-create">Add meeting notes</button></div>`
      : '<p class="br-note">The organizer hasn\'t created meeting notes yet.</p>';
    const c = $('nt-create');
    if (c) c.onclick = async () => {
      try { await notes.create(state.room); } catch (e) { toast(e.message, 'bad'); }
    };
    return;
  }
  const people = [...state.people.values()].map((p) => p.name).filter(Boolean);
  const all = [...notesState.items.values()];
  box.innerHTML = NOTE_SECTIONS.map(([sec, title, add]) => `
    <section class="nt-sec" data-sec="${sec}"><h3>${title}</h3>
      <ul class="nt-list">${all.filter((i) => i.section === sec).map((i) => `
        <li class="nt-item${i.done ? ' done' : ''}" data-nid="${esc(i.id)}">
          ${sec === 'tasks' ? `<label class="pj-check nt-done"><input type="checkbox" data-done="${esc(i.id)}" ${i.done ? 'checked' : ''} aria-label="Done" /></label>` : ''}
          <div class="nt-text" tabindex="0" role="button" data-edit="${esc(i.id)}" title="Edit">${esc(i.body)}</div>
          ${sec === 'tasks' ? `<select class="input nt-who" data-assign="${esc(i.id)}" aria-label="Assigned to"><option value="">Unassigned</option>${
            [...new Set([...people, i.assignee].filter(Boolean))].map((n) => `<option${n === i.assignee ? ' selected' : ''}>${esc(n)}</option>`).join('')}</select>` : ''}
          <button type="button" class="icon-btn nt-del" data-del="${esc(i.id)}" aria-label="Delete"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg></button>
        </li>`).join('')}</ul>
      <input class="input nt-add" data-add="${sec}" maxlength="2000" placeholder="${add}" />
    </section>`).join('');

  const change = async (id, patch) => {
    const it = notesState.items.get(id);
    if (!it) return;
    try {
      const fresh = await notes.edit(state.room, id, { ...patch, version: it.version });
      notesState.items.set(id, fresh);
    } catch (e) {
      if (e.status === 409 && e.body?.item) {
        notesState.items.set(id, e.body.item);
        toast('Someone else changed this item. Showing their version.', 'bad');
      } else toast(e.message || 'Could not save.', 'bad');
    }
    renderNotes();
  };
  box.querySelectorAll('[data-add]').forEach((el) => {
    el.onkeydown = async (e) => {
      if (e.key !== 'Enter' || !el.value.trim()) return;
      const text = el.value.trim();
      el.value = '';
      try {
        const it = await notes.add(state.room, el.dataset.add, text);
        notesState.items.set(it.id, it);
        renderNotes();
        box.querySelector(`[data-add="${el.dataset.add}"]`)?.focus();
      } catch (err) {
        el.value = text;
        toast(err.message || 'Could not add.', 'bad');
      }
    };
  });
  box.querySelectorAll('[data-edit]').forEach((el) => {
    const begin = () => {
      const id = el.dataset.edit;
      notesState.editing = id;
      const ta = document.createElement('textarea');
      ta.className = 'input nt-edit';
      ta.maxLength = 2000;
      ta.value = notesState.items.get(id)?.body ?? '';
      el.replaceWith(ta);
      ta.focus();
      let finished = false;
      const finish = async (save) => {
        if (finished) return;
        finished = true;
        notesState.editing = '';
        const body = ta.value.trim();
        if (save && body && body !== notesState.items.get(id)?.body) await change(id, { body });
        else renderNotes();
      };
      ta.onblur = () => finish(true);
      ta.onkeydown = (e) => {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); finish(true); }
        if (e.key === 'Escape') finish(false);
      };
    };
    el.onclick = begin;
    el.onkeydown = (e) => { if (e.key === 'Enter') begin(); };
  });
  box.querySelectorAll('[data-done]').forEach((el) => (el.onchange = () => change(el.dataset.done, { done: el.checked })));
  box.querySelectorAll('[data-assign]').forEach((el) => (el.onchange = () => change(el.dataset.assign, { assignee: el.value })));
  box.querySelectorAll('[data-del]').forEach((el) => {
    el.onclick = async () => {
      const it = notesState.items.get(el.dataset.del);
      if (!it) return;
      try {
        await notes.remove(state.room, it.id, it.version);
        notesState.items.delete(it.id);
      } catch (e) {
        toast(e.status === 409 ? 'Someone else changed this item first.' : e.message || 'Could not delete.', 'bad');
        await loadNotes();
        return;
      }
      renderNotes();
    };
  });
}

// ---------------------------------------------------------------------------
// The whiteboard (engine room/whiteboard.go, signal/whiteboard.go; web
// whiteboard.js). Share > Whiteboard turns it on for everyone; it takes the
// stage, with the people on the strip beside it.
// ---------------------------------------------------------------------------

const wb = {
  open: false,
  opener: '',
  strokes: new Map(), // id -> stroke
  notes: new Map(), // id -> note
  live: new Map(), // `${author}:${seq}` -> { tool, color, width, points }
  mine: [], // my committed stroke ids, newest last, for Undo
  tool: 'pen', // pen | highlighter | eraser | note
  color: 0,
  width: 3,
  seq: 0,
  drawing: null, // { seq, points, sentTo }
  raf: 0,
};

function wbMayManage() {
  return state.isHost || wb.opener === state.me;
}

function onWhiteboard(msg) {
  if (msg.kind === 'state') {
    wb.open = msg.open;
    wb.opener = msg.opener;
    wb.strokes = new Map(msg.strokes.map((s) => [s.id, s]));
    wb.notes = new Map(msg.notes.map((n) => [n.id, n]));
    wb.live.clear();
    showWhiteboard();
  } else if (msg.kind === 'stroke') {
    wb.strokes.set(msg.id, msg);
    wb.live.delete(`${msg.author}:${msg.seq}`);
    if (msg.author === state.me) wb.mine.push(msg.id);
  } else if (msg.kind === 'live') {
    const key = `${msg.author}:${msg.seq}`;
    const cur = wb.live.get(key);
    if (cur) cur.points.push(...msg.points);
    else wb.live.set(key, { tool: msg.tool, color: msg.color, width: msg.width, points: msg.points });
  } else if (msg.kind === 'erase') {
    wb.strokes.delete(msg.id);
    wb.notes.delete(msg.id);
  } else if (msg.kind === 'note') {
    wb.notes.set(msg.id, msg);
  } else if (msg.kind === 'clear') {
    wb.strokes.clear();
    wb.notes.clear();
    wb.live.clear();
    wb.mine = [];
  } else if (msg.kind === 'close') {
    wb.open = false;
    showWhiteboard();
    return;
  }
  drawBoard();
}

function showWhiteboard() {
  const el = $('wb');
  el.classList.toggle('hidden', !wb.open);
  $('stage').dataset.wb = wb.open ? '1' : '';
  if (wb.open) {
    renderWbTools();
    sizeBoard();
  }
  renderStage();
  reportViewport(true);
}

// The board keeps 16:9 inside whatever room the stage gives it.
function sizeBoard() {
  const host = $('wb');
  const board = $('wb-board');
  const canvas = $('wb-canvas');
  if (!host || !board || !canvas || !wb.open) return;
  const tools = $('wb-tools').getBoundingClientRect().height;
  const avail = host.getBoundingClientRect();
  const w = Math.max(1, Math.min(avail.width, ((avail.height - tools - 8) * BOARD_W) / BOARD_H));
  const h = (w * BOARD_H) / BOARD_W;
  board.style.width = `${Math.floor(w)}px`;
  board.style.height = `${Math.floor(h)}px`;
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.floor(w * dpr);
  canvas.height = Math.floor(h * dpr);
  canvas.style.width = `${Math.floor(w)}px`;
  canvas.style.height = `${Math.floor(h)}px`;
  drawBoard();
}

function drawBoard() {
  if (!wb.open || wb.raf) return;
  wb.raf = requestAnimationFrame(() => {
    wb.raf = 0;
    const canvas = $('wb-canvas');
    const ctx = canvas?.getContext('2d');
    if (!ctx) return;
    const k = canvas.width / BOARD_W;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.fillStyle = '#ffffff';
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    const line = (s) => {
      const p = s.points;
      if (!p || p.length < 2) return;
      ctx.globalAlpha = s.tool === TOOL.highlighter ? 0.35 : 1;
      ctx.strokeStyle = PALETTE[s.color] ?? PALETTE[0];
      ctx.lineWidth = Math.max(1, s.width * (s.tool === TOOL.highlighter ? 12 : 3) * k);
      ctx.lineCap = 'round';
      ctx.lineJoin = 'round';
      ctx.beginPath();
      ctx.moveTo(p[0] * k, p[1] * k);
      if (p.length === 2) ctx.lineTo(p[0] * k + 0.01, p[1] * k);
      for (let i = 2; i < p.length; i += 2) ctx.lineTo(p[i] * k, p[i + 1] * k);
      ctx.stroke();
    };
    for (const s of wb.strokes.values()) line(s);
    for (const s of wb.live.values()) line(s);
    if (wb.drawing) line({ tool: TOOL[wb.tool] ?? 0, color: wb.color, width: wb.width, points: wb.drawing.points });
    ctx.globalAlpha = 1;
    // Sticky notes are elements, so their text wraps and reads as text.
    const box = $('wb-notes');
    const kk = parseFloat($('wb-board').style.width) / BOARD_W;
    box.innerHTML = [...wb.notes.values()].map((n) => `<div class="wb-note" data-note="${n.id}" style="left:${n.x * kk}px;top:${n.y * kk}px;background:${PALETTE[n.color] ?? PALETTE[8]};width:${360 * kk}px;min-height:${220 * kk}px;font-size:${Math.max(10, 40 * kk)}px">${esc(n.text)}</div>`).join('');
  });
}

function renderWbTools() {
  const t = $('wb-tools');
  const tool = (k, label, icon) => `<button type="button" class="wb-tool${wb.tool === k ? ' on' : ''}" data-tool="${k}" aria-pressed="${wb.tool === k}" title="${label}" aria-label="${label}">${icon}</button>`;
  t.innerHTML = `
    ${tool('pen', 'Pen', '<svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-pencil"/></svg>')}
    ${tool('highlighter', 'Highlighter', '<span class="wb-hl">H</span>')}
    ${tool('eraser', 'Eraser', '<span class="wb-er">⌫</span>')}
    ${tool('note', 'Sticky note', '<span class="wb-sn"></span>')}
    <span class="wb-sep"></span>
    ${INK.map((c) => `<button type="button" class="wb-swatch${wb.color === c ? ' on' : ''}" data-color="${c}" style="--c:${PALETTE[c]}" aria-label="Colour ${c + 1}" aria-pressed="${wb.color === c}"></button>`).join('')}
    <span class="wb-sep"></span>
    ${[2, 3, 6].map((w) => `<button type="button" class="wb-width${wb.width === w ? ' on' : ''}" data-width="${w}" aria-label="Thickness ${w}"><span style="width:${w * 2 + 2}px;height:${w * 2 + 2}px"></span></button>`).join('')}
    <span class="wb-sep"></span>
    <button type="button" class="btn btn-quiet sm" id="wb-undo">Undo</button>
    ${wbMayManage() ? '<button type="button" class="btn btn-quiet sm" id="wb-clear">Clear</button><button type="button" class="btn btn-danger-quiet sm" id="wb-close">Turn off whiteboard</button>' : ''}`;
  t.querySelectorAll('[data-tool]').forEach((b) => (b.onclick = () => { wb.tool = b.dataset.tool; renderWbTools(); }));
  t.querySelectorAll('[data-color]').forEach((b) => (b.onclick = () => { wb.color = Number(b.dataset.color); if (wb.tool === 'eraser' || wb.tool === 'note') wb.tool = 'pen'; renderWbTools(); }));
  t.querySelectorAll('[data-width]').forEach((b) => (b.onclick = () => { wb.width = Number(b.dataset.width); renderWbTools(); }));
  $('wb-undo').onclick = () => {
    while (wb.mine.length) {
      const id = wb.mine.pop();
      if (wb.strokes.has(id)) return send((w) => encodeSimple(w, W.erase, id));
    }
  };
  const clear = $('wb-clear');
  if (clear) clear.onclick = async () => {
    if (await confirmAction({ title: 'Clear the whiteboard?', body: 'Everything on it will be removed for everyone.', yes: 'Clear', danger: true }))
      send((w) => encodeSimple(w, W.clear));
  };
  const close = $('wb-close');
  if (close) close.onclick = () => send((w) => encodeSimple(w, W.close));
}

function boardPoint(e) {
  const r = $('wb-canvas').getBoundingClientRect();
  return toBoard(e.clientX - r.left, e.clientY - r.top, r.width, r.height);
}

// What a point touches: a sticky note, else the nearest stroke within reach.
function hitTest(x, y) {
  for (const n of [...wb.notes.values()].reverse()) {
    if (x >= n.x && x <= n.x + 360 && y >= n.y && y <= n.y + 220) return n.id;
  }
  let best = 0, bestD = 60 * 60;
  for (const s of wb.strokes.values()) {
    for (let i = 0; i < s.points.length; i += 2) {
      const dx = s.points[i] - x, dy = s.points[i + 1] - y;
      const d = dx * dx + dy * dy;
      if (d < bestD) { bestD = d; best = s.id; }
    }
  }
  return best;
}

// The stroke in progress goes out in small batches for everyone's live
// preview, coalesced to one frame per 50 ms; the whole stroke follows when
// the pen lifts. A stroke longer than a frame allows is cut into several.
function sendLive() {
  const d = wb.drawing;
  if (!d || d.points.length <= d.sentTo) return;
  // Each batch starts from the last point already sent, so the preview has
  // no gaps between batches.
  const from = Math.max(0, d.sentTo - 2);
  const chunk = d.points.slice(from, Math.min(d.points.length, from + 2 * MAX_POINTS));
  send((w) => encodeStroke(w, W.live, d.seq, TOOL[wb.tool] ?? 0, wb.color, wb.width, chunk));
  d.sentTo = from + chunk.length;
}

function wireWhiteboard() {
  const canvas = $('wb-canvas');
  canvas.onpointerdown = async (e) => {
    if (!wb.open) return;
    e.preventDefault();
    const [x, y] = boardPoint(e);
    if (wb.tool === 'eraser') {
      const id = hitTest(x, y);
      if (id) send((w) => encodeSimple(w, W.erase, id));
      return;
    }
    if (wb.tool === 'note') {
      const v = await formDialog('Sticky note', `<label class="mo-row br-text"><span>Note</span><textarea class="input" name="text" maxlength="500" rows="3"></textarea></label>
        <div class="mo-row"><span>Colour</span><select class="input" name="color">${NOTE_COLORS.map((c, i) => `<option value="${c}">${['Yellow', 'Green', 'Blue', 'Pink'][i]}</option>`).join('')}</select></div>`, 'Add');
      const text = v?.text?.trim();
      if (text) send((w) => encodeNote(w, 0, Math.min(x, BOARD_W - 361), Math.min(y, BOARD_H - 221), Number(v.color), text));
      return;
    }
    canvas.setPointerCapture?.(e.pointerId);
    wb.drawing = { seq: ++wb.seq, points: [x, y], sentTo: 0, timer: setInterval(sendLive, 50) };
    drawBoard();
  };
  canvas.onpointermove = (e) => {
    const d = wb.drawing;
    if (!d) return;
    const [x, y] = boardPoint(e);
    const n = d.points.length;
    if (d.points[n - 2] === x && d.points[n - 1] === y) return;
    d.points.push(x, y);
    drawBoard();
  };
  const end = () => {
    const d = wb.drawing;
    if (!d) return;
    clearInterval(d.timer);
    wb.drawing = null;
    for (let i = 0; i < d.points.length; i += 2 * MAX_POINTS) {
      const part = d.points.slice(i, i + 2 * MAX_POINTS);
      send((w) => encodeStroke(w, W.stroke, d.seq, TOOL[wb.tool] ?? 0, wb.color, wb.width, part));
    }
    // Shown as a live line until the server's copy arrives.
    wb.live.set(`${state.me}:${d.seq}`, { tool: TOOL[wb.tool] ?? 0, color: wb.color, width: wb.width, points: d.points });
    drawBoard();
  };
  canvas.onpointerup = end;
  canvas.onpointercancel = end;
  $('wb-notes').onclick = async (e) => {
    const el = e.target.closest('[data-note]');
    if (!el) return;
    const n = wb.notes.get(Number(el.dataset.note));
    if (!n) return;
    if (wb.tool === 'eraser') return send((w) => encodeSimple(w, W.erase, n.id));
    const v = await formDialog('Sticky note', `<label class="mo-row br-text"><span>Note</span><textarea class="input" name="text" maxlength="500" rows="3">${esc(n.text)}</textarea></label>`, 'Save');
    const text = v?.text?.trim();
    if (text) send((w) => encodeNote(w, n.id, n.x, n.y, n.color, text));
  };
  addEventListener('resize', () => wb.open && sizeBoard());
  // Share > Whiteboard.
  $('share-wb').onclick = () => {
    if (wb.open) return toast('The whiteboard is already on.');
    send((w) => encodeOpen(w));
  };
  $('share-screen').onclick = () => $('btn-screen').click();
}
