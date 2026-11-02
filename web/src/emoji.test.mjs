import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ALL,
  GROUPS,
  searchEmoji,
  insertAt,
  pushRecent,
  isOnlyEmoji,
} from './emoji.js';

test('every entry is a real glyph with keywords, and none is listed twice', () => {
  const seen = new Set();
  for (const e of ALL) {
    assert.ok(e.ch.length > 0, `empty glyph in ${e.group}`);
    assert.ok(e.words.trim().length > 0, `${e.ch} has no keywords`);
    assert.ok(!seen.has(e.ch), `${e.ch} appears twice`);
    seen.add(e.ch);
  }
  assert.ok(GROUPS.length >= 4);
});

test('search matches the start of a word, not the middle of one', () => {
  const th = searchEmoji('th').map((e) => e.ch);
  assert.ok(th.includes('\u{1F44D}'), 'thumbs up not found for "th"');
  assert.ok(th.includes('\u{1F914}'), 'thinking not found for "th"');
  // "umb" is inside "thumbs" but nobody types that.
  assert.deepEqual(searchEmoji('umb'), []);
});

test('an exact keyword outranks a prefix', () => {
  // "fire" is the exact word for the flame; nothing else should come first.
  assert.equal(searchEmoji('fire')[0].ch, '\u{1F525}');
});

test('an empty search shows nothing, so the groups stay visible instead', () => {
  assert.deepEqual(searchEmoji(''), []);
  assert.deepEqual(searchEmoji('   '), []);
});

// The arithmetic that goes wrong first: a selection is replaced, not pushed
// aside, and the caret lands after what was inserted.
test('inserting at the caret puts the caret after the emoji', () => {
  const { text, caret } = insertAt('hello world', 5, 5, '\u{1F44D}');
  assert.equal(text, 'hello\u{1F44D} world');
  assert.equal(caret, 5 + '\u{1F44D}'.length);
});

test('inserting over a selection replaces it', () => {
  const { text, caret } = insertAt('hello world', 6, 11, '\u{1F30D}');
  assert.equal(text, 'hello \u{1F30D}');
  assert.equal(caret, text.length);
});

test('an empty box and an out-of-range caret both behave', () => {
  assert.equal(insertAt('', 0, 0, '\u{1F600}').text, '\u{1F600}');
  assert.equal(insertAt('hi', 99, 99, '!').text, 'hi!');
  assert.equal(insertAt('hi', null, null, '!').text, 'hi!');
  // A backwards range does not delete anything.
  assert.equal(insertAt('hello', 4, 1, 'X').text, 'hellXo');
});

test('recents are most-recent-first, unique and bounded', () => {
  let r = [];
  r = pushRecent(r, 'a');
  r = pushRecent(r, 'b');
  r = pushRecent(r, 'a');
  assert.deepEqual(r, ['a', 'b'], 'sending one twice should not list it twice');

  let many = [];
  for (let i = 0; i < 50; i++) many = pushRecent(many, 'e' + i, 5);
  assert.equal(many.length, 5);
  assert.equal(many[0], 'e49');
});

test('a message of only emoji is recognised, and a normal one is not', () => {
  assert.ok(isOnlyEmoji('\u{1F44D}'));
  assert.ok(isOnlyEmoji('\u{1F389} \u{1F389}'));
  assert.ok(!isOnlyEmoji('nice \u{1F44D}'));
  assert.ok(!isOnlyEmoji('hello'));
  assert.ok(!isOnlyEmoji(''));
  // Digits are only emoji inside a keycap sequence; a year is not a gesture.
  assert.ok(!isOnlyEmoji('2024'));
  // A wall of emoji is a message, not a gesture, so it is not enlarged.
  assert.ok(!isOnlyEmoji('\u{1F44D}'.repeat(20)));
});
