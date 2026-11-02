import test from 'node:test';
import assert from 'node:assert/strict';

import { shouldSend, visibleLines, INTERIM_MS, HOLD_MS } from './captions.js';

// The gate in front of the wire. A recogniser emits interim results many times
// a second and repeats itself while it thinks, so without this a caption track
// puts more frames on the connection than a camera does.

test('a final result always goes, immediately', () => {
  const gate = { last: 1000, text: 'hello there' };
  assert.equal(shouldSend(gate, 'hello there', true, 1001), true);
});

test('an interim that says the same thing again is not sent', () => {
  const gate = { last: 0, text: 'we should ship' };
  assert.equal(shouldSend(gate, 'we should ship', false, 999999), false);
  // Even with whitespace round it, which is how they usually arrive.
  assert.equal(shouldSend(gate, '  we should ship  ', false, 999999), false);
});

test('interim results are throttled to the floor', () => {
  const gate = { last: 1000, text: 'we' };
  assert.equal(shouldSend(gate, 'we should', false, 1000 + INTERIM_MS - 1), false);
  assert.equal(shouldSend(gate, 'we should', false, 1000 + INTERIM_MS), true);
});

test('empty text is never sent, final or not', () => {
  const gate = { last: 0, text: '' };
  assert.equal(shouldSend(gate, '', true, 999999), false);
  assert.equal(shouldSend(gate, '   ', false, 999999), false);
});

// The caption bar is two lines at the bottom of a video, not a transcript.

test('stale lines fall off', () => {
  const now = 100000;
  const by = new Map([
    ['a', { name: 'Alice', text: 'still here', at: now - 100 }],
    ['b', { name: 'Bob', text: 'long gone', at: now - HOLD_MS - 1 }],
  ]);
  const got = visibleLines(by, now);
  assert.deepEqual(got.map((l) => l.id), ['a']);
});

test('the newest speakers win, and order is oldest first', () => {
  const now = 100000;
  const by = new Map([
    ['a', { name: 'A', text: 'one', at: now - 300 }],
    ['b', { name: 'B', text: 'two', at: now - 200 }],
    ['c', { name: 'C', text: 'three', at: now - 100 }],
  ]);
  const got = visibleLines(by, now);
  assert.deepEqual(got.map((l) => l.id), ['b', 'c'], 'want the two newest, in order');
});

test('a speaker who has been cleared shows nothing', () => {
  const now = 100000;
  const by = new Map([['a', { name: 'A', text: '', at: now }]]);
  assert.deepEqual(visibleLines(by, now), []);
});

test('an empty map is an empty bar, not a crash', () => {
  assert.deepEqual(visibleLines(new Map(), 0), []);
});
