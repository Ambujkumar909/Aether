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
