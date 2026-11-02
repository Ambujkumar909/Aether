import test from 'node:test';
import assert from 'node:assert/strict';

// The grouping rule as chat.js uses it, not a copy of it. A run of messages
// from one person shows the header once, and getting the boundaries wrong is
// what makes a thread read like a log file.
import { sameRun, groupRuns, GAP_MS, SPAN_MS } from './runs.js';

const at = (ms) => new Date(1700000000000 + ms).toISOString();
const msg = (who, ms, over = {}) => ({ senderId: who, createdAt: at(ms), ...over });
const MIN = 60_000;

test('consecutive messages from one person are one run', () => {
  assert.equal(sameRun(msg('a', 0), msg('a', 1000)), true);
});

test('a different sender always starts a new run', () => {
  assert.equal(sameRun(msg('a', 0), msg('b', 1000)), false);
});

test('a long gap starts a new run, so the time is shown again', () => {
  assert.equal(sameRun(msg('a', 0), msg('a', GAP_MS - 1)), true);
  assert.equal(sameRun(msg('a', 0), msg('a', GAP_MS)), false);
  assert.equal(sameRun(msg('a', 0), msg('a', GAP_MS + MIN)), false);
});

test('a deleted message never joins a run', () => {
  assert.equal(sameRun(msg('a', 0), msg('a', 1000, { deleted: true })), false);
  assert.equal(sameRun(msg('a', 0, { deleted: true }), msg('a', 1000)), false);
});

test('the first message in a thread has nothing to group with', () => {
  assert.equal(sameRun(null, msg('a', 0)), false);
  assert.equal(sameRun(msg('a', 0), null), false);
});

// The rule is about gaps, not the clock. Typing across a minute boundary --
// or an hour boundary -- is not a new thought.
test('crossing a minute boundary does not start a new run', () => {
  const base = new Date(2026, 8, 11, 10, 50, 55).getTime();
  const a = { senderId: 'a', createdAt: new Date(base).toISOString() };
  const b = { senderId: 'a', createdAt: new Date(base + 10_000).toISOString() }; // 10:51:05
  assert.equal(sameRun(a, b), true);
  const hourEdge = new Date(2026, 8, 11, 10, 59, 58).getTime();
  const c = { senderId: 'a', createdAt: new Date(hourEdge).toISOString() };
  const d = { senderId: 'a', createdAt: new Date(hourEdge + 5_000).toISOString() }; // 11:00:03
  assert.equal(sameRun(c, d), true);
});

// A slow, steady stream: one line every four minutes. Every gap is under five,
// so the gap rule alone would group them forever and the reader would never
// see a time again. The span cap is what shows one.
test('a run cannot outlive the span cap even when every gap is short', () => {
  const stream = [];
  for (let i = 0; i < 10; i++) stream.push(msg('a', i * 4 * MIN)); // 0..36 min
  const grouped = groupRuns(stream);
  assert.equal(grouped[0], false, 'the first message opens the run');
  // 4, 8, 12, 16 minutes: inside the span.
  assert.deepEqual(grouped.slice(1, 5), [true, true, true, true]);
  // 20 minutes from the run's first message: the cap. A new run, with a time.
  assert.equal(grouped[5], false, 'twenty minutes in, the header repeats');
  // ...and the next run counts from there.
  assert.deepEqual(grouped.slice(6, 10), [true, true, true, true]);
  assert.equal(grouped[10], undefined);
});

test('the span is measured from the first message of the run, not the previous', () => {
  const first = msg('a', 0);
  const prev = msg('a', SPAN_MS - MIN);
  const m = msg('a', SPAN_MS + MIN); // 2 min after prev, 21 min after first
  assert.equal(sameRun(prev, m), true, 'without the run start only the gap is known');
  assert.equal(sameRun(prev, m, first), false, 'with it, the cap applies');
});

test('the alternating exchange: each side starts its own run, and continues it', () => {
  const seq = [
    msg('me', 0), msg('me', MIN), msg('me', 2 * MIN),
    msg('you', 3 * MIN), msg('you', 4 * MIN),
    msg('me', 5 * MIN), msg('me', 6 * MIN),
  ];
  assert.deepEqual(groupRuns(seq), [false, true, true, false, true, false, true]);
});

test('a forced break (the "New" divider) cuts a run whatever the timing', () => {
  const seq = [msg('you', 0), msg('you', 1000), msg('you', 2000)];
  const grouped = groupRuns(seq, (m) => m === seq[1]);
  assert.deepEqual(grouped, [false, false, true]);
});
