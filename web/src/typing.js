// Who is typing, per conversation, per person.
//
// The rules, all of them here so they can be tested without a socket:
//   - a "typing" signal keeps a person typing for TYPING_TTL from that
//     signal; the sender re-signals every few seconds while it goes on, so
//     a live typist never lapses and a dead one lapses on time;
//   - an explicit stop, or a message from that person, ends it at once;
//   - several people are named together, three or more are counted;
//   - a person is keyed by id, not name -- two Sams are two people.
// The board says which conversations changed so the caller redraws only
// those, in place.
export const TYPING_TTL = 6000;

export function createTypingBoard({ now = () => Date.now() } = {}) {
  const convs = new Map(); // convId -> Map(whoId -> { name, until })

  function set(convId, whoId, name, on, at = now()) {
    let m = convs.get(convId);
    if (!on) {
      if (!m || !m.has(whoId)) return false;
      m.delete(whoId);
      if (!m.size) convs.delete(convId);
      return true;
    }
    if (!m) convs.set(convId, (m = new Map()));
    const was = m.get(whoId);
    m.set(whoId, { name, until: at + TYPING_TTL });
    return !was; // a refresh of somebody already typing changes nothing on screen
  }

  // Drop whoever has lapsed. Returns the conversations that changed.
  function expire(at = now()) {
    const changed = [];
    for (const [convId, m] of convs) {
      let any = false;
      for (const [id, t] of m) {
        if (t.until <= at) {
          m.delete(id);
          any = true;
        }
      }
      if (!m.size) convs.delete(convId);
      if (any) changed.push(convId);
    }
    return changed;
  }

  // When the next lapse is due, or null when nobody is typing anywhere.
  function nextDue() {
    let due = null;
    for (const m of convs.values()) for (const t of m.values()) if (due == null || t.until < due) due = t.until;
    return due;
  }

  function names(convId, at = now()) {
    const m = convs.get(convId);
    if (!m) return [];
    return [...m.values()].filter((t) => t.until > at).map((t) => t.name);
  }

  // "Priya is typing", "Priya and Sam are typing", "3 people are typing".
  function line(convId, at = now()) {
    const n = names(convId, at);
    if (!n.length) return '';
    if (n.length === 1) return `${n[0]} is typing`;
    if (n.length === 2) return `${n[0]} and ${n[1]} are typing`;
    return `${n.length} people are typing`;
  }

  return { set, expire, nextDue, names, line, get size() { return convs.size; } };
}

// The sender's side: when to say "typing" and when to say "stopped", from
// the text in the box and the clock. Says on at the first character and
// again every REPEAT while the text keeps changing; stop when the box
// empties, when the message is sent, or after QUIET without a change.
export const TYPING_REPEAT = 3000;
export const TYPING_QUIET = 5000;

export function createTypingSender({ now = () => Date.now(), signal } = {}) {
  let active = false;
  let lastSent = 0;
  let lastChange = 0;

  function changed(text, at = now()) {
    if (!text) {
      if (active) {
        active = false;
        signal(false);
      }
      return;
    }
    lastChange = at;
    if (!active || at - lastSent >= TYPING_REPEAT) {
      active = true;
      lastSent = at;
      signal(true);
    }
  }
  // Called on a coarse timer: a typist who paused for QUIET has stopped.
  function tick(at = now()) {
    if (active && at - lastChange >= TYPING_QUIET) {
      active = false;
      signal(false);
    }
  }
  function sent() {
    if (active) {
      active = false;
      signal(false);
    }
  }
  return { changed, tick, sent, get active() { return active; } };
}
