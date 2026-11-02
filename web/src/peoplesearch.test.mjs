import test from 'node:test';
import assert from 'node:assert/strict';

import {
  matchScore,
  rankPeople,
  mergePeople,
  highlight,
  createSearch,
  MATCH_EXACT_EMAIL,
  MATCH_NAME_PREFIX,
  MATCH_WORD_PREFIX,
  NO_MATCH,
} from './peoplesearch.js';

const P = (name, email) => ({ name, email, userId: email });

const PEOPLE = [
  P('Ambuj Kumar', 'ambujkr8@gmail.com'),
  P('Divyanshii', '7298.divyanshi@gmail.com'),
  P('Administrator', 'ambuj.kumar@ddecor.com'),
  P('Kumar Sanu', 'sanu@example.org'),
];

test('a name you are part-way through typing ranks above one you are inside', () => {
  // "kum" begins "Kumar Sanu" and appears inside "Ambuj Kumar". The one that
  // starts with it comes first, which is what makes a live list usable.
  const got = rankPeople(PEOPLE, 'kum').map((p) => p.name);
  assert.equal(got[0], 'Kumar Sanu');
  assert.ok(got.includes('Ambuj Kumar'));
});

test('any word of a name matches, not only the first', () => {
  assert.equal(matchScore(P('Ambuj Kumar', 'a@b.co'), 'kumar'), MATCH_WORD_PREFIX);
  assert.equal(matchScore(P('Ambuj Kumar', 'a@b.co'), 'ambuj'), MATCH_NAME_PREFIX);
});

test('a full email address is the strongest match there is', () => {
  assert.equal(
    matchScore(P('Anyone', 'ambujkr8@gmail.com'), 'ambujkr8@gmail.com'),
    MATCH_EXACT_EMAIL
  );
  const got = rankPeople(PEOPLE, 'ambuj.kumar@ddecor.com');
  assert.equal(got[0].email, 'ambuj.kumar@ddecor.com');
});

test('somebody who matches nothing is not in the list', () => {
  assert.equal(matchScore(P('Ambuj', 'a@b.co'), 'zzzz'), NO_MATCH);
  assert.deepEqual(rankPeople(PEOPLE, 'zzzz'), []);
});

test('the order does not shuffle between keystrokes', () => {
  // Same score, so name breaks the tie -- twice, identically.
  const a = rankPeople(PEOPLE, 'a').map((p) => p.name);
  const b = rankPeople(PEOPLE, 'a').map((p) => p.name);
  assert.deepEqual(a, b);
  assert.deepEqual(a, [...a].sort((x, y) => x.toLowerCase().localeCompare(y.toLowerCase())));
});

test('an empty query keeps everybody, so a picker can show contacts at rest', () => {
  assert.equal(rankPeople(PEOPLE, '').length, PEOPLE.length);
});

test('the same person from two sources appears once', () => {
  const local = [P('Ambuj Kumar', 'ambujkr8@gmail.com')];
  const server = [
    { ...P('Ambuj Kumar', 'ambujkr8@gmail.com'), userId: 'ambujkr8@gmail.com' },
    P('New Person', 'new@example.com'),
  ];
  assert.equal(mergePeople(local, server).length, 2);
});

test('a name that looks like markup is shown, not run', () => {
  const out = highlight('<script>alert(1)</script>', 'script');
  assert.ok(!out.includes('<script>'), out);
  assert.ok(out.includes('<mark>script</mark>'), out);
});

test('highlight marks the typed run and leaves the rest alone', () => {
  assert.equal(highlight('Ambuj Kumar', 'buj'), 'Am<mark>buj</mark> Kumar');
  assert.equal(highlight('Ambuj Kumar', ''), 'Ambuj Kumar');
  assert.equal(highlight('Ambuj Kumar', 'zz'), 'Ambuj Kumar');
});

// The failure that reads as a broken server: you type "am", then "ambuj"; the
// first query is slower, its answer lands second, and the list fills with
// results for a query you have already replaced.
test('a slow answer to an old query never overwrites a new one', async () => {
  const seen = [];
  const delays = { am: 40, ambuj: 1 };
  const search = (q) =>
    new Promise((res) => setTimeout(() => res([{ name: q, email: q + '@x.co' }]), delays[q] ?? 0));

  const s = createSearch(search, (q, r) => seen.push(q), { delay: 0 });
  s.input('am', { immediate: true });
  s.input('ambuj', { immediate: true });

  await new Promise((r) => setTimeout(r, 80));
  assert.deepEqual(seen, ['ambuj'], `saw ${JSON.stringify(seen)}`);
});

test('a search that throws clears the list rather than freezing it', async () => {
  const seen = [];
  const s = createSearch(
    () => Promise.reject(new Error('offline')),
    (q, r) => seen.push([q, r]),
    { delay: 0 }
  );
  s.input('x', { immediate: true });
  await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(seen, [['x', []]]);
});

test('cancelling a closed picker drops the answer still in flight', async () => {
  const seen = [];
  const s = createSearch(
    (q) => new Promise((res) => setTimeout(() => res([q]), 20)),
    (q) => seen.push(q),
    { delay: 0 }
  );
  s.input('x', { immediate: true });
  s.cancel();
  await new Promise((r) => setTimeout(r, 50));
  assert.deepEqual(seen, []);
});

test('keystrokes are debounced into one request', async () => {
  let calls = 0;
  const s = createSearch(
    (q) => {
      calls++;
      return Promise.resolve([]);
    },
    () => {},
    { delay: 20 }
  );
  s.input('a');
  s.input('am');
  s.input('amb');
  await new Promise((r) => setTimeout(r, 60));
  assert.equal(calls, 1, 'three keystrokes should be one request');
});
