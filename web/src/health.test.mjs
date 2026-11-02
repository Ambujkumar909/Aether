import test from 'node:test';
import assert from 'node:assert/strict';

import { health, show } from './health.js';

const s = (at, o) => ({ at, rtt: 42.4, ...o });

test('rates are over the interval, not since the call began', () => {
  const prev = s(0, { out: { audio: { bytes: 1000 } }, in: { audio: { bytes: 5000, lost: 0, recv: 100 } } });
  const cur = s(2000, { out: { audio: { bytes: 1000 + 8000 } }, in: { audio: { bytes: 5000 + 16000, lost: 10, recv: 190 } } });
  const h = health(prev, cur);
  assert.equal(h.audio.sentBitrate, 32); // 8000 B * 8 / 2000 ms
  assert.equal(h.audio.receivedBitrate, 64);
  assert.equal(h.audio.receivedPacketLoss, 10); // 10 lost of 100 in the interval
  assert.equal(h.network.roundTripTime, 42);
});

test('video rows: resolution and frame rate as reported now; audio has none', () => {
  const prev = s(0, { out: { video: { bytes: 0 } }, in: {} });
  const cur = s(1000, { out: { video: { bytes: 125000, w: 1280, h: 720, fps: 29.6 } }, in: { video: { bytes: 1, w: 640, h: 360, fps: 15 } } });
  const h = health(prev, cur);
  assert.equal(h.video.sentBitrate, 1000);
  assert.equal(h.video.sentResolution, '1280x720');
  assert.equal(h.video.sentFrameRate, 30);
  assert.equal(h.video.receivedResolution, '640x360');
  assert.equal(h.audio.sentResolution, null);
});

test('nothing measured reads as a dash, not a zero', () => {
  const h = health(null, s(1000, { out: {}, in: {} }));
  assert.equal(h.audio.sentBitrate, null);
  assert.equal(h.network.receivedPacketLoss, null);
  assert.equal(show('sentBitrate', null), '—');
  // a counter that went backwards (a renegotiated stream) is not a rate
  assert.equal(health(s(0, { out: { audio: { bytes: 900 } } }), s(1000, { out: { audio: { bytes: 100 } } })).audio.sentBitrate, null);
});

test('network: totals across kinds, loss over everything received', () => {
  const prev = s(0, { out: { audio: { bytes: 0 }, video: { bytes: 0 } }, in: { audio: { bytes: 0, lost: 0, recv: 0 }, video: { bytes: 0, lost: 0, recv: 0 } } });
  const cur = s(1000, { out: { audio: { bytes: 4000 }, video: { bytes: 96000 } }, in: { audio: { bytes: 4000, lost: 1, recv: 49 }, video: { bytes: 0, lost: 4, recv: 46 } } });
  const h = health(prev, cur);
  assert.equal(h.network.sentBitrate, 800);
  assert.equal(h.network.receivedPacketLoss, 5); // 5 lost of 100
});

test('units as Teams shows them', () => {
  assert.equal(show('sentBitrate', 850), '850 Kbps');
  assert.equal(show('receivedBitrate', 2400), '2.4 Mbps');
  assert.equal(show('receivedPacketLoss', 1.5), '1.5%');
  assert.equal(show('sentFrameRate', 30), '30 fps');
  assert.equal(show('roundTripTime', 42), '42 ms');
});
