import test from 'node:test';
import assert from 'node:assert/strict';

import { suggestRooms, autoAssign, shuffle, remaining, MAX_ROOMS } from './breakout.js';

test('suggested rooms: four people a room, one at least, fifty at most', () => {
  assert.equal(suggestRooms(0), 1);
  assert.equal(suggestRooms(4), 1);
  assert.equal(suggestRooms(5), 2);
  assert.equal(suggestRooms(9), 3);
  assert.equal(suggestRooms(10000), MAX_ROOMS);
});

test('automatic assignment: everyone once, rooms differ by at most one', () => {
  const ids = Array.from({ length: 11 }, (_, i) => 'p' + i);
  const rooms = autoAssign(ids, 3);
  assert.deepEqual(rooms.map((r) => r.length), [4, 4, 3]);
  assert.deepEqual(rooms.flat().sort(), [...ids].sort());
  assert.deepEqual(autoAssign([], 2), [[], []]);
});

test('shuffle: the same people, evenly, in a different order', () => {
  const ids = Array.from({ length: 8 }, (_, i) => 'p' + i);
  let seed = 7;
  const rand = () => ((seed = (seed * 16807) % 2147483647) / 2147483647);
  const rooms = shuffle(ids, 2, rand);
  assert.deepEqual(rooms.flat().sort(), [...ids].sort());
  assert.deepEqual(rooms.map((r) => r.length), [4, 4]);
  assert.notDeepEqual(rooms, autoAssign(ids, 2));
});

test('remaining time: m:ss, h:mm:ss, never negative, empty without a limit', () => {
  const now = Date.parse('2026-11-02T10:00:00Z');
  const at = (s) => now / 1000 + s;
  assert.equal(remaining(at(299), now), '4:59');
  assert.equal(remaining(at(3725), now), '1:02:05');
  assert.equal(remaining(at(-5), now), '0:00');
  assert.equal(remaining(0, now), '');
});
