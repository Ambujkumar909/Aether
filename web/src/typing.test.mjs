import test from 'node:test';
import assert from 'node:assert/strict';

import { createTypingBoard, createTypingSender, TYPING_TTL, TYPING_REPEAT, TYPING_QUIET } from './typing.js';

test('one, two and many typists are worded; keyed by id, not name', () => {
  const b = createTypingBoard({ now: () => 1000 });
  assert.equal(b.line('c'), '');
  assert.equal(b.set('c', 'u1', 'Priya', true), true, 'a new typist changes the screen');
  assert.equal(b.line('c'), 'Priya is typing');
  assert.equal(b.set('c', 'u1', 'Priya', true), false, 'a refresh of the same typist does not');
  b.set('c', 'u2', 'Sam', true);
  assert.equal(b.line('c'), 'Priya and Sam are typing');
  b.set('c', 'u3', 'Sam', true); // another Sam
  assert.equal(b.line('c'), '3 people are typing');
  assert.equal(b.line('other'), '', 'conversations are separate');
});

test('a typist lapses after the TTL unless they signal again; a stop ends it at once', () => {
  let t = 10_000;
  const b = createTypingBoard({ now: () => t });
  b.set('c', 'u1', 'Priya', true);
  t += TYPING_TTL - 1;
  assert.deepEqual(b.expire(), [], 'not yet');
  assert.equal(b.line('c'), 'Priya is typing');
  b.set('c', 'u1', 'Priya', true); // keeps typing: re-signalled
  t += TYPING_TTL - 1;
  assert.deepEqual(b.expire(), []);
  t += 1;
  assert.deepEqual(b.expire(), ['c'], 'lapsed exactly on the line');
  assert.equal(b.line('c'), '');
  assert.equal(b.size, 0, 'nothing kept for an idle conversation');
  b.set('c', 'u1', 'Priya', true);
  assert.equal(b.set('c', 'u1', 'Priya', false), true, 'an explicit stop changes the screen');
  assert.equal(b.set('c', 'u1', 'Priya', false), false, 'a second stop is nothing');
  assert.equal(b.nextDue(), null);
});

test('nextDue is the earliest lapse across conversations', () => {
  let t = 0;
  const b = createTypingBoard({ now: () => t });
  b.set('a', 'u1', 'A', true);
  t = 2000;
  b.set('b', 'u2', 'B', true);
  assert.equal(b.nextDue(), TYPING_TTL);
});

test('the sender says on at the first character, repeats every REPEAT, stops on empty, quiet or send', () => {
  let t = 100_000;
  const out = [];
  const s = createTypingSender({ now: () => t, signal: (on) => out.push(on) });
  s.changed('h');
  s.changed('he');
  s.changed('hel');
  assert.deepEqual(out, [true], 'one signal for a burst of keys');
  t += TYPING_REPEAT;
  s.changed('hell');
  assert.deepEqual(out, [true, true], 'repeated after REPEAT while still typing');
  s.changed('');
  assert.deepEqual(out, [true, true, false], 'emptying the box stops');
  s.changed('x');
  t += TYPING_QUIET;
  s.tick();
  assert.deepEqual(out, [true, true, false, true, false], 'a pause of QUIET stops');
  s.changed('y');
  s.sent();
  assert.deepEqual(out.slice(-2), [true, false], 'sending stops');
  s.sent();
  s.tick();
  assert.equal(out.length, 7, 'nothing more when already stopped');
});
