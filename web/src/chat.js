// Persistent chat: conversations that outlive any meeting.
//
// Delivery is a push socket, not polling. A conversation with nothing
// happening in it costs nothing, which is the same principle the media path
// is built on: never spend bandwidth in proportion to how long a tab is open.

import { messaging, directory, session, ApiError, deviceId } from './api.js';
import { Writer, Reader, Op, newDictPair, splitBatch } from './wire.js';
import { initials, avatarAttrs } from './avatar.js';
import { attachPicker, isOnlyEmoji, pickEmoji, closeEmojiMenu } from './emoji.js';
import { whenFull } from './when.js';
import { groupRuns } from './runs.js';
import { renderBody, wrapSelection, suggestedReplies, plainText } from './format.js';
import { receiptState, receiptTitle, unreadBy } from './receipts.js';
import { rankPeople, mergePeople, highlight, createSearch } from './peoplesearch.js';
import { presenceIcon, presenceLine, agoText, statusLabel as presenceLabel, isAutoStatus as presenceIsAuto } from './presence.js';
import { createIdleWatch } from './idle.js';
import { createTypingBoard, createTypingSender } from './typing.js';
import { shouldPlay, play as playSound, soundLevel } from './sounds.js';

const $ = (id) => document.getElementById(id);

const esc = (s) =>
  String(s ?? '').replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]
  );

function when(iso) {
  const d = new Date(iso);
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  if (sameDay) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  const days = Math.round((now - d) / 86400000);
  if (days < 7) return d.toLocaleDateString([], { weekday: 'short' });
  return d.toLocaleDateString([], { day: 'numeric', month: 'short' });
}

const state = {
  conversations: [],
  active: null,
  messages: [],
  ws: null,
  dicts: newDictPair(),
  typing: createTypingBoard(), // who is typing where; see typing.js
  pending: [], // File objects chosen but not yet sent
  threadTab: 'chat', // 'chat' | 'shared'
  listFilter: 'all', // 'all' | 'unread' | 'chats' | 'channels' | 'meetings' | 'groups' | 'favourites' | 'muted'
  collapsed: loadCollapsed(), // list sections folded away: 'favourites' | 'chats'
  threadSearch: null, // null, or { q, results, loading } while searching this conversation
  threadOpen: null, // in a channel: { rootId, replies, loading } while a thread pane is open
  draftMentions: new Map(), // "@Name" text -> userId, for the message being written
  priority: '', // '' | 'important' | 'urgent' for the message being written
  scheduleAt: 0, // unix ms, 0 = now
  lastSent: null, // { id, convId, at } for Undo
  memberCache: new Map(), // conversationId -> members page, for big conversations
  shared: new Map(), // conversationId -> {files, links} (last fetched)
  // Where the reader had got to when the thread was opened, so the "New" rule
  // can be drawn. Captured before the read cursor is moved, or it is always
  // "now" and the rule never appears.
  lastReadAt: 0,
  // The message being replied to, if any.
  replyTo: null,
  online: new Set(), // user ids currently connected
  statuses: new Map(), // user id -> { status, note, until, auto } (what they say about themselves)
  lastSeen: new Map(), // user id -> ms when they were last active, for whoever is offline or away
  onUnread: null,
  reconnectDelay: 1000,
};

// ---------------------------------------------------------------------------
// Push socket
// ---------------------------------------------------------------------------

// Ring somebody: the server relays it to every socket they have open.
// Touch has no hover. On a coarse pointer a tap on the bubble opens that
// message's action bar (and closes any other); a tap elsewhere closes it.
if (typeof document !== 'undefined' && typeof matchMedia === 'function') {
  document.addEventListener('click', (e) => {
    if (!matchMedia('(pointer: coarse)').matches) return;
    const bubble = e.target.closest?.('.bubble');
    const inActs = e.target.closest?.('.msg-acts');
    if (inActs) return;
    document.querySelectorAll('.msg-acts.tapped').forEach((a) => a.classList.remove('tapped'));
    if (!bubble) return;
    const acts = bubble.closest('.msg-body')?.querySelector('.msg-acts');
    if (acts) acts.classList.add('tapped');
  });
}

// Ctrl+F while a thread is open finds in that chat, the way a message client
// does; the browser's page find is not useful over a virtualised log.
if (typeof document !== 'undefined') {
  document.addEventListener('keydown', (e) => {
    if (!(e.ctrlKey || e.metaKey) || e.key.toLowerCase() !== 'f' || !state.active) return;
    if (!document.getElementById('thread')) return;
    e.preventDefault();
    if (!state.threadSearch) state.threadSearch = { q: '', results: [], loading: false };
    renderThread();
    setTimeout(() => document.getElementById('ts-input')?.focus(), 30);
  });
}

// Idle, from this device's point of view. Five quiet minutes (no pointer,
// key or touch) and the server is told; the first input tells it otherwise.
// On a phone the app going to the background is enough, as in Teams.
let idleWatch = null;
function startIdleWatch() {
  if (idleWatch) return;
  const coarse = matchMedia?.('(pointer: coarse)')?.matches ?? false;
  idleWatch = createIdleWatch({ hiddenIsIdle: coarse, onChange: (idle) => sendIdle(idle) });
  const poke = () => idleWatch.activity();
  for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart']) {
    document.addEventListener(ev, poke, { passive: true, capture: true });
  }
  document.addEventListener('visibilitychange', () => idleWatch.visibility(document.hidden));
  setInterval(() => idleWatch.tick(), 15_000);
}
function sendIdle(idle) {
  const ws = state.ws;
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  const w = new Writer(state.dicts.tx);
  w.header(Op.Idle);
  w.bool(!!idle);
  w.bits(0, 7);
  ws.send(w.bytes());
}

// --- typing: the indicator others see, and what this client says ---------
// Painting in place: the line under the thread changes text and visibility
// without redrawing the thread (a redraw per keystroke of the other side
// would fight the reader's scroll). A conversation that is not open shows
// it in its list row instead.
function typingMarkup(line) {
  return line ? `${esc(line)}<span class="dots" aria-hidden="true"><i></i><i></i><i></i></span>` : '';
}
function paintTyping(convId) {
  if (state.active?.id === convId) {
    const el = document.querySelector('#thread .typing');
    if (!el) return renderThread();
    const line = state.typing.line(convId);
    el.innerHTML = typingMarkup(line);
    el.classList.toggle('hidden', !line);
  } else {
    renderList();
  }
}
// One timer for every typist everywhere, armed for the earliest lapse.
let typingTimer = null;
function armTypingTimer() {
  clearTimeout(typingTimer);
  typingTimer = null;
  const due = state.typing.nextDue();
  if (due == null) return;
  typingTimer = setTimeout(() => {
    typingTimer = null;
    for (const convId of state.typing.expire()) paintTyping(convId);
    armTypingTimer();
  }, Math.max(50, due - Date.now()));
}
// What this client tells the server. One sender for the app: switching
// conversations stops the old one first, so nobody is left "typing" in a
// chat that was closed.
let typingSenderConv = null;
const typingSender = createTypingSender({ signal: (on) => sendTyping(typingSenderConv, on) });
function bindTypingSender(convId) {
  if (typingSenderConv && typingSenderConv !== convId) typingSender.sent();
  typingSenderConv = convId;
}
function sendTyping(convId, on) {
  const ws = state.ws;
  if (!convId || !ws || ws.readyState !== WebSocket.OPEN) return;
  const w = new Writer(state.dicts.tx);
  w.header(Op.Typing);
  w.str(convId);
  w.bool(!!on);
  w.bits(0, 7);
  ws.send(w.bytes());
}
if (typeof window !== 'undefined') {
  setInterval(() => typingSender.tick(), 1000);
  // Leaving the page mid-sentence: say so, best effort.
  window.addEventListener('pagehide', () => typingSender.sent());
}

export function sendRing(targetUserId, room) {
  const ws = state.ws;
  if (!ws || ws.readyState !== WebSocket.OPEN) return false;
  const w = new Writer(state.dicts.tx);
  w.header(Op.Ring);
  w.str(targetUserId);
  w.str(room);
  ws.send(w.bytes());
  return true;
}

// Answer a ring. The caller is told either way.
export function answerRing(callerUserId, room, accepted) {
  const ws = state.ws;
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  const w = new Writer(state.dicts.tx);
  w.header(Op.RingAnswer);
  w.str(callerUserId);
  w.str(room);
  w.bool(!!accepted);
  w.bits(0, 7);
  ws.send(w.bytes());
}

// The other person in a direct conversation, for the Call button.
// The person an avatar is for, as an identity rather than as whatever text
// the surface happened to hold. A direct conversation's face is the other
// member's; a group's is the group itself; your own notes wear your face.
export function whoOf(c) {
  if (!c) return { name: '' };
  if (c.kind === 'direct') {
    const other = (c.members ?? []).find((m) => m.userId !== session.user?.id);
    if (other) return { userId: other.userId, name: other.name, email: other.email };
    // No other member: this is the thread you keep with yourself.
    const u = session.user;
    return { userId: u?.id, name: u?.displayName, email: u?.email };
  }
  // A team's channel is named with its team, as Teams heads it.
  const team = c.teamId ? (state.teams ?? []).find((t) => t.id === c.teamId) : null;
  if (team) return { name: `${team.name} › ${c.title}` };
  return { name: c.display || c.title || 'Conversation' };
}

// A message's sender, keyed on their account so their colour matches the one
// they wear in the conversation list, in the meeting, and on the dashboard.
function senderOf(m) {
  return { userId: m.senderId, name: m.senderName };
}

export function peerOf(c) {
  if (!c || c.kind !== 'direct') return null;
  return (c.members ?? []).find((m) => m.userId !== session.user?.id) ?? null;
}

export function connectChatSocket(onUnread) {
  state.onUnread = onUnread;
  openSocket();
}

function openSocket() {
  if (state.ws && state.ws.readyState <= WebSocket.OPEN) return;

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws?d=${encodeURIComponent(deviceId)}`);
  ws.binaryType = 'arraybuffer';
  state.ws = ws;

  ws.onopen = () => {
    state.reconnectDelay = 1000;
    // Pushes missed while the socket was down: the count from the source.
    syncActivity();
    startIdleWatch();
    sendIdle(idleWatch.idle);
    // A one byte heartbeat, same as the meeting socket.
    state.ping = setInterval(() => {
      if (ws.readyState === WebSocket.OPEN) {
        const w = new Writer(state.dicts.tx);
        w.header(Op.Ping);
        ws.send(w.bytes());
      }
    }, 25000);
  };

  // One frame. The server coalesces frames that leave within a few
  // milliseconds of each other into one Batch (hub.go, writePump); the
  // frames inside decode in order against the same dictionary.
  const onFrame = (h, r) => {

    if (h.op === Op.ConvMessage) {
      const m = {
        conversationId: r.str(),
        id: r.str(),
        senderId: r.str(),
        senderName: r.str(),
        createdAt: new Date(r.uvarint()).toISOString(),
        deleted: r.bool(),
        edited: r.bool(),
        forwarded: r.bool(),
        system: r.bool(),
      };
      r.bits(4);
      m.body = new TextDecoder().decode(r.blob());
      // The tail: the quote on a reply, then the reactions. Both are what
      // the thread draws, and a push without them used to erase them.
      m.replyTo = r.str();
      if (m.replyTo) {
        const senderId = r.str();
        const senderName = r.str();
        const deleted = r.bool();
        r.bits(7);
        const body = new TextDecoder().decode(r.blob());
        m.quoted = { id: m.replyTo, senderId, senderName, deleted, body };
      }
      const nGroups = r.uvarint();
      const me = session.user?.id;
      m.reactions = [];
      for (let i = 0; i < nGroups && !r.err; i++) {
        const emoji = r.str();
        const count = r.uvarint();
        const nWho = r.uvarint();
        const who = [];
        for (let k = 0; k < nWho && !r.err; k++) who.push(r.str());
        m.reactions.push({ emoji, count, who, mine: who.includes(me) });
      }
      const nAtt = r.uvarint();
      m.attachments = [];
      for (let i = 0; i < nAtt && !r.err; i++) {
        m.attachments.push({ id: r.str(), name: r.str(), mime: r.str(), size: r.uvarint() });
      }
      const nMen = r.uvarint();
      m.mentions = [];
      for (let i = 0; i < nMen && !r.err; i++) m.mentions.push(r.str());
      m.priority = r.str();
      m.meta = new TextDecoder().decode(r.blob());
      // The poll tally: option count (0 = not a poll), then per option the
      // count and up to twelve voter ids, then the distinct total.
      const nOpt = r.uvarint();
      if (nOpt > 0) {
        const counts = [];
        const voters = [];
        let named = 0;
        for (let i = 0; i < nOpt && !r.err; i++) {
          counts.push(Number(r.uvarint()));
          const k = r.uvarint();
          const who = [];
          for (let j = 0; j < k && !r.err; j++) who.push(r.str());
          named += who.length;
          voters.push(who);
        }
        const total = Number(r.uvarint());
        // My own choice: from the voter lists when the poll names people;
        // otherwise (anonymous) from what this client knows it clicked.
        const meId = session.user?.id;
        const mine = named ? voters.map((w, i) => (w.includes(meId) ? i : -1)).filter((i) => i >= 0) : null;
        m.poll = { counts, voters: named ? voters : null, total, mine };
      }
      if (r.err) return;
      onIncoming(m);
    } else if (h.op === Op.ReadCursor) {
      // Somebody's cursor moved: "seen by" is drawn from the members' cursors,
      // so update them in place and redraw if the thread is open.
      const convId = r.str();
      const who = r.str();
      const at = new Date(Number(r.uvarint())).toISOString();
      if (r.err) return;
      const c = state.conversations.find((x) => x.id === convId);
      const mem = c?.members?.find((x) => x.userId === who);
      if (mem) mem.lastRead = at;
      if (state.active?.id === convId) {
        const am = state.active.members?.find((x) => x.userId === who);
        if (am) am.lastRead = at;
        renderThread();
      }
    } else if (h.op === Op.Status) {
      const who = r.str();
      const status = r.str();
      const note = r.str();
      const untilMs = Number(r.uvarint());
      const auto = r.bool();
      const online = r.bool();
      r.bits(6);
      const seenMs = Number(r.uvarint());
      if (r.err) return;
      mergeStatuses({ [who]: { status, note, until: untilMs ? new Date(untilMs).toISOString() : null, auto, online, lastSeen: seenMs ? new Date(seenMs).toISOString() : null } });
      renderList();
      if (state.active?.members?.some((x) => x.userId === who)) renderThread();
      if (who === session.user?.id) window.dispatchEvent(new CustomEvent('aether:my-status'));
    } else if (h.op === Op.Presence) {
      const who = r.str();
      const on = r.bool();
      r.bits(7);
      const seenMs = Number(r.uvarint());
      if (r.err) return;
      if (on) state.online.add(who);
      else state.online.delete(who);
      if (!on && seenMs) state.lastSeen.set(who, seenMs);
      renderList();
      if (state.active) renderThread();
    } else if (h.op === Op.Ring) {
      const callerId = r.str();
      const callerName = r.str();
      const room = r.str();
      if (r.err) return;
      window.dispatchEvent(new CustomEvent('aether:ring', { detail: { callerId, callerName, room } }));
    } else if (h.op === Op.RingAnswer) {
      const calleeId = r.str();
      const calleeName = r.str();
      const room = r.str();
      const accepted = r.bool();
      r.bits(7);
      if (r.err) return;
      window.dispatchEvent(
        new CustomEvent('aether:ring-answer', { detail: { calleeId, calleeName, room, accepted } })
      );
    } else if (h.op === Op.Typing) {
      const convId = r.str();
      const who = r.str();
      const whoId = r.str();
      const on = r.bool();
      r.bits(7);
      if (r.err) return;
      if (state.typing.set(convId, whoId, who, on)) paintTyping(convId);
      armTypingTimer();
    } else if (h.op === Op.Activity) {
      onActivityFrame(r);
    }
  };

  ws.onmessage = (ev) => {
    const r = new Reader(new Uint8Array(ev.data), state.dicts.rx);
    const h = r.header();
    if (!h) return;
    if (h.op === Op.Batch) {
      splitBatch(r, (f) => {
        const fr = new Reader(f, state.dicts.rx);
        const fh = fr.header();
        if (fh) onFrame(fh, fr);
      });
    } else {
      onFrame(h, r);
    }
  };

  ws.onclose = () => {
    clearInterval(state.ping);
    state.ws = null;
    // Reconnect with a widening delay. A server restart should bring every
    // client back without a thundering herd.
    setTimeout(openSocket, state.reconnectDelay);
    state.reconnectDelay = Math.min(state.reconnectDelay * 2, 30000);
  };
}

function previewOf(m) {
  if (m.deleted) return 'Message deleted';
  if (m.body) return m.body.slice(0, 90);
  const n = m.attachments?.length ?? 0;
  if (n === 1) return '📎 ' + m.attachments[0].name;
  if (n > 1) return `📎 ${n} files`;
  return '';
}

function onIncoming(m) {
  const conv = state.conversations.find((c) => c.id === m.conversationId);
  // Their message ends their "is typing" (the render below redraws).
  state.typing.set(m.conversationId, m.senderId, m.senderName, false);
  if (conv) {
    conv.preview = previewOf(m);
    conv.lastMessageAt = m.createdAt;
    // Unread is "not yet seen", not "not the open chat": a message into the
    // open thread while the window is behind another is unread too. It
    // used to count only for other conversations, so the catch-up on
    // focus (which looks at the count) never fired and the sender's tick
    // stayed single until somebody switched chats.
    if (m.senderId !== session.user?.id && !m.system && !isLooking(m.conversationId)) {
      conv.unread = (conv.unread ?? 0) + 1;
    }
    state.conversations.sort(
      (a, b) => new Date(b.lastMessageAt) - new Date(a.lastMessageAt)
    );
  } else {
    // A conversation we have never seen: somebody started one with us. Say
    // so -- a mention in a brand-new group is exactly the message a person
    // must not miss -- then load it.
    maybeNotify(m, null);
    refresh();
    return;
  }

  // The meeting chat panel, if one is open, shows the same threads. It is
  // not a copy of this conversation, it is this conversation.
  window.dispatchEvent(new CustomEvent('aether:dm', { detail: m }));
  maybeNotify(m, conv);

  // In a channel a reply is part of its root's thread: the feed keeps its
  // count, the open thread pane gets the message, the feed does not.
  if (state.active?.id === m.conversationId && state.active.kind === 'channel' && m.replyTo && !m.system) {
    const root = state.messages.find((x) => x.id === m.replyTo);
    const t = state.threadOpen;
    if (t && t.rootId === m.replyTo) {
      const at = t.replies.findIndex((x) => x.id === m.id);
      if (at >= 0) t.replies[at] = m;
      else t.replies.push(m);
    }
    if (root && !(t && t.replies.some((x) => x.id === m.id && at0(t, m.id)))) {
      // Count it once: a re-push of an existing reply (a reaction) must not
      // add a reply.
      if (!m.__seenReply) {
        root.thread = root.thread ?? { count: 0, lastNames: [] };
        if (!knownReply(m.id)) {
          root.thread.count++;
          root.thread.lastAt = m.createdAt;
          root.thread.lastNames = [m.senderName, ...(root.thread.lastNames ?? []).filter((n) => n !== m.senderName)].slice(0, 3);
          rememberReply(m.id);
        }
      }
    }
    if (isLooking(m.conversationId)) markRead(m.conversationId);
    renderThread();
    renderList();
    state.onUnread?.(totalUnread());
    return;
  }

  if (state.active?.id === m.conversationId) {
    const existing = state.messages.findIndex((x) => x.id === m.id);
    if (existing >= 0) {
      const old = state.messages[existing];
      // Saved is this viewer's own mark and never rides on a push; and when
      // a reaction group lists more people than the frame carries ids for,
      // "mine" is kept from what this client already knew.
      m.saved = old.saved;
      for (const g of m.reactions ?? []) {
        if (g.who.length < g.count) {
          const was = old.reactions?.find((x) => x.emoji === g.emoji);
          if (was?.mine) g.mine = true;
        }
      }
      // An anonymous poll's push names nobody; my own choice is what I
      // clicked, kept from the copy I had.
      if (m.poll && m.poll.mine == null) m.poll.mine = old.poll?.mine ?? [];
      state.messages[existing] = m;
    } else {
      placeMessage(m);
    }
    // Read means read: the cursor moves only while the thread is on screen
    // in a focused window. It used to move on arrival whenever this
    // conversation was the active one, so a person on the Meetings tab or
    // with the window behind another was "seen" without having seen.
    if (isLooking(m.conversationId)) markRead(m.conversationId);
    renderThread();
  }
  renderList();
  state.onUnread?.(totalUnread());
}

// placeMessage adds a message to the open thread, or replaces the copy
// already there. A message I send arrives twice when I have another tab or
// the app open as well: the response to my send, and -- if this tab's
// device id was not on the socket -- the push. Either order, one copy.
function placeMessage(m) {
  const at = state.messages.findIndex((x) => x.id === m.id);
  if (at >= 0) state.messages[at] = { ...state.messages[at], ...m };
  else state.messages.push(m);
}

// Reply ids this client has already counted into a root's summary, so a
// re-push of the same reply (somebody reacted to it) does not count twice.
const countedReplies = new Set();
function knownReply(id) {
  return countedReplies.has(id);
}
function rememberReply(id) {
  countedReplies.add(id);
  if (countedReplies.size > 5000) countedReplies.clear();
}
function at0() {
  return false;
}

// Whether the person is looking at this conversation right now: it is the
// active thread, the chat surface is mounted, the tab is visible and the
// window has focus.
function isLooking(convId) {
  if (typeof document === 'undefined') return false;
  return (
    state.active?.id === convId &&
    state.threadTab === 'chat' &&
    !!document.getElementById('thread') &&
    !document.hidden &&
    document.hasFocus()
  );
}

// When the thread comes back into view -- tab shown, window focused -- what
// arrived meanwhile is read now.
if (typeof document !== 'undefined') {
  const catchUp = () => {
    const c = state.active;
    if (c && (c.unread ?? 0) > 0 && isLooking(c.id)) markRead(c.id);
  };
  document.addEventListener('visibilitychange', catchUp);
  window.addEventListener('focus', catchUp);
}

// Whether the other person in a direct conversation is connected. Groups do
// not get a dot: "some of these five people are online" is not information.
//
// What a person says about themselves rides on the same dot: green for
// available, red for busy, red with a bar for do-not-disturb, amber for
// away; grey when their socket is closed whatever they last said.
export function statusOf(userId) {
  const st = state.statuses.get(userId);
  if (!st) return { status: 'available', note: '', until: null };
  if (st.until && new Date(st.until) < new Date()) return { status: 'available', note: '', until: null };
  return st;
}
export function myStatus() {
  return statusOf(session.user?.id);
}
export function isDND() {
  return myStatus().status === 'dnd';
}

// Quiet hours: a window in the day when nothing but urgent gets through,
// kept on this device (it is about where you are, not who you are).
const QUIET_KEY = 'aether.quiet';
export function quietHours() {
  try {
    return { on: false, from: '22:00', to: '08:00', ...JSON.parse(localStorage.getItem(QUIET_KEY) || '{}') };
  } catch {
    return { on: false, from: '22:00', to: '08:00' };
  }
}
export function setQuietHours(q) {
  try {
    localStorage.setItem(QUIET_KEY, JSON.stringify(q));
  } catch {}
}
export function inQuietHours(now = new Date()) {
  const q = quietHours();
  if (!q.on) return false;
  const mins = (s) => {
    const [h, m] = String(s).split(':').map(Number);
    return (h || 0) * 60 + (m || 0);
  };
  const cur = now.getHours() * 60 + now.getMinutes();
  const a = mins(q.from);
  const b = mins(q.to);
  return a <= b ? cur >= a && cur < b : cur >= a || cur < b; // overnight windows wrap
}
// The dot for the signed-in person's own avatar in the bar: what others
// see of them right now. Always "online" from their own point of view.
export function myPresenceDot() {
  const st = myStatus();
  return presenceIcon(st.status, { note: st.note });
}
export const statusLabel = presenceLabel;
// The two states the system sets and nobody picks.
export const isAutoStatus = presenceIsAuto;
// The glyph for one person: their status while they are connected, the
// hollow ring when they are not. The connection wins over what they said.
export function personPresence(userId) {
  if (userId === session.user?.id) return myPresenceDot();
  const st = statusOf(userId);
  return presenceIcon(st.status, { online: isOnline(userId), title: personLine(userId) });
}
function presenceDot(c) {
  const other = peerOf(c);
  if (!other) return '';
  return personPresence(other.userId);
}
// Statuses for people we may not have loaded yet (group members): one
// bounded query, merged into the map every glyph reads from.
async function loadStatuses(ids) {
  const want = [...new Set(ids.filter((id) => id && id !== session.user?.id))].slice(0, 200);
  if (!want.length) return;
  try {
    const res = await messaging.statuses(want);
    mergeStatuses(res.statuses);
  } catch {}
}
// isOnline: connected, and not appearing offline.
export function isOnline(userId) {
  if (userId === session.user?.id) return true;
  return state.online.has(userId) && statusOf(userId).status !== 'offline';
}
// The words under a person's name: their state, their note, when they were
// last seen. See presenceLine.
export function personLine(userId) {
  const st = statusOf(userId);
  return presenceLine(st, { online: isOnline(userId), lastSeen: state.lastSeen.get(userId) || 0 });
}
// "Busy · In a meeting until 3", "Offline · Last seen 2 h ago". The address
// is not here: the header is who they are, and their address is on the
// card a click away.
function peerStatusLine(c) {
  const other = peerOf(c);
  if (!other) return '';
  const line = personLine(other.userId);
  const [word, ...rest] = line.split(' · ');
  const status = isOnline(other.userId) ? statusOf(other.userId).status : 'offline';
  return `<span class="st-word ${esc(status)}">${esc(word)}</span>${rest.length ? ' · ' + esc(rest.join(' · ')) : ''}`;
}

function peerOnline(c) {
  if (c.kind !== 'direct') return false;
  const other = (c.members ?? []).find((m) => m.userId !== session.user?.id);
  return !!other && state.online.has(other.userId);
}

function totalUnread() {
  return state.conversations.reduce((a, c) => a + (c.muted ? 0 : c.unread ?? 0), 0);
}

// ---------------------------------------------------------------------------
// Data
// ---------------------------------------------------------------------------

// mergeStatuses takes what the server said about people -- status, whether
// they are connected, when they were last active -- from any source: the
// list, a status query, a push. Presence rides with a status so a first
// paint is right without waiting for a presence frame.
function mergeStatuses(obj) {
  for (const [id, st] of Object.entries(obj ?? {})) {
    state.statuses.set(id, { status: st.status, note: st.note || '', until: st.until || null, auto: !!st.auto });
    if (typeof st.online === 'boolean') {
      if (st.online) state.online.add(id);
      else state.online.delete(id);
    }
    if (st.lastSeen) state.lastSeen.set(id, new Date(st.lastSeen).getTime());
    else if (st.status === 'offline') state.lastSeen.delete(id); // theirs to hide
  }
}

export async function refresh() {
  try {
    const res = await messaging.list();
    state.conversations = res.conversations ?? [];
    mergeStatuses(res.statuses);
    window.dispatchEvent(new CustomEvent('aether:my-status'));
    renderList();
    state.onUnread?.(totalUnread());
  } catch {
    state.conversations = [];
    renderList();
  }
}

async function open(conv) {
  state.active = conv;
  document.querySelector('.chat-shell')?.classList.add('showing-thread');
  state.messages = [];
  state.replyTo = null;
  state.editing = null;
  state.deleteMenu = null;
  state.pending = [];
  state.threadTab = 'chat';
  state.threadSearch = null;
  state.threadOpen = null;
  closeReactPopover();
  closeReceiptPopover();
  state.draftMentions = new Map();
  state.priority = '';
  state.scheduleAt = 0;
  closeEmojiMenu();
  // Read BEFORE marking read: after that the cursor is now, and "everything
  // since you last looked" is nothing.
  const me = (conv.members ?? []).find((m) => m.userId === session.user?.id);
  state.lastReadAt = me?.lastRead ? new Date(me.lastRead).getTime() : 0;
  renderList();
  renderThread();
  try {
    let res;
    try {
      res = await messaging.messages(conv.id);
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        state.active = null;
        await refresh();
        renderThread();
        toastErr(new Error('You are no longer in that conversation.'));
        return;
      }
      throw e;
    }
    state.messages = res.messages ?? [];
    // The call log for this pair rides alongside the messages, like a phone.
    state.calls = [];
    if (conv.kind === 'direct') {
      try {
        const c = await messaging.calls(conv.id);
        state.calls = c.calls ?? [];
      } catch {
        state.calls = [];
      }
    }
    renderThread();
    markRead(conv.id);
  } catch (e) {
    toastErr(e);
  }
}

async function markRead(convId) {
  const conv = state.conversations.find((c) => c.id === convId);
  if (conv) conv.unread = 0;
  renderList();
  state.onUnread?.(totalUnread());
  try {
    await messaging.markRead(convId);
  } catch {
    // A read receipt that fails is not worth interrupting anybody over.
  }
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

export function renderChatSurface() {
  state.surface = 'chat';
  const main = document.querySelector('.home-main');
  main.innerHTML = `
    <div class="chat-shell${state.active ? ' showing-thread' : ''}">
      <aside class="conv-col">
        <div class="conv-head">
          <h1>Chat</h1>
          <div class="new-menu">
            <button class="icon-btn" id="c-new" title="New group or channel" aria-haspopup="menu" aria-expanded="false">
              <svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-pencil"/></svg>
            </button>
            <div class="new-drop" role="menu" hidden>
              <button role="menuitem" data-new="group">
                <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-people"/></svg> New group
              </button>
              <button role="menuitem" data-new="channel">
                <span class="hash">#</span> New channel
              </button>
              <button role="menuitem" data-new="browse">
                <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-search"/></svg> Browse channels
              </button>
            </div>
          </div>
        </div>
        <input class="input" id="c-search" placeholder="Filter your chats" autocomplete="off" />
        <div class="pills" role="tablist" aria-label="Show">
          <button class="pill-f" data-filter="all" role="tab">All</button>
          <button class="pill-f" data-filter="unread" role="tab">Unread <span class="pill-n" id="pill-unread"></span></button>
          <button class="pill-f" data-filter="meetings" role="tab">Meeting chats</button>
          <button class="pill-f${['chats', 'channels', 'groups', 'favourites', 'muted'].includes(state.listFilter) ? ' active' : ''}" id="c-more-filters" title="More filters" aria-haspopup="menu">
            ${
              { chats: 'Chats', channels: 'Channels', groups: 'Groups', favourites: 'Favourites', muted: 'Muted' }[state.listFilter] || 'More'
            } <svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-chevron"/></svg>
          </button>
          <button class="pill-f density" id="c-density" title="Message spacing">
            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 7h16M4 12h16M4 17h16"/></svg>
          </button>
        </div>
        <div class="conv-list" id="conv-list"></div>
      </aside>
      <section class="thread" id="thread"></section>
    </div>`;

  // A filter over the chats you already have, nothing more. Finding somebody
  // new is the title-bar search's job, and one field that did both would be
  // two half-features sharing a box.
  $('c-search').oninput = renderList;

  // The pencil menu: opens on click, closes on a choice, on any click
  // outside, and on Escape.
  const newBtn = $('c-new');
  const drop = main.querySelector('.new-drop');
  const showMenu = (yes) => {
    drop.hidden = !yes;
    newBtn.setAttribute('aria-expanded', String(yes));
  };
  newBtn.onclick = (e) => {
    e.stopPropagation();
    showMenu(drop.hidden);
  };
  drop.querySelectorAll('[data-new]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      showMenu(false);
      if (b.dataset.new === 'group') newGroupDialog();
      else if (b.dataset.new === 'channel') newChannelDialog();
      else browseChannelsDialog();
    };
  });
  const onDoc = (e) => {
    if (!drop.isConnected) {
      document.removeEventListener('click', onDoc);
      return;
    }
    if (!drop.hidden && !e.target.closest('.new-menu')) showMenu(false);
  };
  document.addEventListener('click', onDoc);

  main.querySelectorAll('[data-filter]').forEach((b) => {
    b.onclick = () => {
      state.listFilter = b.dataset.filter;
      renderChatSurfaceFilters();
      renderList();
    };
  });
  const moreF = $('c-more-filters');
  if (moreF) moreF.onclick = (e) => {
    e.stopPropagation();
    moreFiltersMenu(moreF);
  };
  // Compact or comfortable spacing, remembered.
  $('c-density').onclick = () => {
    const on = document.body.classList.toggle('compact-chat');
    try {
      localStorage.setItem('aether.compact', on ? '1' : '0');
    } catch {}
    toastOk(on ? 'Compact spacing' : 'Comfortable spacing');
  };

  renderList();
  renderThread();
  refresh();
}

function convAvatar(c, extra = '') {
  if (c.avatar) {
    return `<span class="avatar pic${extra ? ' ' + extra : ''}"><img src="${messaging.avatarURL(c.id)}?v=${encodeURIComponent(c.lastMessageAt || '')}" alt="" /></span>`;
  }
  if (c.kind === 'channel') {
    return `<span class="avatar chan${extra ? ' ' + extra : ''}" data-tone="${esc(String((c.title || '').length % 8))}">#</span>`;
  }
  if (c.meetingRoom) {
    return `<span class="avatar meet${extra ? ' ' + extra : ''}"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-cam"/></svg></span>`;
  }
  return `<span ${avatarAttrs(whoOf(c), extra)}>${esc(initials(whoOf(c)))}</span>`;
}

function passesFilter(c) {
  switch (state.listFilter) {
    case 'unread':
      return (c.unread ?? 0) > 0;
    case 'chats':
      return c.kind !== 'channel';
    case 'channels':
      return c.kind === 'channel';
    case 'meetings':
      return !!c.meetingRoom;
    case 'groups':
      return c.kind === 'group' && !c.meetingRoom;
    case 'favourites':
      return !!c.favourite;
    case 'muted':
      return !!c.muted;
    default:
      return true;
  }
}

const SECTIONS_KEY = 'aether.sections';
function loadCollapsed() {
  try {
    return new Set(JSON.parse(localStorage.getItem(SECTIONS_KEY) || '[]'));
  } catch {
    return new Set();
  }
}
function toggleSection(name) {
  if (state.collapsed.has(name)) state.collapsed.delete(name);
  else state.collapsed.add(name);
  try {
    localStorage.setItem(SECTIONS_KEY, JSON.stringify([...state.collapsed]));
  } catch {}
  renderList();
}

// The "more filters" menu under the chevron pill: the filters that are used
// less than Unread and Meeting chats, one click further away.
function moreFiltersMenu(anchor) {
  const el = document.createElement('div');
  el.className = 'ctx-menu';
  el.setAttribute('role', 'menu');
  const item = (key, label) =>
    `<button role="menuitem" data-filter-pick="${key}" class="${state.listFilter === key ? 'on' : ''}">${label}</button>`;
  el.innerHTML =
    item('chats', 'Chats') + item('groups', 'Groups') + item('channels', 'Channels') +
    '<div class="ctx-sep"></div>' + item('favourites', 'Favourites') + item('muted', 'Muted');
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  el.style.left = `${Math.min(r.left, innerWidth - (el.offsetWidth || 200) - 8)}px`;
  el.style.top = `${r.bottom + 4}px`;
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    el.remove();
  };
  const onDoc = (e) => {
    if (!el.contains(e.target)) done();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') done();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);
  el.querySelectorAll('[data-filter-pick]').forEach((b) => {
    b.onclick = () => {
      done();
      state.listFilter = b.dataset.filterPick;
      renderChatSurfaceFilters();
    };
  });
}

// The pill row is part of the surface markup; a filter chosen from the menu
// changes the chevron pill's label, so the surface is re-rendered.
function renderChatSurfaceFilters() {
  const pills = document.querySelector('.pills');
  if (!pills) return renderList();
  const more = document.getElementById('c-more-filters');
  const extra = ['chats', 'channels', 'groups', 'favourites', 'muted'].includes(state.listFilter);
  if (more) {
    more.classList.toggle('active', extra);
    more.firstChild.textContent =
      ({ chats: 'Chats', channels: 'Channels', groups: 'Groups', favourites: 'Favourites', muted: 'Muted' }[state.listFilter] || 'More') + ' ';
  }
  renderList();
}

function renderList() {
  if (state.surface === 'teams') return renderTeamsList();
  if (state.surface === 'activity') return renderActivityList();
  const box = $('conv-list');
  if (!box) return;
  const q = ($('c-search')?.value ?? '').toLowerCase();
  const unreadConvs = state.conversations.filter((c) => (c.unread ?? 0) > 0).length;
  const pillN = $('pill-unread');
  if (pillN) pillN.textContent = unreadConvs ? String(unreadConvs) : '';
  document.querySelectorAll('[data-filter]').forEach((b) =>
    b.classList.toggle('active', b.dataset.filter === state.listFilter)
  );
  const list = state.conversations.filter(
    (c) =>
      passesFilter(c) &&
      (!q || (c.display ?? '').toLowerCase().includes(q) || (c.preview ?? '').toLowerCase().includes(q))
  );

  if (!list.length) {
    const empties = {
      unread: 'Nothing unread. You are caught up.',
      chats: 'No chats yet. Search for someone in the bar at the top to start one.',
      channels: 'No channels yet. Use the pencil to create one, or browse the ones that exist.',
      meetings: 'No meeting chats yet. A meeting with three or more people gets one automatically.',
      groups: 'No groups yet. Use the pencil to create one.',
      favourites: 'No favourites yet. Use ⋯ on a chat to add one.',
      muted: 'Nothing is muted.',
      all: 'No chats yet. Search for someone in the bar at the top to start one.',
    };
    box.innerHTML = `<div class="empty">${q ? 'Nothing matches.' : empties[state.listFilter] || empties.all}</div>`;
    return;
  }

  const row = (c) => `<div class="conv-row">
    <button class="conv ${state.active?.id === c.id ? 'active' : ''}" data-c="${esc(c.id)}">
      <span class="av-wrap">
        ${convAvatar(c)}
        ${presenceDot(c)}
      </span>
      <span class="conv-main">
        <span class="conv-top">
          <span class="conv-name">${esc(c.display)}${
            c.muted ? '<svg class="muted-mark" width="12" height="12" viewBox="0 0 24 24"><use href="#i-mute"/></svg>' : ''
          }</span>
          <span class="conv-when">${esc(when(c.lastMessageAt))}</span>
        </span>
        <span class="conv-preview">${
          state.typing.line(c.id) && state.active?.id !== c.id
            ? `<i class="typing-preview">${esc(state.typing.line(c.id))}…</i>`
            : draftFor(c.id) && state.active?.id !== c.id
              ? `<i class="draft">Draft: ${esc(draftFor(c.id).slice(0, 80))}</i>`
              : esc(plainText(c.preview) || 'No messages yet')
        }</span>
      </span>
      ${c.mentions ? '<span class="mention-badge" title="You were mentioned">@</span>' : ''}
      ${c.unread ? `<span class="unread${c.muted ? ' quiet' : ''}">${c.unread}</span>` : ''}
    </button>
    <button class="conv-more" data-more="${esc(c.id)}" title="More" aria-label="More options for ${esc(c.display)}">⋯</button>
  </div>`;
  const favs = list.filter((c) => c.favourite);
  const rest = list.filter((c) => !c.favourite);
  // Sections fold. A folded section keeps its unread total in the header, so
  // folding "Chats" never hides that something arrived.
  const section = (key, label, rows) => {
    const folded = state.collapsed.has(key);
    const unread = rows.reduce((n, c) => n + (c.muted ? 0 : c.unread ?? 0), 0);
    return `<button class="conv-section${folded ? ' folded' : ''}" data-section="${key}" aria-expanded="${!folded}">
        <svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-chevron"/></svg>${label}
        ${folded && unread ? `<span class="unread sm">${unread}</span>` : ''}
      </button>${folded ? '' : rows.map(row).join('')}`;
  };
  const restLabel = { channels: 'Channels', meetings: 'Meeting chats', groups: 'Groups', muted: 'Muted' }[state.listFilter] || 'Chats';
  box.innerHTML = favs.length
    ? section('favourites', 'Favourites', favs) + (rest.length ? section('chats', restLabel, rest) : '')
    : rest.map(row).join('');
  box.querySelectorAll('[data-section]').forEach((b) => {
    b.onclick = () => toggleSection(b.dataset.section);
  });

  box.querySelectorAll('[data-c]').forEach((b) => {
    b.onclick = () => {
      const c = state.conversations.find((x) => x.id === b.dataset.c);
      if (c) open(c);
    };
    b.oncontextmenu = (e) => {
      e.preventDefault();
      const c = state.conversations.find((x) => x.id === b.dataset.c);
      if (c) convMenu(c, e.clientX, e.clientY);
    };
  });
  box.querySelectorAll('[data-more]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      const c = state.conversations.find((x) => x.id === b.dataset.more);
      const r = b.getBoundingClientRect();
      if (c) convMenu(c, r.right, r.bottom);
    };
  });
}

// The row menu: the decisions one person makes about one chat. Positioned
// at the pointer or under the "more" button, one at a time, gone on a
// choice, a click elsewhere or Escape.
let convMenuEl = null;
function closeConvMenu() {
  if (convMenuEl) {
    convMenuEl.remove();
    convMenuEl = null;
  }
  convMenuFor = '';
}
let convMenuFor = '';
function convMenu(c, x, y) {
  // The same button again closes what it opened.
  if (convMenuEl && convMenuFor === c.id) {
    closeConvMenu();
    convMenuFor = '';
    return;
  }
  closeConvMenu();
  convMenuFor = c.id;
  const el = document.createElement('div');
  el.className = 'ctx-menu';
  el.setAttribute('role', 'menu');
  const item = (key, label, icon, danger = false) =>
    `<button role="menuitem" data-act="${key}" class="${danger ? 'danger' : ''}">
      <svg width="15" height="15" viewBox="0 0 24 24"><use href="#${icon}"/></svg>${label}</button>`;
  el.innerHTML =
    item('unread', c.unread ? 'Mark as read' : 'Mark as unread', c.unread ? 'i-check' : 'i-unread') +
    item('fav', c.favourite ? 'Remove from favourites' : 'Favourite', 'i-heart') +
    item('mute', c.muted ? 'Unmute' : 'Mute', 'i-mute') +
    item('hide', 'Hide', 'i-hide') +
    item('window', 'Open in new window', 'i-forward') +
    (state.active?.id === c.id
      ? '<div class="ctx-sep"></div>' +
        item('find', 'Find in this chat', 'i-search') +
        (c.kind === 'direct' && peerOf(c) ? item('addpeople', 'Add people', 'i-person-add') + item('audio', 'Audio call', 'i-mic') : '')
      : '') +
    (c.kind === 'direct' && peerOf(c)
      ? '<div class="ctx-sep"></div>' + item('block', c.blocked ? 'Unblock' : 'Block', 'i-block', !c.blocked)
      : '') +
    '<div class="ctx-sep"></div>' +
    item('forget', 'Delete for me', 'i-trash', true) +
    (c.kind === 'channel' && c.myRole === 'owner' ? item('delchan', 'Delete channel for everyone', 'i-trash', true) : '');
  document.body.appendChild(el);
  const w = el.offsetWidth || 220;
  const h = el.offsetHeight || 240;
  el.style.left = `${Math.min(Math.max(8, x), innerWidth - w - 8)}px`;
  el.style.top = `${y + h > innerHeight - 8 ? Math.max(8, y - h) : y}px`;
  convMenuEl = el;

  const onDoc = (e) => {
    if (!el.contains(e.target)) done();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') done();
  };
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    closeConvMenu();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);

  el.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      const act = b.dataset.act;
      if (act === 'forget' && b.dataset.armed !== '1') {
        // Two clicks for the one thing that cannot be undone from here.
        b.dataset.armed = '1';
        b.innerHTML = `<svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-trash"/></svg>Delete this chat for you? Click again`;
        return;
      }
      if (act === 'delchan' && b.dataset.armed !== '1') {
        b.dataset.armed = '1';
        b.innerHTML = `<svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-trash"/></svg>Delete ${esc(c.display)} and everything in it, for everyone? Click again`;
        return;
      }
      if (act === 'block' && !c.blocked && b.dataset.armed !== '1') {
        b.dataset.armed = '1';
        b.innerHTML = `<svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-block"/></svg>Block ${esc(c.display)}? They won't be able to message or call you. Click again`;
        return;
      }
      done();
      try {
        if (act === 'unread') {
          if (c.unread) {
            await messaging.markRead(c.id);
            c.unread = 0;
          } else {
            await messaging.markUnread(c.id);
            c.unread = Math.max(1, c.unread ?? 0);
          }
        } else if (act === 'fav') {
          await messaging.setPrefs(c.id, { favourite: !c.favourite });
          c.favourite = !c.favourite;
        } else if (act === 'mute') {
          await messaging.setPrefs(c.id, { muted: !c.muted });
          c.muted = !c.muted;
        } else if (act === 'find') {
          state.threadSearch = { q: '', results: [], loading: false };
          renderThread();
          setTimeout(() => $('ts-input')?.focus(), 30);
        } else if (act === 'addpeople') {
          newGroupDialog({ preset: [peerPerson(c)] });
        } else if (act === 'audio') {
          document.querySelector('[data-call][data-call-audio="1"]')?.click();
        } else if (act === 'block') {
          const peer = peerOf(c);
          if (c.blocked) {
            await messaging.unblock(peer.userId);
            c.blocked = false;
            toastOk(`Unblocked ${c.display}.`);
          } else {
            await messaging.block(peer.userId);
            c.blocked = true;
            toastOk(`Blocked ${c.display}. You can unblock from ⋯ any time.`);
          }
          if (state.active?.id === c.id) renderThread();
        } else if (act === 'window') {
          window.open(`${location.origin}/?chat=${encodeURIComponent(c.id)}`, '_blank', 'width=960,height=720');
        } else if (act === 'hide') {
          await messaging.setPrefs(c.id, { hidden: true });
          state.conversations = state.conversations.filter((x) => x.id !== c.id);
          if (state.active?.id === c.id) state.active = null;
          renderThread();
          toastOk('Hidden. It comes back when somebody writes.');
        } else if (act === 'delchan') {
          await messaging.deleteChannel(c.id);
          state.conversations = state.conversations.filter((x) => x.id !== c.id);
          if (state.active?.id === c.id) state.active = null;
          renderThread();
          toastOk(`Deleted ${c.display}.`);
        } else if (act === 'forget') {
          await messaging.forget(c.id);
          state.conversations = state.conversations.filter((x) => x.id !== c.id);
          if (state.active?.id === c.id) state.active = null;
          renderThread();
          toastOk('Deleted for you.');
        }
        renderList();
        state.onUnread?.(totalUnread());
      } catch (err) {
        toastErr(err);
        refresh();
      }
    };
  });
}

// A call, rendered as an event in the thread -- the way WhatsApp does it.
// Direction, kind, outcome and length, in one line, in the timeline where it
// happened. Click to call back.
function callRow(c) {
  const mine = c.callerId === session.user?.id;
  const kindIcon = c.kind === 'audio' ? '#i-mic' : '#i-cam';
  const kindWord = c.kind === 'audio' ? 'Audio call' : 'Video call';
  const dur = (s) => (s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`);
  let outcome;
  if (c.answered && c.seconds > 0) outcome = dur(c.seconds);
  else if (c.answered) outcome = 'In progress';
  else outcome = mine ? 'No answer' : 'Missed';
  const missed = !c.answered;
  const other = state.active?.members?.find((m) => m.userId !== session.user?.id);
  return `<div class="call-row ${mine ? 'mine' : ''} ${missed ? 'missed' : ''}">
    <span class="call-ic"><svg width="15" height="15" viewBox="0 0 24 24"><use href="${kindIcon}"/></svg></span>
    <span class="call-text">
      <b>${mine ? 'Outgoing' : 'Incoming'} ${kindWord.toLowerCase()}</b>
      <small>${esc(when(c.startedAt))} · ${esc(outcome)}</small>
    </span>
    ${
      other
        ? `<button class="btn btn-quiet sm" data-call="${esc(other.userId)}" data-call-name="${esc(state.active.display)}"
             data-call-audio="${c.kind === 'audio' ? '1' : '0'}" title="Call back">Call back</button>`
        : ''
    }
  </div>`;
}

function renderThread() {
  const box = $('thread');
  if (!box) return;

  if (!state.active) {
    box.innerHTML = `<div class="thread-empty">
      <span class="avatar lg">💬</span>
      <p>${state.surface === 'activity' ? 'Choose an item to read it where it happened.' : 'Pick a conversation, or start a new one.'}</p>
    </div>`;
    return;
  }

  const typingLine = state.typing.line(state.active.id);

  box.innerHTML = `
    <header class="thread-head">
      <button class="icon-btn thread-back" id="c-back" title="Back to chats" aria-label="Back to chats">
        <svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-back"/></svg>
      </button>
      ${
        peerOf(state.active)
          ? `<button class="who-btn av-wrap" data-person="${esc(peerOf(state.active).userId)}" data-person-name="${esc(state.active.display)}" title="About ${esc(state.active.display)}" aria-label="About ${esc(state.active.display)}">${convAvatar(state.active)}${presenceDot(state.active)}</button>`
          : `<span class="av-wrap">${convAvatar(state.active)}${presenceDot(state.active)}</span>`
      }
      <div>
        <h2>${
          peerOf(state.active)
            ? `<button class="who-link" data-person="${esc(peerOf(state.active).userId)}" data-person-name="${esc(state.active.display)}">${esc(state.active.display)}</button>`
            : esc(state.active.teamId ? whoOf(state.active).name : state.active.display)
        }</h2>
        <span>${
          state.active.kind === 'group' || state.active.kind === 'channel'
            ? `${state.active.meetingRoom ? 'Meeting chat · ' : ''}${state.active.memberCount || state.active.members?.length || 0} members${
                state.active.topic ? ' · ' + esc(state.active.topic) : ''
              }${state.active.kind === 'channel' && state.active.visibility === 'private' ? ' · Private' : ''}`
            : peerStatusLine(state.active)
        }</span>
      </div>
      <nav class="thread-tabs" role="tablist" aria-label="Conversation views">
        <button class="ttab${state.threadTab === 'chat' ? ' active' : ''}" role="tab" data-ttab="chat"
                aria-selected="${state.threadTab === 'chat'}">Chat</button>
        <button class="ttab${state.threadTab === 'shared' ? ' active' : ''}" role="tab" data-ttab="shared"
                aria-selected="${state.threadTab === 'shared'}">Shared</button>
        <button class="ttab${state.threadTab === 'photos' ? ' active' : ''}" role="tab" data-ttab="photos"
                aria-selected="${state.threadTab === 'photos'}">Photos</button>
      </nav>
      <div class="spacer"></div>
      ${
        state.active.kind !== 'direct'
          ? `<button class="icon-btn" id="c-people" title="Members" aria-label="Members">
               <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-people"/></svg>
             </button>`
          : peerOf(state.active)
            ? `<button class="icon-btn" id="c-add-people" title="Add people (starts a group with ${esc(state.active.display)})" aria-label="Add people">
               <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-person-add"/></svg>
             </button>`
            : ''
      }
      <button class="icon-btn${state.threadSearch ? ' on' : ''}" id="c-find-here" title="Find in this chat (Ctrl+F)" aria-label="Find in this chat" aria-pressed="${!!state.threadSearch}">
        <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-search"/></svg>
      </button>
      <button class="icon-btn" id="c-thread-more" title="More" aria-label="More options" aria-haspopup="menu">
        <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-more"/></svg>
      </button>
      ${
        peerOf(state.active)
          ? `<div class="call-menu">
               <button class="btn btn-primary sm call-main" data-call="${esc(peerOf(state.active).userId)}"
                       data-call-name="${esc(state.active.display)}" data-call-audio="0" title="Video call">
                 <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-cam"/></svg>
               </button>
               <button class="btn btn-primary sm call-caret" data-call-toggle title="More ways to call" aria-haspopup="menu">▾</button>
               <div class="call-drop" role="menu">
                 <button role="menuitem" data-call="${esc(peerOf(state.active).userId)}" data-call-name="${esc(state.active.display)}" data-call-audio="0">
                   <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-cam"/></svg> Video call
                 </button>
                 <button role="menuitem" data-call="${esc(peerOf(state.active).userId)}" data-call-name="${esc(state.active.display)}" data-call-audio="1">
                   <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-mic"/></svg> Audio call
                 </button>
               </div>
             </div>`
          : `<button class="btn" id="c-call" title="Start a meeting" aria-label="Start a meeting"><svg class="c-call-icon" width="16" height="16" viewBox="0 0 24 24" aria-hidden="true"><use href="#i-cam"/></svg><span class="c-call-label">Start a meeting</span></button>`
      }
    </header>
    ${
      state.active.pinned
        ? `<div class="pinned-bar">
             <span class="pin-ico">📌</span>
             <button class="pin-body" data-goto="${esc(state.active.pinnedMessageId)}" title="Jump to the pinned message">
               <b>${esc(state.active.pinned.senderName)}</b>
               <span>${esc(state.active.pinned.deleted ? 'Message deleted' : state.active.pinned.body)}</span>
             </button>
             <button class="icon-btn sm" data-unpin title="Unpin" aria-label="Unpin">
               <svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-close"/></svg>
             </button>
           </div>`
        : ''
    }
    <div class="thread-log" id="thread-log">${(() => {
      // Messages and calls, one timeline. A call is an event in the
      // conversation, so it sits where it happened.
      // Messages grouped into runs by sender, with calls interleaved by time
      // and a rule where the reader last left off.
      const msgs = [...state.messages].sort(
        (a, b) => new Date(a.createdAt) - new Date(b.createdAt)
      );
      const items = [];
      // The "New" divider goes before the first message from somebody else
      // since you last looked, and it breaks the run: what follows it is
      // attributed again, because it is what you came back to read.
      let dividerAt = null;
      if (state.lastReadAt) {
        dividerAt = msgs.find(
          (m) => new Date(m.createdAt).getTime() > state.lastReadAt &&
            m.senderId !== session.user?.id
        ) ?? null;
      }
      const grouped = groupRuns(msgs, (m) => m === dividerAt || m.system);
      msgs.forEach((m, i) => {
        if (m.system && i + 1 < grouped.length) grouped[i + 1] = false;
      });
      msgs.forEach((m, i) => {
        const t = new Date(m.createdAt).getTime();
        if (m === dividerAt) {
          items.push({ t, html: '<div class="last-read"><span>New</span></div>' });
        }
        items.push({ t, html: bubble(m, grouped[i]) });
      });
      for (const c of state.calls ?? []) {
        items.push({ t: new Date(c.startedAt).getTime(), html: callRow(c) });
      }
      items.sort((a, b) => a.t - b.t);
      return items.length
        ? items.map((i) => i.html).join('')
        : '<div class="empty">No messages yet. Say something.</div>';
    })()}</div>
    <div class="typing${typingLine ? '' : ' hidden'}" aria-live="polite">${typingMarkup(typingLine)}</div>
    ${
      state.editing
        ? `<div class="replying editing">
             <span class="replying-bar"></span>
             <span class="replying-text">
               <b>Editing message</b>
               <small>${esc((state.editing.body || '').slice(0, 90))}</small>
             </span>
             <button class="icon-btn sm" data-edit-cancel title="Cancel editing" aria-label="Cancel editing">
               <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg>
             </button>
           </div>`
        : ''
    }
    ${
      state.replyTo
        ? `<div class="replying">
             <span class="replying-bar"></span>
             <span class="replying-text">
               <b>Replying to ${esc(state.replyTo.senderName)}</b>
               <small>${esc((state.replyTo.body || '').slice(0, 90))}</small>
             </span>
             <button class="replying-x" data-reply-cancel title="Cancel reply">
               <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg>
             </button>
           </div>`
        : ''
    }
    ${pendingStrip()}
    ${suggestionsStrip()}
    <div class="composer">
      <div class="mention-host" id="c-mentions" hidden></div>
      ${formatBar()}
      <button type="button" class="icon-btn fmt-toggle${fmtPinned() ? ' on' : ''}" id="c-format" title="Formatting (Ctrl+Shift+X)" aria-label="Formatting" aria-pressed="${fmtPinned()}">
        <svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-format"/></svg>
      </button>
      <input id="c-input" class="input" placeholder="${
        state.active.kind === 'direct' ? 'Write a message…' : 'Write a message… (@ to mention)'
      }" autocomplete="off" />
      <button class="emoji-btn" id="c-emoji" title="Emoji" aria-expanded="false">🙂</button>
      <div class="emoji-panel" id="c-emoji-panel" hidden></div>
      <button class="icon-btn" id="c-attach" title="Attach files" aria-label="Attach files">
        <svg width="17" height="17" viewBox="0 0 24 24"><use href="#i-clip"/></svg>
      </button>
      <input type="file" id="c-files" multiple hidden />
      <button class="send" id="c-send" title="Send">
        <svg width="19" height="19" viewBox="0 0 24 24"><use href="#i-send"/></svg>
      </button>
    </div>`;

  const log = $('thread-log');
  // To the newest message, twice: once now, and once after layout has
  // settled. The first is what you see on a fast machine; the second is what
  // stops the last bubble sitting just under the fold when fonts, reaction
  // rows or the chat shell's own height land a frame later -- measured at 56px
  // short before this.
  const toBottom = () => {
    log.scrollTop = log.scrollHeight;
  };
  toBottom();
  requestAnimationFrame(toBottom);
  setTimeout(toBottom, 60);

  const input = $('c-input');
  if (state.active.kind !== 'direct') attachMentionPicker(input, $('c-mentions'));
  wireComposerExtras(input);
  // Rebuilt with the thread on every render, so the picker is attached to the
  // element that exists now rather than the one that was here a message ago.
  attachPicker($('c-emoji'), $('c-emoji-panel'), input);
  const doSend = async () => {
    if (typeof Notification !== 'undefined' && Notification.permission === 'default') {
      Notification.requestPermission().catch(() => {});
    }
    const body = input.value.trim();
    if (state.pending.length && !state.editing) {
      const files = state.pending;
      state.pending = [];
      input.value = '';
      const replyTo = state.replyTo?.id ?? '';
      state.replyTo = null;
      renderThread();
      try {
        const m = await messaging.upload(state.active.id, files, body, replyTo, mentionsIn(body), {
          priority: state.priority, scheduleAt: state.scheduleAt,
        });
        state.draftMentions = new Map();
        state.priority = '';
        state.scheduleAt = 0;
        clearDraft(state.active.id);
        offerUndo(m);
        placeMessage(m);
        const conv = state.conversations.find((c) => c.id === state.active.id);
        if (conv) {
          conv.preview = previewOf(m);
          conv.lastMessageAt = m.createdAt;
          state.conversations.sort((a, b) => new Date(b.lastMessageAt) - new Date(a.lastMessageAt));
        }
        renderThread();
        renderList();
      } catch (e) {
        // Put them back: a failed upload is not a reason to pick them again.
        state.pending = files;
        input.value = body;
        renderThread();
        toastErr(e);
      }
      return;
    }
    if (!body) return;
    input.value = '';
    if (state.editing) {
      const target = state.editing;
      state.editing = null;
      try {
        const m = await messaging.edit(target.id, body);
        const at = state.messages.findIndex((x) => x.id === target.id);
        if (at >= 0) {
          state.messages[at] = m?.id ? m : { ...target, body, editedAt: new Date().toISOString() };
        }
        renderThread();
      } catch (e) {
        toastErr(e);
        renderThread();
      }
      return;
    }
    try {
      const mentions = mentionsIn(body);
      const opts = { priority: state.priority, scheduleAt: state.scheduleAt };
      const m = mentions.length || opts.priority || opts.scheduleAt
        ? await messaging.sendFull(state.active.id, body, state.replyTo?.id ?? '', mentions, opts)
        : await messaging.send(state.active.id, body, state.replyTo?.id ?? '');
      state.draftMentions = new Map();
      state.priority = '';
      state.scheduleAt = 0;
      clearDraft(state.active.id);
      offerUndo(m);
      state.replyTo = null;
      typingSender.sent();
      placeMessage(m);
      const conv = state.conversations.find((c) => c.id === state.active.id);
      if (conv) {
        conv.preview = body.slice(0, 90);
        conv.lastMessageAt = m.createdAt;
        state.conversations.sort(
          (a, b) => new Date(b.lastMessageAt) - new Date(a.lastMessageAt)
        );
      }
      renderThread();
      renderList();
    } catch (e) {
      toastErr(e);
    }
  };
  $('c-send').onclick = doSend;

  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') doSend();
  });
  // Typing, told once at the first character and then at most every few
  // seconds while the text keeps changing; stopped when the box empties,
  // the message goes, or the typist pauses. See typing.js for the rules
  // and the reasoning; the wire cost is in ARCHITECTURE.md.
  bindTypingSender(state.active.id);
  input.addEventListener('input', () => typingSender.changed(input.value));
  input.focus();

  // Files: the paperclip opens the picker; anything dropped on the thread
  // is taken too. Both add to the pending strip; Send does the upload.
  const picker = $('c-files');
  $('c-attach').onclick = () => picker.click();
  picker.onchange = () => {
    addPending([...picker.files]);
    picker.value = '';
  };
  const threadEl = $('thread');
  threadEl.ondragover = (e) => {
    if ([...e.dataTransfer.types].includes('Files')) {
      e.preventDefault();
      threadEl.classList.add('dropping');
    }
  };
  threadEl.ondragleave = () => threadEl.classList.remove('dropping');
  threadEl.ondrop = (e) => {
    threadEl.classList.remove('dropping');
    if (!e.dataTransfer.files?.length) return;
    e.preventDefault();
    addPending([...e.dataTransfer.files]);
  };
  box.querySelectorAll('[data-unpend]').forEach((b) => {
    b.onclick = () => {
      state.pending.splice(Number(b.dataset.unpend), 1);
      renderThread();
      $('c-input')?.focus();
    };
  });
  box.querySelectorAll('[data-ttab]').forEach((b) => {
    b.onclick = () => {
      if (state.threadTab === b.dataset.ttab) return;
      state.threadTab = b.dataset.ttab;
      renderThread();
    };
  });
  // Channel thread pane: beside the log on a wide screen, over it on a phone.
  box.classList.toggle('with-thread', !!state.threadOpen);
  if (state.threadOpen) {
    const pane = document.createElement('div');
    pane.innerHTML = threadPaneHTML();
    box.appendChild(pane.firstElementChild);
    $('tp-close').onclick = closeThread;
    $('tp-send').onclick = sendThreadReply;
    const tpi = $('tp-input');
    tpi.onkeydown = (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        sendThreadReply();
      }
      if (e.key === 'Escape') closeThread();
    };
    const log = $('tp-log');
    if (log) log.scrollTop = log.scrollHeight;
  }
  box.querySelectorAll('[data-thread]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      if (state.threadOpen?.rootId === b.dataset.thread) closeThread();
      else openThread(b.dataset.thread);
    };
  });
  const back = $('c-back');
  if (back)
    back.onclick = () => {
      // One column on a phone: the list comes back, the thread waits.
      document.querySelector('.chat-shell')?.classList.remove('showing-thread');
      closeReactPopover();
    };
  const peopleBtn = $('c-people');
  if (peopleBtn) peopleBtn.onclick = () => membersDialog(state.active);
  const addBtn = $('c-add-people');
  if (addBtn) addBtn.onclick = () => newGroupDialog({ preset: [peerPerson(state.active)] });
  const moreBtn = $('c-thread-more');
  if (moreBtn)
    moreBtn.onclick = (e) => {
      e.stopPropagation();
      const r = moreBtn.getBoundingClientRect();
      convMenu(state.active, r.right, r.bottom + 4);
    };
  const findBtn = $('c-find-here');
  if (findBtn)
    findBtn.onclick = () => {
      state.threadSearch = state.threadSearch ? null : { q: '', results: [], loading: false };
      renderThread();
      if (state.threadSearch) setTimeout(() => $('ts-input')?.focus(), 30);
    };
  if (state.threadSearch) {
    const pane = document.createElement('div');
    pane.className = 'thread-search';
    pane.id = 'thread-search';
    box.querySelector('.pinned-bar, .thread-head')?.after(pane);
    paintThreadSearch(pane);
  }
  if (state.active.blocked) {
    const composer = box.querySelector('.composer');
    if (composer) {
      composer.innerHTML = `<div class="blocked-bar">
        <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-block"/></svg>
        <span>You blocked ${esc(state.active.display)}. They can't message or call you, and you can't message them.</span>
        <button class="btn sm" id="c-unblock">Unblock</button>
      </div>`;
      composer.classList.add('is-blocked');
      $('c-unblock').onclick = async () => {
        try {
          await messaging.unblock(peerOf(state.active).userId);
          state.active.blocked = false;
          renderList();
          renderThread();
          toastOk(`Unblocked ${state.active.display}.`);
        } catch (e) {
          toastErr(e);
        }
      };
    }
  }
  box.classList.toggle('showing-shared', state.threadTab !== 'chat');
  if (state.threadTab === 'shared' || state.threadTab === 'photos') {
    const pane = document.createElement('div');
    pane.className = 'shared-pane';
    pane.id = 'shared-pane';
    box.querySelector('.thread-head')?.after(pane);
    if (state.threadTab === 'shared') paintShared(pane, state.active);
    else paintPhotos(pane, state.active);
  }

  // Groups only: a direct thread has the call menu in its header instead,
  // and this button is not rendered for it.
  const meet = $('c-call');
  if (meet) {
    meet.onclick = () => {
      const room = 'chat-' + state.active.id.slice(0, 8).toLowerCase();
      window.dispatchEvent(
        new CustomEvent('aether:join', { detail: { room, title: state.active.display } })
      );
    };
  }
}

// The six reactions on the hover bar. The full picker is one click further,
// because a bar of thirty emoji is a menu, not a gesture.
const DEFAULT_QUICK = ['\u{1F44D}', '\u2764\uFE0F', '\u{1F602}', '\u{1F62E}', '\u{1F622}', '\u{1F44F}'];
// The six on the bar are the ones you reach for: your recent picks first,
// the defaults filling in behind. Customised by use rather than by a
// settings page.
function quickReactions() {
  let recent = [];
  try {
    recent = JSON.parse(localStorage.getItem('aether.emoji.recent') || '[]');
  } catch {}
  const out = [];
  for (const e of [...recent, ...DEFAULT_QUICK]) {
    if (!out.includes(e)) out.push(e);
    if (out.length === 6) break;
  }
  return out;
}

// Reaction chips under a message: emoji, count, and whether one of them is
// yours. Clicking toggles your own.
// Reactions under a bubble: one pill, the way Teams and iMessage draw it.
// The distinct emoji are stacked (three at most, then "+N"), the total sits
// after them, and the pill is lit when one of them is yours. Everything
// else -- who reacted with what, adding yours, taking yours away -- lives in
// the popover the pill opens (reactionsPopover). Names come from the
// conversation's members; the push carries up to twelve ids per emoji and
// the count beyond that reads as "and N more".
function reactionRow(m) {
  const rs = (m.reactions ?? []).filter((r) => r.count > 0);
  if (!rs.length) return '';
  const total = rs.reduce((n, r) => n + r.count, 0);
  const mine = rs.some((r) => r.mine);
  const shown = rs.slice(0, 3);
  const extra = rs.length - shown.length;
  return `<div class="reacts">
    <button class="react-pill${mine ? ' mine' : ''}" data-react-pill="${esc(m.id)}"
      aria-haspopup="dialog" aria-label="${total} ${total === 1 ? 'reaction' : 'reactions'}. Show who reacted"
      title="${esc(reactionSummary(rs))}">
      <span class="react-stack">${shown.map((r) => `<span class="react-e">${esc(r.emoji)}</span>`).join('')}${
        extra > 0 ? `<span class="react-e more">+${extra}</span>` : ''
      }</span>
      <span class="react-n">${total}</span>
    </button>
  </div>`;
}

// "You, Divyanshii and 2 more reacted" -- the tooltip.
function reactionSummary(rs) {
  const names = [];
  let hidden = 0;
  const seen = new Set();
  for (const r of rs) {
    for (const id of r.who ?? []) {
      if (seen.has(id)) continue;
      seen.add(id);
      names.push(id === session.user?.id ? 'You' : memberName(id) || 'Someone');
    }
    hidden += Math.max(0, r.count - (r.who?.length ?? 0));
  }
  if (names.includes('You')) names.splice(names.indexOf('You'), 1), names.unshift('You');
  const head = names.slice(0, 3);
  const rest = names.length - head.length + hidden;
  const list = head.length ? head.join(', ') + (rest > 0 ? ` and ${rest} more` : '') : `${rs.reduce((n, r) => n + r.count, 0)} people`;
  return `${list} reacted`;
}

// The popover: one row per emoji with its count and names; a row you are on
// removes yours, any other adds yours; and the quick reactions to add one
// that is not there yet. Nobody's reaction but your own is ever touched --
// the server keys every change on the actor, so a click here can only move
// your own row.
let reactPopEl = null;
function closeReactPopover() {
  if (reactPopEl) {
    reactPopEl.remove();
    reactPopEl = null;
  }
}
function reactionsPopover(m, anchor) {
  closeReactPopover();
  const me = session.user?.id;
  const rs = (m.reactions ?? []).filter((r) => r.count > 0);
  const el = document.createElement('div');
  el.className = 'react-pop';
  el.setAttribute('role', 'dialog');
  el.setAttribute('aria-label', 'Reactions');
  const who = (r) => {
    const names = (r.who ?? []).map((id) => (id === me ? 'You' : memberName(id) || 'Someone'));
    if (names.includes('You')) names.splice(names.indexOf('You'), 1), names.unshift('You');
    const hidden = Math.max(0, r.count - (r.who?.length ?? 0));
    return names.join(', ') + (hidden > 0 ? ` and ${hidden} more` : '');
  };
  const present = new Set(rs.map((r) => r.emoji));
  el.innerHTML = `
    <div class="react-pop-rows">${rs
      .map(
        (r) => `<button class="react-pop-row${r.mine ? ' mine' : ''}" data-pop-emoji="${esc(r.emoji)}"
            title="${r.mine ? 'Remove your reaction' : 'React with ' + esc(r.emoji) + ' too'}">
          <span class="react-e">${esc(r.emoji)}</span>
          <span class="react-who">${esc(who(r))}</span>
          <span class="react-n">${r.count}</span>
          ${r.mine ? '<span class="react-yours">yours · click to remove</span>' : ''}
        </button>`
      )
      .join('')}</div>
    <div class="react-pop-add">
      ${quickReactions()
        .filter((e) => !present.has(e))
        .slice(0, 6)
        .map((e) => `<button class="act-e" data-pop-emoji="${e}" title="React ${e}">${e}</button>`)
        .join('')}
      <button class="act-e more" data-pop-more title="More reactions" aria-label="More reactions">
        <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-plus"/></svg>
      </button>
    </div>`;
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  const w = el.offsetWidth || 260;
  const h = el.offsetHeight || 120;
  const left = Math.min(Math.max(8, r.left), innerWidth - w - 8);
  const top = r.bottom + 6 + h > innerHeight - 8 ? Math.max(8, r.top - h - 6) : r.bottom + 6;
  el.style.left = `${left}px`;
  el.style.top = `${top}px`;
  reactPopEl = el;
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    closeReactPopover();
  };
  const onDoc = (e) => {
    if (!el.contains(e.target)) done();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') done();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);
  el.querySelectorAll('[data-pop-emoji]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      done();
      toggleReaction(m.id, b.dataset.popEmoji);
    };
  });
  el.querySelector('[data-pop-more]').onclick = async (e) => {
    e.stopPropagation();
    const ch = await pickEmoji(el.querySelector('[data-pop-more]'));
    done();
    if (ch) toggleReaction(m.id, ch);
  };
}

// toggleReaction is the one way a reaction changes from here: yours on if it
// is off, off if it is on. Applied locally first, confirmed by the push,
// undone if the server refuses.
async function toggleReaction(id, emoji) {
  const m = state.messages.find((x) => x.id === id);
  const on = !m?.reactions?.find((r) => r.emoji === emoji)?.mine;
  applyReactionLocally(id, emoji, on);
  renderThread();
  try {
    await messaging.react(id, emoji, on);
  } catch (err) {
    applyReactionLocally(id, emoji, !on);
    renderThread();
    toastErr(err);
  }
}

// What a reply quotes, drawn above it. The quote is the message as it reads
// NOW -- the server sends it fresh -- so editing a message fixes it everywhere
// it is quoted, and deleting it empties the quote.
function quoteRow(m) {
  if (!m.quoted) return '';
  const q = m.quoted;
  return `<button class="quote" data-goto="${esc(q.id)}">
    <span class="quote-who">${esc(q.senderName)}</span>
    <span class="quote-body">${q.deleted || !q.body ? 'Message deleted' : esc(q.body)}</span>
  </button>`;
}

// The actions that appear on hover, the way Teams does it: react, reply, save,
// and -- on your own message -- edit and delete. Hidden until the pointer is
// on the message, so a thread at rest is text rather than a control panel.
function actionBar(m, mine) {
  if (state.deleteMenu === m.id) {
    return `<div class="msg-acts open" data-del-menu>
      <button class="act-text" data-del-me="${esc(m.id)}">Delete for me</button>
      ${
        mine
          ? `<button class="act-text danger" data-del-all="${esc(m.id)}">Delete for everyone</button>`
          : ''
      }
      <button class="act-text quiet" data-del-cancel>Cancel</button>
    </div>`;
  }
  const quick = quickReactions().map((e) => {
    const on = !!m.reactions?.find((r) => r.emoji === e)?.mine;
    return `<button class="act-e${on ? ' on' : ''}" data-react-msg="${esc(m.id)}" data-react-emoji="${e}"
             title="${on ? 'Remove your ' + e : 'React ' + e}">${e}</button>`;
  }
  ).join('');
  return `<div class="msg-acts">
    ${quick}
    <button class="act-e more" data-react-more="${esc(m.id)}" title="More reactions" aria-label="More reactions">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
        <circle cx="12" cy="12" r="9"/><path d="M8.5 14a4 4 0 007 0M9 9.5h.01M15 9.5h.01"/>
      </svg>
      <span class="more-plus">+</span>
    </button>
    <span class="act-sep"></span>
    <button class="act" data-reply="${esc(m.id)}" title="Reply">
      <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-reply"/></svg>
    </button>
    <button class="act" data-forward="${esc(m.id)}" title="Forward">
      <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-forward"/></svg>
    </button>
    <button class="act${state.active?.pinnedMessageId === m.id ? ' on' : ''}" data-pin="${esc(m.id)}" title="${
      state.active?.pinnedMessageId === m.id ? 'Unpin' : 'Pin'
    }">📌</button>
    <button class="act" data-msg-more="${esc(m.id)}" title="More" aria-label="More options">⋯</button>
    <button class="act${m.saved ? ' on' : ''}" data-save="${esc(m.id)}"
            data-save-on="${m.saved ? '0' : '1'}" title="${m.saved ? 'Unsave' : 'Save'}">
      <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-bookmark"/></svg>
    </button>
    ${
      mine
        ? `<button class="act" data-edit="${esc(m.id)}" title="Edit">
             <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-pencil"/></svg>
           </button>`
        : ''
    }
    <button class="act danger" data-delete-menu="${esc(m.id)}" title="Delete">
      <svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-trash"/></svg>
    </button>
  </div>`;
}

/**
 * One message.
 *
 * `grouped` means the message before it was from the same person within a few
 * minutes, so the name, time and avatar are drawn once for the run rather than
 * on every line. That is the single biggest difference between a thread that
 * reads like a conversation and one that reads like a log file.
 */
// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

const MAX_FILE_BYTES = 25 * 1024 * 1024;
const MAX_FILES = 10;

export function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

// The same list the server renders inline. Anything else -- including SVG,
// which is XML that can carry script -- is a download, never an <img>.
const INLINE_IMAGE = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

function addPending(files) {
  const room = MAX_FILES - state.pending.length;
  const accepted = [];
  for (const f of files) {
    if (accepted.length >= room) {
      toastErr(new Error(`At most ${MAX_FILES} files in one message.`));
      break;
    }
    if (f.size > MAX_FILE_BYTES) {
      toastErr(new Error(`${f.name} is larger than 25 MB.`));
      continue;
    }
    if (f.size === 0) {
      toastErr(new Error(`${f.name} is empty.`));
      continue;
    }
    accepted.push(f);
  }
  if (!accepted.length) return;
  state.pending = [...state.pending, ...accepted];
  renderThread();
  $('c-input')?.focus();
}

function pendingStrip() {
  if (!state.pending.length) return '';
  return `<div class="pending">
    ${state.pending
      .map(
        (f, i) => `<span class="pend">
          <svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-clip"/></svg>
          <span class="pend-name">${esc(f.name)}</span>
          <span class="pend-size">${fmtSize(f.size)}</span>
          <button type="button" class="pend-x" data-unpend="${i}" title="Remove" aria-label="Remove ${esc(f.name)}">
            <svg width="11" height="11" viewBox="0 0 24 24"><use href="#i-close"/></svg>
          </button>
        </span>`
      )
      .join('')}
  </div>`;
}

const INLINE_VIDEO = new Set(['video/mp4', 'video/webm']);

function priorityTag(m) {
  if (m.priority === 'urgent') return '<div class="prio urgent"><span>!</span> URGENT</div>';
  if (m.priority === 'important') return '<div class="prio important"><span>!</span> IMPORTANT</div>';
  return '';
}

function scheduledTag(m) {
  if (!m.deliverAt || new Date(m.deliverAt).getTime() <= Date.now()) return '';
  return `<div class="scheduled-tag">Scheduled · ${esc(whenFull(m.deliverAt))} <button class="link" data-delete-menu="${esc(m.id)}">Cancel</button></div>`;
}

// A card from the message's meta: praise today, whatever else tomorrow.
function cardOf(m) {
  if (!m.meta) return '';
  let meta;
  try {
    meta = JSON.parse(m.meta);
  } catch {
    return '';
  }
  if (meta?.poll) return pollCard(m, meta.poll);
  if (meta?.announcement) {
    const a = meta.announcement;
    return `<div class="ann-card ${esc(a.color || 'brand')}">
      <div class="ann-kicker"><svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-megaphone"/></svg>Announcement</div>
      <h3>${esc(a.headline)}</h3>
      ${a.sub ? `<p>${esc(a.sub)}</p>` : ''}
    </div>`;
  }
  if (meta?.praise) {
    const p = meta.praise;
    return `<div class="praise-card">
      <div class="praise-badge">${esc(p.emoji || '🏆')}</div>
      <div class="praise-main">
        <b>${esc(p.badge || 'Praise')}</b>
        ${p.to ? `<span class="praise-to">to ${esc(p.to)}</span>` : ''}
      </div>
    </div>`;
  }
  return '';
}

// A poll: the question, each option as a bar with its count, your choice
// lit, and the people who chose it when the poll names them. Clicking an
// option votes (or, on a single-choice poll, moves your vote); clicking your
// own choice withdraws it. Applied locally first, confirmed by the push.
// A poll's body is its question (for previews and search); the card draws
// it, so the bubble does not repeat it.
function isPollMessage(m) {
  return !!m.meta && (m.meta.includes('"poll"') || m.meta.includes('"announcement"'));
}

function pollCard(m, p) {
  const t = m.poll ?? { counts: p.options.map(() => 0), voters: null, total: 0, mine: [] };
  const mine = new Set(t.mine ?? []);
  const total = Math.max(t.total, 1);
  const max = Math.max(...t.counts, 0);
  return `<div class="poll-card" data-poll="${esc(m.id)}">
    <div class="poll-head"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-poll"/></svg>
      <b>${esc(p.q)}</b>
      <small>${p.multi ? 'Choose any' : 'Choose one'}${p.anon ? ' · anonymous' : ''}</small></div>
    <div class="poll-options">${p.options
      .map((o, i) => {
        const n = t.counts[i] ?? 0;
        const pct = t.total ? Math.round((100 * n) / total) : 0;
        const who = t.voters?.[i] ?? [];
        return `<button type="button" class="poll-opt${mine.has(i) ? ' mine' : ''}${n && n === max ? ' lead' : ''}" data-poll-opt="${i}"
            aria-pressed="${mine.has(i)}" title="${mine.has(i) ? 'Withdraw your vote' : 'Vote ' + esc(o)}">
          <span class="poll-bar" style="width:${pct}%"></span>
          <span class="poll-label">${esc(o)}</span>
          <span class="poll-n">${n}${t.total ? ` · ${pct}%` : ''}</span>
          ${who.length ? `<span class="poll-who">${who.slice(0, 6).map((id) => `<span ${avatarAttrs({ userId: id, name: memberName(id) }, 'xs')}>${esc(initials({ userId: id, name: memberName(id) }))}</span>`).join('')}${
            n > who.length ? `<i>+${n - who.length}</i>` : ''
          }</span>` : ''}
        </button>`;
      })
      .join('')}</div>
    <div class="poll-foot">${t.total ? `${t.total} ${t.total === 1 ? 'vote' : 'votes'}` : 'No votes yet'}${mine.size ? ' · you voted' : ''}</div>
  </div>`;
}

async function castVote(messageId, option) {
  const m = state.messages.find((x) => x.id === messageId);
  if (!m) return;
  let p;
  try {
    p = JSON.parse(m.meta).poll;
  } catch {
    return;
  }
  const t = m.poll ?? { counts: p.options.map(() => 0), voters: null, total: 0, mine: [] };
  const before = JSON.parse(JSON.stringify(t));
  let mine = new Set(t.mine ?? []);
  if (mine.has(option)) mine.delete(option);
  else if (p.multi) mine.add(option);
  else mine = new Set([option]);
  // Local first: move my count(s), keep everyone else's.
  const me = session.user?.id;
  const counts = t.counts.map((n, i) => n - ((t.mine ?? []).includes(i) ? 1 : 0) + (mine.has(i) ? 1 : 0));
  const had = (t.mine ?? []).length > 0;
  const total = t.total - (had ? 1 : 0) + (mine.size ? 1 : 0);
  const voters = t.voters ? t.voters.map((w, i) => (mine.has(i) ? [...w.filter((x) => x !== me), me] : w.filter((x) => x !== me))) : null;
  m.poll = { counts, voters, total, mine: [...mine].sort() };
  renderThread();
  try {
    const res = await messaging.vote(messageId, [...mine]);
    if (res?.poll) {
      m.poll = res.poll;
      renderThread();
    }
  } catch (e) {
    m.poll = before;
    renderThread();
    toastErr(e);
  }
}

// An announcement: a headline, a line under it, a colour. The message body
// is the headline, for previews and search; the card is what the thread
// draws.
function announcementDialog() {
  const { host, close, arm } = dialogHost();
  const colors = ['brand', 'amber', 'rose', 'sky', 'violet'];
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="an-title">
      <h2 id="an-title">Announcement</h2>
      <label class="field"><span>Headline</span>
        <input id="an-head" class="input" placeholder="All hands on Friday" maxlength="80" autocomplete="off" /></label>
      <label class="field" style="margin-top:10px"><span>Details <small>(optional)</small></span>
        <input id="an-sub" class="input" placeholder="3pm, main room. Bring questions." maxlength="200" autocomplete="off" /></label>
      <div class="ann-colors">${colors.map((c, i) => `<button type="button" class="ann-swatch ${c}${i === 0 ? ' active' : ''}" data-color="${c}" aria-label="${c}"></button>`).join('')}</div>
      <div class="ann-preview" id="an-preview"></div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm disabled>Post</button>
      </div>
    </div>`;
  arm();
  let color = 'brand';
  const confirmBtn = host.querySelector('[data-confirm]');
  const preview = () => {
    const h = $('an-head').value.trim();
    confirmBtn.disabled = !h;
    $('an-preview').innerHTML = h
      ? `<div class="ann-card ${color}"><div class="ann-kicker"><svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-megaphone"/></svg>Announcement</div><h3>${esc(h)}</h3>${
          $('an-sub').value.trim() ? `<p>${esc($('an-sub').value.trim())}</p>` : ''
        }</div>`
      : '';
  };
  host.querySelectorAll('[data-color]').forEach((b) => {
    b.onclick = () => {
      color = b.dataset.color;
      host.querySelectorAll('[data-color]').forEach((x) => x.classList.toggle('active', x === b));
      preview();
    };
  });
  $('an-head').oninput = preview;
  $('an-sub').oninput = preview;
  setTimeout(() => $('an-head').focus(), 50);
  confirmBtn.onclick = async () => {
    confirmBtn.disabled = true;
    const headline = $('an-head').value.trim();
    const sub = $('an-sub').value.trim();
    try {
      await messaging.sendFull(state.active.id, headline, '', [], { meta: JSON.stringify({ announcement: { headline, sub, color } }) });
      close();
    } catch (e) {
      confirmBtn.disabled = false;
      toastErr(e);
    }
  };
}

// Compose a poll: a question, two to ten options, choose-one or choose-any,
// named or anonymous.
function pollDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="pl-title">
      <h2 id="pl-title">New poll</h2>
      <label class="field"><span>Question</span>
        <input id="pl-q" class="input" placeholder="Where shall we have lunch?" maxlength="200" autocomplete="off" /></label>
      <div class="pl-options" id="pl-options"></div>
      <button type="button" class="btn sm" id="pl-add">Add option</button>
      <div class="pl-switches">
        <label><input type="checkbox" id="pl-multi" /> People can choose more than one</label>
        <label><input type="checkbox" id="pl-anon" /> Anonymous (no names on the results)</label>
      </div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm disabled>Send poll</button>
      </div>
    </div>`;
  arm();
  const opts = $('pl-options');
  const confirmBtn = host.querySelector('[data-confirm]');
  const values = () => [...opts.querySelectorAll('input')].map((i) => i.value.trim()).filter(Boolean);
  const check = () => {
    confirmBtn.disabled = !$('pl-q').value.trim() || values().length < 2;
    $('pl-add').disabled = opts.querySelectorAll('input').length >= 10;
  };
  const addOpt = (v = '') => {
    if (opts.querySelectorAll('input').length >= 10) return;
    const row = document.createElement('div');
    row.className = 'pl-row';
    row.innerHTML = `<input class="input" placeholder="Option ${opts.children.length + 1}" maxlength="80" value="${esc(v)}" />
      <button type="button" class="icon-btn" title="Remove" aria-label="Remove option"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg></button>`;
    row.querySelector('input').oninput = check;
    row.querySelector('button').onclick = () => {
      if (opts.children.length > 2) row.remove();
      else row.querySelector('input').value = '';
      check();
    };
    opts.appendChild(row);
    check();
    return row;
  };
  addOpt();
  addOpt();
  $('pl-add').onclick = () => addOpt().querySelector('input').focus();
  $('pl-q').oninput = check;
  setTimeout(() => $('pl-q').focus(), 50);
  confirmBtn.onclick = async () => {
    confirmBtn.disabled = true;
    const q = $('pl-q').value.trim();
    const meta = JSON.stringify({ poll: { q, options: values(), multi: $('pl-multi').checked, anon: $('pl-anon').checked } });
    try {
      await messaging.sendFull(state.active.id, q, '', [], { meta });
      close();
    } catch (e) {
      confirmBtn.disabled = false;
      toastErr(e);
    }
  };
}

function attachmentsRow(m) {
  const atts = m.attachments ?? [];
  if (!atts.length) return '';
  const images = atts.filter((a) => INLINE_IMAGE.has(a.mime));
  const videos = atts.filter((a) => INLINE_VIDEO.has(a.mime));
  const audios = atts.filter((a) => /^audio\//.test(a.mime));
  const files = atts.filter((a) => !INLINE_IMAGE.has(a.mime) && !INLINE_VIDEO.has(a.mime) && !/^audio\//.test(a.mime));
  return `${
    audios.length
      ? audios
          .map(
            (a) => `<div class="att-audio"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-mic"/></svg>
              <audio controls preload="metadata" src="${messaging.fileURL(a.id)}" title="${esc(a.name)}"></audio></div>`
          )
          .join('')
      : ''
  }${
    videos.length
      ? videos
          .map(
            (a) => `<video class="att-video" controls preload="metadata" src="${messaging.fileURL(a.id)}" title="${esc(a.name)}"></video>`
          )
          .join('')
      : ''
  }${
    images.length
      ? `<div class="att-images n${Math.min(images.length, 4)}">${images
          .map(
            (a) => `<a class="att-img" href="${messaging.fileURL(a.id)}" target="_blank" rel="noopener" title="${esc(a.name)}">
              <img src="${messaging.fileURL(a.id)}" alt="${esc(a.name)}" loading="lazy" decoding="async" />
            </a>`
          )
          .join('')}</div>`
      : ''
  }${
    files.length
      ? `<div class="att-files">${files
          .map(
            (a) => `<a class="att-file" href="${messaging.fileURL(a.id)}" download="${esc(a.name)}" title="Download ${esc(a.name)}">
              <span class="att-ico"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-clip"/></svg></span>
              <span class="att-main">
                <span class="att-name">${esc(a.name)}</span>
                <span class="att-meta">${fmtSize(a.size)}</span>
              </span>
            </a>`
          )
          .join('')}</div>`
      : ''
  }`;
}

// Everything shared in the conversation, in place of the thread: files
// newest first, then the links people posted. The reference lays it out as a
// tab beside "Chat" under the person's name, so that is where it lives.
let sharedSub = 'files';
// Find in this chat. The same index the title-bar search uses, narrowed to
// one conversation; a result jumps to the message.
let threadSearchTimer = 0;
function paintThreadSearch(pane) {
  const ts = state.threadSearch;
  if (!ts || !pane.isConnected) return;
  pane.innerHTML = `
    <div class="ts-bar">
      <svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-search"/></svg>
      <input id="ts-input" class="input" placeholder="Find in this chat…" value="${esc(ts.q)}" autocomplete="off" />
      <span class="ts-count">${ts.loading ? 'Searching…' : ts.q.length >= 3 ? `${ts.results.length} ${ts.results.length === 1 ? 'result' : 'results'}` : ''}</span>
      <button class="icon-btn" id="ts-close" title="Close" aria-label="Close search"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-close"/></svg></button>
    </div>
    ${
      ts.q.length >= 3 && !ts.loading
        ? ts.results.length
          ? `<div class="ts-results">${ts.results
              .map(
                (m) => `<button class="ts-row" data-ts-msg="${esc(m.id)}">
                  <span class="ts-who">${esc(m.senderName)} · ${esc(when(m.createdAt))}</span>
                  <span class="ts-body">${esc(plainText(m.body || '').slice(0, 160))}</span>
                </button>`
              )
              .join('')}</div>`
          : '<div class="ts-empty">Nothing in this chat matches.</div>'
        : ''
    }`;
  const input = $('ts-input');
  input.oninput = () => {
    ts.q = input.value.trim();
    clearTimeout(threadSearchTimer);
    if (ts.q.length < 3) {
      ts.results = [];
      ts.loading = false;
      paintThreadSearch(pane);
      return;
    }
    ts.loading = true;
    paintThreadSearch(pane);
    const q = ts.q;
    threadSearchTimer = setTimeout(async () => {
      try {
        const res = await messaging.searchIn(state.active.id, q);
        if (state.threadSearch !== ts || ts.q !== q) return; // stale
        ts.results = res.messages ?? [];
      } catch {
        ts.results = [];
      }
      ts.loading = false;
      paintThreadSearch(pane);
    }, 180);
  };
  input.onkeydown = (e) => {
    if (e.key === 'Escape') {
      state.threadSearch = null;
      renderThread();
    }
  };
  // Keep the caret at the end after each repaint.
  input.setSelectionRange(input.value.length, input.value.length);
  $('ts-close').onclick = () => {
    state.threadSearch = null;
    renderThread();
  };
  pane.querySelectorAll('[data-ts-msg]').forEach((b) => {
    b.onclick = () => openConversationAt(state.active.id, b.dataset.tsMsg).catch(toastErr);
  });
}

// The other person of a direct conversation as a picker entry.
function peerPerson(c) {
  const p = peerOf(c);
  return p ? { id: p.userId, name: p.name || c.display, email: p.email || '' } : null;
}

async function paintShared(pane, conv) {
  const draw = (data, loading) => {
    if (!pane.isConnected) return;
    const files = data?.files ?? [];
    const links = data?.links ?? [];
    pane.innerHTML = `
      <div class="tabs shared-tabs" role="tablist">
        <button class="tab${sharedSub === 'files' ? ' active' : ''}" data-stab="files" role="tab">Files <span class="count">${files.length}</span></button>
        <button class="tab${sharedSub === 'links' ? ' active' : ''}" data-stab="links" role="tab">Links <span class="count">${links.length}</span></button>
      </div>
      ${
        loading
          ? '<div class="loading">Loading…</div>'
          : sharedSub === 'files'
            ? files.length
              ? `<div class="shared-list">${files
                  .map(
                    (a) => `<a class="shared-row" href="${messaging.fileURL(a.id)}" ${
                      INLINE_IMAGE.has(a.mime) ? 'target="_blank" rel="noopener"' : `download="${esc(a.name)}"`
                    }>
                      ${
                        INLINE_IMAGE.has(a.mime)
                          ? `<img class="shared-thumb" src="${messaging.fileURL(a.id)}" alt="" loading="lazy" />`
                          : `<span class="att-ico"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-clip"/></svg></span>`
                      }
                      <span class="shared-main">
                        <span class="att-name">${esc(a.name)}</span>
                        <span class="att-meta">${esc(a.senderName || '')} · ${esc(whenFull(a.createdAt))} · ${fmtSize(a.size)}</span>
                      </span>
                    </a>`
                  )
                  .join('')}</div>`
              : `<div class="empty">No files shared here yet. Drop one onto the chat, or use the paperclip.</div>`
            : links.length
              ? `<div class="shared-list">${links
                  .map(
                    (l) => `<a class="shared-row" href="${esc(l.url)}" target="_blank" rel="noopener noreferrer">
                      <span class="att-ico"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-link"/></svg></span>
                      <span class="shared-main">
                        <span class="att-name">${esc(l.url)}</span>
                        <span class="att-meta">${esc(l.senderName)} · ${esc(whenFull(l.at))}</span>
                      </span>
                    </a>`
                  )
                  .join('')}</div>`
              : `<div class="empty">No links posted here yet.</div>`
      }`;
    pane.querySelectorAll('[data-stab]').forEach((b) => {
      b.onclick = () => {
        sharedSub = b.dataset.stab;
        draw(data, false);
      };
    });
  };
  // What was fetched last time paints instantly; the fresh answer replaces it.
  draw(state.shared.get(conv.id), !state.shared.has(conv.id));
  try {
    const data = await messaging.shared(conv.id);
    state.shared.set(conv.id, data);
    if (state.active?.id === conv.id && state.threadTab === 'shared') draw(data, false);
  } catch (e) {
    toastErr(e);
  }
}

// Photos: every image shared in the conversation, as a grid, newest first.
async function paintPhotos(pane, conv) {
  const draw = (data, loading) => {
    if (!pane.isConnected) return;
    const photos = (data?.files ?? []).filter((a) => INLINE_IMAGE.has(a.mime));
    pane.innerHTML = loading
      ? '<div class="loading">Loading…</div>'
      : photos.length
        ? `<div class="photo-grid">${photos
            .map(
              (a) => `<a class="photo" href="${messaging.fileURL(a.id)}" target="_blank" rel="noopener" title="${esc(a.name)} · ${esc(a.senderName || '')} · ${esc(whenFull(a.createdAt))}">
                <img src="${messaging.fileURL(a.id)}" alt="${esc(a.name)}" loading="lazy" decoding="async" />
              </a>`
            )
            .join('')}</div>`
        : `<div class="empty-photos">
             <div class="empty-art"><svg width="34" height="34" viewBox="0 0 24 24"><use href="#i-image"/></svg></div>
             <h3>No photos shared in the chat</h3>
             <p>Photos added to the chat automatically show up here.</p>
           </div>`;
  };
  draw(state.shared.get(conv.id), !state.shared.has(conv.id));
  try {
    const data = await messaging.shared(conv.id);
    state.shared.set(conv.id, data);
    if (state.active?.id === conv.id && state.threadTab === 'photos') draw(data, false);
  } catch (e) {
    toastErr(e);
  }
}

// ---------------------------------------------------------------------------
// Composer extras: formatting, priority, schedule, praise, clips, drafts,
// suggestions, undo, per-message menu
// ---------------------------------------------------------------------------

const DRAFT_KEY = 'aether.drafts';
function drafts() {
  try {
    return JSON.parse(localStorage.getItem(DRAFT_KEY) || '{}');
  } catch {
    return {};
  }
}
export function draftFor(convId) {
  return drafts()[convId] || '';
}
function saveDraft(convId, text) {
  try {
    const d = drafts();
    if (text) d[convId] = text;
    else delete d[convId];
    localStorage.setItem(DRAFT_KEY, JSON.stringify(d));
  } catch {}
}
function clearDraft(convId) {
  saveDraft(convId, '');
}

function suggestionsStrip() {
  const last = [...state.messages].reverse().find((m) => !m.system && !m.deleted && m.body);
  if (!last || last.senderId === session.user?.id) return '';
  const picks = suggestedReplies(last.body);
  if (!picks.length) return '';
  return `<div class="suggest-strip">${picks
    .map((p) => `<button type="button" class="suggest-chip" data-suggest="${esc(p)}">${esc(p)}</button>`)
    .join('')}</div>`;
}

const FMT_KEY = 'aether.fmtbar';
function fmtPinned() {
  try {
    return localStorage.getItem(FMT_KEY) === '1';
  } catch {
    return false;
  }
}

// The formatting toolbar: text styles, then what a message can carry with it
// (priority, a send time, praise, a clip), then chips for whatever is set on
// the message being written. Icons, grouped, with the shortcut in the
// tooltip -- a row of letters and emoji reads as decoration; this reads as
// controls.
function formatBar() {
  const sched = state.scheduleAt ? whenFull(new Date(state.scheduleAt).toISOString()) : '';
  const ico = (id) => `<svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-${id}"/></svg>`;
  const btn = (attrs, id, title) =>
    `<button type="button" class="fmt" ${attrs} title="${esc(title)}" aria-label="${esc(title)}">${ico(id)}</button>`;
  return `<div class="fmt-bar" role="toolbar" aria-label="Formatting">
    <div class="fmt-group">
      ${btn('data-fmt="**"', 'bold', 'Bold  Ctrl+B')}
      ${btn('data-fmt="*"', 'italic', 'Italic  Ctrl+I')}
      ${btn('data-fmt="~~"', 'strike', 'Strikethrough')}
      ${btn('data-fmt="\`"', 'code', 'Code  Ctrl+E')}
      ${btn('data-fmt="\`\`\`"', 'codeblock', 'Code block')}
    </div>
    <span class="fmt-sep"></span>
    <div class="fmt-group">
      <button type="button" class="fmt prio-btn ${state.priority}" id="c-prio" title="Priority: ${esc(
        state.priority || 'normal'
      )} (click to change)" aria-label="Priority">${ico('alert')}</button>
      <button type="button" class="fmt${state.scheduleAt ? ' on' : ''}" id="c-later" title="${esc(
        sched ? 'Scheduled for ' + sched : 'Send later'
      )}" aria-label="Send later">${ico('clock')}</button>
      ${btn('id="c-praise"', 'award', 'Send praise')}
      ${btn('id="c-poll"', 'poll', 'Create a poll')}
      ${state.active.kind !== 'direct' ? btn('id="c-announce"', 'megaphone', 'Post an announcement') : ''}
      ${btn('id="c-clip"', 'video', 'Record a video clip')}
      ${btn('id="c-voice"', 'mic', 'Record a voice message')}
    </div>
    <span class="fmt-status" id="c-fmt-status">${
      state.priority ? `<span class="fmt-chip ${state.priority}">${ico('alert')}${state.priority}</span>` : ''
    }${sched ? `<span class="fmt-chip sched">${ico('clock')}Sends ${esc(sched)}</span>` : ''}</span>
  </div>`;
}

function wireComposerExtras(input) {
  // Drafts: restore, then keep.
  const d = draftFor(state.active.id);
  if (d && !input.value) input.value = d;
  input.addEventListener('input', () => saveDraft(state.active.id, input.value));

  const apply = (mark) => {
    const start = input.selectionStart ?? input.value.length;
    const end = input.selectionEnd ?? start;
    if (mark === '```') {
      const sel = input.value.slice(start, end);
      const text = `${input.value.slice(0, start)}\n\`\`\`\n${sel}\n\`\`\`\n${input.value.slice(end)}`;
      input.value = text;
      const caret = start + 5 + sel.length;
      input.setSelectionRange(caret, caret);
    } else {
      const r = wrapSelection(input.value, start, end, mark);
      input.value = r.text;
      input.setSelectionRange(r.start, r.end);
    }
    input.focus();
    saveDraft(state.active.id, input.value);
  };
  document.querySelectorAll('.fmt-bar [data-fmt]').forEach((b) => {
    b.onmousedown = (e) => e.preventDefault();
    b.onclick = () => apply(b.dataset.fmt);
  });
  // The toolbar pin. Pinned, the bar is part of the composer; unpinned, it
  // appears while you are writing and goes when you are not.
  const toggle = $('c-format');
  const composer = toggle.closest('.composer');
  // Aa is a switch: one click opens the toolbar, another closes it, and a
  // click anywhere else closes it too. (It used to be a remembered pin that
  // nothing but Aa could undo, and the toolbar also appeared on focus; both
  // read as "it will not go away".)
  const setPinned = (on) => {
    try {
      localStorage.setItem(FMT_KEY, on ? '1' : '0');
    } catch {}
    toggle.classList.toggle('on', on);
    toggle.setAttribute('aria-pressed', String(on));
    composer.classList.toggle('fmt-pinned', on);
    input.focus();
  };
  composer.classList.toggle('fmt-pinned', fmtPinned());
  toggle.onmousedown = (e) => e.preventDefault();
  toggle.onclick = (e) => {
    e.stopPropagation();
    setPinned(!fmtPinned());
  };
  if (!wireComposerExtras._outside) {
    wireComposerExtras._outside = true;
    document.addEventListener('click', (e) => {
      if (!fmtPinned()) return;
      const bar = document.querySelector('.composer .fmt-bar');
      const tg = document.getElementById('c-format');
      if (bar?.contains(e.target) || tg?.contains(e.target)) return;
      // A dialog the toolbar opened (schedule, praise, poll) is part of it.
      if (e.target.closest?.('.dialog-host, .emoji-panel, .ctx-menu, .react-pop')) return;
      try {
        localStorage.setItem(FMT_KEY, '0');
      } catch {}
      tg?.classList.remove('on');
      tg?.setAttribute('aria-pressed', 'false');
      document.querySelector('.composer')?.classList.remove('fmt-pinned');
    });
  }
  input.addEventListener('keydown', (e) => {
    if (!(e.ctrlKey || e.metaKey)) return;
    const k = e.key.toLowerCase();
    if (k === 'x' && e.shiftKey) {
      e.preventDefault();
      setPinned(!fmtPinned());
    } else if (k === 'b') {
      e.preventDefault();
      apply('**');
    } else if (k === 'i') {
      e.preventDefault();
      apply('*');
    } else if (k === 'e') {
      e.preventDefault();
      apply('`');
    }
  });

  $('c-prio').onclick = () => {
    state.priority = state.priority === '' ? 'important' : state.priority === 'important' ? 'urgent' : '';
    renderThread();
    $('c-input')?.focus();
  };
  $('c-later').onclick = () => scheduleDialog();
  $('c-praise').onclick = () => praiseDialog();
  $('c-poll').onclick = () => pollDialog();
  const ann = $('c-announce');
  if (ann) ann.onclick = () => announcementDialog();
  $('c-clip').onclick = () => clipDialog();
  $('c-voice').onclick = () => voiceDialog();

  document.querySelectorAll('[data-suggest]').forEach((b) => {
    b.onclick = () => {
      input.value = b.dataset.suggest;
      input.focus();
      $('c-send').click();
    };
  });
}

// Send later: a time picker, bounded the way the server bounds it.
function scheduleDialog() {
  const { host, close, arm } = dialogHost();
  const min = new Date(Date.now() + 60_000);
  const pad = (n) => String(n).padStart(2, '0');
  const local = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const preset = state.scheduleAt ? new Date(state.scheduleAt) : new Date(Date.now() + 60 * 60_000);
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="sl-title">
      <h2 id="sl-title">Send later</h2>
      <p class="dialog-sub">The message is kept as a draft only you can see, and delivered at the time you pick.</p>
      <div class="quick-times">
        <button class="btn sm" data-in="60">In an hour</button>
        <button class="btn sm" data-at="tomorrow9">Tomorrow 9:00</button>
        <button class="btn sm" data-at="monday9">Monday 9:00</button>
      </div>
      <label class="field" style="margin-top:10px"><span>Or pick a time</span>
        <input id="sl-when" class="input" type="datetime-local" min="${local(min)}" value="${local(preset)}" /></label>
      <div class="dialog-actions">
        ${state.scheduleAt ? '<button class="btn danger-quiet" data-clear>Send now instead</button><span class="spacer"></span>' : ''}
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm>Schedule</button>
      </div>
    </div>`;
  arm();
  const set = (ms) => {
    state.scheduleAt = ms;
    close();
    renderThread();
    $('c-input')?.focus();
  };
  host.querySelectorAll('[data-in]').forEach((b) => (b.onclick = () => set(Date.now() + Number(b.dataset.in) * 60_000)));
  host.querySelectorAll('[data-at]').forEach((b) => {
    b.onclick = () => {
      const d = new Date();
      d.setSeconds(0, 0);
      d.setHours(9, 0);
      if (b.dataset.at === 'tomorrow9') d.setDate(d.getDate() + 1);
      else {
        const add = ((8 - d.getDay()) % 7) || 7;
        d.setDate(d.getDate() + add);
      }
      set(d.getTime());
    };
  });
  host.querySelector('[data-clear]')?.addEventListener('click', () => set(0));
  host.querySelector('[data-confirm]').onclick = () => {
    const v = $('sl-when').value;
    const ms = v ? new Date(v).getTime() : 0;
    if (!ms || ms < Date.now() + 30_000) {
      toastErr(new Error('Pick a time at least a minute from now.'));
      return;
    }
    set(ms);
  };
}

// Praise: a badge and who it is for, sent as a card.
function praiseDialog() {
  const { host, close, arm } = dialogHost();
  const badges = [
    ['🏆', 'Awesome'], ['🙏', 'Thank you'], ['💡', 'Problem solver'], ['🎯', 'Achiever'],
    ['🤝', 'Team player'], ['🚀', 'Above and beyond'], ['❤️', 'Kind heart'], ['🧠', 'Creative'],
  ];
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="pr-title">
      <h2 id="pr-title">Send praise</h2>
      <div class="badge-grid">${badges
        .map(([e, t], i) => `<button type="button" class="badge${i === 0 ? ' active' : ''}" data-badge="${i}"><span>${e}</span>${esc(t)}</button>`)
        .join('')}</div>
      <label class="field" style="margin-top:10px"><span>To <small>(optional)</small></span>
        <input id="pr-to" class="input" placeholder="Priya" maxlength="60" /></label>
      <label class="field" style="margin-top:10px"><span>A note <small>(optional)</small></span>
        <input id="pr-note" class="input" placeholder="For getting the release out on time" maxlength="300" /></label>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm>Send</button>
      </div>
    </div>`;
  arm();
  let pick = 0;
  host.querySelectorAll('[data-badge]').forEach((b) => {
    b.onclick = () => {
      pick = Number(b.dataset.badge);
      host.querySelectorAll('[data-badge]').forEach((x) => x.classList.toggle('active', x === b));
    };
  });
  host.querySelector('[data-confirm]').onclick = async () => {
    const [emoji, badge] = badges[pick];
    const to = $('pr-to').value.trim();
    const note = $('pr-note').value.trim();
    const meta = JSON.stringify({ praise: { emoji, badge, to } });
    try {
      const m = await messaging.sendFull(state.active.id, note || `${badge}!`, '', [], { meta });
      close();
      placeMessage(m);
      renderThread();
      offerUndo(m);
    } catch (e) {
      toastErr(e);
    }
  };
}

// A video clip from the camera, recorded here and sent as a file.
function clipDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="vc-title">
      <h2 id="vc-title">Record a video clip</h2>
      <video id="vc-preview" class="clip-preview" autoplay muted playsinline></video>
      <div class="clip-status" id="vc-status">Camera starting…</div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" id="vc-rec" disabled>Record</button>
        <button class="btn btn-primary" id="vc-send" hidden>Send clip</button>
      </div>
    </div>`;
  arm();
  let stream = null;
  let rec = null;
  let chunks = [];
  let blob = null;
  let timer = null;
  const status = (t) => {
    const el = $('vc-status');
    if (el) el.textContent = t;
  };
  const stop = () => {
    clearInterval(timer);
    stream?.getTracks().forEach((t) => t.stop());
  };
  host.querySelector('[data-cancel]').addEventListener('click', stop);
  (async () => {
    try {
      stream = await navigator.mediaDevices.getUserMedia({ video: { width: { ideal: 1280 } }, audio: true });
      $('vc-preview').srcObject = stream;
      $('vc-rec').disabled = false;
      status('Ready. Clips can be up to 60 seconds.');
    } catch {
      status('Camera or microphone unavailable.');
    }
  })();
  $('vc-rec').onclick = () => {
    if (!stream) return;
    if (rec && rec.state === 'recording') {
      rec.stop();
      return;
    }
    chunks = [];
    const mime = MediaRecorder.isTypeSupported('video/webm;codecs=vp9,opus') ? 'video/webm;codecs=vp9,opus' : 'video/webm';
    rec = new MediaRecorder(stream, { mimeType: mime, videoBitsPerSecond: 1_500_000 });
    rec.ondataavailable = (e) => e.data.size && chunks.push(e.data);
    rec.onstop = () => {
      blob = new Blob(chunks, { type: 'video/webm' });
      const v = $('vc-preview');
      v.srcObject = null;
      v.muted = false;
      v.controls = true;
      v.src = URL.createObjectURL(blob);
      $('vc-rec').textContent = 'Record again';
      $('vc-send').hidden = false;
      status(`Recorded · ${fmtSize(blob.size)}`);
      clearInterval(timer);
    };
    rec.start(250);
    const t0 = Date.now();
    $('vc-rec').textContent = 'Stop';
    timer = setInterval(() => {
      const s = Math.round((Date.now() - t0) / 1000);
      status(`Recording… ${s}s`);
      if (s >= 60) rec.stop();
    }, 250);
  };
  $('vc-send').onclick = async () => {
    if (!blob) return;
    const file = new File([blob], `clip-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.webm`, { type: 'video/webm' });
    stop();
    close();
    addPending([file]);
  };
}

// A voice message: record from the microphone, hear it back, send it as an
// audio attachment the thread plays inline. Opus in WebM, which every
// browser here decodes and the server serves inline (audio/webm is on the
// allowlist).
function voiceDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="vm-title">
      <h2 id="vm-title">Voice message</h2>
      <div class="vm-meter"><span id="vm-level"></span></div>
      <div class="clip-status" id="vm-status">Microphone starting…</div>
      <audio id="vm-play" controls hidden></audio>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" id="vm-rec" disabled>Record</button>
        <button class="btn btn-primary" id="vm-send" hidden>Send</button>
      </div>
    </div>`;
  arm();
  let stream = null;
  let rec = null;
  let blob = null;
  let t0 = 0;
  let timer = 0;
  let actx = null;
  const status = $('vm-status');
  const stop = () => {
    clearInterval(timer);
    try {
      if (rec && rec.state !== 'inactive') rec.stop();
    } catch {}
    stream?.getTracks().forEach((t) => t.stop());
    try {
      actx?.close();
    } catch {}
  };
  host.querySelector('[data-cancel]').addEventListener('click', stop);
  (async () => {
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch {
      status.textContent = 'No microphone. Check the permission and try again.';
      return;
    }
    // A live level, so "is it picking me up?" is answered before recording.
    try {
      actx = new AudioContext();
      const src = actx.createMediaStreamSource(stream);
      const an = actx.createAnalyser();
      an.fftSize = 256;
      src.connect(an);
      const buf = new Uint8Array(an.frequencyBinCount);
      const tick = () => {
        if (!host.isConnected) return;
        an.getByteTimeDomainData(buf);
        let peak = 0;
        for (const v of buf) peak = Math.max(peak, Math.abs(v - 128));
        const el = $('vm-level');
        if (el) el.style.width = `${Math.min(100, (peak / 128) * 160)}%`;
        requestAnimationFrame(tick);
      };
      tick();
    } catch {}
    status.textContent = 'Ready. Up to two minutes.';
    $('vm-rec').disabled = false;
  })();
  $('vm-rec').onclick = () => {
    if (rec && rec.state === 'recording') {
      rec.stop();
      return;
    }
    const chunks = [];
    const mime = MediaRecorder.isTypeSupported('audio/webm;codecs=opus') ? 'audio/webm;codecs=opus' : 'audio/webm';
    rec = new MediaRecorder(stream, { mimeType: mime, audioBitsPerSecond: 48_000 });
    rec.ondataavailable = (e) => e.data.size && chunks.push(e.data);
    rec.onstop = () => {
      clearInterval(timer);
      blob = new Blob(chunks, { type: 'audio/webm' });
      const play = $('vm-play');
      play.src = URL.createObjectURL(blob);
      play.hidden = false;
      status.textContent = `${Math.round((Date.now() - t0) / 1000)}s recorded. Listen, then send.`;
      $('vm-rec').textContent = 'Record again';
      $('vm-send').hidden = false;
    };
    rec.start(250);
    t0 = Date.now();
    $('vm-rec').textContent = 'Stop';
    $('vm-send').hidden = true;
    $('vm-play').hidden = true;
    timer = setInterval(() => {
      const s = Math.round((Date.now() - t0) / 1000);
      status.textContent = `Recording… ${s}s`;
      if (s >= 120) rec.stop();
    }, 250);
  };
  $('vm-send').onclick = () => {
    if (!blob) return;
    const file = new File([blob], `voice-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.webm`, { type: 'audio/webm' });
    stop();
    close();
    addPending([file]);
  };
}

// Undo: a few seconds to take a message back, which is delete-for-everyone
// before anybody has had time to read it.
function offerUndo(m) {
  if (!m?.id) return;
  const el = $('toast');
  if (!el) return;
  clearTimeout(toastErr._t);
  el.innerHTML = `${m.deliverAt && new Date(m.deliverAt) > new Date() ? 'Scheduled' : 'Sent'} <button class="link" id="undo-send">Undo</button>`;
  el.className = 'toast show ok';
  const t = setTimeout(() => (el.className = 'toast'), 6000);
  toastErr._t = t;
  $('undo-send').onclick = async () => {
    clearTimeout(t);
    el.className = 'toast';
    try {
      await messaging.remove(m.id);
      const at = state.messages.findIndex((x) => x.id === m.id);
      if (at >= 0) state.messages.splice(at, 1);
      renderThread();
      toastOk('Message taken back');
    } catch (e) {
      toastErr(e);
    }
  };
}

// The per-message menu: copy the text, copy a link to it, hear it.
function messageMenu(m, anchor) {
  closeConvMenu();
  const el = document.createElement('div');
  el.className = 'ctx-menu';
  el.setAttribute('role', 'menu');
  const item = (key, label, icon) =>
    `<button role="menuitem" data-act="${key}"><svg width="15" height="15" viewBox="0 0 24 24"><use href="#${icon}"/></svg>${label}</button>`;
  el.innerHTML =
    item('copy', 'Copy text', 'i-pencil') +
    item('link', 'Copy link to message', 'i-link') +
    item('aloud', 'Read aloud', 'i-mic') +
    item('window', 'Open chat in new window', 'i-forward');
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  const w = el.offsetWidth || 220;
  const h = el.offsetHeight || 160;
  el.style.left = `${Math.min(Math.max(8, r.left), innerWidth - w - 8)}px`;
  el.style.top = `${r.bottom + h > innerHeight - 8 ? Math.max(8, r.top - h) : r.bottom + 4}px`;
  convMenuEl = el;
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    closeConvMenu();
  };
  const onDoc = (e) => {
    if (!el.contains(e.target)) done();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') done();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);
  el.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      done();
      try {
        if (b.dataset.act === 'copy') {
          await navigator.clipboard.writeText(m.body || '');
          toastOk('Copied');
        } else if (b.dataset.act === 'link') {
          await navigator.clipboard.writeText(messageLink(state.active.id, m.id));
          toastOk('Link copied');
        } else if (b.dataset.act === 'aloud') {
          const u = new SpeechSynthesisUtterance(`${m.senderName} said: ${m.body}`);
          speechSynthesis.cancel();
          speechSynthesis.speak(u);
        } else if (b.dataset.act === 'window') {
          window.open(`${location.origin}/?chat=${encodeURIComponent(state.active.id)}`, '_blank', 'width=960,height=720');
        }
      } catch (err) {
        toastErr(err);
      }
    };
  });
}

export function messageLink(convId, msgId) {
  return `${location.origin}/?chat=${encodeURIComponent(convId)}&msg=${encodeURIComponent(msgId)}`;
}

/** Open a conversation and scroll to one message, loading older pages if needed. */
export async function openConversationAt(convId, msgId) {
  const c = await openConversationById(convId);
  for (let page = 0; page < 6 && msgId && !state.messages.some((m) => m.id === msgId); page++) {
    const oldest = state.messages[0];
    if (!oldest) break;
    const res = await messaging.messages(c.id, { before: new Date(oldest.createdAt).getTime(), limit: 60 });
    const older = res.messages ?? [];
    if (!older.length) break;
    state.messages = [...older, ...state.messages];
  }
  renderThread();
  if (msgId) {
    const el = document.querySelector(`[data-msg="${CSS.escape(msgId)}"]`);
    if (el) {
      el.scrollIntoView({ block: 'center' });
      el.classList.add('flash');
      setTimeout(() => el.classList.remove('flash'), 1400);
    }
  }
  return c;
}

// Desktop notifications for what arrives while you are not looking: every
// message from somebody else when the tab is hidden or the thread is not the
// one on screen; urgent ones insist. Permission is asked for once, when you
// first send something, which is the moment a person has decided to use this.
// cue plays one of Aether's sounds (sounds.js) if the person's level, do
// not disturb and quiet hours allow it. Every sound in the client goes
// through here, so the policy is in one place.
export function cue(kind, opts = {}) {
  if (!shouldPlay(kind, { level: soundLevel(), dnd: isDND(), quiet: inQuietHours(), ...opts })) return false;
  return playSound(kind);
}

export function maybeNotify(m, conv) {
  if (m.system || m.senderId === session.user?.id) return;
  // Do not disturb means exactly that; an urgent message is the one thing
  // that still comes through.
  // A message that names you is a notification in its own right: it comes
  // through a muted conversation and through quiet hours, and says who
  // mentioned you where. Do not disturb still holds, urgent excepted.
  const me = session.user?.id;
  const mentioned = !!m.mentions?.includes(me);
  // A reply to something you wrote is addressed to you as surely as a
  // mention is, and is treated the same.
  const replied = !!m.quoted && m.quoted.senderId === me;
  const personal = mentioned || replied;
  if (isDND() && m.priority !== 'urgent') return;
  if (inQuietHours() && m.priority !== 'urgent' && !personal) return;
  const looking = !document.hidden && document.hasFocus() && state.active?.id === m.conversationId && state.threadTab === 'chat';
  if (looking) return;
  if (conv?.muted && m.priority !== 'urgent' && !personal) return;
  // Heard before it is seen: the chime for a message, the longer one for
  // a message about you.
  cue(personal || m.priority === 'urgent' ? 'mention' : 'message', { about: personal || conv?.kind === 'direct', urgent: m.priority === 'urgent' });
  const where = conv && conv.kind !== 'direct' ? conv.display : '';
  const title = mentioned
    ? `${m.senderName} mentioned you${where ? ' in ' + where : ''}`
    : replied
      ? `${m.senderName} replied to you${where ? ' in ' + where : ''}`
      : conv?.display || m.senderName;
  const body = (m.priority === 'urgent' ? 'URGENT · ' : '') + (plainText(m.body || '') || (m.attachments?.length ? '📎 ' + m.attachments[0].name : ''));
  // In the desktop shell the notification is Windows' own, from the shell;
  // the browser's Notification API is for browsers.
  if (typeof window.aetherNotify === 'function') {
    window.aetherNotify(title, body.slice(0, 240));
    return;
  }
  if (typeof Notification === 'undefined' || Notification.permission !== 'granted') return;
  try {
    const n = new Notification(conv?.display || m.senderName, {
      body: (m.priority === 'urgent' ? 'URGENT · ' : '') + (m.body || (m.attachments?.length ? '📎 ' + m.attachments[0].name : '')),
      tag: m.conversationId,
      requireInteraction: m.priority === 'urgent',
    });
    n.onclick = () => {
      window.focus();
      openConversationAt(m.conversationId, m.id).catch(() => {});
      n.close();
    };
  } catch {}
}

// A member's display name, from the conversation's members or the cache a
// big conversation's people panel filled.
function memberName(userId) {
  const c = state.active;
  const m = c?.members?.find((x) => x.userId === userId) ?? state.memberCache.get(c?.id)?.find((x) => x.userId === userId);
  if (m) return m.name || m.email;
  if (userId === session.user?.id) return session.user.displayName || session.user.email;
  return '';
}

// The body with each "@Name" it names lit up, over the formatted text.
// Escaped and formatted first, matched after, longest names first so
// "@Priya Sharma" is not eaten by "@Priya".
function withMentions(m) {
  let html = renderBody(m.body);
  if (m.mentions?.length > 1 || /@(everyone|all|channel)\b/i.test(m.body)) {
    const me = m.mentions?.includes(session.user?.id);
    html = html.replace(/(^|\s)@(everyone|all|channel)\b/gi, (_, pre, w) => `${pre}<span class="mention${me ? ' me' : ''}">@${w}</span>`);
  }
  const names = (m.mentions ?? [])
    .map((id) => ({ id, name: memberName(id) }))
    .filter((x) => x.name)
    .sort((a, b) => b.name.length - a.name.length);
  for (const { id, name } of names) {
    const needle = esc('@' + name);
    html = html.split(needle).join(`<span class="mention${id === session.user?.id ? ' me' : ''}">${needle}</span>`);
  }
  return html;
}

// "Seen" under the newest of my messages that somebody has read, in
// conversations small enough for it to mean anything.
// The receipt beside one of your messages: one tick for sent, a filled
// double tick for seen (all of them, in a group), a double tick with a count
// while some have read it. Clicking it in a group lists who has and who has
// not. Drawn from the members' read cursors, which the server pushes, so it
// moves the moment the other person opens the thread.
function receiptIcon(m, standalone = false) {
  const c = state.active;
  const st = receiptState(m, c, session.user?.id);
  if (st.kind === 'none') return '';
  const title = receiptTitle(st, c);
  const group = c.kind !== 'direct';
  const ico = st.kind === 'sent' ? 'i-check' : 'i-check-double';
  const count = group && st.kind === 'seen' ? `<span class="rc-n">${st.readers.length}</span>` : '';
  const tag = group ? 'button' : 'span';
  return `<${tag} class="receipt ${st.kind}${standalone ? ' standalone' : ''}" ${group ? `type="button" data-receipt="${esc(m.id)}" aria-haspopup="dialog"` : ''}
      title="${esc(title)}" aria-label="${esc(title)}">
    <svg width="14" height="14" viewBox="0 0 24 24"><use href="#${ico}"/></svg>${count}</${tag}>`;
}

// What a message said before its edits.
let historyPopEl = null;
async function historyPopover(id, anchor) {
  historyPopEl?.remove();
  const el = document.createElement('div');
  el.className = 'react-pop hist-pop';
  el.setAttribute('role', 'dialog');
  el.setAttribute('aria-label', 'Edit history');
  el.innerHTML = '<div class="rc-head">Edit history</div><div class="loading">Loading…</div>';
  document.body.appendChild(el);
  historyPopEl = el;
  const r = anchor.getBoundingClientRect();
  el.style.left = `${Math.min(Math.max(8, r.left), innerWidth - (el.offsetWidth || 280) - 8)}px`;
  el.style.top = `${r.bottom + 6}px`;
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    el.remove();
    if (historyPopEl === el) historyPopEl = null;
  };
  const onDoc = (ev) => {
    if (!el.contains(ev.target)) done();
  };
  const onKey = (ev) => {
    if (ev.key === 'Escape') done();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);
  try {
    const res = await messaging.history(id);
    const m = state.messages.find((x) => x.id === id) ?? state.threadOpen?.replies.find((x) => x.id === id);
    const rows = (res.edits ?? []).map((e) => `<div class="hist-row"><time>${esc(whenFull(e.editedAt))}</time><div class="hist-body">${renderBody(e.body)}</div></div>`);
    if (m) rows.push(`<div class="hist-row now"><time>Now</time><div class="hist-body">${renderBody(m.body)}</div></div>`);
    el.innerHTML = `<div class="rc-head">Edit history</div><div class="hist-list">${rows.join('') || '<div class="rc-empty">No earlier versions.</div>'}</div>`;
    el.style.left = `${Math.min(Math.max(8, r.left), innerWidth - el.offsetWidth - 8)}px`;
    if (r.bottom + 6 + el.offsetHeight > innerHeight - 8) el.style.top = `${Math.max(8, r.top - el.offsetHeight - 6)}px`;
  } catch (e) {
    el.innerHTML = `<div class="rc-head">Edit history</div><div class="rc-empty">${esc(e.message || 'Could not load.')}</div>`;
  }
}

// Who has read, who has not: the popover a group receipt opens.
let receiptPopEl = null;
function closeReceiptPopover() {
  if (receiptPopEl) {
    receiptPopEl.remove();
    receiptPopEl = null;
  }
}
function receiptPopover(m, anchor) {
  closeReceiptPopover();
  const c = state.active;
  const me = session.user?.id;
  const st = receiptState(m, c, me);
  if (st.kind === 'none') return;
  const notYet = unreadBy(m, c.members, me);
  const row = (x) => `<div class="rc-row"><span ${avatarAttrs(x, 'sm')}>${esc(initials(x))}</span><span class="rc-name">${esc(x.name || x.email)}</span>
    ${x.lastRead && new Date(x.lastRead) >= new Date(m.createdAt) ? `<span class="rc-when">${esc(when(x.lastRead))}</span>` : ''}</div>`;
  const el = document.createElement('div');
  el.className = 'react-pop rc-pop';
  el.setAttribute('role', 'dialog');
  el.setAttribute('aria-label', 'Read by');
  el.innerHTML = `
    <div class="rc-head"><svg width="14" height="14" viewBox="0 0 24 24"><use href="#i-eye"/></svg>Read by ${st.readers.length} of ${st.total}</div>
    ${st.readers.length ? `<div class="rc-list">${st.readers.map(row).join('')}</div>` : '<div class="rc-empty">Nobody yet</div>'}
    ${notYet.length ? `<div class="rc-sub">Not yet</div><div class="rc-list muted">${notYet.slice(0, 20).map(row).join('')}${
      notYet.length > 20 ? `<div class="rc-more">and ${notYet.length - 20} more</div>` : ''
    }</div>` : ''}`;
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  const w = el.offsetWidth || 260;
  const h = el.offsetHeight || 120;
  el.style.left = `${Math.min(Math.max(8, r.left - w + r.width), innerWidth - w - 8)}px`;
  el.style.top = `${r.bottom + 6 + h > innerHeight - 8 ? Math.max(8, r.top - h - 6) : r.bottom + 6}px`;
  receiptPopEl = el;
  const done = () => {
    document.removeEventListener('click', onDoc);
    document.removeEventListener('keydown', onKey);
    closeReceiptPopover();
  };
  const onDoc = (e) => {
    if (!el.contains(e.target)) done();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') done();
  };
  setTimeout(() => document.addEventListener('click', onDoc), 0);
  document.addEventListener('keydown', onKey);
}

function seenBy(m) {
  const c = state.active;
  if (!c || m.senderId !== session.user?.id || !c.members?.length || c.members.length > 40) return '';
  const last = [...state.messages].reverse().find((x) => x.senderId === session.user?.id && !x.deleted && !x.system);
  if (!last || last.id !== m.id) return '';
  const at = new Date(m.createdAt).getTime();
  const readers = c.members.filter((x) => x.userId !== session.user?.id && new Date(x.lastRead).getTime() >= at);
  if (!readers.length) return '';
  if (c.kind === 'direct') return '<div class="seen">Seen</div>';
  const names = readers.map((x) => (x.name || x.email).split(/[ @]/)[0]);
  const shown = names.slice(0, 3).join(', ') + (names.length > 3 ? ` and ${names.length - 3} more` : '');
  return `<div class="seen">Seen by ${esc(shown)}</div>`;
}

// The @ picker in the composer: members of this conversation, filtered by
// what follows the @, inserted as "@Name " and remembered for the send.
async function membersForPicker() {
  const c = state.active;
  if (!c) return [];
  if (c.members?.length) return c.members;
  if (state.memberCache.has(c.id)) return state.memberCache.get(c.id);
  try {
    const res = await messaging.members(c.id, 0, 200);
    state.memberCache.set(c.id, res.members ?? []);
    return res.members ?? [];
  } catch {
    return [];
  }
}

function attachMentionPicker(input, host) {
  let items = [];
  let index = 0;
  let range = null; // [start, end) of the "@query" in the input

  const close = () => {
    host.hidden = true;
    host.innerHTML = '';
    items = [];
    range = null;
  };
  const paint = () => {
    if (!items.length) {
      close();
      return;
    }
    host.hidden = false;
    host.innerHTML = items
      .map(
        (m, i) => `<button type="button" class="mention-row${i === index ? ' active' : ''}" data-i="${i}">
          <span ${avatarAttrs(m, 'sm')}>${esc(initials(m))}</span>
          <span class="mention-name">${esc(m.name || m.email)}</span>
          <span class="mention-email">${esc(m.email)}</span>
        </button>`
      )
      .join('');
    host.querySelectorAll('[data-i]').forEach((b) => {
      b.onmousedown = (e) => {
        e.preventDefault(); // keep the caret in the input
        choose(Number(b.dataset.i));
      };
    });
  };
  const choose = (i) => {
    const m = items[i];
    if (!m || !range) return;
    const name = m.name || m.email;
    const before = input.value.slice(0, range[0]);
    const after = input.value.slice(range[1]);
    input.value = `${before}@${name} ${after}`;
    const caret = before.length + name.length + 2;
    input.setSelectionRange(caret, caret);
    state.draftMentions.set('@' + name, m.userId);
    close();
    input.focus();
  };
  const update = async () => {
    const caret = input.selectionStart ?? input.value.length;
    const upto = input.value.slice(0, caret);
    const at = upto.lastIndexOf('@');
    if (at < 0 || (at > 0 && !/\s/.test(upto[at - 1]))) {
      close();
      return;
    }
    const q = upto.slice(at + 1);
    if (/\s{2,}/.test(q) || q.length > 40) {
      close();
      return;
    }
    const members = (await membersForPicker()).filter((m) => m.userId !== session.user?.id);
    const needle = q.toLowerCase();
    items = members
      .filter((m) => !needle || (m.name || '').toLowerCase().includes(needle) || (m.email || '').toLowerCase().startsWith(needle))
      .slice(0, 7);
    // "@everyone" notifies every member of a group or channel.
    if (state.active && state.active.kind !== 'direct' && (!needle || 'everyone'.startsWith(needle) || 'channel'.startsWith(needle) || 'all'.startsWith(needle))) {
      items.unshift({ userId: '*', name: 'everyone', email: 'Notify all members', everyone: true });
    }
    index = 0;
    range = [at, caret];
    paint();
  };
  input.addEventListener('input', update);
  input.addEventListener('click', update);
  input.addEventListener('keydown', (e) => {
    if (host.hidden) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      index = (index + 1) % items.length;
      paint();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      index = (index - 1 + items.length) % items.length;
      paint();
    } else if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault();
      e.stopImmediatePropagation();
      choose(index);
    } else if (e.key === 'Escape') {
      close();
    }
  }, true);
  input.addEventListener('blur', () => setTimeout(close, 120));
}

// The ids to send: the names still present in the text.
// "@all", "@everyone", "@channel": typed by hand or picked, in a group or
// channel, they are the whole room. The server expands "*" to the members.
const EVERYONE_RE = /(^|\s)@(all|everyone|channel)\b/i;
function mentionsIn(body) {
  const out = [];
  for (const [text, id] of state.draftMentions) {
    if (body.includes(text) && !out.includes(id)) out.push(id);
  }
  if (state.active && state.active.kind !== 'direct' && EVERYONE_RE.test(body) && !out.includes('*')) out.push('*');
  return out;
}

// No avatar on a message. The thread header already says who the other
// person is, and in a two-person thread a face beside every line is a column
// of the same two letters. The name carries the run; the bubble's side
// carries the direction.
function bubble(m, grouped = false) {
  const mine = m.senderId === session.user?.id;
  if (m.system) {
    return `<div class="sys-row" data-msg="${esc(m.id)}">
      <span>${esc(mine ? 'You' : m.senderName)} ${esc(m.body)}</span>
      <time>${esc(whenFull(m.createdAt))}</time>
    </div>`;
  }
  const head = grouped
    ? ''
    : `<div class="msg-who">${
        mine ? '' : `<button class="who-link" data-person="${esc(m.senderId)}" data-person-name="${esc(m.senderName)}">${esc(m.senderName)}</button> · `
      }${esc(
        whenFull(m.createdAt)
      )}${m.editedAt || m.edited ? ` · <button class="edited-tag" data-history="${esc(m.id)}" title="See what it said before">edited</button>` : ''}</div>`;

  if (m.deleted) {
    return `<div class="msg ${mine ? 'mine' : ''}${grouped ? ' grouped' : ''}" data-msg="${esc(m.id)}">
      <div class="msg-body">${head}
        <div class="bubble gone">Message deleted</div>
      </div>
    </div>`;
  }

  return `<div class="msg ${mine ? 'mine' : ''}${grouped ? ' grouped' : ''}" data-msg="${esc(m.id)}">
    <div class="msg-body">${head}
      ${quoteRow(m)}
      ${m.forwarded ? '<div class="fwd-tag"><svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-forward"/></svg>Forwarded</div>' : ''}
      ${priorityTag(m)}
      ${scheduledTag(m)}
      ${cardOf(m)}
      ${attachmentsRow(m)}
      ${m.body && !isPollMessage(m) ? `<div class="bubble-line">${receiptIcon(m)}<div class="bubble${isOnlyEmoji(m.body) ? ' jumbo' : ''}${m.saved ? ' saved' : ''}${
        m.mentions?.includes(session.user?.id) ? ' mentions-me' : ''
      }">${withMentions(m)}</div></div>` : receiptIcon(m, true)}
      ${reactionRow(m)}
      ${threadPill(m)}
      ${actionBar(m, mine)}
    </div>
  </div>`;
}

// "3 replies · Priya, Sam · 2:14 PM" under a channel post, and "Reply" on a
// post with none yet. Opens the thread pane.
function threadPill(m) {
  if (state.active?.kind !== 'channel' || m.replyTo || m.system || m.deleted) return '';
  const t = m.thread;
  const open = state.threadOpen?.rootId === m.id;
  if (!t?.count) {
    return `<button class="thread-pill empty${open ? ' open' : ''}" data-thread="${esc(m.id)}">
      <svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-thread"/></svg>Reply</button>`;
  }
  const names = (t.lastNames ?? []).slice(0, 2).map((n) => esc(n.split(' ')[0])).join(', ');
  return `<button class="thread-pill${open ? ' open' : ''}" data-thread="${esc(m.id)}" title="Open the thread">
    <svg width="13" height="13" viewBox="0 0 24 24"><use href="#i-thread"/></svg>
    <b>${t.count} ${t.count === 1 ? 'reply' : 'replies'}</b>${names ? ` · ${names}` : ''}${t.lastAt ? ` · <span class="thread-when">${esc(when(t.lastAt))}</span>` : ''}
  </button>`;
}

// The thread pane: the root, its replies, and a composer that replies to
// the root. A side pane beside the feed on a wide screen, a sheet over it on
// a phone (CSS). Loaded once per opening; pushes append.
async function openThread(rootId) {
  const root = state.messages.find((x) => x.id === rootId);
  if (!root) return;
  state.threadOpen = { rootId, replies: [], loading: true };
  renderThread();
  try {
    const res = await messaging.replies(rootId);
    if (state.threadOpen?.rootId !== rootId) return;
    state.threadOpen.replies = res.messages ?? [];
    state.threadOpen.loading = false;
    for (const r of state.threadOpen.replies) rememberReply(r.id);
    // The summary on the root follows the truth we just loaded.
    root.thread = root.thread ?? { count: 0, lastNames: [] };
    root.thread.count = state.threadOpen.replies.length;
    renderThread();
  } catch (e) {
    state.threadOpen = null;
    renderThread();
    toastErr(e);
  }
}
function closeThread() {
  state.threadOpen = null;
  renderThread();
}
function threadPaneHTML() {
  const t = state.threadOpen;
  if (!t) return '';
  const root = state.messages.find((x) => x.id === t.rootId);
  if (!root) return '';
  const grouped = groupRuns(t.replies, () => false);
  return `<aside class="thread-pane" aria-label="Thread">
    <div class="tp-head">
      <b>Thread</b>
      <span class="tp-count">${t.replies.length} ${t.replies.length === 1 ? 'reply' : 'replies'}</span>
      <button class="icon-btn" id="tp-close" title="Close thread" aria-label="Close thread"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-close"/></svg></button>
    </div>
    <div class="tp-log" id="tp-log">
      <div class="tp-root">${bubble({ ...root, thread: undefined }, false)}</div>
      ${t.loading ? '<div class="loading">Loading…</div>' : t.replies.length ? `<div class="tp-replies">${t.replies.map((r, i) => bubble({ ...r, quoted: undefined, replyTo: '' }, grouped[i])).join('')}</div>` : '<div class="tp-empty">No replies yet. Start the thread.</div>'}
    </div>
    <div class="tp-composer">
      <input id="tp-input" class="input" placeholder="Reply in thread…" autocomplete="off" />
      <button class="send" id="tp-send" title="Reply"><svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-send"/></svg></button>
    </div>
  </aside>`;
}
async function sendThreadReply() {
  const t = state.threadOpen;
  const input = $('tp-input');
  if (!t || !input) return;
  const body = input.value.trim();
  if (!body) return;
  input.value = '';
  try {
    const m = await messaging.send(state.active.id, body, t.rootId);
    // The push confirms; but the pane shows it now.
    if (m?.id && !t.replies.some((x) => x.id === m.id)) {
      t.replies.push(m);
      const root = state.messages.find((x) => x.id === t.rootId);
      if (root) {
        root.thread = root.thread ?? { count: 0, lastNames: [] };
        if (!knownReply(m.id)) {
          root.thread.count++;
          root.thread.lastAt = m.createdAt;
          root.thread.lastNames = [m.senderName, ...(root.thread.lastNames ?? []).filter((n) => n !== m.senderName)].slice(0, 3);
          rememberReply(m.id);
        }
      }
      renderThread();
      $('tp-input')?.focus();
    }
  } catch (e) {
    input.value = body;
    toastErr(e);
  }
}

// ---------------------------------------------------------------------------
// New conversation
// ---------------------------------------------------------------------------

/**
 * Open (creating if needed) the direct conversation with one account, and
 * show it. The entry point for the title-bar search: a person chosen there
 * lands in their thread, which is what choosing a person means everywhere
 * else in the product.
 */
export async function openChatWith(userId) {
  const conv = await messaging.openDirectById(userId);
  await refresh();
  const found = state.conversations.find((c) => c.id === conv.id) ?? conv;
  await open(found);
  return found;
}

/** Open a conversation already in the list, by id. */
export async function openConversationById(id) {
  let c = state.conversations.find((x) => x.id === id);
  if (!c) {
    await refresh();
    c = state.conversations.find((x) => x.id === id);
  }
  if (!c) throw new Error('That conversation is not in your list.');
  await open(c);
  return c;
}

/**
 * Your own thread. The most-used one in every product that has it -- it is
 * where a link, a code or a thought goes on its way between two machines.
 */
export async function openSelfChat() {
  const conv = await messaging.openSelf();
  await refresh();
  const found = state.conversations.find((c) => c.id === conv.id) ?? conv;
  await open(found);
  return found;
}

// ---------------------------------------------------------------------------
// Groups and channels
// ---------------------------------------------------------------------------

// The card for one person: who they are, how they are, and the ways to
// reach them. Opens from a name or an avatar anywhere -- the chat header,
// a message, the members list, a meeting's people panel -- through the
// [data-person] delegation below. Their address lives here, not in the
// header. Actions are the three things one does with a person: message,
// audio call, video call; none of them for yourself.
export async function openPersonCard(userId, hint = {}) {
  if (!userId) return;
  const me = userId === session.user?.id;
  let who = { userId, name: hint.name || '', email: hint.email || '' };
  if (me) who = { userId, name: session.user?.displayName || who.name, email: session.user?.email || who.email };
  for (const c of state.conversations) {
    const m = c.members?.find((x) => x.userId === userId);
    if (m) {
      who.name = who.name || m.name || '';
      who.email = who.email || m.email || '';
    }
  }
  const { host, close, arm } = dialogHost();
  const paint = () => {
    const st = statusOf(userId);
    const online = isOnline(userId);
    const status = online ? st.status : 'offline';
    host.innerHTML = `
      <div class="dialog-card person-card" role="dialog" aria-modal="true" aria-labelledby="pc-name">
        <div class="pc-head">
          <span class="av-wrap"><span ${avatarAttrs(who, 'lg')}>${esc(initials(who))}</span>${personPresence(userId)}</span>
          <div class="pc-id">
            <h2 id="pc-name">${esc(who.name || who.email || 'Someone')}${me ? ' <small>(you)</small>' : ''}</h2>
            <div class="pc-status"><span class="st-word ${esc(status)}">${esc(personLine(userId).split(' · ')[0])}</span>${
              personLine(userId).includes(' · ') ? ' · ' + esc(personLine(userId).split(' · ').slice(1).join(' · ')) : ''
            }</div>
            ${who.email ? `<a class="pc-mail" href="mailto:${esc(who.email)}">${esc(who.email)}</a>` : ''}
          </div>
        </div>
        ${
          me
            ? ''
            : `<div class="pc-actions">
          <button class="btn" data-pc-message><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-chat"/></svg>Message</button>
          <button class="btn" data-pc-call="1"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-mic"/></svg>Audio call</button>
          <button class="btn btn-primary" data-pc-call="0"><svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-cam"/></svg>Video call</button>
        </div>`
        }
        <div class="dialog-actions"><span class="spacer"></span><button class="btn" data-cancel>Close</button></div>
      </div>`;
    arm();
    host.querySelector('[data-pc-message]')?.addEventListener('click', async () => {
      close();
      try {
        await openChatWith(userId);
      } catch {}
    });
    host.querySelectorAll('[data-pc-call]').forEach((b) => {
      b.onclick = () => {
        close();
        window.dispatchEvent(
          new CustomEvent('aether:call', { detail: { targetId: userId, targetName: who.name || who.email, audioOnly: b.dataset.pcCall === '1' } })
        );
      };
    });
  };
  paint();
  // The freshest word on them, then paint again if it changed anything.
  await loadStatuses([userId]);
  if (!host.classList.contains('hidden') && host.querySelector('.person-card')) paint();
}

function dialogHost() {
  const host = $('dialog');
  const close = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
  };
  const arm = () => {
    host.classList.remove('hidden');
    host.querySelectorAll('[data-cancel]').forEach((b) => (b.onclick = close));
    host.onclick = (e) => {
      if (e.target === host) close();
    };
  };
  return { host, close, arm };
}

// A people picker: one field, contacts instantly, the directory merged.
// Returns the chosen people and repaints itself; used by "new group" and
// "add people".
// What "nobody" means here, said precisely. The directory reaches the
// people you already share a conversation with, everyone at your
// organisation's own email domains, and one address typed in full. A
// deployment without an organisation domain -- everybody on a public
// provider -- has only the first and the last, and the person searching
// deserves to be told that rather than shown an empty list.
function plainNobody(q) {
  const looksLikeEmail = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(q);
  if (looksLikeEmail) return `Nobody has signed up with ${esc(q)} yet.`;
  return `Nobody you chat with is called “${esc(q)}”. To add someone new, type their full email address.`;
}
function nobodyFound(q) {
  const admin = typeof session.can === 'function' && session.can('user.read');
  return `<div class="empty nobody">
    <p>${plainNobody(q)}</p>
    ${
      admin
        ? `<p class="hint">Want people to find each other by name? In the console, set your organisation's email domains, or open the address book to everyone.
             <button type="button" class="link" data-open-policy>Open console</button></p>`
        : ''
    }
  </div>`;
}

function peoplePicker(searchEl, listEl, pickedEl, onChange, { exclude = new Set() } = {}) {
  const chosen = new Map();
  let lastResults = [];
  let lastQuery = '';
  const paintChosen = () => {
    pickedEl.innerHTML = [...chosen.values()]
      .map(
        (p) => `<span class="chip-person" data-drop="${esc(p.id)}">
          ${esc(p.name)}<svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-close"/></svg>
        </span>`
      )
      .join('');
    pickedEl.querySelectorAll('[data-drop]').forEach((el) => {
      el.onclick = () => {
        chosen.delete(el.dataset.drop);
        paintChosen();
        paintResults(lastResults, lastQuery);
        onChange([...chosen.values()]);
      };
    });
  };
  const paintResults = (people, q) => {
    lastResults = people;
    lastQuery = q;
    const rows = people.filter((p) => !exclude.has(p.id));
    if (!rows.length) {
      listEl.innerHTML = q ? nobodyFound(q) : '<div class="empty">Start typing a name, or paste an email address.</div>';
      listEl.querySelector('[data-open-policy]')?.addEventListener('click', () => {
        document.dispatchEvent(new CustomEvent('aether:open-console', { detail: 'policy' }));
      });
      return;
    }
    listEl.innerHTML = rows
      .map(
        (p) => `<button type="button" class="person-row ${chosen.has(p.id) ? 'picked' : ''}" data-p="${esc(p.id)}">
          <span ${avatarAttrs(p)}>${esc(initials(p))}</span>
          <span class="person-main">
            <span class="person-n">${highlight(p.name, q)}</span>
            <span class="person-e">${highlight(p.email, q)}</span>
          </span>
          ${chosen.has(p.id) ? '<span class="tick">✓</span>' : ''}
        </button>`
      )
      .join('');
    listEl.querySelectorAll('[data-p]').forEach((b) => {
      b.onclick = () => {
        const p = rows.find((x) => x.id === b.dataset.p);
        if (!p) return;
        if (chosen.has(p.id)) chosen.delete(p.id);
        else chosen.set(p.id, p);
        paintChosen();
        paintResults(people, q);
        onChange([...chosen.values()]);
      };
    });
  };
  // Everyone you already share a conversation with -- 1:1s first, then the
  // people in your groups and channels -- so a colleague from any of your
  // rooms is matched instantly and offline, before the server answers.
  const local = () => {
    const out = frequentContacts(200, session.user?.id).map((c) => ({ id: c.userId, userId: c.userId, name: c.name, email: c.email }));
    const seen = new Set(out.map((p) => p.id));
    for (const c of state.conversations) {
      if (c.kind === 'direct') continue;
      for (const m of c.members ?? []) {
        if (!m.userId || m.userId === session.user?.id || seen.has(m.userId)) continue;
        seen.add(m.userId);
        out.push({ id: m.userId, userId: m.userId, name: m.name || m.email || 'Someone', email: m.email ?? '' });
      }
    }
    return out;
  };
  const searcher = createSearch(
    async (q) => {
      const near = rankPeople(local(), q, 12);
      if (!q) return near;
      let remote = [];
      try {
        remote = ((await directory.search(q, 20)).people ?? []).map((p) => ({ ...p, userId: p.id }));
      } catch {
        // contacts still stand
      }
      return mergePeople(near, rankPeople(remote, q, 20)).slice(0, 20);
    },
    (q, people) => paintResults(people, q)
  );
  searchEl.oninput = (e) => searcher.input(e.target.value.trim());
  searcher.input('', { immediate: true });
  paintChosen();
  return {
    chosen,
    cancel: () => searcher.cancel(),
    add: (p) => {
      chosen.set(p.id, p);
      paintChosen();
      onChange([...chosen.values()]);
    },
  };
}

function newGroupDialog({ preset = [] } = {}) {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="g-title">
      <h2 id="g-title">New group</h2>
      <label class="field"><span>Group name</span>
        <input id="g-name" class="input" placeholder="Launch team" maxlength="120" autocomplete="off" /></label>
      <input id="g-search" class="input" placeholder="Add people by name or email" autocomplete="off" style="margin-top:10px" />
      <div class="picked" id="g-picked"></div>
      <div class="people-list" id="g-people"></div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm disabled>Create group</button>
      </div>
    </div>`;
  arm();
  const confirmBtn = host.querySelector('[data-confirm]');
  const picker = peoplePicker($('g-search'), $('g-people'), $('g-picked'), (people) => {
    confirmBtn.disabled = people.length === 0;
    confirmBtn.textContent = people.length > 1 ? `Create group (${people.length + 1})` : 'Create group';
  }, { exclude: new Set([session.user?.id]) });
  // "Add people" from a 1:1 starts here with the other person already in.
  for (const p of preset.filter(Boolean)) picker.add(p);
  setTimeout(() => $('g-name')?.focus(), 50);
  confirmBtn.onclick = async () => {
    const people = [...picker.chosen.values()];
    if (!people.length) return;
    confirmBtn.disabled = true;
    try {
      const conv = await messaging.createGroup($('g-name').value.trim(), people.map((p) => p.email));
      picker.cancel();
      close();
      await refresh();
      open(state.conversations.find((c) => c.id === conv.id) ?? conv);
    } catch (e) {
      confirmBtn.disabled = false;
      toastErr(e);
    }
  };
}

function newChannelDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="ch-title">
      <h2 id="ch-title">New channel</h2>
      <label class="field"><span>Name</span>
        <div class="hash-field"><span class="hash">#</span>
          <input id="ch-name" class="input" placeholder="design-reviews" maxlength="60" autocomplete="off" /></div></label>
      <label class="field" style="margin-top:10px"><span>Topic <small>(optional)</small></span>
        <input id="ch-topic" class="input" placeholder="What this channel is for" maxlength="240" autocomplete="off" /></label>
      <div class="seg" role="radiogroup" aria-label="Visibility" style="margin-top:12px">
        <button type="button" class="seg-b active" data-vis="public" role="radio" aria-checked="true">
          <b>Public</b><small>Anyone in your organisation can find and join</small>
        </button>
        <button type="button" class="seg-b" data-vis="private" role="radio" aria-checked="false">
          <b>Private</b><small>Invisible; people are added by members</small>
        </button>
      </div>
      <label class="field" style="margin-top:12px"><span>Members <small>(optional; you can add more later)</small></span>
        <input id="ch-search" class="input" placeholder="Add people by name or email" autocomplete="off" /></label>
      <div class="picked" id="ch-picked"></div>
      <div class="people-list compact" id="ch-people"></div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm disabled>Create channel</button>
      </div>
    </div>`;
  arm();
  const chPicker = peoplePicker($('ch-search'), $('ch-people'), $('ch-picked'), () => {}, {
    exclude: new Set([session.user?.id]),
  });
  let vis = 'public';
  host.querySelectorAll('[data-vis]').forEach((b) => {
    b.onclick = () => {
      vis = b.dataset.vis;
      host.querySelectorAll('[data-vis]').forEach((x) => {
        x.classList.toggle('active', x === b);
        x.setAttribute('aria-checked', String(x === b));
      });
    };
  });
  const confirmBtn = host.querySelector('[data-confirm]');
  const name = $('ch-name');
  name.oninput = () => {
    confirmBtn.disabled = !name.value.trim();
  };
  setTimeout(() => name.focus(), 50);
  confirmBtn.onclick = async () => {
    confirmBtn.disabled = true;
    try {
      const conv = await messaging.createChannel(name.value.trim(), $('ch-topic').value.trim(), vis);
      const people = [...chPicker.chosen.values()];
      chPicker.cancel();
      if (people.length) {
        try {
          const res = await messaging.addMembers(conv.id, people.map((p) => p.email), { mode: 'all' });
          if (res.skipped?.length) toastErr(new Error(`Could not add: ${res.skipped.join(', ')}`));
        } catch (e) {
          toastErr(e);
        }
      }
      close();
      await refresh();
      open(state.conversations.find((c) => c.id === conv.id) ?? conv);
    } catch (e) {
      confirmBtn.disabled = false;
      toastErr(e);
    }
  };
}

function browseChannelsDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card wide" role="dialog" aria-modal="true" aria-labelledby="b-title">
      <h2 id="b-title">Browse channels</h2>
      <input id="b-search" class="input" placeholder="Search channels by name or topic" autocomplete="off" />
      <div class="people-list" id="b-list"><div class="loading">Loading…</div></div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Close</button>
        <button class="btn" data-newch>New channel</button>
      </div>
    </div>`;
  arm();
  host.querySelector('[data-newch]').onclick = () => {
    close();
    newChannelDialog();
  };
  const paint = (rows, q) => {
    const box = $('b-list');
    if (!box) return;
    if (!rows.length) {
      box.innerHTML = `<div class="empty">${q ? 'No channel matches.' : 'No public channels yet. Create the first one.'}</div>`;
      return;
    }
    box.innerHTML = rows
      .map(
        (ch) => `<div class="person-row static">
          <span class="avatar chan">#</span>
          <span class="person-main">
            <span class="person-n">${highlight(ch.title, q)}</span>
            <span class="person-e">${ch.memberCount} member${ch.memberCount === 1 ? '' : 's'}${
              ch.topic ? ' · ' + esc(ch.topic) : ''
            }</span>
          </span>
          <button class="btn sm ${ch.joined ? '' : 'btn-primary'}" data-join="${esc(ch.id)}" data-joined="${ch.joined ? '1' : '0'}">${
            ch.joined ? 'Open' : 'Join'
          }</button>
        </div>`
      )
      .join('');
    box.querySelectorAll('[data-join]').forEach((b) => {
      b.onclick = async () => {
        b.disabled = true;
        try {
          const conv = b.dataset.joined === '1'
            ? state.conversations.find((c) => c.id === b.dataset.join)
            : await messaging.joinChannel(b.dataset.join);
          searcher.cancel();
          close();
          await refresh();
          open(state.conversations.find((c) => c.id === (conv?.id ?? b.dataset.join)) ?? conv);
        } catch (e) {
          b.disabled = false;
          toastErr(e);
        }
      };
    });
  };
  const searcher = createSearch(
    async (q) => (await messaging.browseChannels(q, 50)).channels ?? [],
    (q, rows) => paint(rows, q)
  );
  $('b-search').oninput = (e) => searcher.input(e.target.value.trim());
  searcher.input('', { immediate: true });
  setTimeout(() => $('b-search')?.focus(), 50);
}

// Members of a group or channel: who is here, add people, remove (owner),
// rename (owner for channels, anyone for groups), leave.
async function membersDialog(conv) {
  const { host, close, arm } = dialogHost();
  const isOwner = conv.myRole === 'owner';
  const canRename = conv.kind === 'group' || isOwner;
  host.innerHTML = `
    <div class="dialog-card wide" role="dialog" aria-modal="true" aria-labelledby="m-title">
      <h2 id="m-title">${esc(conv.display)}</h2>
      ${
        canRename
          ? `<div class="rename-row">
               <input id="m-name" class="input" value="${esc(conv.title)}" maxlength="60" aria-label="Name" />
               <input id="m-topic" class="input" value="${esc(conv.topic || '')}" placeholder="Topic" maxlength="240" aria-label="Topic" />
               <button class="btn sm" id="m-save">Save</button>
             </div>`
          : conv.topic
            ? `<p class="dialog-sub">${esc(conv.topic)}</p>`
            : ''
      }
      <div class="pic-row">
        ${convAvatar(conv, 'lg')}
        <label class="btn sm">Change picture<input type="file" id="m-pic" accept="image/png,image/jpeg,image/gif,image/webp" hidden /></label>
        <small>PNG, JPEG, GIF or WebP · up to 5 MB</small>
      </div>
      <div class="tabs"><button class="tab active">Members <span class="count" id="m-count">${conv.memberCount || ''}</span></button></div>
      <div class="people-list" id="m-list"><div class="loading">Loading…</div></div>
      <details class="add-people">
        <summary>Add people</summary>
        <input id="m-search" class="input" placeholder="Search by name or email" autocomplete="off" />
        <div class="picked" id="m-picked"></div>
        <div class="people-list" id="m-people"></div>
        <div class="history-choice" role="radiogroup" aria-label="Chat history for new people">
          <label><input type="radio" name="m-hist" value="none" /> Don't include chat history</label>
          <label><input type="radio" name="m-hist" value="days" /> Include the last <input type="number" id="m-days" class="input tiny" value="7" min="1" max="3650" /> days</label>
          <label><input type="radio" name="m-hist" value="all" checked /> Include all chat history</label>
        </div>
        <div class="dialog-actions"><button class="btn btn-primary" id="m-add" disabled>Add</button></div>
      </details>
      <div class="dialog-actions">
        <button class="btn danger-quiet" id="m-leave">Leave ${conv.kind === 'channel' ? 'channel' : 'group'}</button>
        <span class="spacer"></span>
        <button class="btn" data-cancel>Close</button>
      </div>
    </div>`;
  arm();

  const paintMembers = async () => {
    const box = $('m-list');
    if (!box) return;
    try {
      const res = await messaging.members(conv.id, 0, 200);
      const ms = res.members ?? [];
      $('m-count').textContent = res.total ?? ms.length;
      await loadStatuses(ms.map((m) => m.userId));
      box.innerHTML = ms
        .map(
          (m) => `<div class="person-row static">
            <button class="who-btn av-wrap" data-person="${esc(m.userId)}" data-person-name="${esc(m.name || m.email)}" data-person-email="${esc(m.email)}" aria-label="About ${esc(m.name || m.email)}"><span ${avatarAttrs(m)}>${esc(initials(m))}</span>${personPresence(m.userId)}</button>
            <span class="person-main">
              <span class="person-n"><button class="who-link" data-person="${esc(m.userId)}" data-person-name="${esc(m.name || m.email)}" data-person-email="${esc(m.email)}">${esc(m.name || m.email)}</button>${m.userId === session.user?.id ? ' (you)' : ''}${
                m.role === 'owner' ? ' <span class="role">Owner</span>' : ''
              }</span>
              <span class="person-e">${esc(personLine(m.userId))}</span>
            </span>
            ${
              isOwner && m.userId !== session.user?.id
                ? `<button class="btn sm quiet" data-remove="${esc(m.userId)}" title="Remove">Remove</button>`
                : ''
            }
          </div>`
        )
        .join('');
      box.querySelectorAll('[data-remove]').forEach((b) => {
        b.onclick = async () => {
          b.disabled = true;
          try {
            await messaging.removeMember(conv.id, b.dataset.remove);
            await paintMembers();
            await refresh();
          } catch (e) {
            b.disabled = false;
            toastErr(e);
          }
        };
      });
    } catch (e) {
      toastErr(e);
    }
  };
  paintMembers();

  if (canRename) {
    $('m-save').onclick = async () => {
      try {
        const c = await messaging.rename(conv.id, $('m-name').value.trim(), $('m-topic').value.trim());
        await refresh();
        const fresh = state.conversations.find((x) => x.id === conv.id) ?? c;
        if (state.active?.id === conv.id) {
          state.active = fresh;
          renderThread();
        }
        $('m-title').textContent = fresh.display;
        toastOk('Saved');
      } catch (e) {
        toastErr(e);
      }
    };
  }

  const addBtn = $('m-add');
  const picker = peoplePicker($('m-search'), $('m-people'), $('m-picked'), (people) => {
    addBtn.disabled = people.length === 0;
    addBtn.textContent = people.length > 1 ? `Add ${people.length} people` : 'Add';
  });
  addBtn.onclick = async () => {
    const people = [...picker.chosen.values()];
    if (!people.length) return;
    addBtn.disabled = true;
    try {
      const mode = host.querySelector('input[name="m-hist"]:checked')?.value || 'all';
      const days = Number($('m-days')?.value) || 7;
      const res = await messaging.addMembers(conv.id, people.map((p) => p.email), { mode, days });
      if (res.skipped?.length) toastErr(new Error(`Could not add: ${res.skipped.join(', ')}`));
      else toastOk(people.length > 1 ? `Added ${people.length} people` : `Added ${people[0].name}`);
      picker.chosen.clear();
      $('m-picked').innerHTML = '';
      await paintMembers();
      await refresh();
      if (state.active?.id === conv.id) {
        state.active = state.conversations.find((x) => x.id === conv.id) ?? state.active;
        renderThread();
      }
    } catch (e) {
      addBtn.disabled = false;
      toastErr(e);
    }
  };

  $('m-pic').onchange = async () => {
    const f = $('m-pic').files?.[0];
    if (!f) return;
    try {
      await messaging.setAvatar(conv.id, f);
      await refresh();
      const fresh = state.conversations.find((x) => x.id === conv.id);
      if (fresh) {
        conv.avatar = true;
        host.querySelector('.pic-row .avatar').outerHTML = convAvatar(fresh, 'lg');
        if (state.active?.id === conv.id) {
          state.active = fresh;
          renderThread();
        }
      }
      toastOk('Picture updated');
    } catch (e) {
      toastErr(e);
    }
  };

  $('m-leave').onclick = async () => {
    try {
      await messaging.leave(conv.id);
      picker.cancel();
      close();
      if (state.active?.id === conv.id) state.active = null;
      await refresh();
      renderThread();
    } catch (e) {
      toastErr(e);
    }
  };
}

// ---------------------------------------------------------------------------
// Forward
// ---------------------------------------------------------------------------

/**
 * Pick where a message goes. One field, searched as you type: the chats you
 * already have rank first, then people from the directory (inside the same
 * rules as everywhere else -- names inside the organisation, a full address
 * outside it). Choosing a person you have no chat with opens one on the way.
 */
function forwardDialog(m) {
  const host = $('dialog');
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="f-title">
      <h2 id="f-title">Forward message</h2>
      <div class="fwd-preview">
        <span class="quote-who">${esc(m.senderName)}</span>
        <span class="quote-body">${esc((m.body || '').slice(0, 140))}</span>
      </div>
      <input id="f-search" class="input" placeholder="Search chats and people" autocomplete="off" spellcheck="false" />
      <div class="picked" id="f-picked"></div>
      <div class="people-list" id="f-people"></div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn btn-primary" data-confirm disabled>Forward</button>
      </div>
    </div>`;
  host.classList.remove('hidden');

  let searcher = null; // assigned below; close() may run before that
  const close = () => {
    searcher?.cancel();
    host.classList.add('hidden');
    host.innerHTML = '';
  };
  host.querySelector('[data-cancel]').onclick = close;
  host.onclick = (e) => {
    if (e.target === host) close();
  };

  // key -> {key, convId?, userId?, name, sub}
  const chosen = new Map();
  const confirmBtn = host.querySelector('[data-confirm]');

  const paintChosen = () => {
    const box = $('f-picked');
    if (!box) return;
    box.innerHTML = [...chosen.values()]
      .map(
        (p) => `<span class="chip-person" data-drop="${esc(p.key)}">
          ${esc(p.name)}<svg width="12" height="12" viewBox="0 0 24 24"><use href="#i-close"/></svg>
        </span>`
      )
      .join('');
    box.querySelectorAll('[data-drop]').forEach((el) => {
      el.onclick = () => {
        chosen.delete(el.dataset.drop);
        paintChosen();
        paintResults(lastResults, lastQuery);
      };
    });
    const n = chosen.size;
    confirmBtn.disabled = n === 0 || n > 10;
    confirmBtn.textContent = n > 1 ? `Forward to ${n} chats` : 'Forward';
  };

  let lastResults = [];
  let lastQuery = '';
  const paintResults = (items, q) => {
    lastResults = items;
    lastQuery = q;
    const box = $('f-people');
    if (!box) return;
    if (!items.length) {
      box.innerHTML = `<div class="empty">${
        q ? plainNobody(q) : 'Start typing a name.'
      }</div>`;
      return;
    }
    box.innerHTML = items
      .map(
        (p) => `<button class="person-row ${chosen.has(p.key) ? 'picked' : ''}" data-p="${esc(p.key)}">
          <span ${avatarAttrs(p.who)}>${esc(initials(p.who))}</span>
          <span class="person-main">
            <span class="person-n">${highlight(p.name, q)}</span>
            <span class="person-e">${esc(p.sub)}</span>
          </span>
          ${chosen.has(p.key) ? '<span class="tick">✓</span>' : ''}
        </button>`
      )
      .join('');
    box.querySelectorAll('[data-p]').forEach((b) => {
      b.onclick = () => {
        const p = items.find((x) => x.key === b.dataset.p);
        if (!p) return;
        if (chosen.has(p.key)) chosen.delete(p.key);
        else chosen.set(p.key, p);
        paintChosen();
        paintResults(items, q);
      };
    });
  };

  // Existing chats carry their conversation id; people from the directory
  // carry an account id and get a chat opened for them on confirm.
  const chatItems = () =>
    state.conversations.map((c) => ({
      key: 'c:' + c.id,
      convId: c.id,
      name: c.display,
      email: c.kind === 'direct' ? peerOf(c)?.email ?? '' : '',
      sub: c.kind === 'group' ? `${c.members?.length ?? 0} people` : c.selfChat ? 'Your notes' : 'Chat',
      who: whoOf(c),
    }));

  searcher = createSearch(
    async (q) => {
      const chats = q ? rankPeople(chatItems(), q, 8) : chatItems().slice(0, 8);
      if (!q) return chats;
      let people = [];
      try {
        people = ((await directory.search(q, 10)).people ?? []).map((p) => ({
          key: 'u:' + p.id,
          userId: p.id,
          name: p.name,
          email: p.email,
          sub: p.email,
          who: { userId: p.id, name: p.name, email: p.email },
        }));
      } catch {
        // The chats still stand.
      }
      // A person you already chat with is the chat, not a second row.
      const known = new Set(
        state.conversations.filter((c) => c.kind === 'direct').map((c) => peerOf(c)?.userId)
      );
      return [...chats, ...people.filter((p) => !known.has(p.userId))].slice(0, 12);
    },
    (q, items) => paintResults(items, q)
  );
  $('f-search').oninput = (e) => searcher.input(e.target.value.trim());
  searcher.input('', { immediate: true });
  paintChosen();
  setTimeout(() => $('f-search')?.focus(), 50);

  confirmBtn.onclick = async () => {
    const picks = [...chosen.values()];
    if (!picks.length) return;
    confirmBtn.disabled = true;
    try {
      const convIds = [];
      for (const p of picks) {
        if (p.convId) convIds.push(p.convId);
        else convIds.push((await messaging.openDirectById(p.userId)).id);
      }
      const res = await messaging.forward(m.id, convIds);
      close();
      // The sender is not pushed their own messages; put the ones landing in
      // the open thread in place directly.
      for (const fm of res.messages ?? []) {
        if (state.active?.id === fm.conversationId) state.messages.push(fm);
      }
      renderThread();
      await refresh();
      const n = (res.messages ?? []).length;
      toastOk(n > 1 ? `Forwarded to ${n} chats` : 'Forwarded');
    } catch (e) {
      confirmBtn.disabled = false;
      toastErr(e);
    }
  };
}

function toastOk(msg) {
  const el = $('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = 'toast show ok';
  clearTimeout(toastErr._t);
  toastErr._t = setTimeout(() => (el.className = 'toast'), 2500);
}

function toastErr(e) {
  const el = $('toast');
  if (!el) return;
  el.textContent = e instanceof ApiError ? e.message : 'Something went wrong';
  el.className = 'toast show bad';
  clearTimeout(toastErr._t);
  toastErr._t = setTimeout(() => (el.className = 'toast'), 4000);
}

export function unreadTotal() {
  return totalUnread();
}

/**
 * The people you actually talk to, most recent first.
 *
 * Drawn from direct conversations rather than from meeting invitee lists,
 * because those answer different questions. An invitee list is who was on a
 * calendar entry -- which includes the whole team, the room account, and
 * whoever was copied in once. A direct conversation is somebody you chose to
 * speak to, which is what "the people I work with" means.
 *
 * Ranked by last message rather than by message count. Volume rewards the one
 * argument you had in March over the colleague you exchange two lines with
 * every morning, and it is the second person you want one click away.
 */
// The persistent conversation with one account, opened from a meeting.
//
// The point of the feature: a chat that survives the call. Nothing here is
// meeting-scoped -- it is the same conversation as the one in the sidebar,
// reached by account id because that is what the roster carries.
export async function directWith(userId) {
  const conv = await messaging.openDirectById(userId);
  const res = await messaging.messages(conv.id);
  return { conv, messages: res.messages ?? [] };
}

export async function sendDirect(convId, body) {
  return messaging.send(convId, body);
}

/** Chats, groups and channels whose name matches, for the title-bar search. */
export function matchConversations(q, limit = 5) {
  const needle = q.trim().toLowerCase();
  if (!needle) return [];
  return state.conversations
    .filter((c) => c.kind !== 'direct' && (c.display || '').toLowerCase().includes(needle))
    .slice(0, limit)
    .map((c) => ({ id: c.id, display: c.display, kind: c.kind, memberCount: c.memberCount }));
}

export function frequentContacts(limit = 8, selfId = '') {
  const out = [];
  const seen = new Set();

  const direct = state.conversations
    .filter((c) => c.kind === 'direct')
    .sort(
      (a, b) => new Date(b.lastMessageAt ?? 0) - new Date(a.lastMessageAt ?? 0)
    );

  for (const c of direct) {
    for (const m of c.members ?? []) {
      if (!m.userId || m.userId === selfId) continue;
      if (seen.has(m.userId)) continue;
      seen.add(m.userId);
      out.push({
        userId: m.userId,
        email: m.email ?? '',
        name: m.name || m.email || 'Someone',
        conversationId: c.id,
        lastAt: c.lastMessageAt ?? null,
        unread: c.unread ?? 0,
      });
      if (out.length >= limit) return out;
    }
  }
  return out;
}

// Message actions, delegated for the same reason the call button is: the
// thread is redrawn on every message, and a handler bound to a button inside
// it is thrown away with the button.
document.addEventListener('click', async (e) => {
  // A name or an avatar anywhere: the person's card.
  const person = e.target.closest?.('[data-person]');
  if (person && person.dataset.person) {
    e.preventDefault();
    e.stopPropagation();
    openPersonCard(person.dataset.person, { name: person.dataset.personName, email: person.dataset.personEmail });
    return;
  }
  // The delete menu. Opens from the trash; the two choices are separate
  // requests because they are separate facts (see api.js). Any click outside
  // it closes it, and the click then goes on to do whatever it was for.
  const openMenu = e.target.closest?.('[data-delete-menu]');
  if (openMenu) {
    state.deleteMenu = openMenu.dataset.deleteMenu;
    renderThread();
    return;
  }
  const delMe = e.target.closest?.('[data-del-me]');
  if (delMe) {
    const id = delMe.dataset.delMe;
    state.deleteMenu = null;
    const kept = state.messages;
    state.messages = state.messages.filter((x) => x.id !== id);
    renderThread();
    try {
      await messaging.hide(id);
    } catch (err) {
      state.messages = kept;
      renderThread();
      toastErr(err);
    }
    return;
  }
  const delAll = e.target.closest?.('[data-del-all]');
  if (delAll) {
    const id = delAll.dataset.delAll;
    state.deleteMenu = null;
    const m = state.messages.find((x) => x.id === id);
    const was = m ? { body: m.body, deleted: m.deleted } : null;
    if (m) {
      m.deleted = true;
      m.body = '';
    }
    renderThread();
    try {
      await messaging.remove(id);
    } catch (err) {
      if (m && was) Object.assign(m, was);
      renderThread();
      toastErr(err);
    }
    return;
  }
  if (e.target.closest?.('[data-del-cancel]')) {
    state.deleteMenu = null;
    renderThread();
    return;
  }
  if (state.deleteMenu && !e.target.closest?.('[data-del-menu]')) {
    state.deleteMenu = null;
    renderThread();
  }

  const moreBtn = e.target.closest?.('[data-msg-more]');
  if (moreBtn) {
    const m = state.messages.find((x) => x.id === moreBtn.dataset.msgMore);
    if (m) messageMenu(m, moreBtn);
    return;
  }

  const pinBtn = e.target.closest?.('[data-pin]');
  const unpin = e.target.closest?.('[data-unpin]');
  if (pinBtn || unpin) {
    const id = pinBtn && state.active?.pinnedMessageId !== pinBtn.dataset.pin ? pinBtn.dataset.pin : '';
    try {
      await messaging.pin(state.active.id, id);
      await refresh();
      state.active = state.conversations.find((c) => c.id === state.active.id) ?? state.active;
      renderThread();
    } catch (err) {
      toastErr(err);
    }
    return;
  }

  const fwd = e.target.closest?.('[data-forward]');
  if (fwd) {
    const m = state.messages.find((x) => x.id === fwd.dataset.forward);
    if (m && !m.deleted) forwardDialog(m);
    return;
  }

  // Edit: the message's text goes into the composer, and Send saves it.
  const editBtn = e.target.closest?.('[data-edit]');
  if (editBtn) {
    const m = state.messages.find((x) => x.id === editBtn.dataset.edit);
    if (!m) return;
    state.editing = m;
    state.replyTo = null;
    renderThread();
    const inp = document.getElementById('c-input');
    if (inp) {
      inp.value = m.body;
      inp.focus();
      try {
        inp.setSelectionRange(inp.value.length, inp.value.length);
      } catch {
        // Not every input type takes a selection; the text is in.
      }
    }
    return;
  }
  if (e.target.closest?.('[data-edit-cancel]')) {
    state.editing = null;
    renderThread();
    const inp = document.getElementById('c-input');
    if (inp) inp.value = '';
    return;
  }

  const ht = e.target.closest?.('[data-history]');
  if (ht) {
    historyPopover(ht.dataset.history, ht);
    return;
  }

  const rc = e.target.closest?.('[data-receipt]');
  if (rc) {
    const m = state.messages.find((x) => x.id === rc.dataset.receipt);
    if (m) receiptPopover(m, rc);
    return;
  }

  const opt = e.target.closest?.('[data-poll-opt]');
  if (opt) {
    const card = opt.closest('[data-poll]');
    if (card) castVote(card.dataset.poll, Number(opt.dataset.pollOpt));
    return;
  }

  const pill = e.target.closest?.('[data-react-pill]');
  if (pill) {
    const m = state.messages.find((x) => x.id === pill.dataset.reactPill);
    if (m) reactionsPopover(m, pill);
    return;
  }

  const more = e.target.closest?.('[data-react-more]');
  if (more) {
    const id = more.dataset.reactMore;
    const ch = await pickEmoji(more);
    if (ch) toggleReaction(id, ch);
    return;
  }

  const react = e.target.closest?.('[data-react-msg]');
  if (react) {
    // Toggle, always: the same emoji again takes yours away. Applied locally
    // first so the bar responds to the click, then confirmed by the push.
    toggleReaction(react.dataset.reactMsg, react.dataset.reactEmoji);
    return;
  }

  const save = e.target.closest?.('[data-save]');
  if (save) {
    const id = save.dataset.save;
    const on = save.dataset.saveOn === '1';
    const m = state.messages.find((x) => x.id === id);
    if (m) m.saved = on;
    renderThread();
    try {
      await messaging.saveMessage(id, on);
    } catch (err) {
      if (m) m.saved = !on;
      renderThread();
      toastErr(err);
    }
    return;
  }

  const reply = e.target.closest?.('[data-reply]');
  if (reply) {
    const m = state.messages.find((x) => x.id === reply.dataset.reply);
    if (!m) return;
    state.replyTo = m;
    renderThread();
    document.getElementById('c-input')?.focus();
    return;
  }

  if (e.target.closest?.('[data-reply-cancel]')) {
    state.replyTo = null;
    renderThread();
    document.getElementById('c-input')?.focus();
    return;
  }

  // Jumping to a quoted message: scroll it into view and flash it, rather
  // than doing nothing, which is what an unhandled quote click looks like.
  const goto = e.target.closest?.('[data-goto]');
  if (goto) {
    const el = document.querySelector(`[data-msg="${CSS.escape(goto.dataset.goto)}"]`);
    if (el) {
      el.scrollIntoView({ block: 'center', behavior: 'smooth' });
      el.classList.add('flash');
      setTimeout(() => el.classList.remove('flash'), 1200);
    }
    return;
  }
});

// The local half of a reaction, so the chip moves on the click.
function applyReactionLocally(id, emoji, on) {
  const m = state.messages.find((x) => x.id === id);
  if (!m) return;
  m.reactions = m.reactions ?? [];
  const g = m.reactions.find((r) => r.emoji === emoji);
  const me = session.user?.id;
  if (on) {
    if (g) {
      if (!g.mine) {
        g.count++;
        g.mine = true;
        if (me && !(g.who ?? []).includes(me)) g.who = [...(g.who ?? []), me];
      }
    } else {
      m.reactions.push({ emoji, count: 1, mine: true, who: me ? [me] : [] });
    }
  } else if (g && g.mine) {
    // Only yours comes off. A group you are not part of is not yours to
    // change, whatever the caller asked.
    g.count = Math.max(0, g.count - 1);
    g.mine = false;
    g.who = (g.who ?? []).filter((id) => id !== me);
    if (g.count === 0) m.reactions = m.reactions.filter((r) => r !== g);
  }
}

// Delegated once: the header is re-rendered on every message, and a handler
// attached to the button would be thrown away with it.
document.addEventListener('click', (e) => {
  // The menu opens on hover (CSS); the caret is a focus target for keyboards.
  if (e.target.closest?.('[data-call-toggle]')) return;
  const b = e.target.closest?.('[data-call]');
  if (!b) return;
  window.dispatchEvent(
    new CustomEvent('aether:call', {
      detail: { targetId: b.dataset.call, targetName: b.dataset.callName, audioOnly: b.dataset.callAudio === '1' },
    })
  );
});

// ---------------------------------------------------------------------------
// Teams (engine messaging/teams.go)
//
// Teams' "Teams" view: your teams down the left, each with its channels, and
// the channel itself on the right -- the same thread as any chat, since a
// team's channel is a channel. Joining a team puts you in General and every
// standard channel; private channels show only to the people in them.
// ---------------------------------------------------------------------------

const teamFolded = new Set();

export async function renderTeamsSurface() {
  state.surface = 'teams';
  const main = document.querySelector('.home-main');
  main.innerHTML = `
    <div class="chat-shell teams-shell${state.active?.teamId ? ' showing-thread' : ''}">
      <aside class="conv-col teams-col">
        <div class="conv-head">
          <h1>Teams</h1>
          <button class="icon-btn" id="t-join-top" title="Join or create a team" aria-label="Join or create a team">
            <svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-plus"/></svg>
          </button>
        </div>
        <div class="teams-list" id="teams-list" role="tree" aria-label="Your teams"></div>
        <button class="btn teams-join" id="t-join"><svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-people"/></svg> Join or create a team</button>
      </aside>
      <section class="thread" id="thread"></section>
    </div>`;
  $('t-join').onclick = $('t-join-top').onclick = () => joinOrCreateDialog();
  if (state.active && !state.active.teamId) state.active = null;
  renderThread();
  renderTeamsList();
  await loadTeams();
}

async function loadTeams() {
  try {
    const [t] = await Promise.all([messaging.teams(), state.conversations.length ? null : refresh()]);
    state.teams = t.teams ?? [];
  } catch (e) {
    state.teams = state.teams ?? [];
    toastErr(e);
  }
  renderTeamsList();
}

function teamOf(teamId) {
  return (state.teams ?? []).find((t) => t.id === teamId);
}

function renderTeamsList() {
  const box = $('teams-list');
  if (!box) return;
  const teams = state.teams ?? [];
  if (!teams.length) {
    box.innerHTML = `<div class="teams-empty"><p><b>You are not on a team yet.</b></p><p>Create one for a project or a group of people, or join one with a code.</p></div>`;
    return;
  }
  const unreadOf = (id) => state.conversations.find((c) => c.id === id)?.unread ?? 0;
  box.innerHTML = teams
    .map((t) => {
      const folded = teamFolded.has(t.id);
      const chans = (t.channels ?? [])
        .map((ch) => {
          const n = unreadOf(ch.id);
          const on = state.active?.id === ch.id;
          return `<button class="tch${on ? ' active' : ''}${n ? ' unread' : ''}" data-tch="${esc(ch.id)}" role="treeitem" aria-current="${on}">
            ${ch.visibility === 'private' ? '<svg class="tch-lock" width="12" height="12" viewBox="0 0 24 24" aria-label="Private"><use href="#i-lock"/></svg>' : ''}
            <span class="tch-name">${esc(ch.title)}</span>${n ? `<span class="tch-n">${n > 99 ? '99+' : n}</span>` : ''}</button>`;
        })
        .join('');
      return `<div class="team" data-team="${esc(t.id)}">
        <div class="team-row">
          <button class="team-toggle" data-fold="${esc(t.id)}" aria-expanded="${!folded}">
            <svg class="team-chev" width="12" height="12" viewBox="0 0 24 24"><use href="#i-chevron"/></svg>
            <span ${avatarAttrs({ name: t.name }, 'sm square')}>${esc(initials({ name: t.name }))}</span>
            <span class="team-name">${esc(t.name)}</span>
          </button>
          <button class="icon-btn team-more" data-tmore="${esc(t.id)}" aria-label="More options for ${esc(t.name)}" aria-haspopup="menu">
            <svg width="16" height="16" viewBox="0 0 24 24"><use href="#i-more"/></svg>
          </button>
        </div>
        <div class="team-chans" role="group"${folded ? ' hidden' : ''}>${chans}</div>
      </div>`;
    })
    .join('');
  box.querySelectorAll('[data-fold]').forEach((b) => {
    b.onclick = () => {
      const id = b.dataset.fold;
      if (teamFolded.has(id)) teamFolded.delete(id);
      else teamFolded.add(id);
      renderTeamsList();
    };
  });
  box.querySelectorAll('[data-tch]').forEach((b) => {
    b.onclick = async () => {
      let c = state.conversations.find((x) => x.id === b.dataset.tch);
      if (!c) {
        await refresh();
        c = state.conversations.find((x) => x.id === b.dataset.tch);
      }
      if (c) open(c);
      else teamWarn('That channel is not available.');
    };
  });
  box.querySelectorAll('[data-tmore]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      const r = b.getBoundingClientRect();
      teamMenu(teamOf(b.dataset.tmore), r.left, r.bottom + 4);
    };
  });
}

function teamMenu(t, x, y) {
  if (!t) return;
  closeConvMenu();
  const owner = t.myRole === 'owner';
  const el = document.createElement('div');
  el.className = 'ctx-menu';
  el.setAttribute('role', 'menu');
  const item = (key, label, icon, danger = false) =>
    `<button role="menuitem" data-act="${key}" class="${danger ? 'danger' : ''}"><svg width="15" height="15" viewBox="0 0 24 24"><use href="#${icon}"/></svg>${label}</button>`;
  el.innerHTML =
    item('manage', 'Manage team', 'i-people') +
    item('channel', 'Add channel', 'i-plus') +
    (owner ? item('member', 'Add member', 'i-person-add') + item('code', 'Team code', 'i-link') + item('edit', 'Edit team', 'i-pencil') : '') +
    '<div class="ctx-sep"></div>' +
    item('leave', 'Leave team', 'i-back', true) +
    (owner ? item('delete', 'Delete team', 'i-trash', true) : '');
  document.body.appendChild(el);
  const w = el.offsetWidth || 220, h = el.offsetHeight || 260;
  el.style.left = `${Math.min(Math.max(8, x), innerWidth - w - 8)}px`;
  el.style.top = `${y + h > innerHeight - 8 ? Math.max(8, y - h - 40) : y}px`;
  convMenuEl = el;
  el.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = () => {
      closeConvMenu();
      ({ manage: manageTeamDialog, channel: teamChannelDialog, member: addTeamMemberDialog, code: teamCodeDialog, edit: editTeamDialog, leave: leaveTeamDialog, delete: deleteTeamDialog })[b.dataset.act]?.(t);
    };
  });
  setTimeout(() => addEventListener('click', closeConvMenu, { once: true }), 0);
}

// Create, join with a code, or browse public teams.
function joinOrCreateDialog() {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card teams-dialog" role="dialog" aria-modal="true" aria-labelledby="tj-title">
      <h2 id="tj-title">Join or create a team</h2>
      <div class="tj-row">
        <button class="btn btn-primary" data-create>Create team</button>
      </div>
      <form class="tj-code" id="tj-code">
        <label class="field"><span>Join a team with a code</span>
          <div class="tj-code-row"><input class="input" name="code" maxlength="7" placeholder="Enter join code." autocomplete="off" spellcheck="false" /><button class="btn" type="submit">Join</button></div></label>
      </form>
      <label class="field"><span>Public teams</span><input class="input" id="tj-q" placeholder="Search for teams" autocomplete="off" /></label>
      <div class="tj-list" id="tj-list"><p class="tj-note">Loading…</p></div>
      <div class="dialog-actions"><span class="spacer"></span><button class="btn" data-cancel>Close</button></div>
    </div>`;
  arm();
  host.querySelector('[data-create]').onclick = () => {
    close();
    createTeamDialog();
  };
  $('tj-code').onsubmit = async (e) => {
    e.preventDefault();
    const code = new FormData(e.target).get('code')?.toString().trim();
    if (!code) return;
    try {
      const t = await messaging.joinTeamByCode(code);
      close();
      toastOk(`You joined ${t.name}`);
      await loadTeams();
    } catch (err) {
      toastErr(err);
    }
  };
  let seq = 0;
  const search = async () => {
    const my = ++seq;
    try {
      const res = await messaging.browseTeams($('tj-q').value.trim());
      if (my !== seq) return; // a stale answer
      const list = (res.teams ?? []).filter((t) => !t.myRole);
      $('tj-list').innerHTML = list.length
        ? list
            .map(
              (t) => `<div class="tj-team"><span ${avatarAttrs({ name: t.name }, 'sm square')}>${esc(initials({ name: t.name }))}</span>
                <span class="tj-meta"><b>${esc(t.name)}</b><small>${esc(t.description || '')}${t.description ? ' · ' : ''}${t.memberCount} member${t.memberCount === 1 ? '' : 's'}</small></span>
                <button class="btn sm" data-join="${esc(t.id)}">Join team</button></div>`
            )
            .join('')
        : '<p class="tj-note">No public teams to join.</p>';
      $('tj-list').querySelectorAll('[data-join]').forEach((b) => {
        b.onclick = async () => {
          b.disabled = true;
          try {
            const t = await messaging.joinTeam(b.dataset.join);
            close();
            toastOk(`You joined ${t.name}`);
            await loadTeams();
          } catch (err) {
            b.disabled = false;
            toastErr(err);
          }
        };
      });
    } catch (err) {
      toastErr(err);
    }
  };
  $('tj-q').oninput = search;
  search();
}

function seg(name, options, value) {
  return `<div class="seg" role="radiogroup" aria-label="${esc(name)}">${options
    .map(
      (o) => `<button type="button" class="seg-b${o.value === value ? ' active' : ''}" data-seg="${esc(o.value)}" role="radio" aria-checked="${o.value === value}"><b>${esc(o.label)}</b><small>${esc(o.hint)}</small></button>`
    )
    .join('')}</div>`;
}

function wireSeg(host, onPick) {
  host.querySelectorAll('[data-seg]').forEach((b) => {
    b.onclick = () => {
      host.querySelectorAll('[data-seg]').forEach((x) => {
        x.classList.toggle('active', x === b);
        x.setAttribute('aria-checked', String(x === b));
      });
      onPick(b.dataset.seg);
    };
  });
}

const PRIVACY = [
  { value: 'private', label: 'Private', hint: 'Only team owners can add members' },
  { value: 'public', label: 'Public', hint: 'Anyone in your organization can join' },
];

function createTeamDialog(edit = null) {
  const { host, close, arm } = dialogHost();
  let vis = edit?.visibility ?? 'private';
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="tc-title">
      <h2 id="tc-title">${edit ? 'Edit team' : 'Create team'}</h2>
      <label class="field"><span>Team name</span><input id="tc-name" class="input" maxlength="80" placeholder="Give your team a name" autocomplete="off" value="${esc(edit?.name ?? '')}" /></label>
      <label class="field" style="margin-top:10px"><span>Description <small>(optional)</small></span><textarea id="tc-desc" class="input" rows="2" maxlength="1024" placeholder="Let people know what this team is all about">${esc(edit?.description ?? '')}</textarea></label>
      <div style="margin-top:12px">${seg('Privacy', PRIVACY, vis)}</div>
      <div class="dialog-actions"><button class="btn" data-cancel>Cancel</button><button class="btn btn-primary" data-confirm ${edit ? '' : 'disabled'}>${edit ? 'Save' : 'Create'}</button></div>
    </div>`;
  arm();
  wireSeg(host, (v) => (vis = v));
  const ok = host.querySelector('[data-confirm]');
  $('tc-name').oninput = () => (ok.disabled = !$('tc-name').value.trim());
  setTimeout(() => $('tc-name')?.focus(), 50);
  ok.onclick = async () => {
    ok.disabled = true;
    try {
      const name = $('tc-name').value.trim(), description = $('tc-desc').value.trim();
      if (edit) await messaging.updateTeam(edit.id, { name, description, visibility: vis });
      else {
        const t = await messaging.createTeam(name, description, vis);
        teamFolded.delete(t.id);
      }
      close();
      await refresh();
      await loadTeams();
    } catch (e) {
      ok.disabled = false;
      toastErr(e);
    }
  };
}

const editTeamDialog = (t) => createTeamDialog(t);

function teamChannelDialog(t) {
  const { host, close, arm } = dialogHost();
  let vis = 'standard';
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="tch-title">
      <h2 id="tch-title">Create a channel for "${esc(t.name)}" team</h2>
      <label class="field"><span>Channel name</span><input id="tch-name" class="input" maxlength="60" autocomplete="off" /></label>
      <label class="field" style="margin-top:10px"><span>Description <small>(optional)</small></span><input id="tch-topic" class="input" maxlength="240" placeholder="Help others find the right channel" autocomplete="off" /></label>
      <div style="margin-top:12px">${seg('Privacy', [
        { value: 'standard', label: 'Standard', hint: 'Everyone on the team has access' },
        { value: 'private', label: 'Private', hint: 'Only for people added' },
      ], vis)}</div>
      <div class="dialog-actions"><button class="btn" data-cancel>Cancel</button><button class="btn btn-primary" data-confirm disabled>Create</button></div>
    </div>`;
  arm();
  wireSeg(host, (v) => (vis = v));
  const ok = host.querySelector('[data-confirm]');
  $('tch-name').oninput = () => (ok.disabled = !$('tch-name').value.trim());
  setTimeout(() => $('tch-name')?.focus(), 50);
  ok.onclick = async () => {
    ok.disabled = true;
    try {
      const c = await messaging.createTeamChannel(t.id, $('tch-name').value.trim(), $('tch-topic').value.trim(), vis === 'private');
      close();
      await refresh();
      await loadTeams();
      open(state.conversations.find((x) => x.id === c.id) ?? c);
    } catch (e) {
      ok.disabled = false;
      toastErr(e);
    }
  };
}

function addTeamMemberDialog(t) {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="tm-title">
      <h2 id="tm-title">Add members to ${esc(t.name)}</h2>
      <label class="field"><span>Start typing a name or email.</span><input id="tm-search" class="input" placeholder="Type a name or email" autocomplete="off" /></label>
      <div class="picked" id="tm-picked"></div>
      <div class="people-list compact" id="tm-people"></div>
      <div class="dialog-actions"><button class="btn" data-cancel>Cancel</button><button class="btn btn-primary" data-confirm>Add</button></div>
    </div>`;
  arm();
  const picker = peoplePicker($('tm-search'), $('tm-people'), $('tm-picked'), () => {}, { exclude: new Set([session.user?.id]) });
  host.querySelector('[data-confirm]').onclick = async () => {
    const people = [...picker.chosen.values()];
    if (!people.length) return close();
    try {
      const res = await messaging.addTeamMembers(t.id, people.map((p) => p.email));
      picker.cancel();
      close();
      const added = people.length - (res.skipped?.length ?? 0);
      toastOk(`${added} ${added === 1 ? 'person' : 'people'} added to team`);
      if (res.skipped?.length) teamWarn(`Could not add: ${res.skipped.join(', ')}`);
      await loadTeams();
    } catch (e) {
      toastErr(e);
    }
  };
}

async function manageTeamDialog(t) {
  const { host, close, arm } = dialogHost();
  const owner = t.myRole === 'owner';
  const draw = (members) => {
    const row = (m) => `<div class="tmm-row">
        <span ${avatarAttrs({ userId: m.userId, name: m.name, email: m.email }, 'sm')}>${esc(initials({ userId: m.userId, name: m.name, email: m.email }))}</span>
        <span class="tmm-name"><b>${esc(m.name || m.email)}${m.userId === session.user?.id ? ' (You)' : ''}</b><small>${esc(m.email)}</small></span>
        ${owner && m.userId !== session.user?.id
          ? `<select class="input tmm-role" data-role="${esc(m.userId)}" aria-label="Role of ${esc(m.name || m.email)}"><option value="owner"${m.role === 'owner' ? ' selected' : ''}>Owner</option><option value="member"${m.role === 'member' ? ' selected' : ''}>Member</option></select>
             <button class="icon-btn" data-remove="${esc(m.userId)}" aria-label="Remove from team"><svg width="15" height="15" viewBox="0 0 24 24"><use href="#i-close"/></svg></button>`
          : `<span class="tmm-role-label">${m.role === 'owner' ? 'Owner' : 'Member'}</span>`}
      </div>`;
    const owners = members.filter((m) => m.role === 'owner'), rest = members.filter((m) => m.role !== 'owner');
    host.innerHTML = `
      <div class="dialog-card teams-dialog" role="dialog" aria-modal="true" aria-labelledby="mt-title">
        <h2 id="mt-title">${esc(t.name)}</h2>
        <p class="tj-note">${t.visibility === 'public' ? 'Public - Anyone in your organization can join' : 'Private - Only team owners can add members'} · ${members.length} member${members.length === 1 ? '' : 's'}</p>
        <h3 class="tmm-h">Owners (${owners.length})</h3>${owners.map(row).join('')}
        <h3 class="tmm-h">Members and guests (${rest.length})</h3>${rest.map(row).join('') || '<p class="tj-note">Nobody else yet.</p>'}
        <div class="dialog-actions">${owner ? '<button class="btn" data-add>Add member</button>' : ''}<span class="spacer"></span><button class="btn" data-cancel>Close</button></div>
      </div>`;
    arm();
    host.querySelector('[data-add]')?.addEventListener('click', () => {
      close();
      addTeamMemberDialog(t);
    });
    host.querySelectorAll('[data-role]').forEach((s) =>
      s.addEventListener('change', async () => {
        try {
          await messaging.setTeamRole(t.id, s.dataset.role, s.value);
        } catch (e) {
          toastErr(e);
        }
        load();
      })
    );
    host.querySelectorAll('[data-remove]').forEach((b) =>
      b.addEventListener('click', async () => {
        try {
          await messaging.removeTeamMember(t.id, b.dataset.remove);
          toastOk('Removed from team');
        } catch (e) {
          toastErr(e);
        }
        load();
      })
    );
  };
  const load = async () => {
    try {
      const res = await messaging.teamMembers(t.id);
      draw(res.members ?? []);
    } catch (e) {
      close();
      toastErr(e);
    }
  };
  host.innerHTML = '<div class="dialog-card"><p class="tj-note">Loading…</p></div>';
  arm();
  load();
}

function teamCodeDialog(t) {
  const { host, close, arm } = dialogHost();
  const draw = (code) => {
    host.innerHTML = `
      <div class="dialog-card" role="dialog" aria-modal="true" aria-labelledby="tcd-title">
        <h2 id="tcd-title">Team code</h2>
        ${code
          ? `<p class="team-code mono" aria-label="Team code is ${esc(code)}">${esc(code)}</p><p class="tj-note">Share it with people who should join ${esc(t.name)}. Anyone with the code can join.</p>`
          : '<p class="tj-note">Generate a team code people can use to join this team.</p>'}
        <div class="dialog-actions">
          ${code ? '<button class="btn" data-remove>Remove</button><button class="btn" data-reset>Reset</button><button class="btn" data-copy>Copy</button>' : '<button class="btn btn-primary" data-gen>Generate</button>'}
          <span class="spacer"></span><button class="btn" data-cancel>Close</button>
        </div>
      </div>`;
    arm();
    const gen = async () => {
      try {
        const res = await messaging.teamCode(t.id);
        t.joinCode = res.code;
        draw(res.code);
      } catch (e) {
        toastErr(e);
      }
    };
    host.querySelector('[data-gen]')?.addEventListener('click', gen);
    host.querySelector('[data-reset]')?.addEventListener('click', gen);
    host.querySelector('[data-copy]')?.addEventListener('click', () => navigator.clipboard?.writeText(code).then(() => toastOk('Copied')));
    host.querySelector('[data-remove]')?.addEventListener('click', async () => {
      try {
        await messaging.removeTeamCode(t.id);
        t.joinCode = '';
        draw('');
      } catch (e) {
        toastErr(e);
      }
    });
  };
  draw(t.joinCode || '');
}

function confirmTeam(title, body, yes, run) {
  const { host, close, arm } = dialogHost();
  host.innerHTML = `
    <div class="dialog-card" role="alertdialog" aria-modal="true" aria-labelledby="tx-title">
      <h2 id="tx-title">${esc(title)}</h2><p>${esc(body)}</p>
      <div class="dialog-actions"><span class="spacer"></span><button class="btn" data-cancel>Cancel</button><button class="btn btn-danger" data-confirm>${esc(yes)}</button></div>
    </div>`;
  arm();
  host.querySelector('[data-confirm]').onclick = async () => {
    try {
      await run();
      close();
      if (state.active?.teamId) state.active = null;
      await refresh();
      await loadTeams();
      renderThread();
    } catch (e) {
      toastErr(e);
    }
  };
}

const leaveTeamDialog = (t) =>
  confirmTeam(`Leave the team "${t.name}"?`, "You'll leave all of its channels, and won't see new posts unless someone adds you again.", 'Leave team', () => messaging.removeTeamMember(t.id, session.user?.id));
const deleteTeamDialog = (t) =>
  confirmTeam(`Delete the team "${t.name}"?`, 'All channels, conversations and files in it will be deleted for everyone. This cannot be undone.', 'Delete team', () => messaging.deleteTeam(t.id));

function teamWarn(msg) {
  const el = $('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = 'toast show bad';
  clearTimeout(toastErr._t);
  toastErr._t = setTimeout(() => (el.className = 'toast'), 4000);
}

// ---------------------------------------------------------------------------
// Activity: mentions, replies, reactions and team adds (messaging/activity.go).
//
// One list of everything this client knows, newest first; every view is a
// filter of it, so a filter answers at once and the server's page merges in
// when it lands. Pushes add, mark read and take back; the count on the badge
// is always the server's, carried on every frame.
// ---------------------------------------------------------------------------
const activity = { items: [], unread: 0, filter: 'all', more: false, seq: 0, loaded: false };
const ACT_FILTERS = [
  ['all', 'All'],
  ['unread', 'Unread'],
  ['mention', 'Mentions'],
  ['reply', 'Replies'],
  ['reaction', 'Reactions'],
  ['team', 'Teams'],
];

function paintActivityBadge() {
  const b = $('activity-badge');
  if (!b) return;
  const n = activity.unread;
  b.textContent = n > 99 ? '99+' : String(n);
  b.classList.toggle('hidden', n === 0);
}

function mergeActivity(items) {
  const byId = new Map(activity.items.map((x) => [x.id, x]));
  for (const x of items) byId.set(x.id, x);
  activity.items = [...byId.values()].sort((a, b) => (a.at === b.at ? (a.id < b.id ? 1 : -1) : a.at < b.at ? 1 : -1));
}

async function syncActivity() {
  try {
    const res = await messaging.activity({ limit: 50 });
    activity.unread = res.unread ?? 0;
    mergeActivity(res.items ?? []);
    activity.loaded = true;
    paintActivityBadge();
    if (state.surface === 'activity') renderActivityList();
  } catch {
    /* the next push or open tries again */
  }
}

function onActivityFrame(r) {
  const kind = r.bits(2);
  r.bits(6);
  if (kind === 0) {
    const f = [];
    for (let i = 0; i < 12; i++) f.push(r.str());
    const at = new Date(Number(r.uvarint())).toISOString();
    const read = r.bool();
    r.bits(7);
    const snippet = new TextDecoder().decode(r.blob());
    const unread = Number(r.uvarint());
    if (r.err) return;
    const [id, k, actorId, actorName, conversationId, conversationKind, conversationTitle, teamId, teamName, messageId, rootId, emoji] = f;
    actFresh.add(id);
    mergeActivity([{ id, kind: k, actorId, actorName, conversationId, conversationKind, conversationTitle, teamId, teamName, messageId, rootId, emoji, snippet, at, read }]);
    activity.unread = unread;
  } else if (kind === 1) {
    const n = Number(r.uvarint());
    const ids = new Set();
    for (let i = 0; i < n && !r.err; i++) ids.add(r.str());
    const unread = Number(r.uvarint());
    if (r.err) return;
    for (const x of activity.items) if (ids.has(x.id)) x.read = true;
    activity.unread = unread;
  } else if (kind === 2) {
    const id = r.str();
    const unread = Number(r.uvarint());
    if (r.err) return;
    activity.items = activity.items.filter((x) => x.id !== id);
    activity.unread = unread;
  }
  paintActivityBadge();
  if (state.surface === 'activity') renderActivityList();
}

const actMatches = (x, f) => f === 'all' || (f === 'unread' ? !x.read : x.kind === f);

function actWhere(x) {
  if (x.kind === 'team') return x.teamName;
  if (x.teamName) return `${x.teamName} › ${x.conversationTitle}`;
  const c = state.conversations.find((y) => y.id === x.conversationId);
  if (c) return whoOf(c).name;
  if (x.conversationKind === 'direct') return 'Chat';
  return x.conversationTitle || 'Group chat';
}

// Who did it, for a group: "Ada", "Ada and Ben", "Ada, Ben and 3 others".
function actWho(g) {
  const names = [...new Set(g.items.map((x) => x.actorName || 'Someone'))];
  const b = (n) => `<b>${esc(n)}</b>`;
  if (names.length === 1) return b(names[0]);
  if (names.length === 2) return `${b(names[0])} and ${b(names[1])}`;
  const rest = names.length - 2;
  return `${b(names[0])}, ${b(names[1])} and ${rest} ${rest === 1 ? 'other' : 'others'}`;
}

function actLine(g) {
  const x = g.items[0];
  const who = actWho(g);
  switch (x.kind) {
    case 'mention':
      return `${who} mentioned you`;
    case 'reply':
      return `${who} replied to you`;
    case 'reaction': {
      const em = [...new Set(g.items.map((y) => y.emoji))].map((e) => `<span class="act-emoji">${esc(e)}</span>`).join('');
      return `${who} reacted ${em}`;
    }
    case 'team':
      return `${who} added you to <b>${esc(x.teamName)}</b>`;
    default:
      return who;
  }
}

// The mark on the avatar's corner: what kind of thing happened. (A
// reaction's emoji are in its line.)
function actMark(g) {
  const x = g.items[0];
  const icon = { mention: 'i-at', reply: 'i-reply', reaction: 'i-heart', team: 'i-people' }[x.kind];
  return icon ? `<span class="act-mark" aria-hidden="true"><svg width="10" height="10" viewBox="0 0 24 24"><use href="#${icon}"/></svg></span>` : '';
}

// Which section an item sits in: Today, Yesterday, a weekday this week, then
// the date.
function actDay(iso) {
  const d = new Date(iso);
  const start = (t) => new Date(t.getFullYear(), t.getMonth(), t.getDate()).getTime();
  const days = Math.round((start(new Date()) - start(d)) / 86400000);
  if (days <= 0) return 'Today';
  if (days === 1) return 'Yesterday';
  if (days < 7) return d.toLocaleDateString([], { weekday: 'long' });
  return d.toLocaleDateString([], { day: 'numeric', month: 'long', year: d.getFullYear() === new Date().getFullYear() ? undefined : 'numeric' });
}

// Sections of groups. Reactions to one message within a section fold into
// one row; everything else is a row of its own.
function actSections(items) {
  const sections = [];
  let cur = null;
  let folded = new Map();
  for (const x of items) {
    const day = actDay(x.at);
    if (!cur || cur.day !== day) {
      cur = { day, groups: [] };
      sections.push(cur);
      folded = new Map();
    }
    if (x.kind === 'reaction' && folded.has(x.messageId)) {
      folded.get(x.messageId).items.push(x);
      continue;
    }
    const g = { items: [x] };
    if (x.kind === 'reaction') folded.set(x.messageId, g);
    cur.groups.push(g);
  }
  return sections;
}

const actTime = (iso) => new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
// Ids that arrived by push since the list was last drawn: drawn once with
// the arrival tint.
const actFresh = new Set();

export async function renderActivitySurface() {
  state.surface = 'activity';
  state.active = null;
  const main = document.querySelector('.home-main');
  main.innerHTML = `
    <div class="chat-shell activity-shell">
      <aside class="conv-col activity-col">
        <div class="conv-head">
          <h1>Activity</h1>
          <button class="icon-btn" id="act-all-read" title="Mark all as read" aria-label="Mark all as read">
            <svg width="18" height="18" viewBox="0 0 24 24"><use href="#i-check-double"/></svg>
          </button>
        </div>
        <div class="act-filters pills" role="tablist" aria-label="Show">
          ${ACT_FILTERS.map(([k, label]) => `<button class="pill-f${activity.filter === k ? ' active' : ''}" role="tab" aria-selected="${activity.filter === k}" data-act-filter="${k}">${label}</button>`).join('')}
        </div>
        <div class="act-list" id="act-list" role="list"></div>
      </aside>
      <section class="thread" id="thread"></section>
    </div>`;
  $('act-all-read').onclick = async () => {
    for (const x of activity.items) x.read = true;
    activity.unread = 0;
    paintActivityBadge();
    renderActivityList();
    try {
      const res = await messaging.readActivity([], true);
      activity.unread = res.unread ?? 0;
      paintActivityBadge();
    } catch (e) {
      toastErr(e);
      syncActivity();
    }
  };
  main.querySelectorAll('[data-act-filter]').forEach((b) => {
    b.onclick = () => {
      activity.filter = b.dataset.actFilter;
      main.querySelectorAll('[data-act-filter]').forEach((x) => {
        x.classList.toggle('active', x === b);
        x.setAttribute('aria-selected', String(x === b));
      });
      renderActivityList();
      loadActivity();
    };
  });
  renderThread();
  renderActivityList();
  if (!state.conversations.length) refresh().then(() => renderActivityList()).catch(() => {});
  await loadActivity();
}

// One page from the server for the current filter, merged; an answer for a
// filter that is no longer chosen is dropped.
async function loadActivity(before = '') {
  const seq = ++activity.seq;
  const f = activity.filter;
  try {
    const res = await messaging.activity({
      before,
      kind: f === 'all' || f === 'unread' ? '' : f,
      unread: f === 'unread',
      limit: 50,
    });
    if (seq !== activity.seq) return;
    mergeActivity(res.items ?? []);
    activity.unread = res.unread ?? 0;
    activity.more = (res.items ?? []).length === 50;
    activity.loaded = true;
    paintActivityBadge();
    renderActivityList();
  } catch (e) {
    if (seq === activity.seq) toastErr(e);
  }
}

function renderActivityList() {
  const box = $('act-list');
  if (!box) return;
  const items = activity.items.filter((x) => actMatches(x, activity.filter));
  if (!items.length) {
    const empty = {
      all: ['No activity yet', 'When someone mentions you, replies to you, reacts to something you wrote or adds you to a team, it is listed here.'],
      unread: ['No unread activity', 'Everything here has been read.'],
      mention: ['No mentions', 'Messages that @mention you are listed here.'],
      reply: ['No replies', 'Replies to your messages are listed here.'],
      reaction: ['No reactions', 'Reactions to your messages are listed here.'],
      team: ['No team invitations', 'When someone adds you to a team, it is listed here.'],
    }[activity.filter];
    box.innerHTML = activity.loaded ? `<div class="act-empty"><p class="act-empty-h">${empty[0]}</p><p>${empty[1]}</p></div>` : '';
    return;
  }
  const row = (g) => {
    const x = g.items[0];
    const unread = g.items.some((y) => !y.read);
    const on = state.active && x.conversationId === state.active.id;
    const fresh = g.items.some((y) => actFresh.has(y.id));
    const where = actWhere(x);
    return `<button class="act-item${unread ? ' is-unread' : ''}${on ? ' active' : ''}${fresh ? ' fresh' : ''}" role="listitem" data-act="${esc(x.id)}">
        <span class="act-face"><span ${avatarAttrs({ id: x.actorId, name: x.actorName })}>${esc(initials({ name: x.actorName }))}</span>${actMark(g)}</span>
        <span class="act-body">
          <span class="act-top"><span class="act-line">${actLine(g)}</span><time class="act-when" datetime="${esc(x.at)}">${esc(actTime(x.at))}</time></span>
          ${where && x.kind !== 'team' ? `<span class="act-where">${esc(where)}</span>` : ''}
          ${x.snippet ? `<span class="act-snippet">${esc(x.snippet)}</span>` : ''}
        </span>
      </button>`;
  };
  box.innerHTML =
    actSections(items)
      .map((sec) => `<section class="act-sec" aria-label="${esc(sec.day)}"><h2 class="act-day">${esc(sec.day)}</h2>${sec.groups.map(row).join('')}</section>`)
      .join('') + (activity.more ? `<button class="btn act-more" id="act-more">Show older</button>` : '');
  actFresh.clear();
  const groups = new Map(actSections(items).flatMap((sec) => sec.groups.map((g) => [g.items[0].id, g])));
  box.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = () => openActivity(groups.get(b.dataset.act));
  });
  const more = $('act-more');
  if (more) {
    more.onclick = () => {
      const last = items[items.length - 1];
      loadActivity(`${new Date(last.at).getTime()}:${last.id}`);
    };
  }
}

async function openActivity(g) {
  if (!g) return;
  const x = g.items[0];
  const ids = g.items.filter((y) => !y.read).map((y) => y.id).slice(0, 100);
  if (ids.length) {
    for (const y of g.items) y.read = true;
    activity.unread = Math.max(0, activity.unread - ids.length);
    paintActivityBadge();
    renderActivityList();
    messaging
      .readActivity(ids)
      .then((res) => {
        activity.unread = res.unread ?? activity.unread;
        paintActivityBadge();
      })
      .catch(() => syncActivity());
  }
  if (x.kind === 'team') {
    document.querySelector('[data-home="teams"]')?.click();
    return;
  }
  try {
    // A reply in a channel lives in its root's thread.
    const channelReply = x.conversationKind === 'channel' && x.rootId;
    await openConversationAt(x.conversationId, channelReply ? x.rootId : x.messageId);
    if (channelReply) await openThread(x.rootId);
    renderActivityList();
  } catch (e) {
    toastErr(e);
  }
}
