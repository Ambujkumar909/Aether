import test from 'node:test';
import assert from 'node:assert/strict';

import { initials, toneOf, personKey, avatarAttrs, avatarHTML } from './avatar.js';

// The reported bug: the same person showed different letters and different
// colours depending on which screen you were looking at. Both halves came from
// whatever text a call site happened to hold, so these tests are all the same
// test -- one person, one face, however they are described.

const DIVYANSHI = {
  userId: 'u-divy',
  name: 'Divyanshii',
  email: '7298.divyanshi@gmail.com',
};

test('one person has one set of letters, however they are described', () => {
  // The conversation list has the name; a bubble may have only the address;
  // an invitee row has the address alone. All three must agree.
  assert.equal(initials(DIVYANSHI), 'D');
  assert.equal(initials({ email: '7298.divyanshi@gmail.com' }), 'D');
  assert.equal(initials('Divyanshii'), 'D');
});

test('an address never contributes its domain', () => {
  // "ambuj.kumar@ddecor.com" must not become "AD" by picking up the domain,
  // which is what splitting on "@" and "." together did.
  assert.equal(initials({ email: 'ambuj.kumar@ddecor.com' }), 'AK');
  assert.equal(initials({ name: 'Ambuj Kumar', email: 'ambuj.kumar@ddecor.com' }), 'AK');
  // Two colleagues share a domain, so a domain letter says nothing.
  assert.notEqual(
    initials({ email: 'sam@ddecor.com' }),
    initials({ email: 'alex@ddecor.com' })
  );
});

test('leading digits are not a person', () => {
  assert.equal(initials({ email: '7298.divyanshi@gmail.com' }), 'D');
  assert.equal(initials({ name: '2024 Planning' }), 'P');
  // But an account that is only digits still gets something.
  assert.equal(initials({ email: '9090@x.co' }), '9');
});

test('a name that is an emoji is shown as the emoji', () => {
  assert.equal(initials({ name: '🎉 Party planning' }), '🎉');
  assert.equal(initials({ name: '👍' }), '👍');
});

test('colour follows identity, not the text on screen', () => {
  // The same account described three ways is the same colour. This is the
  // bug: hashing the visible string made one person change colour between
  // the sidebar, the meeting and the dashboard.
  const byId = toneOf({ userId: 'u-divy', name: 'Divyanshii' });
  const byIdAgain = toneOf({ userId: 'u-divy', email: '7298.divyanshi@gmail.com' });
  assert.equal(byId, byIdAgain);

  const byEmail = toneOf({ email: 'Ambuj.Kumar@DDecor.com' });
  const byEmailCased = toneOf({ email: 'ambuj.kumar@ddecor.com' });
  assert.equal(byEmail, byEmailCased, 'case changed the colour');
});

test('two different people usually get different colours', () => {
  const tones = new Set(
    ['a@x.co', 'b@x.co', 'c@x.co', 'd@x.co', 'e@x.co', 'f@x.co'].map((e) => toneOf({ email: e }))
  );
  assert.ok(tones.size >= 3, `six people produced ${tones.size} colours`);
});

test('every tone is one of the eight the stylesheet defines', () => {
  for (const e of ['a@x.co', 'zzz@y.org', 'Ambuj', '🎉', '', '9090@x.co']) {
    const t = toneOf({ email: e });
    assert.ok(Number.isInteger(t) && t >= 0 && t <= 7, `tone ${t} for ${e}`);
  }
});

test('an avatar always carries its tone, so nothing renders untoned', () => {
  // The sidebar drew `class="avatar"` with no tone at all, which is why
  // everybody there was one colour. Going through this helper makes that
  // impossible.
  const html = avatarHTML(DIVYANSHI);
  assert.match(html, /class="avatar"/);
  assert.match(html, /data-tone="[0-7]"/);
  assert.match(avatarAttrs(DIVYANSHI, 'sm'), /class="avatar sm"/);
});

test('a name that looks like markup is escaped', () => {
  const html = avatarHTML({ name: '<img src=x onerror=1>' });
  assert.ok(!html.includes('<img'), html);
});

test('nobody at all still renders something', () => {
  assert.equal(initials({}), '?');
  assert.equal(initials(''), '?');
  assert.equal(personKey({}), '');
});
