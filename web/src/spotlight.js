// Spotlight, the client's half. The server holds the list and sends it whole
// (engine/internal/room/spotlight.go); this decides what the person is told
// about a change, and who of the spotlighted takes the stage.

// Teams' limit (numberOfAllowedSpotlights in its shipped config). The server
// enforces it; the client checks first so it can say why instead of
// sending a request that is silently refused.
export const MAX_SPOTLIGHTS = 7;

// What to say about a change from prev to next, in Teams' words. me is your
// participant id; nameOf(id) gives a display name. Returns {notices, added}:
// added is whether anybody new is on the stage, which is what clears a
// personal pin (a spotlight is "pin for everyone").
export function spotlightChange(prev, next, me, nameOf) {
  const was = new Set(prev);
  const now = new Set(next);
  const added = next.filter((id) => !was.has(id));
  const removed = prev.filter((id) => !now.has(id));
  const notices = [];
  if (added.includes(me)) notices.push("You're spotlighted");
  if (removed.includes(me)) notices.push("You're no longer spotlighted");
  if (prev.length && !next.length && !removed.includes(me)) notices.push('All spotlights stopped');
  else {
    for (const id of added) if (id !== me) notices.push(`${nameOf(id)} is spotlighted.`);
  }
  return { notices, added: added.length > 0 };
}

// Who of the spotlighted holds the stage: whoever of them is speaking, else
// the first spotlighted. list entries are {id, rank, presentation}.
export function pickSpotlit(list, ids) {
  const lit = list.filter((x) => !x.presentation && ids.includes(x.id));
  if (!lit.length) return null;
  const speaking = lit.filter((x) => x.rank >= 0).sort((a, b) => a.rank - b.rank)[0];
  return speaking ?? lit.sort((a, b) => ids.indexOf(a.id) - ids.indexOf(b.id))[0];
}
