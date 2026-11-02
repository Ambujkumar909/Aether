import test from 'node:test';
import assert from 'node:assert/strict';

import { createIdleWatch, IDLE_AFTER } from './idle.js';

function clock(start = 1_000_000) {
  let t = start;
  return { now: () => t, advance: (ms) => (t += ms) };
}

test('five quiet minutes is idle; any input is not; the change is reported once', () => {
  const c = clock();
  const changes = [];
  const w = createIdleWatch({ now: c.now, onChange: (v) => changes.push(v) });
  assert.equal(IDLE_AFTER, 5 * 60_000);
  c.advance(IDLE_AFTER - 1);
  w.tick();
  assert.equal(w.idle, false, 'one millisecond short');
  c.advance(1);
  w.tick();
  assert.equal(w.idle, true, 'on the line');
  w.tick();
  w.tick();
  assert.deepEqual(changes, [true], 'reported once, not on every tick');
  w.activity();
  assert.equal(w.idle, false);
  assert.deepEqual(changes, [true, false]);
  w.activity();
  assert.deepEqual(changes, [true, false], 'input while active reports nothing');
});

test('input resets the clock', () => {
  const c = clock();
  const w = createIdleWatch({ now: c.now });
  c.advance(4 * 60_000);
  w.activity();
  c.advance(4 * 60_000);
  w.tick();
  assert.equal(w.idle, false, 'eight minutes with input in the middle is not idle');
  c.advance(60_000);
  w.tick();
  assert.equal(w.idle, true);
});

test('a desktop in the background is judged by the timer; a phone in the background is away at once', () => {
  const c = clock();
  const desk = createIdleWatch({ now: c.now });
  desk.visibility(true);
  desk.tick();
  assert.equal(desk.idle, false, 'hidden desktop, not yet');
  c.advance(IDLE_AFTER);
  desk.tick();
  assert.equal(desk.idle, true);
  desk.visibility(false);
  assert.equal(desk.idle, false, 'coming back is activity');

  const phone = createIdleWatch({ now: c.now, hiddenIsIdle: true });
  phone.visibility(true);
  assert.equal(phone.idle, true, 'app in the background');
  phone.visibility(false);
  assert.equal(phone.idle, false);
});
