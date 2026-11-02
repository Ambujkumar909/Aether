import test from 'node:test';
import assert from 'node:assert/strict';

import { readersOf, unreadBy, receiptState, receiptTitle } from './receipts.js';

const t0 = '2026-09-14T10:00:00.000Z';
const before = '2026-09-14T09:59:59.000Z';
const after = '2026-09-14T10:00:01.000Z';
const me = 'u-me';
const members = (reads) =>
  Object.entries(reads).map(([userId, lastRead]) => ({ userId, name: userId.replace('u-', '').toUpperCase(), email: `${userId}@x.test`, lastRead }));
const msg = (over = {}) => ({ id: 'm1', senderId: me, createdAt: t0, ...over });

test('a reader is a member whose cursor is at or past the message, never the sender', () => {
  const mem = members({ [me]: after, 'u-a': after, 'u-b': before, 'u-c': t0 });
  assert.deepEqual(readersOf(msg(), mem, me).map((x) => x.userId), ['u-a', 'u-c']);
  assert.deepEqual(unreadBy(msg(), mem, me).map((x) => x.userId), ['u-b']);
});

test('one to one: sent until they read it, then seen', () => {
  const conv = { kind: 'direct', members: members({ [me]: after, 'u-a': before }) };
  assert.equal(receiptState(msg(), conv, me).kind, 'sent');
  conv.members[1].lastRead = after;
  const st = receiptState(msg(), conv, me);
  assert.equal(st.kind, 'all');
  assert.equal(receiptTitle(st, conv), 'Seen');
  assert.equal(receiptTitle({ kind: 'sent' }, conv), 'Sent');
});

test('group: a count of readers, everybody when all have', () => {
  const conv = { kind: 'group', members: members({ [me]: after, 'u-a': after, 'u-b': before, 'u-c': before }) };
  let st = receiptState(msg(), conv, me);
  assert.equal(st.kind, 'seen');
  assert.equal(st.readers.length, 1);
  assert.equal(st.total, 3);
  assert.equal(receiptTitle(st, conv), 'Seen by A · 1 of 3');
  conv.members.forEach((x) => (x.lastRead = after));
  st = receiptState(msg(), conv, me);
  assert.equal(st.kind, 'all');
  assert.equal(receiptTitle(st, conv), 'Seen by everyone (3)');
});

test('nothing for other people\'s messages, deleted rows, system rows, pending scheduled sends, or big rooms', () => {
  const conv = { kind: 'group', members: members({ [me]: after, 'u-a': after }) };
  assert.equal(receiptState(msg({ senderId: 'u-a' }), conv, me).kind, 'none');
  assert.equal(receiptState(msg({ deleted: true }), conv, me).kind, 'none');
  assert.equal(receiptState(msg({ system: true }), conv, me).kind, 'none');
  assert.equal(receiptState(msg({ deliverAt: '2999-01-01T00:00:00Z' }), conv, me).kind, 'none');
  const big = { kind: 'channel', members: Array.from({ length: 41 }, (_, i) => ({ userId: 'u' + i, lastRead: after })) };
  assert.equal(receiptState(msg(), big, me).kind, 'none');
  assert.equal(receiptState(msg(), { kind: 'direct', members: members({ [me]: after }) }, me).kind, 'none', 'notes to self have no reader');
});
