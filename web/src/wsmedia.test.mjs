import test from 'node:test';
import assert from 'node:assert/strict';

import { pickVideoConfig, VP9, CAMERA_Q, SCREEN_Q } from './wsmedia.js';

test('VP9 at a constant quality where the browser can; the screen gets the finer quantizer', async () => {
  const yes = async (c) => ({ supported: c.codec === VP9 && c.bitrateMode === 'quantizer' });
  const cam = await pickVideoConfig(1280, 720, 30, false, yes);
  assert.equal(cam.vp9, true);
  assert.equal(cam.config.codec, VP9);
  assert.equal(cam.config.bitrateMode, 'quantizer');
  assert.equal(cam.config.bitrate, undefined, 'no bitrate target: quality decides');
  assert.deepEqual(cam.perFrame, { vp9: { quantizer: CAMERA_Q } });
  const scr = await pickVideoConfig(1920, 1080, 30, true, yes);
  assert.deepEqual(scr.perFrame, { vp9: { quantizer: SCREEN_Q } });
  assert.ok(SCREEN_Q < CAMERA_Q);
});

test('VP8 at the old target where it cannot, or where asking throws', async () => {
  for (const no of [async () => ({ supported: false }), async () => { throw new Error('nope'); }]) {
    const cam = await pickVideoConfig(640, 360, 30, false, no);
    assert.equal(cam.vp9, false);
    assert.equal(cam.config.codec, 'vp8');
    assert.equal(cam.config.bitrate, 600_000);
    assert.deepEqual(cam.perFrame, {});
    const scr = await pickVideoConfig(640, 360, 30, true, no);
    assert.equal(scr.config.bitrate, 2_500_000);
  }
});
