import test from 'node:test';
import assert from 'node:assert/strict';
import { preferVideoCodecs } from './rtc.js';

// Chrome's own video capability list, abridged, in Chrome's order.
const chrome = [
  { mimeType: 'video/VP8' },
  { mimeType: 'video/rtx' },
  { mimeType: 'video/H264', sdpFmtpLine: 'profile-level-id=42001f' },
  { mimeType: 'video/AV1' },
  { mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=2' },
  { mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=0' },
  { mimeType: 'video/red' },
  { mimeType: 'video/ulpfec' },
];

test('VP9 profile 0 first, then VP9 profile 2, then VP8, then everything else in its own order', () => {
  const out = preferVideoCodecs(chrome).map((c) => c.mimeType + (c.sdpFmtpLine ? ' ' + c.sdpFmtpLine : ''));
  assert.deepEqual(out, [
    'video/VP9 profile-id=0',
    'video/VP9 profile-id=2',
    'video/VP8',
    'video/rtx',
    'video/H264 profile-level-id=42001f',
    'video/AV1',
    'video/red',
    'video/ulpfec',
  ]);
});

test('nothing is dropped, and the input is not mutated', () => {
  const copy = chrome.slice();
  const out = preferVideoCodecs(chrome);
  assert.equal(out.length, chrome.length);
  assert.deepEqual(chrome, copy);
  for (const c of chrome) assert.ok(out.includes(c));
});

test('a browser without VP9 keeps VP8 first', () => {
  const noVp9 = chrome.filter((c) => c.mimeType !== 'video/VP9');
  assert.equal(preferVideoCodecs(noVp9)[0].mimeType, 'video/VP8');
});

test('case of the mime type does not matter', () => {
  assert.equal(preferVideoCodecs([{ mimeType: 'video/vp8' }, { mimeType: 'video/vp9' }])[0].mimeType, 'video/vp9');
});
