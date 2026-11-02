import test from 'node:test';
import assert from 'node:assert/strict';

import { micConstraints, processingReport, acquireMic } from './echo.js';

test('constraints: processing on, as a requirement when strict, the device pinned when named', () => {
  assert.deepEqual(micConstraints(), { audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  const strict = micConstraints('mic-7', { strict: true });
  assert.deepEqual(strict.audio.echoCancellation, { exact: true });
  assert.deepEqual(strict.audio.deviceId, { exact: 'mic-7' });
});

test('the report names what is off and warns only when echo cancellation is', () => {
  assert.deepEqual(processingReport({ echoCancellation: true, noiseSuppression: true, autoGainControl: true }), { ok: true, off: [], message: '' });
  const r = processingReport({ echoCancellation: false, noiseSuppression: true, autoGainControl: false });
  assert.equal(r.ok, false);
  assert.deepEqual(r.off, ['echoCancellation', 'autoGainControl']);
  assert.match(r.message, /echo cancellation, automatic gain off/);
  assert.match(r.message, /Use headphones/);
  const quiet = processingReport({ echoCancellation: true, noiseSuppression: false });
  assert.equal(quiet.ok, true, 'noise suppression off is not an echo problem');
  assert.doesNotMatch(quiet.message, /headphones/);
  assert.equal(processingReport(undefined).ok, true, 'no settings at all: nothing to accuse');
});

const fakeStream = (settings) => ({ getAudioTracks: () => [{ getSettings: () => settings }] });

test('acquireMic: a device that processes is opened with processing required', async () => {
  const asked = [];
  const gum = async (c) => {
    asked.push(c);
    return fakeStream({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
  };
  const got = await acquireMic(gum, 'mic-1');
  assert.equal(asked.length, 1);
  assert.deepEqual(asked[0].audio.echoCancellation, { exact: true });
  assert.equal(got.report.ok, true);
});

test('acquireMic: a device that cannot process is used anyway and reported', async () => {
  const asked = [];
  const gum = async (c) => {
    asked.push(c);
    if (c.audio.echoCancellation?.exact) {
      const e = new Error('no');
      e.name = 'OverconstrainedError';
      throw e;
    }
    return fakeStream({ echoCancellation: false, noiseSuppression: false, autoGainControl: false });
  };
  const got = await acquireMic(gum);
  assert.equal(asked.length, 2, 'required, then best effort');
  assert.equal(asked[1].audio.echoCancellation, true);
  assert.equal(got.report.ok, false);
  assert.match(got.report.message, /Others may hear themselves/);
});

test('acquireMic: no permission is not softened into a second attempt', async () => {
  const gum = async () => {
    const e = new Error('denied');
    e.name = 'NotAllowedError';
    throw e;
  };
  await assert.rejects(() => acquireMic(gum), /denied/);
});
