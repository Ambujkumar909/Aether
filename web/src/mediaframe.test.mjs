import test from 'node:test';
import assert from 'node:assert/strict';

import { Writer, Reader, Op, encodeMediaFrame, decodeMediaFrame, newDictPair } from './wire.js';

const opus = new Uint8Array(120).fill(0x5a); // 48 kbit/s x 20 ms

test('an audio frame is the one-byte header, one byte of flags and the Opus bytes: 122 for 120', () => {
  const w = new Writer(null);
  encodeMediaFrame(w, { kind: 0, key: true, ts: 123456789, payload: opus });
  assert.equal(w.bytes().length, 122, 'one-byte opcode, no timestamp, no length');
});

test('a video frame carries its timestamp and nothing else more', () => {
  const w = new Writer(null);
  encodeMediaFrame(w, { kind: 1, key: true, ts: 1_000_000, payload: opus, screen: true });
  assert.equal(w.bytes().length, 122 + 3, 'a three-byte varint for one second');
});

test('round trip through the server-side shape, both kinds, payload to the end', () => {
  const dicts = newDictPair();
  for (const c of [
    { kind: 0, key: true, ts: 0, screen: false },
    { kind: 1, key: false, ts: 1_033_333, screen: true },
  ]) {
    // What the server emits: publisher first, then the client's frame body.
    const w = new Writer(dicts.tx);
    w.header(Op.MediaFrame);
    w.str('publisher-42');
    w.bits(c.kind, 1);
    w.bits(c.key ? 1 : 0, 1);
    w.bits(c.screen ? 1 : 0, 6);
    if (c.kind === 1) w.uvarint(c.ts);
    w.raw(opus);
    const r = new Reader(w.bytes(), dicts.rx);
    r.header();
    const f = decodeMediaFrame(r);
    assert.equal(f.publisher, 'publisher-42');
    assert.equal(f.kind, c.kind);
    assert.equal(f.key, c.key);
    assert.equal(f.screen, c.screen);
    assert.equal(f.ts, c.kind === 1 ? c.ts : 0);
    assert.deepEqual([...f.payload], [...opus]);
    assert.equal(r.remaining, 0, 'nothing left over');
  }
});
