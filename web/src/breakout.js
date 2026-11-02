// Breakout rooms, the client's half: the frames (engine signal/breakout.go)
// and the arithmetic of assigning people. Teams' numbers, from its package:
// at most `maxBreakoutRoomCount = 50` rooms; it suggests a room count from
// `breakoutRoomsSizeConfig` (`avgParticipantsPerRoom: 4`).

import { Op } from './wire.js';

export const MAX_ROOMS = 50;
export const MAX_NAME = 35;
export const MAX_ANNOUNCEMENT = 500;
export const PEOPLE_PER_ROOM = 4;
export const K = { set: 0, state: 0, open: 1, move: 1, close: 2, ret: 2, announce: 3, info: 4, del: 5 };

export function suggestRooms(people) {
  return Math.min(MAX_ROOMS, Math.max(1, Math.ceil(people / PEOPLE_PER_ROOM)));
}

// "Automatically assign": everyone, as evenly as the rooms allow, in order.
export function autoAssign(ids, rooms) {
  const out = Array.from({ length: rooms }, () => []);
  ids.forEach((id, i) => out[i % rooms].push(id));
  return out;
}

// "Shuffle": the same, in a random order. rand is injectable for tests.
export function shuffle(ids, rooms, rand = Math.random) {
  const a = [...ids];
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return autoAssign(a, rooms);
}

// "Remaining time 4:59" from the server's close time (unix seconds).
export function remaining(closesAtSec, now = Date.now()) {
  if (!closesAtSec) return '';
  const s = Math.max(0, Math.ceil(closesAtSec - now / 1000));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = String(s % 60).padStart(2, '0');
  return h ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
}

const text = (b) => new TextDecoder().decode(b);

// One server frame, decoded in full (every string may teach the dictionary).
export function decodeBreakout(r) {
  const kind = r.bits(3);
  if (kind === K.state) {
    const autoMove = r.bool(), allowReturn = r.bool(), open = r.bool();
    r.bits(2);
    const limit = Number(r.uvarint()), closes = Number(r.uvarint());
    const n = Number(r.uvarint());
    const rooms = [];
    for (let i = 0; i < n && i < MAX_ROOMS && !r.err; i++) {
      const id = r.str(), name = r.str(), present = Number(r.uvarint());
      const m = Number(r.uvarint());
      const members = [];
      for (let j = 0; j < m && j < 500 && !r.err; j++) members.push({ id: r.str(), name: r.str() });
      rooms.push({ id, name, present, members });
    }
    return { kind: 'state', autoMove, allowReturn, open, limit, closes, rooms };
  }
  if (kind === K.move) {
    const prompt = r.bool(), allowReturn = r.bool();
    r.bits(3);
    return { kind: 'move', prompt, allowReturn, room: r.str(), name: r.str(), ticket: text(r.blob()), back: text(r.blob()), parent: r.str(), closes: Number(r.uvarint()) };
  }
  if (kind === K.ret) {
    const expired = r.bool();
    r.bits(4);
    return { kind: 'return', expired, parent: r.str(), ticket: text(r.blob()), delay: Number(r.uvarint()) };
  }
  if (kind === K.announce) {
    r.bits(5);
    return { kind: 'announce', from: r.str(), text: text(r.blob()) };
  }
  if (kind === K.info) {
    const allowReturn = r.bool();
    r.bits(4);
    return { kind: 'info', allowReturn, name: r.str(), parent: r.str(), back: text(r.blob()), closes: Number(r.uvarint()) };
  }
  return null;
}

// The rooms a host sets: names, members (participant ids), and settings.
export function encodeSet(w, { names, members, limitSec = 0, autoMove = true, allowReturn = true }) {
  w.header(Op.Breakout);
  w.bits(K.set, 3);
  w.bool(autoMove);
  w.bool(allowReturn);
  w.bits(0, 3);
  w.uvarint(limitSec);
  w.uvarint(names.length);
  names.forEach((name, i) => {
    w.str(name);
    w.uvarint(members[i]?.length ?? 0);
    for (const id of members[i] ?? []) w.str(id);
  });
}

export function encodeCmd(w, kind) {
  w.header(Op.Breakout);
  w.bits(kind, 3);
  w.bits(0, 5);
}

export function encodeAnnounce(w, msg) {
  w.header(Op.Breakout);
  w.bits(K.announce, 3);
  w.bits(0, 5);
  w.blob(new TextEncoder().encode(msg));
}
