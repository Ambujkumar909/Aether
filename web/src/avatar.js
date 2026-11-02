// One person, one face, everywhere.
//
// There were two implementations and they disagreed twice over. The chat
// sidebar drew a bare `<span class="avatar">` with no tone attribute, so
// everybody there was the same colour while the same people were individually
// coloured in the meeting and on the dashboard. And the letters were derived
// from whatever string a given call site happened to hold -- a display name in
// one place, an email address in another -- so one person was "D" in the
// conversation list and "7D" in a bubble.
//
// Both bugs have the same root: identity was being inferred from whatever text
// was on hand. So this module takes the person, not the text, and derives both
// halves from a stable key.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

// Anything outside the Basic Multilingual Plane, plus the symbol and
// pictograph blocks that live inside it. A name that begins with an emoji is
// shown as that emoji rather than reduced to a letter it does not have.
const EMOJI_START =
  /^(?:[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}\u{2190}-\u{21FF}\u{2B00}-\u{2BFF}\u{FE0F}\u{1F1E6}-\u{1F1FF}])/u;

/**
 * The person this avatar is for. Accepts a plain string for the many call
 * sites that only have a name, or an object when identity is known.
 */
function person(who) {
  if (who && typeof who === 'object') return who;
  return { name: String(who ?? ''), email: '', userId: '' };
}

/**
 * The stable identity behind an avatar, lowercased.
 *
 * Colour is hashed on this rather than on the visible text, which is the fix
 * for the same person changing colour between two screens: the account id if
 * there is one, then the address, and only then the name. A guest with neither
 * still gets a consistent colour from their name, which is all they have.
 */
export function personKey(who) {
  const p = person(who);
  return String(p.userId || p.email || p.name || '')
    .trim()
    .toLowerCase();
}

/**
 * One or two letters for a person.
 *
 * A real name gives its initials. An address gives the initials of its local
 * part, so `ambuj.kumar@ddecor.com` is "AK" and not "AK" from one screen and
 * "AD" from another -- the domain is never part of it, because two colleagues
 * share a domain and it says nothing about who they are. Leading digits are
 * skipped for the same reason: `7298.divyanshi@gmail.com` is "D", not "7D".
 */
export function initials(who) {
  const p = person(who);
  const emoji = EMOJI_START.exec(String(p.name ?? '').trim());
  if (emoji) return emoji[0];

  let source = String(p.name ?? '').trim();
  if (!source) source = String(p.email ?? '').trim();
  if (!source) return '?';

  // An address, whether it arrived as the name or as the email: keep the
  // local part only.
  const at = source.indexOf('@');
  if (at > 0) source = source.slice(0, at);

  const words = source
    .split(/[\s._\-+]+/)
    .map((w) => w.trim())
    .filter(Boolean);

  // Prefer parts that start with a letter. A purely numeric account still
  // gets its digits rather than nothing at all.
  const lettered = words.filter((w) => /^[^\W\d_]/u.test(w));
  const use = (lettered.length ? lettered : words).slice(0, 2);
  if (!use.length) return '?';
  return use
    .map((w) => [...w][0])
    .join('')
    .toUpperCase();
}

/** Eight muted hues; see the tone table in styles.css. */
export function toneOf(who) {
  const s = personKey(who);
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h % 8;
}

/**
 * The class and tone attributes for an avatar element, so no call site has to
 * remember the second half -- which is exactly how the sidebar came to have
 * none.
 */
export function avatarAttrs(who, extra = '') {
  return `class="avatar${extra ? ' ' + extra : ''}" data-tone="${toneOf(who)}"`;
}

/** The whole element, for call sites that want one string. */
export function avatarHTML(who, extra = '') {
  return `<span ${avatarAttrs(who, extra)}>${esc(initials(who))}</span>`;
}
