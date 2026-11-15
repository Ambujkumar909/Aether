import test from 'node:test';
import assert from 'node:assert/strict';

import { presenceIcon, statusLabel, isAutoStatus, presenceLine, agoText, oooActive, oooText } from './presence.js';

// Every state has its own glyph and label; a shape, not only a colour.
test('each status draws a distinct glyph with its label', () => {
  const kinds = ['available', 'busy', 'dnd', 'away', 'incall', 'presenting'];
  const seen = new Set();
  for (const k of kinds) {
    const html = presenceIcon(k);
    assert.ok(html.includes(`class="presence pi-${k}"`), k);
    assert.ok(html.includes(`title="${statusLabel(k)}"`), `${k} label`);
    assert.ok(html.includes('<svg'), `${k} is an svg`);
    seen.add(html.replace(/pi-\w+|title="[^"]*"|aria-label="[^"]*"/g, ''));
  }
  // busy is the bare disc; the other five each add something to it
  assert.equal(seen.size, kinds.length, 'six different drawings');
});

test('away is a clock: a face and two hands', () => {
  const html = presenceIcon('away');
  assert.ok(html.includes('class="face"'), 'a clock face');
  assert.ok(/M8 4\.9V8l2\.1 1\.5/.test(html), 'hands from the centre');
});

test('offline is a hollow ring whatever the status says', () => {
  const html = presenceIcon('dnd', { online: false });
  assert.ok(html.includes('pi-offline'));
  assert.ok(html.includes('class="ring"'));
  assert.ok(!html.includes('class="bg"'), 'no filled disc');
  assert.ok(html.includes('title="Offline"'));
});

test('an unknown status draws as available rather than nothing', () => {
  assert.ok(presenceIcon('what').includes('pi-available'));
  assert.ok(presenceIcon(undefined).includes('pi-available'));
});

test('the note rides in the title, escaped', () => {
  const html = presenceIcon('busy', { note: 'in <b>"a"</b> & out' });
  assert.ok(html.includes('title="Busy · in &lt;b&gt;&quot;a&quot;&lt;/b&gt; &amp; out"'));
  assert.ok(!html.includes('<b>'));
});

test('only in a call and presenting are automatic', () => {
  assert.deepEqual(['available', 'busy', 'dnd', 'away', 'incall', 'presenting'].filter(isAutoStatus), ['incall', 'presenting']);
});

test('the line: state, note, and last seen only for absence', () => {
  const now = Date.parse('2026-09-22T12:00:00Z');
  const seen = now - 10 * 60_000;
  assert.equal(presenceLine({ status: 'away', note: 'Back at 3' }, { online: true, lastSeen: seen, now }), 'Away · Back at 3', 'a picked Away is not absence');
  assert.equal(presenceLine({ status: 'away', auto: true }, { online: true, lastSeen: seen, now }), 'Away · Last seen 10 min ago', 'an idle Away is');
  assert.equal(presenceLine({ status: 'dnd', note: 'Heads down' }, { online: false, lastSeen: now - 2 * 3_600_000, now }), 'Offline · Last seen 2 h ago', 'offline wins over the pick');
  assert.equal(presenceLine({ status: 'offline' }, { online: false, lastSeen: 0 }), 'Offline', 'appearing offline: nothing more');
  assert.equal(presenceLine({ status: 'incall', note: 'In a call' }, { online: true }), 'In a call', 'a note that repeats the state is not repeated');
  assert.equal(agoText(now - 30_000, now), 'just now');
  assert.equal(agoText(now - 26 * 3_600_000, now).startsWith('yesterday at '), true);
});

test('out of office is said beside the state, and stops at its end', () => {
  const now = new Date(2026, 10, 16, 10, 0).getTime(); // Mon 16 Nov, 10:00
  const today = new Date(2026, 10, 16, 15, 30).toISOString();
  const friday = new Date(2026, 10, 20, 9, 0).toISOString();
  const hm = new Date(today).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const day = new Date(friday).toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short' });

  assert.equal(oooActive(null, now), null);
  assert.equal(oooActive({ message: '' }, now), null, 'no line, nothing to say');
  assert.equal(oooActive({ message: 'Away', until: new Date(now).toISOString() }, now), null, 'the end is not included');
  assert.equal(oooActive({ message: 'Away', until: new Date(now - 1).toISOString() }, now), null, 'past its end');
  assert.ok(oooActive({ message: 'Away', until: new Date(now + 1).toISOString() }, now), 'a millisecond before its end');
  assert.ok(oooActive({ message: 'Away' }, now), 'open-ended');

  assert.equal(oooText({ message: 'Away' }, now), 'Out of office');
  assert.equal(oooText({ message: 'Away', until: today }, now), `Out of office until ${hm}`, 'ending today: the time');
  assert.equal(oooText({ message: 'Away', until: friday }, now), `Out of office until ${day}`, 'ending another day: the day');

  const ooo = { message: 'Back Friday', until: friday };
  assert.equal(presenceLine({ status: 'available', ooo }, { online: true, now }), `Available · Out of office until ${day}`);
  assert.equal(presenceLine({ status: 'busy', note: 'Packing', ooo }, { online: true, now }), `Busy · Packing · Out of office until ${day}`);
  assert.equal(
    presenceLine({ status: 'available', ooo }, { online: false, lastSeen: now - 2 * 3_600_000, now }),
    `Offline · Last seen 2 h ago · Out of office until ${day}`,
    'it stands when they are offline: that is when it matters',
  );
  assert.equal(presenceLine({ status: 'available', ooo: { message: 'x', until: new Date(now - 5).toISOString() } }, { online: true, now }), 'Available', 'an ended one is not said');
});
