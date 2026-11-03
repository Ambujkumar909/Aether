import { Op } from './wire.js';

// Together mode's cut-outs (ARCHITECTURE §6bo).
//
// Teams puts everyone in one scene, cut out of their own rooms. Here each
// sender, while the meeting is in Together mode, sends the outline of the
// person in its own picture alongside the video it already sends: a 64 x 36
// mask, run-length coded -- about a hundred bytes. Receivers cut each video
// out with it and seat it in the scene. Nobody segments anybody else's video,
// and the video itself is unchanged for whoever is not looking at the scene.
//
// A mask is self-contained: losing one costs the next tenth of a second of
// outline, nothing more (rule 5).

export const MASK_W = 64;
export const MASK_H = 36;
export const MAX_MASK_BYTES = 1024; // a worst case checkerboard is ~2.3 KB; a person is ~100 B

// The segmenter's confidence (any size) to a 64 x 36 grid of 0/1: a cell is
// the person when more than half its area is. With the previous grid, with
// hysteresis: a cell joins the person above 0.6 and leaves below 0.4, so the
// segmenter's wobble at an edge does not flip it back and forth -- each flip
// would be an outline on the wire for nothing.
export const HYST = 0.1;
export function quantize(conf, w, h, prev = null) {
  const out = new Uint8Array(MASK_W * MASK_H);
  for (let y = 0; y < MASK_H; y++) {
    const y0 = Math.floor((y * h) / MASK_H), y1 = Math.max(y0 + 1, Math.floor(((y + 1) * h) / MASK_H));
    for (let x = 0; x < MASK_W; x++) {
      const x0 = Math.floor((x * w) / MASK_W), x1 = Math.max(x0 + 1, Math.floor(((x + 1) * w) / MASK_W));
      let sum = 0, n = 0;
      for (let yy = y0; yy < y1; yy++) for (let xx = x0; xx < x1; xx++, n++) sum += conf[yy * w + xx];
      const k = y * MASK_W + x;
      const t = prev ? (prev[k] ? 0.5 - HYST : 0.5 + HYST) : 0.5;
      out[k] = sum / n > t ? 1 : 0;
    }
  }
  return out;
}

// Runs of alternating value, starting with background (0), as unsigned
// varints. An empty or full mask is one run.
export function encodeMask(bits) {
  const runs = [];
  let cur = 0, n = 0;
  for (const b of bits) {
    if (b === cur) n++;
    else {
      runs.push(n);
      cur = b;
      n = 1;
    }
  }
  runs.push(n);
  const out = [];
  for (let r of runs) {
    while (r >= 0x80) {
      out.push((r & 0x7f) | 0x80);
      r >>>= 7;
    }
    out.push(r);
  }
  return Uint8Array.from(out);
}

// Null for anything that is not exactly one 64 x 36 mask.
export function decodeMask(bytes) {
  if (!bytes || bytes.length > MAX_MASK_BYTES) return null;
  const bits = new Uint8Array(MASK_W * MASK_H);
  let i = 0, pos = 0, cur = 0;
  while (i < bytes.length) {
    let r = 0, shift = 0, b;
    do {
      if (i >= bytes.length || shift > 21) return null;
      b = bytes[i++];
      r |= (b & 0x7f) << shift;
      shift += 7;
    } while (b & 0x80);
    if (pos + r > bits.length) return null;
    if (cur) bits.fill(1, pos, pos + r);
    pos += r;
    cur ^= 1;
  }
  return pos === bits.length ? bits : null;
}

// ---------------------------------------------------------------------------
// On the wire (OpTogether 0x128, engine/internal/signal/together.go).
// ---------------------------------------------------------------------------


export function encodeSet(w, on, scene) {
  w.header(Op.Together);
  w.bits(0, 3);
  w.bool(on);
  w.bits(0, 4);
  w.uvarint(scene);
}

export function encodeOutline(w, rle) {
  w.header(Op.Together);
  w.bits(1, 3);
  w.bits(0, 5);
  w.blob(rle);
}

export function decodeTogether(r) {
  const kind = r.bits(3);
  if (kind === 0) {
    const on = r.bool();
    r.bits(4);
    const s = { kind: 'state', on, by: '', scene: 0 };
    if (on) {
      s.by = r.str();
      s.scene = Number(r.uvarint());
    }
    return r.err ? null : s;
  }
  if (kind === 1) {
    r.bits(5);
    const from = r.str();
    const bits = decodeMask(r.blob());
    return r.err || !bits ? null : { kind: 'mask', from, bits };
  }
  return null;
}

// ---------------------------------------------------------------------------
// Scenes: where each seat is, as fractions of the stage, for n people.
// ---------------------------------------------------------------------------

export const SCENES = [
  { id: 0, name: 'Auditorium' },
  { id: 1, name: 'Café' },
  { id: 2, name: 'Conference room' },
];

// Seats in rows across the stage, the back rows smaller and higher, as in a
// raked auditorium. Each seat is { x, y, w, h } as fractions of a 16:9 scene,
// the person's 16:9 picture drawn into it. Rows tighten as people arrive.
// PORTRAIT is the scene's shape on a tall screen (a phone held upright):
// 3:4, at most five to a row. Drawn at 16:9 on a phone, fifty people sat
// thirteen to a row in a strip a third of the screen tall, and their names
// were five pixels high (the 50-person stress run, ARCHITECTURE 6br).
export const PORTRAIT = 3 / 4;

export function seats(scene, n, portrait = false) {
  if (n <= 0) return [];
  if (portrait) return portraitSeats(scene, n);
  const rows = n <= 4 ? 1 : n <= 10 ? 2 : n <= 18 ? 3 : 4;
  const out = [];
  let left = n;
  for (let row = 0; row < rows; row++) {
    const inRow = Math.ceil(left / (rows - row));
    left -= inRow;
    // Front row (row 0) is lowest and largest.
    const depth = rows === 1 ? 0 : row / (rows - 1);
    const scale = 1 - 0.32 * depth;
    const spread = scene === 2 ? 0.9 : 0.94; // the conference table is narrower
    // A few people get larger seats: the room is theirs.
    const w = Math.min((spread / inRow) * 0.92, rows === 1 ? 0.34 : 0.3) * scale;
    const h = w; // the scene is drawn at 16:9, so a 16:9 seat is as tall as it is wide here
    const y = (rows === 1 ? 0.56 : 0.62) - depth * 0.42 - (scene === 1 ? 0.04 : 0);
    const total = inRow * w + (inRow - 1) * w * 0.12;
    const x0 = (1 - total) / 2;
    for (let i = 0; i < inRow; i++) out.push({ x: x0 + i * w * 1.12, y, w, h, row });
  }
  return out;
}

// The upright scene: five to a row at most, rows stepping back up the
// screen. Coordinates are fractions of a 3:4 scene; a seat is 16:9 on screen,
// so its height as a fraction is its width x (3/4) x (9/16).
function portraitSeats(scene, n) {
  const perRow = Math.min(5, Math.max(2, Math.ceil(Math.sqrt(n * 0.6))));
  const rows = Math.ceil(n / perRow);
  const out = [];
  let left = n;
  const spread = scene === 2 ? 0.88 : 0.94;
  // Room for every row between the floor (0.9) and the top of the wall.
  const front = 0.9;
  const top = 0.06;
  let bottom = front;
  for (let row = 0; row < rows; row++) {
    const inRow = Math.ceil(left / (rows - row));
    left -= inRow;
    const depth = rows === 1 ? 0 : row / (rows - 1);
    const scale = 1 - 0.22 * depth;
    let w = Math.min((spread / perRow) * 0.92, 0.42) * scale;
    let h = w * PORTRAIT * (9 / 16);
    // Rows overlap a little (the back row's desk hides behind the front
    // row's heads), and every row must fit above the floor.
    const pitch = Math.min(h * 0.95, (front - top) / Math.max(1, rows));
    if (h * 0.95 > pitch) {
      w *= pitch / (h * 0.95);
      h = w * PORTRAIT * (9 / 16);
    }
    const y = bottom - h;
    bottom -= pitch;
    const total = inRow * w + (inRow - 1) * w * 0.12;
    const x0 = (1 - total) / 2;
    for (let i = 0; i < inRow; i++) out.push({ x: x0 + i * w * 1.12, y, w, h, row });
  }
  return out;
}
