import test from 'node:test';
import assert from 'node:assert/strict';

import { Writer, Reader } from './wire.js';
import { writeStyle, readStyle, writePoints, readPoints, toBoard, MAX_POINTS, BOARD_W, BOARD_H } from './whiteboard.js';

// The same hex as engine/internal/signal/whiteboard_test.go wbGolden.
const GOLDEN = '19' + '0364c80106030e18';
const hex = (b) => [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
const unhex = (s) => Uint8Array.from(s.match(/../g).map((h) => parseInt(h, 16)));

test('the bytes match the server, both ways', () => {
  const w = new Writer(null);
  writeStyle(w, 0, 3, 2);
  writePoints(w, [100, 200, 103, 198, 110, 210]);
  assert.equal(hex(w.bytes()), GOLDEN);
  const r = new Reader(unhex(GOLDEN), null);
  assert.deepEqual(readStyle(r), { tool: 0, color: 3, width: 2 });
  assert.deepEqual(readPoints(r), [100, 200, 103, 198, 110, 210]);
});

test('a count past the bound, or a point off the board, is refused', () => {
  const many = [];
  for (let i = 0; i <= MAX_POINTS; i++) many.push(i % 4000, 5);
  const w = new Writer(null);
  writePoints(w, many);
  assert.equal(readPoints(new Reader(w.bytes(), null)), null);
  const off = new Writer(null);
  off.uvarint(2); off.uvarint(4000); off.uvarint(10); off.svarint(200); off.svarint(0);
  assert.equal(readPoints(new Reader(off.bytes(), null)), null);
});

test('screen to board: scaled to the 4096 x 2304 board and clamped onto it', () => {
  assert.deepEqual(toBoard(0, 0, 800, 450), [0, 0]);
  assert.deepEqual(toBoard(400, 225, 800, 450), [2048, 1152]);
  assert.deepEqual(toBoard(800, 450, 800, 450), [BOARD_W - 1, BOARD_H - 1]);
  assert.deepEqual(toBoard(-5, 900, 800, 450), [0, BOARD_H - 1]);
});
