import test from 'node:test';
import assert from 'node:assert/strict';

import { Writer, Reader, Op, newDictPair, decodeRoster } from './wire.js';

// The roster entry, encoded exactly as internal/signal/gateway.go writes it
// and decoded by the client. If the two ever drift, the client reads a name
// out of the middle of an account id and every tile is mislabelled -- which
// is the kind of failure that looks like a rendering bug for a day.
//
// The account id is what the in-meeting chat addresses a private message to.
// A guest has none, and the empty string must survive the round trip rather
// than shifting every field after it.

function rosterBytes({ id, name, role, userId }) {
  const d = newDictPair();
  const w = new Writer(d.out);
  w.header(Op.Roster);
  w.bool(true); // present
  w.str(id);
  w.str(name);
  w.bits(role, 2);
  w.bits(0, 5);
  w.str(userId);
  return { bytes: w.bytes(), dicts: d };
}

function readRoster(bytes, dicts) {
  const r = new Reader(bytes, dicts.in);
  r.header();
  return decodeRoster(r);
}

test('a roster entry round-trips, account id and all', () => {
  const { bytes, dicts } = rosterBytes({
    id: 'p-7Xq',
    name: 'Divyanshi',
    role: 1,
    userId: 'u-4410',
  });
  const got = readRoster(bytes, dicts);
  assert.equal(got.present, true);
  assert.equal(got.id, 'p-7Xq');
  assert.equal(got.name, 'Divyanshi');
  assert.equal(got.role, 1);
  assert.equal(got.userId, 'u-4410');
});

test('a guest has no account, and the empty id does not shift the fields', () => {
  const { bytes, dicts } = rosterBytes({
    id: 'p-guest',
    name: 'Guest',
    role: 0,
    userId: '',
  });
  const got = readRoster(bytes, dicts);
  assert.equal(got.name, 'Guest');
  assert.equal(got.userId, '');
});

test('a departure carries only the id', () => {
  const d = newDictPair();
  const w = new Writer(d.out);
  w.header(Op.Roster);
  w.bool(false);
  w.str('p-7Xq');
  const r = new Reader(w.bytes(), d.in);
  r.header();
  const got = decodeRoster(r);
  assert.equal(got.present, false);
  assert.equal(got.id, 'p-7Xq');
});
