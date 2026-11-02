import test from 'node:test';
import assert from 'node:assert/strict';

import { spotlightChange, pickSpotlit } from './spotlight.js';

const name = (id) => id.toUpperCase();

test('being spotlighted and released is said to you, in Teams words', () => {
  assert.deepEqual(spotlightChange([], ['me'], 'me', name), { notices: ["You're spotlighted"], added: true });
  assert.deepEqual(spotlightChange(['me'], [], 'me', name).notices, ["You're no longer spotlighted"]);
});

test('somebody else spotlighted is announced; an unchanged list says nothing', () => {
  assert.deepEqual(spotlightChange(['a'], ['a', 'b'], 'me', name), { notices: ['B is spotlighted.'], added: true });
  assert.deepEqual(spotlightChange(['a', 'b'], ['a', 'b'], 'me', name), { notices: [], added: false });
  // Removal of someone else is not a new arrival: no pin is cleared.
  assert.equal(spotlightChange(['a', 'b'], ['a'], 'me', name).added, false);
});

test('stop all is one notice, not one per person', () => {
  assert.deepEqual(spotlightChange(['a', 'b'], [], 'me', name).notices, ['All spotlights stopped']);
});

test('the stage goes to the spotlighted speaker, else the first spotlighted, never a presentation', () => {
  const list = [
    { id: 'a', rank: -1 },
    { id: 'b', rank: 2 },
    { id: 'c', rank: 0 },
    { id: 'b\u0000screen', rank: -1, presentation: true },
  ];
  assert.equal(pickSpotlit(list, ['a', 'b']).id, 'b'); // b speaks, a does not
  assert.equal(pickSpotlit(list, ['c', 'b']).id, 'c'); // best rank wins
  assert.equal(pickSpotlit([{ id: 'x', rank: -1 }, { id: 'y', rank: -1 }], ['y', 'x']).id, 'y'); // order kept
  assert.equal(pickSpotlit(list, ['b\u0000screen']), null);
  assert.equal(pickSpotlit(list, []), null);
});
