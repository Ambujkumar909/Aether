// One glyph set for a person's availability, drawn wherever a person is:
// the chat list, the chat header, group members, meeting participants, the
// directory and the profile menu. The shape carries the meaning as well as
// the colour, so the states read apart for anyone who does not see red and
// green as different: a tick for available, a bar for do not disturb, a
// clock for away, a handset for a call, a screen for presenting, a hollow
// ring for offline. Busy is the plain filled disc.
//
// Pure: strings in, markup out. No DOM, so it is unit-tested directly.

const LABELS = {
  available: 'Available',
  busy: 'Busy',
  dnd: 'Do not disturb',
  away: 'Away',
  incall: 'In a call',
  presenting: 'Presenting',
  offline: 'Offline',
};

export function statusLabel(s) {
  return LABELS[s] || LABELS.available;
}

// The two states the system sets and nobody picks.
export function isAutoStatus(s) {
  return s === 'incall' || s === 'presenting';
}

const esc = (s) =>
  String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

// Glyphs on a 16x16 grid, over a filled disc of radius 8. Strokes are the
// page's card colour so they cut out of the disc whatever the theme.
const GLYPHS = {
  available: '<path class="fg" d="M4.7 8.3l2.2 2.2 4.5-4.8"/>',
  busy: '',
  dnd: '<path class="fg" d="M4.6 8h6.8"/>',
  // A wall clock: the face is a fine ring inside the disc, the hands point
  // to about ten past twelve.
  away: '<circle class="face" cx="8" cy="8" r="5.1"/><path class="fg" d="M8 4.9V8l2.1 1.5"/>',
  incall:
    '<path class="fgf" d="M4.5 4.7a1 1 0 011.2-.3l1.5.7c.4.2.6.7.5 1.1l-.3 1.2c-.1.3 0 .7.3.9l1 1c.2.3.6.4.9.3l1.2-.3c.4-.1.9.1 1.1.5l.7 1.5a1 1 0 01-.3 1.2c-.9.8-2.3.9-3.4.2A10.7 10.7 0 014.3 8.1c-.7-1.1-.6-2.5.2-3.4z"/>',
  presenting: '<rect class="fg" x="4.1" y="4.6" width="7.8" height="5" rx="1"/><path class="fg" d="M8 9.6v1.6M6.3 11.4h3.4"/>',
  offline: '',
};

// agoText says how long ago a moment was, the way a person would: "just
// now", "5 min ago", "2 h ago", "yesterday at 16:20", or the date. The
// clock is a parameter so the words are tested without waiting.
export function agoText(ms, now = Date.now()) {
  if (!ms) return '';
  const d = Math.max(0, now - ms);
  if (d < 60_000) return 'just now';
  if (d < 3_600_000) return `${Math.floor(d / 60_000)} min ago`;
  if (d < 24 * 3_600_000) return `${Math.floor(d / 3_600_000)} h ago`;
  const then = new Date(ms);
  const hm = then.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (d < 48 * 3_600_000) return `yesterday at ${hm}`;
  return `on ${then.toLocaleDateString([], { day: 'numeric', month: 'short' })}`;
}

// presenceLine is the words beside a glyph: the state, then the note or
// the last-seen time. "Away · Back at 3", "Offline · Last seen 2 h ago",
// "In a call". A person appearing offline reads plainly "Offline".
export function presenceLine(st, { online = true, lastSeen = 0, now = Date.now() } = {}) {
  const status = online ? st?.status || 'available' : 'offline';
  const parts = [statusLabel(status)];
  if (online && st?.note && st.note !== statusLabel(status)) parts.push(st.note);
  // Last seen belongs to absence: offline, or away because every device
  // went quiet. Someone who picked Away while typing is not absent.
  if ((status === 'offline' || (status === 'away' && st?.auto)) && lastSeen) parts.push(`Last seen ${agoText(lastSeen, now)}`);
  return parts.join(' · ');
}

// presenceIcon draws one status. `online: false` draws the offline ring
// whatever the status says, because a status is what a person said and
// offline is what the connection says, and the connection wins.
export function presenceIcon(status, { online = true, title, note = '' } = {}) {
  let kind = online ? status : 'offline';
  if (!(kind in GLYPHS)) kind = 'available';
  const label = title ?? (note ? `${statusLabel(kind)} · ${note}` : statusLabel(kind));
  const disc =
    kind === 'offline' ? '<circle class="ring" cx="8" cy="8" r="6.6"/>' : '<circle class="bg" cx="8" cy="8" r="8"/>';
  return (
    `<i class="presence pi-${kind}" title="${esc(label)}" aria-label="${esc(label)}">` +
    `<svg viewBox="0 0 16 16" aria-hidden="true" focusable="false">${disc}${GLYPHS[kind]}</svg></i>`
  );
}
