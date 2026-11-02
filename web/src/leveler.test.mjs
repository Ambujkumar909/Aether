import test from 'node:test';
import assert from 'node:assert/strict';
import { createLeveler, RATE, TARGET_DB, MAX_GAIN_DB } from './leveler.js';

const dB = (v) => 20 * Math.log10(v + 1e-12);
const rms = (x, a = 0, b = x.length) => { let s = 0; for (let i = a; i < b; i++) s += x[i] * x[i]; return Math.sqrt(s / (b - a)); };
// "Speech": a 300 Hz tone at level dBFS (RMS), on for onS, off for offS, repeated.
function talk(levelDb, seconds, onS = 0.6, offS = 0.3, noiseDb = -200) {
  const n = Math.round(seconds * RATE), x = new Float32Array(n);
  const amp = Math.pow(10, levelDb / 20) * Math.SQRT2, nAmp = Math.pow(10, noiseDb / 20) * Math.sqrt(3);
  let seed = 7;
  for (let i = 0; i < n; i++) {
    const t = i / RATE, on = t % (onS + offS) < onS;
    seed = (seed * 1664525 + 1013904223) >>> 0;
    x[i] = (on ? amp * Math.sin(2 * Math.PI * 300 * t) : 0) + nAmp * (seed / 4294967296 * 2 - 1);
  }
  return x;
}
const run = (x) => { const L = createLeveler(), y = new Float32Array(x.length); for (let i = 0; i < x.length; i += 128) L.process(x.subarray(i, i + 128), y.subarray(i, i + 128)); return { y, L }; };
// Level of the "on" parts in the last `s` seconds.
function onLevel(y, s, onS = 0.6, offS = 0.3) {
  let sum = 0, n = 0;
  for (let i = y.length - Math.round(s * RATE); i < y.length; i++) if ((i / RATE) % (onS + offS) < onS - 0.02 && (i / RATE) % (onS + offS) > 0.02) { sum += y[i] * y[i]; n++; }
  return dB(Math.sqrt(sum / n));
}

test('a quiet voice is brought up to talking level', () => {
  const { y } = run(talk(-38, 8));
  const lv = onLevel(y, 2);
  assert.ok(Math.abs(lv - TARGET_DB) < 2, `quiet -38 dBFS voice ends at ${lv.toFixed(1)} dBFS (target ${TARGET_DB})`);
});

test('a loud voice is brought down, and nothing clips on the way', () => {
  const { y } = run(talk(-8, 6));
  const lv = onLevel(y, 2);
  assert.ok(Math.abs(lv - TARGET_DB) < 2.5, `loud -8 dBFS voice ends at ${lv.toFixed(1)} dBFS`);
  let peak = 0;
  for (const v of y) peak = Math.max(peak, Math.abs(v));
  assert.ok(peak <= 0.9, `peak ${dB(peak).toFixed(1)} dBFS never clips`);
});

test('a voice already at talking level is left alone', () => {
  const x = talk(-20, 6);
  const { y } = run(x);
  assert.ok(Math.abs(onLevel(y, 2) - onLevel(x, 2)) < 1.5, 'within 1.5 dB of the input');
});

test('noise alone is never turned up: no speech, no gain', () => {
  const { y, L } = run(talk(-200, 6, 0.6, 0.3, -50));
  assert.ok(Math.abs(L.gainDb) < 0.01, `gain stayed at ${L.gainDb.toFixed(2)} dB`);
  assert.ok(Math.abs(dB(rms(y)) - -50) < 1, 'output noise at the input noise level');
});

test('the gain holds through pauses: noise in a pause is not pumped up', () => {
  // Quiet talker in a noisy room; after it converges, the pause level must
  // be the input pause level plus the (held) speech gain -- the SNR the
  // talker had -- not raised further.
  const x = talk(-38, 10, 0.6, 0.3, -58);
  const { y, L } = run(x);
  const g = L.gainDb;
  assert.ok(g > 12 && g <= MAX_GAIN_DB, `gain ${g.toFixed(1)} dB`);
  // SNR in, SNR out: within 2 dB.
  const snrIn = onLevel(x, 3) - dB(rms(x, x.length - Math.round(0.28 * RATE), x.length - Math.round(0.02 * RATE)));
  const snrOut = onLevel(y, 3) - dB(rms(y, y.length - Math.round(0.28 * RATE), y.length - Math.round(0.02 * RATE)));
  assert.ok(Math.abs(snrIn - snrOut) < 2, `SNR ${snrIn.toFixed(1)} dB in, ${snrOut.toFixed(1)} dB out`);
});

test('bounded: a whisper at -70 is not raised by more than the ceiling', () => {
  const { L } = run(talk(-62, 20));
  assert.ok(L.gainDb <= MAX_GAIN_DB + 1e-9, `gain ${L.gainDb.toFixed(1)} dB`);
});

test('silence in, silence out', () => {
  const { y } = run(new Float32Array(RATE));
  assert.ok(y.every((v) => v === 0));
});

test('a quiet talker is at talking level within a second of starting, not after seconds', () => {
  // Syllables: 250 ms of voice, 80 ms gaps, from the first sample.
  const x = talk(-38, 3, 0.25, 0.08);
  const { y } = run(x);
  // The voiced parts between 0.7 and 1.0 s.
  let s = 0, n = 0;
  for (let i = Math.round(0.7 * RATE); i < Math.round(1.0 * RATE); i++) if (Math.abs(x[i]) > 0) { s += y[i] * y[i]; n++; }
  const lv = dB(Math.sqrt(s / n));
  assert.ok(Math.abs(lv - TARGET_DB) < 2, `voiced level 0.7-1.0 s in: ${lv.toFixed(1)} dBFS`);
});

test('in a crowd, background talkers in your pauses are not turned up to your level', () => {
  // You: 300 Hz bursts at -30 dBFS, 0.6 s on / 0.4 s off. The crowd: a
  // quieter 700 Hz voice at -42 dBFS in short bursts that land only in your
  // pauses -- what RNNoise's output looks like in a crowd: it keeps your voice
  // while you talk and lets bursts of the crowd through between your words.
  const n = 12 * RATE, x = new Float32Array(n);
  for (let i = 0; i < n; i++) {
    const t = i / RATE, you = t % 1 < 0.6;
    x[i] = (you ? Math.pow(10, -30 / 20) * Math.SQRT2 * Math.sin(2 * Math.PI * 300 * t) : 0) + (!you && (t * 7) % 1 < 0.5 ? Math.pow(10, -42 / 20) * Math.SQRT2 * Math.sin(2 * Math.PI * 700 * t) : 0);
  }
  const { y, L } = run(x);
  assert.ok(Math.abs(L.speechDb - -30) < 2, `your level is tracked, not the crowd's: ${L.speechDb.toFixed(1)} dBFS for a -30 dBFS talker`);
  // Last 4 s: level while you talk vs while only the crowd does.
  let ys = 0, yn = 0, bs = 0, bn = 0, xs = 0, xb = 0;
  for (let i = n - 4 * RATE; i < n; i++) {
    const ph = (i / RATE) % 1;
    if (ph > 0.05 && ph < 0.55) { ys += y[i] * y[i]; yn++; xs += x[i] * x[i]; }
    if (ph > 0.65 && ph < 0.95) { bs += y[i] * y[i]; bn++; xb += x[i] * x[i]; }
  }
  const snrIn = 10 * Math.log10((xs / yn) / (xb / bn)), snrOut = 10 * Math.log10((ys / yn) / (bs / bn));
  assert.ok(snrOut > snrIn - 2, `you over the crowd: ${snrIn.toFixed(1)} dB in, ${snrOut.toFixed(1)} dB out`);
  assert.ok(Math.abs(10 * Math.log10(ys / yn) - TARGET_DB) < 1.5, `you are at talking level (${(10 * Math.log10(ys / yn)).toFixed(1)} dBFS)`);
});

test('a talker who really moves away is re-acquired within a few seconds', () => {
  // 5 s close at -24, then 8 s further back at -40 (16 dB quieter).
  const a = talk(-24, 5.4), b = talk(-40, 8.1); // whole 0.9 s cycles, so the phases line up
  const x = new Float32Array(a.length + b.length);
  x.set(a); x.set(b, a.length);
  const { y } = run(x);
  const lv = onLevel(y, 2);
  assert.ok(Math.abs(lv - TARGET_DB) < 2.5, `after moving back the talker is at ${lv.toFixed(1)} dBFS`);
});
