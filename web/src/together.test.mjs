import test from 'node:test';
import assert from 'node:assert/strict';

import { quantize, encodeMask, decodeMask, MASK_W, MASK_H, MAX_MASK_BYTES } from './together.js';

const N = MASK_W * MASK_H;

test('a person-shaped mask round-trips in about a hundred bytes', () => {
  // A head and shoulders: an ellipse and a block, on 256 x 256 confidences.
  const W = 256, H = 256, conf = new Float32Array(W * H);
  for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) {
    const head = ((x - 128) / 40) ** 2 + ((y - 80) / 50) ** 2 < 1;
    const body = y > 140 && x > 50 && x < 206;
    conf[y * W + x] = head || body ? 0.95 : 0.03;
  }
  const bits = quantize(conf, W, H);
  const bytes = encodeMask(bits);
  assert.ok(bytes.length < 160, `${bytes.length} bytes`);
  assert.deepEqual(decodeMask(bytes), bits);
  // The head is the person and a corner is not.
  assert.equal(bits[Math.floor((80 / 256) * MASK_H) * MASK_W + MASK_W / 2], 1);
  assert.equal(bits[0], 0);
});

test('empty and full masks are one run each', () => {
  assert.deepEqual([...encodeMask(new Uint8Array(N))], [N & 0x7f | 0x80, N >> 7]);
  const full = new Uint8Array(N).fill(1);
  const e = encodeMask(full);
  assert.equal(e[0], 0); // no background first
  assert.deepEqual(decodeMask(e), full);
});

test('a cell is the person when more than half of it is', () => {
  const conf = new Float32Array(MASK_W * 2 * MASK_H * 2); // 2x2 per cell
  conf[0] = conf[1] = conf[MASK_W * 2] = 1; // three of the first cell's four
  conf[2] = 1; // one of the second's four
  const bits = quantize(conf, MASK_W * 2, MASK_H * 2);
  assert.equal(bits[0], 1);
  assert.equal(bits[1], 0);
});

test('anything that is not exactly one mask is refused', () => {
  assert.equal(decodeMask(new Uint8Array([5])), null, 'too short');
  const good = encodeMask(new Uint8Array(N));
  assert.equal(decodeMask(Uint8Array.from([...good, 1])), null, 'one cell too many');
  assert.equal(decodeMask(new Uint8Array([0xff, 0xff, 0xff, 0xff])), null, 'a run that never ends');
  assert.equal(decodeMask(new Uint8Array(MAX_MASK_BYTES + 1)), null, 'over the size bound');
  assert.equal(decodeMask(null), null);
});

import { Writer, Reader, newDictPair } from './wire.js';
import { encodeSet, encodeOutline, decodeTogether, seats, SCENES, PORTRAIT } from './together.js';

test('the state and an outline decode as the server writes them', () => {
  const { tx, rx } = newDictPair();
  const w = new Writer(tx);
  w.header(0x128); w.bits(0, 3); w.bool(true); w.bits(0, 4); w.str('Maya Chen'); w.uvarint(2);
  const r = new Reader(w.bytes(), rx); r.header();
  assert.deepEqual(decodeTogether(r), { kind: 'state', on: true, by: 'Maya Chen', scene: 2 });
  const bits = new Uint8Array(N); bits.fill(1, 100, 400);
  const w2 = new Writer(tx);
  w2.header(0x128); w2.bits(1, 3); w2.bits(0, 5); w2.str('p7'); w2.blob(encodeMask(bits));
  const r2 = new Reader(w2.bytes(), rx); r2.header();
  assert.deepEqual(decodeTogether(r2), { kind: 'mask', from: 'p7', bits });
});

test('what a client sends: set is 4 bytes with the header, an outline its RLE plus 3', () => {
  const w = new Writer(null); encodeSet(w, true, 1);
  assert.equal(w.bytes().length, 4);
  const rle = encodeMask(new Uint8Array(N));
  const w2 = new Writer(null); encodeOutline(w2, rle);
  assert.equal(w2.bytes().length, 2 + 1 + 1 + rle.length);
});

test('seats: one each, on the stage, not overlapping, front row largest', () => {
  for (const scene of SCENES.map((s) => s.id)) for (const n of [1, 2, 4, 5, 9, 12, 24]) {
    const s = seats(scene, n);
    assert.equal(s.length, n, `scene ${scene}, ${n} people`);
    for (const a of s) assert.ok(a.x >= 0 && a.y >= 0 && a.x + a.w <= 1 && a.y + a.h <= 1.0001, `on stage: ${JSON.stringify(a)}`);
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const a = s[i], b = s[j];
      const ox = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x), oy = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
      if (a.row === b.row) assert.ok(ox <= 1e-9, `seats ${i} and ${j} overlap in a row`);
    }
    if (n > 4) assert.ok(s[0].w > s[n - 1].w, 'the front row is larger');
  }
  assert.deepEqual(seats(0, 0), []);
});

test('seats, upright: on the stage, at most five to a row, readable at fifty', () => {
  for (const scene of SCENES.map((x) => x.id)) for (const n of [1, 3, 7, 12, 25, 49, 50]) {
    const s = seats(scene, n, true);
    assert.equal(s.length, n);
    const perRow = new Map();
    for (const a of s) {
      assert.ok(a.x >= 0 && a.y >= 0 && a.x + a.w <= 1 + 1e-9 && a.y + a.h <= 1 + 1e-9, `on stage: ${JSON.stringify(a)}`);
      perRow.set(a.row, (perRow.get(a.row) ?? 0) + 1);
      // A seat is 16:9 on screen in a 3:4 scene.
      assert.ok(Math.abs(a.h - a.w * PORTRAIT * (9 / 16)) < 1e-9, 'the seat keeps its shape');
    }
    assert.ok(Math.max(...perRow.values()) <= 5, `${n}: at most five to a row`);
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const a = s[i], b = s[j];
      if (a.row === b.row) assert.ok(Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x) <= 1e-9, 'no overlap in a row');
    }
    if (n > 5) assert.ok(s[0].w >= s[n - 1].w, 'the front row is not smaller');
    // On a 390-px phone a seat is wide enough to show a first name.
    if (n === 50) assert.ok(Math.min(...s.map((a) => a.w)) * 390 >= 44, `smallest seat ${Math.min(...s.map((a) => a.w)) * 390} px`);
  }
});

test('hysteresis: a wobbling edge does not flip a cell', () => {
  const conf = new Float32Array(MASK_W * MASK_H);
  conf[0] = 0.55; // just over a half
  const first = quantize(conf, MASK_W, MASK_H);
  assert.equal(first[0], 1, 'no history: over a half is the person');
  conf[0] = 0.45; // the segmenter wobbles under
  assert.equal(quantize(conf, MASK_W, MASK_H, first)[0], 1, 'stays the person above 0.4');
  conf[0] = 0.35;
  assert.equal(quantize(conf, MASK_W, MASK_H, first)[0], 0, 'leaves below 0.4');
  const off = new Uint8Array(MASK_W * MASK_H);
  conf[0] = 0.55;
  assert.equal(quantize(conf, MASK_W, MASK_H, off)[0], 0, 'and rejoins only above 0.6');
  conf[0] = 0.65;
  assert.equal(quantize(conf, MASK_W, MASK_H, off)[0], 1);
});
