import test from 'node:test';
import assert from 'node:assert/strict';

import { BINDINGS, matches, resolve, format, typing, groups } from './shortcuts.js';

const key = (k, mods = {}, target = { tagName: 'DIV' }) => ({ key: k, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false, ...mods, target });

test('every binding is unique within its scope, on both keyboards', () => {
  for (const mac of [false, true]) {
    const seen = new Map();
    for (const b of BINDINGS) {
      for (const combo of [b.combo, b.alt].filter(Boolean)) {
        const k = `${b.scope === 'any' ? '*' : b.scope}:${format(combo, mac)}`;
        assert.ok(!seen.has(k), `${k} is bound twice (${seen.get(k)} and ${b.action})`);
        seen.set(k, b.action);
        // "Anywhere" must not collide with either scope.
        if (b.scope === 'any') for (const s of ['meeting', 'home']) assert.ok(!seen.has(`${s}:${format(combo, mac)}`), `${k} collides in ${s}`);
      }
    }
  }
});

test('a bare letter is the letter, a chord is the chord, and shift tells them apart', () => {
  assert.ok(matches(key('m'), 'M'));
  assert.ok(matches(key('M', { shiftKey: true }), 'Shift+M'));
  assert.ok(!matches(key('M', { shiftKey: true }), 'M'), 'a capital M is not the mute key');
  assert.ok(!matches(key('m', { ctrlKey: true }), 'M'), 'Ctrl+M is not M');
  assert.ok(matches(key('m', { ctrlKey: true, shiftKey: true }), 'Mod+Shift+M', false));
  assert.ok(!matches(key('m', { ctrlKey: true, shiftKey: true }), 'Mod+Shift+M', true), 'on a Mac Ctrl is not Mod');
  assert.ok(matches(key('m', { metaKey: true, shiftKey: true }), 'Mod+Shift+M', true));
  assert.ok(matches(key(' ', { ctrlKey: true }), 'Mod+Space', false));
  assert.ok(matches(key('?', { shiftKey: true }), '?'), 'the question mark comes with shift held, and is still ?');
  assert.ok(matches(key('.', { ctrlKey: true }), 'Mod+.', false));
  assert.ok(matches(key('2', { ctrlKey: true }), 'Mod+2', false));
  assert.ok(!matches(key('2', { ctrlKey: true, altKey: true }), 'Mod+2', false), 'Alt changes the chord');
});

test('typing in a box eats bare letters but not chords', () => {
  const input = { tagName: 'INPUT' };
  const editable = { tagName: 'DIV', isContentEditable: true };
  assert.ok(typing(input) && typing(editable) && !typing({ tagName: 'DIV' }));
  assert.equal(resolve(key('m', {}, input), 'meeting', false), null);
  assert.equal(resolve(key('m', { ctrlKey: true, shiftKey: true }, input), 'meeting', false)?.binding.action, 'mic');
  assert.equal(resolve(key('m'), 'meeting', false)?.binding.action, 'mic');
  assert.equal(resolve(key('m'), 'home', false), null, 'a meeting key does nothing at home');
  assert.equal(resolve(key('?', { shiftKey: true }, input), 'home', false), null, 'typing ? in a box is typing');
  assert.equal(resolve(key('?', { shiftKey: true }), 'home', false)?.binding.action, 'help');
  assert.equal(resolve(key('/'), 'home', false)?.binding.action, 'search');
  assert.equal(resolve(key('2', { ctrlKey: true }), 'home', false)?.binding.action, 'tab:calendar');
});

test('combos read the way the keyboard is labelled', () => {
  assert.equal(format('Mod+Shift+M', false), 'Ctrl + Shift + M');
  assert.equal(format('Mod+Shift+M', true), '⌘ ⇧ M');
  assert.equal(format('Mod+Space', false), 'Ctrl + Space');
  assert.equal(format('?', false), '?');
  assert.equal(format('m', false), 'M');
});

test('the sheet lists every binding once, grouped', () => {
  const g = groups();
  assert.deepEqual(g.map((x) => x.title), ['In a meeting', 'At home', 'Anywhere']);
  assert.equal(g.reduce((n, x) => n + x.items.length, 0), BINDINGS.length);
});
