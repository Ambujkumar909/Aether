import test from 'node:test';
import assert from 'node:assert/strict';

import { createJitterClock, FLOOR, MAX, STEP, SLACK, WINDOW, DECAY_AFTER, GAP } from './jitter.js';

const D = 0.02; // a 20 ms Opus chunk
const close = (a, b, eps = 1e-9) => Math.abs(a - b) < eps;

test('steady arrivals: the first chunk anchors at the floor and the rest follow contiguously, none dropped', () => {
  const c = createJitterClock();
  let now = 10;
  const r0 = c.schedule(now, D);
  assert.ok(close(r0.at, now + FLOOR), 'anchored one floor ahead');
  for (let i = 1; i < 300; i++) {
    now += D;
    const r = c.schedule(now, D);
    assert.equal(r.drop, undefined, `chunk ${i} kept`);
    assert.ok(close(r.at, 10 + FLOOR + i * D), `chunk ${i} contiguous`);
  }
  assert.equal(c.underruns, 0);
  assert.equal(c.trimmed, 0);
});

test('bursty arrivals -- three chunks at once every 60 ms -- are not trimmed and not torn', () => {
  const c = createJitterClock();
  let now = 0;
  let drops = 0;
  let scheduled = [];
  for (let g = 0; g < 100; g++) {
    for (let k = 0; k < 3; k++) {
      const r = c.schedule(now, D);
      if (r.drop) drops++;
      else scheduled.push(r.at);
    }
    now += 3 * D;
  }
  assert.equal(drops, 0, 'a normal TCP burst is inside the slack, nothing dropped');
  assert.equal(c.underruns, 0);
  for (let i = 1; i < scheduled.length; i++) assert.ok(close(scheduled[i] - scheduled[i - 1], D), 'contiguous playout');
});

test('an underrun grows the target by one step and re-anchors; repeated underruns stop at MAX', () => {
  const c = createJitterClock();
  c.schedule(0, D);
  const r = c.schedule(0.2, D); // 100 ms late: its slot has passed
  assert.equal(c.underruns, 1);
  assert.ok(close(c.target, FLOOR + STEP));
  assert.ok(close(r.at, 0.2 + FLOOR + STEP), 're-anchored at the new target');
  // Every chunk 200 ms late -- inside GAP, so each is a real underrun.
  let now = 0.2;
  for (let i = 0; i < 40; i++) {
    now += 0.2;
    c.schedule(now, D);
  }
  assert.ok(close(c.target, MAX), `target capped at MAX (${c.target})`);
});

test('a start-up burst is worked off at twice real time, then steady state is clean', () => {
  const c = createJitterClock();
  const now0 = 5;
  // Twenty chunks land at once: 400 ms of audio ahead of a 60 ms target.
  for (let i = 0; i < 20; i++) assert.equal(c.schedule(now0, D).drop, undefined, 'the burst itself is scheduled, not judged chunk by chunk');
  let now = now0;
  let lastAhead = Infinity;
  let dropsInFirstWindow = 0;
  // The first window still holds the anchor chunk, whose lead was the
  // floor, so its low-water mark is fine: nothing is trimmed yet. The
  // second window sees the burst's true lead and starts working it off.
  for (let i = 0; i < 2 * WINDOW - 21; i++) {
    now += D;
    if (c.schedule(now, D).drop) dropsInFirstWindow++;
  }
  assert.equal(dropsInFirstWindow, 0, 'no trimming before a whole window above target has been observed');
  // Over the next second the excess (~360 ms) is worked off, every other chunk.
  let drops = 0;
  for (let i = 0; i < 60; i++) {
    now += D;
    const r = c.schedule(now, D);
    if (r.drop) drops++;
    else lastAhead = r.ahead;
  }
  assert.ok(drops >= 15 && drops <= 20, `about eighteen chunks dropped to absorb 360 ms (${drops})`);
  assert.ok(lastAhead <= FLOOR + SLACK + 2 * D + 1e-9, `lead back near the target (${lastAhead})`);
  // Then steady: nothing more dropped for a long while.
  let later = 0;
  for (let i = 0; i < 300; i++) {
    now += D;
    if (c.schedule(now, D).drop) later++;
  }
  assert.equal(later, 0, 'steady state after the burst is not trimmed');
});

test('with no underrun for DECAY_AFTER seconds the target eases back toward the floor', () => {
  const c = createJitterClock();
  c.schedule(0, D);
  c.schedule(0.2, D); // one underrun (inside GAP): target = floor + step
  assert.ok(close(c.target, FLOOR + STEP));
  let now = 0.2;
  for (let i = 0; i < 400; i++) {
    now += D;
    c.schedule(now, D);
  }
  assert.ok(c.target > FLOOR, 'still above the floor inside the quiet window');
  for (let i = 0; i < 3000; i++) {
    now += D;
    c.schedule(now, D);
  }
  assert.ok(close(c.target, FLOOR), `back at the floor (${c.target})`);
  assert.ok(now - 0.2 > DECAY_AFTER);
});

test('a pause in the stream -- a muted sender sends nothing -- is not an underrun', () => {
  const c = createJitterClock();
  let now = 0;
  for (let i = 0; i < 50; i++) {
    c.schedule(now, D);
    now += D;
  }
  now += GAP + 1; // silence: no chunks at all for over a second
  const r = c.schedule(now, D);
  assert.equal(c.underruns, 0, 'the gap did not count as an underrun');
  assert.ok(close(c.target, FLOOR), 'the target did not grow');
  assert.ok(close(r.at, now + FLOOR), 'anchored afresh at the floor');
  // Twenty mute/unmute cycles: still at the floor.
  for (let k = 0; k < 20; k++) {
    for (let i = 0; i < 25; i++) {
      now += D;
      c.schedule(now, D);
    }
    now += GAP + 0.5;
  }
  c.schedule(now, D);
  assert.equal(c.underruns, 0);
  assert.ok(close(c.target, FLOOR), `twenty pauses later the target is still the floor (${c.target})`);
});

test('reset forgets the schedule so the next chunk anchors afresh', () => {
  const c = createJitterClock();
  c.schedule(1, D);
  c.reset();
  const r = c.schedule(50, D);
  assert.ok(close(r.at, 50 + FLOOR));
  assert.equal(c.underruns, 0, 'a reset is not an underrun');
});
