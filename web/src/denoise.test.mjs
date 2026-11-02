import test from 'node:test';
import assert from 'node:assert/strict';
import { denoiseWanted, setDenoiseWanted, denoiseSupported, wasmSimd, wasmUrlFor, loadWasm, awaitReady, Denoiser, WASM_URL, WASM_SIMD_URL, SETTING } from './denoise.js';

const store = () => {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, v) };
};

test('noise suppression is on unless turned off, and the choice is remembered', () => {
  const s = store();
  assert.equal(denoiseWanted(s), true, 'default on');
  setDenoiseWanted(false, s);
  assert.equal(denoiseWanted(s), false);
  assert.equal(s.getItem(SETTING), 'off');
  setDenoiseWanted(true, s);
  assert.equal(denoiseWanted(s), true);
  assert.equal(denoiseWanted(null), true, 'no storage at all is still on');
  const throwing = { getItem() { throw new Error('private mode'); }, setItem() { throw new Error('private mode'); } };
  assert.equal(denoiseWanted(throwing), true, 'a storage that throws does not turn it off');
  assert.doesNotThrow(() => setDenoiseWanted(false, throwing));
});

test('support means AudioWorklet plus WebAssembly; anything less is reported, not attempted', () => {
  const full = { AudioContext() {}, AudioWorkletNode() {}, WebAssembly: { validate() {} } };
  assert.equal(denoiseSupported(full), true);
  assert.equal(denoiseSupported({ ...full, AudioWorkletNode: undefined }), false, 'no worklet');
  assert.equal(denoiseSupported({ ...full, WebAssembly: undefined }), false, 'no wasm');
  assert.equal(denoiseSupported({}), false);
});

test('the SIMD probe picks the SIMD binary only when the engine validates SIMD', () => {
  assert.equal(wasmSimd({ validate: () => true }), true);
  assert.equal(wasmSimd({ validate: () => false }), false);
  assert.equal(wasmSimd({ validate: () => { throw new Error('no'); } }), false, 'a throwing validate is "no SIMD"');
  assert.equal(wasmSimd(null), false, "no WebAssembly object at all");
  assert.equal(wasmUrlFor(true), WASM_SIMD_URL);
  assert.equal(wasmUrlFor(false), WASM_URL);
  // This Node validates real SIMD; the probe is a real module.
  assert.equal(wasmSimd(), WebAssembly.validate(new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 123, 3, 2, 1, 0, 10, 10, 1, 8, 0, 65, 0, 253, 15, 253, 98, 11])));
});

test('the binary is fetched once per page and a failed fetch is an error, not silence', async () => {
  let calls = 0;
  const bytes = new Uint8Array([0, 97, 115, 109]).buffer;
  const fetchFn = async (url) => {
    calls++;
    assert.equal(url, WASM_SIMD_URL);
    return { ok: true, arrayBuffer: async () => bytes };
  };
  const a = await loadWasm(fetchFn, true);
  const b = await loadWasm(fetchFn, true);
  assert.equal(a, bytes);
  assert.equal(b, bytes, 'the same buffer, cached');
  assert.equal(calls, 1, 'fetched once');
});

test('stop() ends the device track, the processed track and the graph, once', () => {
  let rawStops = 0, trackStops = 0, closes = 0, destroys = 0, disconnects = 0, removed = 0;
  const raw = { stop: () => rawStops++, addEventListener() {}, removeEventListener: () => removed++ };
  const track = { stop: () => trackStops++ };
  const d = new Denoiser({
    ctx: { close: () => (closes++, Promise.resolve()) },
    node: { port: { postMessage: (m) => m === 'destroy' && destroys++ } },
    src: { disconnect: () => disconnects++ },
    dest: {},
    raw,
    track,
    simd: true,
  });
  assert.equal(d.track, track);
  d.stop();
  d.stop();
  assert.deepEqual({ rawStops, trackStops, closes, destroys, disconnects, removed }, { rawStops: 1, trackStops: 1, closes: 1, destroys: 1, disconnects: 1, removed: 1 });
});

test('the device track ending ends the processed track, so the app sees one dead track', () => {
  let ended = null;
  let trackStops = 0;
  const raw = { stop() {}, addEventListener: (n, f) => n === 'ended' && (ended = f), removeEventListener() {} };
  const d = new Denoiser({ ctx: { close: () => Promise.resolve() }, node: { port: { postMessage() {} } }, src: { disconnect() {} }, dest: {}, raw, track: { stop: () => trackStops++ }, simd: false });
  assert.ok(ended, 'listens for the device ending');
  ended();
  assert.equal(trackStops, 1);
  void d;
});

// A MessagePort stand-in: listeners, start(), and a way to post from the
// worklet's side.
const fakePort = () => {
  const ls = new Set();
  return {
    started: 0,
    addEventListener: (n, f) => n === 'message' && ls.add(f),
    removeEventListener: (n, f) => n === 'message' && ls.delete(f),
    start() { this.started++; },
    emit(data) { for (const f of [...ls]) f({ data }); },
    get listeners() { return ls.size; },
  };
};

test('the worklet is used only once it reports ready; an error or silence is a failure, not a silent microphone', async () => {
  const p1 = fakePort();
  const ready = awaitReady(p1, 1000);
  assert.equal(p1.started, 1, 'the port is started, or messages never arrive');
  p1.emit({ type: 'ready' });
  await ready;
  assert.equal(p1.listeners, 0, 'listener removed after ready');

  const p2 = fakePort();
  const failing = awaitReady(p2, 1000);
  p2.emit({ type: 'error', message: "Refused to compile or instantiate WebAssembly module because 'unsafe-eval' is not an allowed source" });
  await assert.rejects(failing, /rnnoise: Refused to compile/);
  assert.equal(p2.listeners, 0);

  const p3 = fakePort();
  await assert.rejects(awaitReady(p3, 30), /did not report ready within 30 ms/);
  assert.equal(p3.listeners, 0, 'listener removed after the timeout');

  const p4 = fakePort();
  const r4 = awaitReady(p4, 1000);
  p4.emit('destroy-echo');
  p4.emit({ type: 'other' });
  p4.emit({ type: 'ready' });
  await r4;
});
