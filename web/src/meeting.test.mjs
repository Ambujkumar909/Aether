import test from 'node:test';
import assert from 'node:assert/strict';

import { meetingPhase, isUpcoming, phaseLabel } from './meeting.js';

const at = (mins) => new Date(Date.now() + mins * 60000);
const NOW = Date.now();

const m = (over) => ({ start: at(-30), end: at(30), status: 'scheduled', ...over });

test('a meeting with people in it is live, however late it runs', () => {
  const overrunning = m({ start: at(-180), end: at(-120), live: true });
  assert.equal(meetingPhase(overrunning, NOW), 'live');
  assert.equal(phaseLabel(overrunning, NOW), 'Happening now');
  assert.ok(isUpcoming(overrunning, NOW), 'an overrunning meeting left the list');
});

test('a meeting everybody left is completed, even inside its window', () => {
  const done = m({ status: 'completed', live: false });
  assert.equal(meetingPhase(done, NOW), 'completed');
  assert.equal(phaseLabel(done, NOW), 'Completed');
  assert.ok(!isUpcoming(done, NOW), 'a completed meeting stayed in upcoming');
});

test('occupancy outranks a status row: they came back', () => {
  // The last person left, the server wrote "completed", and then somebody
  // rejoined. The room is the truth.
  assert.equal(meetingPhase(m({ status: 'completed', live: true }), NOW), 'live');
});

test('a cancelled meeting is cancelled, not upcoming', () => {
  const off = m({ status: 'cancelled', start: at(60), end: at(120) });
  assert.equal(meetingPhase(off, NOW), 'cancelled');
  assert.ok(!isUpcoming(off, NOW));
});

test('nobody has arrived yet: the clock is all there is', () => {
  assert.equal(meetingPhase(m({ start: at(60), end: at(120) }), NOW), 'upcoming');
  assert.equal(meetingPhase(m({ start: at(-5), end: at(55) }), NOW), 'now');
  assert.equal(meetingPhase(m({ start: at(-120), end: at(-60) }), NOW), 'ended');
});

test('an empty room past its end has ended and leaves the list', () => {
  const gone = m({ start: at(-120), end: at(-60), live: false });
  assert.equal(phaseLabel(gone, NOW), 'Ended');
  assert.ok(!isUpcoming(gone, NOW));
});

import { endNoticeDelay } from './meeting.js';

test('the five-minute notice: when to show it, and when to say nothing', () => {
  const now = Date.parse('2026-11-02T10:00:00Z');
  const min = 60 * 1000;
  assert.equal(endNoticeDelay(new Date(now + 30 * min), now), 25 * min, 'half an hour to go: in 25 minutes');
  assert.equal(endNoticeDelay(new Date(now + 3 * min), now), 0, 'three minutes to go: now');
  assert.equal(endNoticeDelay(new Date(now + 30 * 1000), now), null, 'under a minute: nothing');
  assert.equal(endNoticeDelay(new Date(now - min), now), null, 'already over: nothing');
  assert.equal(endNoticeDelay(undefined, now), null, 'no scheduled end: nothing');
});
