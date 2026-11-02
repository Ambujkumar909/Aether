import test from 'node:test';
import assert from 'node:assert/strict';

import { normalize, defaultSettings, needsPipeline, needsSegmenter, blurRadius, edgeAlpha, EDGE_LO, EDGE_HI, smoothMask, brightnessGain, meanLuma, personFilter } from './effects.js';

test('settings from storage are never trusted', () => {
  assert.deepEqual(normalize(null), defaultSettings());
  assert.deepEqual(normalize({ background: 'lasers', image: 7, softFocus: 'yes', brightness: 1 }), defaultSettings());
  assert.deepEqual(normalize({ background: 'image', image: 'teal', softFocus: true, brightness: true }), { background: 'image', image: 'teal', softFocus: true, brightness: true });
  assert.equal(normalize({ image: 'x'.repeat(500) }).image.length, 64);
});

test('no effect, no pipeline; only backgrounds need the segmenter', () => {
  assert.equal(needsPipeline(defaultSettings()), false);
  assert.equal(needsPipeline({ ...defaultSettings(), brightness: true }), true);
  assert.equal(needsSegmenter({ ...defaultSettings(), brightness: true }), false);
  assert.equal(needsSegmenter({ ...defaultSettings(), background: 'blur' }), true);
});

test('blur is the same look at any resolution, portrait heavier', () => {
  assert.equal(blurRadius('blur', 720), 13);
  assert.equal(blurRadius('portrait', 720), 26);
  assert.equal(blurRadius('blur', 1080), 20);
  assert.equal(blurRadius('blur', 90), 2);
});

test('the mask edge is soft and monotonic', () => {
  assert.equal(edgeAlpha(0), 0);
  assert.equal(edgeAlpha(EDGE_LO), 0);
  assert.equal(edgeAlpha(EDGE_HI), 255);
  assert.equal(edgeAlpha(1), 255);
  assert.equal(edgeAlpha((EDGE_LO + EDGE_HI) / 2), 128);
  let last = -1;
  for (let c = 0; c <= 1.0001; c += 0.01) {
    const a = edgeAlpha(c);
    assert.ok(a >= last, `not monotonic at ${c}`);
    last = a;
  }
});

test('the mask is smoothed over time, and resets on a size change', () => {
  const prev = new Float32Array([0, 1]);
  const out = smoothMask(prev, new Float32Array([1, 0]), 0.5);
  assert.deepEqual([...out], [0.5, 0.5]);
  assert.equal(out, prev); // in place
  assert.deepEqual([...smoothMask(new Float32Array(3), new Float32Array([1, 1]))], [1, 1]);
});

test('"Adjust brightness" lifts a dim picture and leaves a bright one alone', () => {
  assert.equal(brightnessGain(0.6), 1);
  assert.equal(brightnessGain(0.46), 1);
  assert.ok(Math.abs(brightnessGain(0.3) - 0.46 / 0.3) < 1e-9); // between: in proportion
  assert.equal(brightnessGain(0.23), 1.8); // capped
  assert.equal(brightnessGain(0.1), 1.8);
  assert.equal(brightnessGain(0), 1.8);
  assert.equal(brightnessGain(NaN), 1.8);
  const grey = new Uint8ClampedArray([128, 128, 128, 255, 128, 128, 128, 255]);
  assert.ok(Math.abs(meanLuma(grey, 1) - 128 / 255) < 1e-6);
  assert.equal(personFilter({ brightness: false }, 1.5), 'none');
  assert.equal(personFilter({ brightness: true }, 1), 'none');
  assert.equal(personFilter({ brightness: true }, 1.5), 'brightness(1.50) contrast(1.04)');
});
