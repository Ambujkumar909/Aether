import test from 'node:test';
import assert from 'node:assert/strict';

import { Writer, Reader, newDictPair } from './wire.js';
import { encodeSet, encodeCaption, decodeRecording, clampUtf8, banner, elapsed, pickMime, ChunkSink, fileName, WHAT } from './recording.js';

const hex = (b) => [...b].map((x) => x.toString(16).padStart(2, '0')).join('');

test('set and caption frames carry what the server reads', () => {
  const w = new Writer(null);
  encodeSet(w, WHAT.captions, true);
  const b = w.bytes();
  // header (2 bytes for an extended op), then kind 0 | what 2 | on 1 | pad.
  assert.equal(b.length, 3);
  assert.equal(b[2], 0b000_10_1_00);
  const c = new Writer(null);
  encodeCaption(c, true, 3, 'hi');
  assert.equal(hex(c.bytes().slice(2)), '30' + '03' + '02' + '6869');
});

test('a server state and caption decode, strings through the dictionary', () => {
  const { tx, rx } = newDictPair();
  const w = new Writer(tx);
  w.header(0x102); w.bits(0, 3); w.bool(true); w.bool(true); w.bool(false); w.bits(0, 2);
  w.str('Hana'); w.uvarint(1700000000000); w.str('Hana'); w.uvarint(1700000001000);
  const r = new Reader(w.bytes(), rx);
  r.header();
  assert.deepEqual(decodeRecording(r), { kind: 'state', recording: true, transcribing: true, captions: false, recBy: 'Hana', recSince: 1700000000000, trBy: 'Hana', trSince: 1700000001000 });

  const w2 = new Writer(tx);
  w2.header(0x102); w2.bits(1, 3); w2.bool(false); w2.bits(0, 4); w2.str('p1'); w2.uvarint(9); w2.blob(new TextEncoder().encode('héllo'));
  const r2 = new Reader(w2.bytes(), rx);
  r2.header();
  assert.deepEqual(decodeRecording(r2), { kind: 'caption', final: false, speaker: 'p1', utt: 9, text: 'héllo' });
});

test('a refusal says why', () => {
  const w = new Writer(null);
  w.header(0x102); w.bits(2, 3); w.bits(0, 2); w.bits(0, 1); w.bits(0, 2);
  const r = new Reader(w.bytes(), null);
  r.header();
  assert.deepEqual(decodeRecording(r), { kind: 'refused', what: 0, reason: 'deployment' });
  const w2 = new Writer(null);
  w2.header(0x102); w2.bits(2, 3); w2.bits(1, 2); w2.bits(1, 1); w2.bits(0, 2);
  const r2 = new Reader(w2.bytes(), null);
  r2.header();
  assert.deepEqual(decodeRecording(r2), { kind: 'refused', what: 1, reason: 'role' });
});

test('long text is cut to the bound on a character boundary', () => {
  assert.equal(clampUtf8('abc').length, 3);
  const cut = clampUtf8('é'.repeat(300)); // 600 bytes of two-byte characters
  assert.equal(cut.length, 500);
  assert.equal(new TextDecoder('utf-8', { fatal: true }).decode(cut), 'é'.repeat(250));
  const odd = clampUtf8('a' + 'é'.repeat(300)); // the 500th byte is mid-character
  assert.equal(odd.length, 499);
  assert.doesNotThrow(() => new TextDecoder('utf-8', { fatal: true }).decode(odd));
});

test("Teams' banner words, the clock, the container", () => {
  assert.equal(banner({ recording: true, transcribing: true }), 'This meeting is being recorded and transcribed.');
  assert.equal(banner({ recording: true }), 'This meeting is being recorded.');
  assert.equal(banner({ transcribing: true }), 'This meeting is being transcribed.');
  assert.equal(banner({}), '');
  assert.equal(elapsed(12_400), '00:12');
  assert.equal(elapsed(3_723_000), '1:02:03');
  assert.equal(pickMime((m) => m === 'video/webm;codecs=vp8,opus' || m === 'video/webm'), 'video/webm;codecs=vp8,opus');
  assert.equal(pickMime(() => false), '');
  assert.equal(fileName(new Date(2026, 10, 2, 14, 5), 'video/webm'), 'Meeting recording 2026-11-02 1405.webm');
});

test('memory is bounded: the chunk that would pass the cap is refused', () => {
  const s = new ChunkSink(100);
  assert.equal(s.push({ size: 60 }), true);
  assert.equal(s.push({ size: 40 }), true); // exactly at the cap
  assert.equal(s.push({ size: 1 }), false);
  assert.equal(s.bytes, 100);
  assert.equal(s.chunks.length, 2);
});
