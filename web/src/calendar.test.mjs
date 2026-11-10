import test from 'node:test';
import assert from 'node:assert/strict';

import { weekOf, hourSpan, layoutColumns, recurrenceText, busyBlocks, freeSlot } from './calendar.js';

const at = (d, h, m = 0) => new Date(2026, 10, d, h, m); // November 2026; the 9th is a Monday
const ev = (title, s, e) => ({ title, start: s, end: e });

test('the week starts on Monday, whatever day it is asked about', () => {
  assert.equal(weekOf(at(11, 15)).getTime(), at(9, 0).getTime()); // Wednesday
  assert.equal(weekOf(at(15, 23, 59)).getTime(), at(9, 0).getTime()); // Sunday
  assert.equal(weekOf(at(9, 0)).getTime(), at(9, 0).getTime()); // Monday itself
});

test('the hour span covers the working day and grows around meetings with air', () => {
  assert.deepEqual(hourSpan([]), [8, 18]);
  assert.deepEqual(hourSpan([ev('early', at(9, 6), at(9, 6, 45))]), [5, 18]);
  assert.deepEqual(hourSpan([ev('late', at(9, 19, 30), at(9, 20, 15))]), [8, 22]);
  assert.deepEqual(hourSpan([ev('night', at(9, 0), at(9, 23, 30))]), [0, 24]);
});

test('overlapping events share the width; others take all of it', () => {
  const a = ev('a', at(9, 10), at(9, 11));
  const b = ev('b', at(9, 10, 30), at(9, 11, 30));
  const c = ev('c', at(9, 11), at(9, 12)); // after a ends: a's column is free again
  const d = ev('d', at(9, 14), at(9, 15)); // alone
  const got = layoutColumns([d, c, b, a]);
  const by = Object.fromEntries(got.map((x) => [x.m.title, [x.col, x.cols]]));
  assert.deepEqual(by, { a: [0, 2], b: [1, 2], c: [0, 2], d: [0, 1] });
});

test('a repeat rule reads as a sentence', () => {
  assert.equal(recurrenceText(null), '');
  assert.equal(recurrenceText({ freq: 'daily' }), 'Every day');
  assert.equal(recurrenceText({ freq: 'weekly', interval: 2, days: [4, 1] }), 'Every 2 weeks on Mon and Thu');
  assert.equal(recurrenceText({ freq: 'weekly', days: [1, 3, 5] }), 'Every week on Mon, Wed and Fri');
  const until = new Date(2026, 11, 12).toLocaleDateString([], { day: 'numeric', month: 'short' });
  assert.equal(recurrenceText({ freq: 'monthly', until: new Date(2026, 11, 12).toISOString() }), `Every month, until ${until}`);
});

test('busy spans become bars inside the visible hours, clipped', () => {
  const day = at(9, 0);
  const blocks = busyBlocks(
    [
      { from: at(9, 9), to: at(9, 10) }, // 1 of 10 hours, starting at the left edge
      { from: at(9, 7), to: at(9, 8, 30) }, // clipped to 8:00-8:30
      { from: at(10, 9), to: at(10, 10) }, // another day: dropped
    ],
    day,
    8,
    18
  );
  assert.deepEqual(blocks, [
    { left: 10, width: 10 },
    { left: 0, width: 5 },
  ]);
});

test('the first free half hour inside working hours, skipping the weekend', () => {
  const busy = {
    'a@x': [{ from: at(13, 9), to: at(13, 12) }], // Friday morning
    'b@x': [{ from: at(13, 12), to: at(13, 17, 30) }], // Friday afternoon
  };
  // Asked on Friday 08:40 for an hour: 09:00 is taken, every slot until 17:30
  // overlaps somebody, 17:30 would end at 18:30; Monday 08:00 is the answer.
  assert.equal(freeSlot(busy, at(13, 8, 40), 60).getTime(), at(16, 8).getTime());
  // Thirty minutes fits at 17:30.
  assert.equal(freeSlot(busy, at(13, 8, 40), 30).getTime(), at(13, 17, 30).getTime());
  // Nobody busy: the next step boundary.
  assert.equal(freeSlot({}, at(9, 10, 1), 30).getTime(), at(9, 10, 30).getTime());
  // Nothing within the window.
  assert.equal(freeSlot({ 'a@x': [{ from: at(9, 0), to: at(30, 0) }] }, at(9, 8), 30), null);
});
