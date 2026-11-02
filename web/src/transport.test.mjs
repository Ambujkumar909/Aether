// The transport decision is load-bearing. WebRTC is the default because it
// measured better for speech under loss (ARCHITECTURE.md); the socket path
// is the explicit opt-in and the automatic fallback. If this silently
// flipped, every call would take the transport that turns a stall into a
// hole in the voice.
//
//   node --test src/transport.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { decideMediaMode, chooseTransport } from './transport.js';

test('the default is WebRTC', () => {
  assert.equal(decideMediaMode(''), 'webrtc');
  assert.equal(decideMediaMode('?room=abc'), 'webrtc');
  assert.equal(decideMediaMode(undefined), 'webrtc');
  assert.equal(decideMediaMode(null), 'webrtc');
});

test('the socket path is opt-in, by either spelling, and old ?wsmedia=1 links still work', () => {
  assert.equal(decideMediaMode('?media=ws'), 'ws');
  assert.equal(decideMediaMode('?wsmedia=1'), 'ws');
  assert.equal(decideMediaMode('?room=x&wsmedia=1'), 'ws');
  assert.equal(decideMediaMode('?room=x&media=ws&webrtc=1'), 'ws', 'an explicit socket wins over a stray webrtc flag');
});

test('WebRTC can be asked for explicitly too', () => {
  assert.equal(decideMediaMode('?media=webrtc'), 'webrtc');
  assert.equal(decideMediaMode('?webrtc=1'), 'webrtc');
});

test('junk is the default, never the socket', () => {
  assert.equal(decideMediaMode('?media=WS'), 'webrtc');
  assert.equal(decideMediaMode('?media=socket'), 'webrtc');
  assert.equal(decideMediaMode('?wsmedia=true'), 'webrtc');
  assert.equal(decideMediaMode('?wsmedia=0'), 'webrtc');
});

test('a browser without WebCodecs gets WebRTC whatever the link asked; one with it gets what it asked', () => {
  assert.equal(chooseTransport('', true), 'webrtc');
  assert.equal(chooseTransport('', false), 'webrtc');
  assert.equal(chooseTransport('?media=ws', true), 'ws');
  assert.equal(chooseTransport('?media=ws', false), 'webrtc', 'the socket path cannot run there');
  assert.equal(chooseTransport('?media=webrtc', true), 'webrtc');
});
