import test from 'node:test';
import assert from 'node:assert/strict';

import { CUES, LEVELS, shouldPlay } from './sounds.js';

// The scores themselves: every cue is short, within loudness, in tune with
// the one motif, and no two cues are the same sequence.
test('every cue is bounded, in the motif, and distinct', () => {
  const pentatonic = new Set([0, 2, 4, 7, 9]); // semitones above A in the major pentatonic
  const seen = new Set();
  for (const [name, cue] of Object.entries(CUES)) {
    assert.ok(cue.notes.length >= 1 && cue.notes.length <= 4, `${name}: ${cue.notes.length} notes`);
    assert.ok(cue.length > 0 && cue.length <= 1.5, `${name}: ${cue.length}s`);
    if (cue.repeat) assert.ok(cue.repeat > cue.length && cue.repeat <= 4, `${name}: repeats every ${cue.repeat}s`);
    for (const n of cue.notes) {
      assert.ok(n.gain > 0 && n.gain <= 0.5, `${name}: gain ${n.gain}`);
      assert.ok(n.at + n.len <= cue.length + 1e-9, `${name}: a note outlives the cue`);
      const semis = Math.round(12 * Math.log2(n.hz / 440));
      assert.ok(pentatonic.has(((semis % 12) + 12) % 12), `${name}: ${n.hz.toFixed(1)} Hz is off the motif`);
    }
    const sig = cue.notes.map((n) => `${n.at}:${Math.round(n.hz)}`).join(',');
    assert.ok(!seen.has(sig), `${name} repeats another cue's notes`);
    seen.add(sig);
  }
});

test('the ring and the ringback are the two that repeat, and differ in register', () => {
  assert.ok(CUES.ring.repeat && CUES.ringback.repeat);
  const top = (c) => Math.max(...c.notes.map((n) => n.hz));
  assert.ok(top(CUES.ringback) < 440 && top(CUES.ring) > 600, 'the outgoing tone sits low, the incoming one high');
  for (const [k, c] of Object.entries(CUES)) if (k !== 'ring' && k !== 'ringback') assert.equal(c.repeat, undefined, `${k} must not loop`);
});

test('what plays: levels, do not disturb and quiet hours', () => {
  assert.deepEqual(LEVELS, ['all', 'important', 'calls']);
  // Everything, by default.
  assert.ok(shouldPlay('message'));
  assert.ok(shouldPlay('join'));
  // "important": calls, mentions, what is about you; not a room filling up.
  assert.ok(shouldPlay('ring', { level: 'important' }));
  assert.ok(shouldPlay('mention', { level: 'important' }));
  assert.ok(shouldPlay('message', { level: 'important', about: true }));
  assert.ok(!shouldPlay('message', { level: 'important' }));
  assert.ok(!shouldPlay('join', { level: 'important' }));
  // "calls": only the call's own sounds.
  assert.ok(shouldPlay('ring', { level: 'calls' }));
  assert.ok(shouldPlay('ended', { level: 'calls' }));
  assert.ok(!shouldPlay('mention', { level: 'calls' }));
  assert.ok(!shouldPlay('hand', { level: 'calls' }));
  // Do not disturb: an incoming ring is silent; your own call's progress
  // and an urgent message are not.
  assert.ok(!shouldPlay('ring', { dnd: true }));
  assert.ok(!shouldPlay('message', { dnd: true, about: true }));
  assert.ok(shouldPlay('ringback', { dnd: true }));
  assert.ok(shouldPlay('message', { dnd: true, urgent: true }));
  // Quiet hours: what is about you, and calls.
  assert.ok(!shouldPlay('message', { quiet: true }));
  assert.ok(shouldPlay('message', { quiet: true, about: true }));
  assert.ok(shouldPlay('ring', { quiet: true }));
  assert.ok(!shouldPlay('join', { quiet: true }));
  // Unknown cues never play.
  assert.ok(!shouldPlay('bell'));
});
