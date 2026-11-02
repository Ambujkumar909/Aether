import test from 'node:test';
import assert from 'node:assert/strict';

import { bestGrid, galleryOrder, tileCap } from './gallery.js';

test('one person fills the box at 16:9', () => {
  const g = bestGrid(1, 1600, 900, 8);
  assert.deepEqual([g.cols, g.rows, g.w, g.h], [1, 1, 1600, 900]);
});

test('a wide box puts two side by side, a tall phone stacks them', () => {
  assert.equal(bestGrid(2, 1600, 600, 8).cols, 2);
  assert.equal(bestGrid(2, 358, 600, 8).cols, 1);
});

test('nine on a desktop stage is three by three', () => {
  const g = bestGrid(9, 1200, 680, 8);
  assert.deepEqual([g.cols, g.rows], [3, 3]);
  // and the tiles fit: three widths and two gaps inside the box, same for height
  assert.ok(g.w * 3 + 16 <= 1200 && g.h * 3 + 16 <= 680);
});

test('the chosen grid is never beaten by another column count', () => {
  for (const [n, W, H] of [[5, 900, 500], [7, 390, 600], [12, 1280, 700], [49, 1900, 1000], [3, 820, 900]]) {
    const best = bestGrid(n, W, H, 6);
    for (let cols = 1; cols <= n; cols++) {
      const rows = Math.ceil(n / cols);
      const cw = (W - 6 * (cols - 1)) / cols, ch = (H - 6 * (rows - 1)) / rows;
      const w = Math.min(cw, ch * 16 / 9);
      assert.ok(Math.floor(w) <= best.w, `n=${n} ${W}x${H}: ${cols} cols gives ${w}, chose ${best.cols} -> ${best.w}`);
    }
  }
});

test('nothing to lay out, or no room, is a zero tile rather than a crash', () => {
  assert.equal(bestGrid(0, 800, 600).w, 0);
  assert.equal(bestGrid(4, 0, 600).w, 0);
});

test('gallery order: video first, speakers first among them, then join order; self and presentations out', () => {
  const list = [
    { id: 'me', self: true, hasVideo: true, rank: -1 },
    { id: 'a', hasVideo: false, rank: -1 },
    { id: 'b', hasVideo: true, rank: -1 },
    { id: 'c', hasVideo: true, rank: 0 },
    { id: 'd', hasVideo: false, rank: 1 },
    { id: 'b\u0000screen', presentation: true, hasVideo: true, rank: -1 },
  ];
  assert.deepEqual(galleryOrder(list).map((p) => p.id), ['c', 'b', 'd', 'a']);
});

test('caps: gallery 9, large gallery 49', () => {
  assert.equal(tileCap('gallery'), 9);
  assert.equal(tileCap('large'), 49);
});
