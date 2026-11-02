import test from 'node:test';
import assert from 'node:assert/strict';

import { whenFull } from './when.js';

// A fixed "now", so the tests do not change meaning at midnight.
const now = new Date(2026, 8, 11, 9, 0); // Fri 11 Sep 2026, 09:00 local
const at = (y, mo, d, h, mi) => new Date(y, mo, d, h, mi).toISOString();
const time = (y, mo, d, h, mi) =>
  new Date(y, mo, d, h, mi).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

test('today is named, not implied', () => {
  assert.equal(whenFull(at(2026, 8, 11, 8, 41), now), `Today · ${time(2026, 8, 11, 8, 41)}`);
});

test('yesterday is named', () => {
  assert.equal(whenFull(at(2026, 8, 10, 23, 5), now), `Yesterday · ${time(2026, 8, 10, 23, 5)}`);
});

test('within the week the weekday carries the day', () => {
  const s = whenFull(at(2026, 8, 8, 14, 30), now); // Tuesday
  assert.equal(s, `${new Date(2026, 8, 8).toLocaleDateString([], { weekday: 'short' })} · ${time(2026, 8, 8, 14, 30)}`);
});

test('a week or more ago the date carries the day', () => {
  const s = whenFull(at(2026, 8, 4, 10, 0), now); // 7 days
  assert.equal(
    s,
    `${new Date(2026, 8, 4).toLocaleDateString([], { day: 'numeric', month: 'short' })} · ${time(2026, 8, 4, 10, 0)}`
  );
});

// The boundary is the calendar day, not 24 hours: 23:50 yesterday seen at
// 00:10 is still "Yesterday", not "Today".
test('the day boundary is midnight, not 24 hours', () => {
  const justAfterMidnight = new Date(2026, 8, 11, 0, 10);
  assert.match(whenFull(at(2026, 8, 10, 23, 50), justAfterMidnight), /^Yesterday · /);
});
