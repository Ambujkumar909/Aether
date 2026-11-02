// The whiteboard's client half (engine signal/whiteboard.go): the frames,
// the board's coordinate space, and the palette. Both halves are pinned to
// the same bytes by a golden test.

import { Op } from './wire.js';

export const BOARD_W = 4096;
export const BOARD_H = 2304;
export const MAX_POINTS = 4096;
export const W = { state: 0, stroke: 1, erase: 2, note: 3, clear: 4, close: 5, live: 6 };
export const TOOL = { pen: 0, highlighter: 1 };

// Ink colours (0-7) and sticky-note colours (8-11). Indices go on the wire;
// the board is white in both themes, as a whiteboard is.
export const PALETTE = ['#1b1f24', '#0b7a62', '#2563eb', '#dc2626', '#ea580c', '#ca8a04', '#7c3aed', '#ffffff', '#fde68a', '#bbf7d0', '#bfdbfe', '#fbcfe8'];
export const INK = [0, 1, 2, 3, 4, 5, 6];
export const NOTE_COLORS = [8, 9, 10, 11];

export function writeStyle(w, tool, color, width) {
  w.bits(tool, 1);
  w.bits(color, 4);
  w.bits(width - 1, 3);
}

export function readStyle(r) {
  return { tool: r.bits(1), color: r.bits(4), width: r.bits(3) + 1 };
}

// Count, first point absolute, then zigzag deltas: two bytes a point for a
// pen moving a few units between samples.
export function writePoints(w, pts) {
  const n = pts.length / 2;
  w.uvarint(n);
  let px = 0, py = 0;
  for (let i = 0; i < n; i++) {
    const x = pts[2 * i], y = pts[2 * i + 1];
    if (i === 0) {
      w.uvarint(x);
      w.uvarint(y);
    } else {
      w.svarint(x - px);
      w.svarint(y - py);
    }
    px = x;
    py = y;
  }
}

export function readPoints(r) {
  const n = Number(r.uvarint());
  if (!n || n > MAX_POINTS || r.err) return null;
  const pts = new Array(2 * n);
  let x = 0, y = 0;
  for (let i = 0; i < n; i++) {
    if (i === 0) {
      x = Number(r.uvarint());
      y = Number(r.uvarint());
    } else {
      x += Number(r.svarint());
      y += Number(r.svarint());
    }
    if (r.err || x < 0 || y < 0 || x >= BOARD_W || y >= BOARD_H) return null;
    pts[2 * i] = x;
    pts[2 * i + 1] = y;
  }
  return pts;
}

// Screen to board: a point on a canvas of size cw x ch, clamped onto the board.
export function toBoard(x, y, cw, ch) {
  const bx = Math.round((x / cw) * BOARD_W);
  const by = Math.round((y / ch) * BOARD_H);
  return [Math.min(BOARD_W - 1, Math.max(0, bx)), Math.min(BOARD_H - 1, Math.max(0, by))];
}

// Frames, client to server.
export function encodeOpen(w) {
  w.header(Op.Whiteboard);
  w.bits(W.state, 3);
  w.bits(0, 5);
}

export function encodeStroke(w, kind, seq, tool, color, width, pts) {
  w.header(Op.Whiteboard);
  w.bits(kind, 3);
  w.bits(0, 5);
  w.uvarint(seq);
  writeStyle(w, tool, color, width);
  writePoints(w, pts);
}

export function encodeSimple(w, kind, id = 0) {
  w.header(Op.Whiteboard);
  w.bits(kind, 3);
  w.bits(0, 5);
  if (kind === W.erase) w.uvarint(id);
}

export function encodeNote(w, id, x, y, color, text) {
  w.header(Op.Whiteboard);
  w.bits(W.note, 3);
  w.bits(0, 5);
  w.uvarint(id);
  w.uvarint(x);
  w.uvarint(y);
  w.bits(color, 4);
  w.bits(0, 4);
  w.blob(new TextEncoder().encode(text));
}

// One server frame, decoded in full.
export function decodeWhiteboard(r) {
  const kind = r.bits(3);
  if (kind === W.state) {
    const open = r.bool();
    r.bits(4);
    const opener = r.str();
    const strokes = [];
    const ns = Number(r.uvarint());
    for (let i = 0; i < ns && !r.err; i++) {
      const id = Number(r.uvarint());
      const author = r.str();
      const st = readStyle(r);
      const points = readPoints(r);
      if (!points) return null;
      strokes.push({ id, author, ...st, points });
    }
    const notes = [];
    const nn = Number(r.uvarint());
    for (let i = 0; i < nn && !r.err; i++) {
      const id = Number(r.uvarint());
      const author = r.str();
      const x = Number(r.uvarint()), y = Number(r.uvarint());
      const color = r.bits(4);
      r.bits(4);
      notes.push({ id, author, x, y, color, text: new TextDecoder().decode(r.blob()) });
    }
    return { kind: 'state', open, opener, strokes, notes };
  }
  r.bits(5);
  if (kind === W.stroke) {
    const id = Number(r.uvarint()), seq = Number(r.uvarint()), author = r.str();
    const st = readStyle(r);
    const points = readPoints(r);
    return points ? { kind: 'stroke', id, seq, author, ...st, points } : null;
  }
  if (kind === W.live) {
    const seq = Number(r.uvarint()), author = r.str();
    const st = readStyle(r);
    const points = readPoints(r);
    return points ? { kind: 'live', seq, author, ...st, points } : null;
  }
  if (kind === W.erase) return { kind: 'erase', id: Number(r.uvarint()) };
  if (kind === W.note) {
    const id = Number(r.uvarint()), author = r.str();
    const x = Number(r.uvarint()), y = Number(r.uvarint());
    const color = r.bits(4);
    r.bits(4);
    return { kind: 'note', id, author, x, y, color, text: new TextDecoder().decode(r.blob()) };
  }
  if (kind === W.clear) return { kind: 'clear' };
  if (kind === W.close) return { kind: 'close' };
  return null;
}
