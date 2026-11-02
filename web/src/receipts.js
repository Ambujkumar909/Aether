// Read receipts, from the read cursors the server already keeps.
//
// Every member row carries lastRead, the moment that person last opened the
// conversation, and the server pushes it (OpReadCursor) to conversations
// small enough for it to mean something. A message of yours is "seen" by a
// member whose cursor is at or past the message's time. That is the whole
// model: no per-message receipt rows, no extra traffic, and it is exact.
//
// Pure functions, so the rule is testable without a DOM.

/** Members who have read a message: cursor at or after it, not the sender. */
export function readersOf(m, members, meId) {
  if (!m || !members?.length) return [];
  const at = new Date(m.createdAt).getTime();
  if (!Number.isFinite(at)) return [];
  return members.filter((x) => x.userId && x.userId !== meId && x.lastRead && new Date(x.lastRead).getTime() >= at);
}

/** Members who have not (yet): everyone else but the sender. */
export function unreadBy(m, members, meId) {
  const seen = new Set(readersOf(m, members, meId).map((x) => x.userId));
  return (members ?? []).filter((x) => x.userId && x.userId !== meId && !seen.has(x.userId));
}

/**
 * What the icon beside one of your messages should say.
 *   { kind: 'none' }                        not yours, deleted, a system row, or too big a room
 *   { kind: 'sent' }                        nobody has read it yet
 *   { kind: 'seen', readers, total }        in a 1:1: seen; in a group: N of total have read
 *   { kind: 'all',  readers, total }        everybody has read it
 */
export function receiptState(m, conv, meId, { maxMembers = 40 } = {}) {
  if (!m || !conv || !meId || m.senderId !== meId || m.deleted || m.system) return { kind: 'none' };
  const members = conv.members ?? [];
  const others = members.filter((x) => x.userId && x.userId !== meId);
  if (!others.length || members.length > maxMembers) return { kind: 'none' };
  // A scheduled message that has not gone out has not been sent.
  if (m.deliverAt && new Date(m.deliverAt).getTime() > Date.now()) return { kind: 'none' };
  const readers = readersOf(m, members, meId);
  if (!readers.length) return { kind: 'sent', readers: [], total: others.length };
  if (readers.length >= others.length) return { kind: 'all', readers, total: others.length };
  return { kind: 'seen', readers, total: others.length };
}

/** "Seen by Priya, Sam and 2 more" -- the tooltip. */
export function receiptTitle(st, conv) {
  if (st.kind === 'sent') return conv?.kind === 'direct' ? 'Sent' : `Sent · nobody has read it yet`;
  if (st.kind === 'none') return '';
  const first = (x) => (x.name || x.email || '').split(/[ @]/)[0];
  if (conv?.kind === 'direct') return 'Seen';
  const names = st.readers.map(first);
  const shown = names.slice(0, 3).join(', ') + (names.length > 3 ? ` and ${names.length - 3} more` : '');
  return st.kind === 'all' ? `Seen by everyone (${st.total})` : `Seen by ${shown} · ${st.readers.length} of ${st.total}`;
}
