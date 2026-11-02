// Neural noise suppression for the microphone.
//
// The browser's own processing (echo.js asks for it as a requirement) takes
// the room out of a voice: echo, gain, and a spectral-subtraction noise
// suppressor tuned to be safe rather than strong. What it leaves is the
// fan, the air conditioner, the keyboard, the road -- the steady noise that
// makes a voice tiring to listen to for an hour. RNNoise (Valin, 2018; the
// same network Jitsi and others ship) is a small recurrent network trained
// to separate speech from exactly that. It runs here in an AudioWorklet on
// the capture thread, on 48 kHz audio in 10 ms frames, between the device
// and the encoder, so BOTH transports send the cleaned voice and nothing on
// the receiving side changes.
//
// Fullband on purpose. The alternative in the same package (GTCRN) scores
// better on some benchmarks but runs at 16 kHz and returns 16 kHz audio:
// everything above 8 kHz -- the sibilants, the air that makes a voice sound
// present rather than telephoned -- would be gone before Opus ever saw it.
// Clarity is the point; a narrower voice is not clearer.
//
// Cost: about a millisecond of CPU per 10 ms frame on a laptop core, some
// 27 ms of added latency (the wrapper's ring buffer holds 1280 samples
// ahead of the 480-sample frame; measured end to end in ARCHITECTURE.md),
// and one download of ~150 KB (gzipped ~60 KB), fetched once and cached.
// It is on by default and a switch in the pre-join turns it off; the
// choice is remembered.
//
// Pure apart from the Web Audio objects it builds, which are injected for
// the tests (denoise.test.mjs). The processor code and the WebAssembly are
// vendored under /vendor/rnnoise (MIT / BSD; see the LICENSE there).

export const SETTING = 'aether.denoise';
export const WORKLET_URL = '/vendor/rnnoise/worklet.js';
export const WASM_URL = '/vendor/rnnoise/rnnoise.wasm';
export const WASM_SIMD_URL = '/vendor/rnnoise/rnnoise_simd.wasm';
export const PROCESSOR = '@sapphi-red/web-noise-suppressor/rnnoise';
// The voice leveler (leveler.js) runs after the suppressor, in both modes.
export const LEVELER_URL = '/src/leveler.worklet.js';
export const LEVELER = 'aether-leveler';
// RNNoise's native rate; the context is pinned to it so the worklet never
// sees anything else, whatever the device runs at.
export const RATE = 48000;

// The remembered choice: on unless turned off.
export function denoiseWanted(storage = safeStorage()) {
  try {
    return storage?.getItem(SETTING) !== 'off';
  } catch {
    return true;
  }
}

export function setDenoiseWanted(on, storage = safeStorage()) {
  try {
    storage?.setItem(SETTING, on ? 'on' : 'off');
  } catch {
    /* private mode; the choice lasts the session */
  }
}

function safeStorage() {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

// What the worklet needs from the platform: AudioWorklet and WebAssembly.
// Chromium (Chrome, Edge, Android Chrome and WebView, Windows WebView2),
// Firefox and Safari 14.1+ all have both.
export function denoiseSupported(w = globalThis) {
  return (
    typeof w.AudioContext === 'function' &&
    typeof w.AudioWorkletNode === 'function' &&
    typeof w.WebAssembly === 'object' &&
    typeof w.WebAssembly.validate === 'function'
  );
}

// A one-instruction SIMD module; validate() says whether this engine runs
// SIMD, which picks the build that is about twice as fast per frame.
const SIMD_PROBE = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 123, 3, 2, 1, 0, 10, 10, 1, 8, 0, 65, 0, 253, 15, 253, 98, 11]);
export function wasmSimd(wasm = globalThis.WebAssembly) {
  try {
    return !!wasm?.validate(SIMD_PROBE);
  } catch {
    return false;
  }
}

// Which binary to fetch for this engine.
export function wasmUrlFor(simd) {
  return simd ? WASM_SIMD_URL : WASM_URL;
}

// The worklet says when its network is built, or why it could not be: the
// vendored processor posts {type:'ready'} or {type:'error', message}. Until
// then it passes audio through unchanged, so waiting costs nothing audible;
// not waiting cost a whole call once -- a policy that blocked the
// WebAssembly left the processor never ready, and the page believed
// suppression was on. Rejects on error or after timeoutMs.
export function awaitReady(port, timeoutMs = 3000) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanup();
      reject(new Error(`rnnoise: the worklet did not report ready within ${timeoutMs} ms`));
    }, timeoutMs);
    const on = (e) => {
      const m = e?.data;
      if (m?.type === 'ready') {
        cleanup();
        resolve();
      } else if (m?.type === 'error') {
        cleanup();
        reject(new Error(`rnnoise: ${m.message}`));
      }
    };
    const cleanup = () => {
      clearTimeout(timer);
      port.removeEventListener('message', on);
    };
    port.addEventListener('message', on);
    port.start?.();
  });
}

let binary = null; // the fetched module bytes, once per page
export async function loadWasm(fetchFn = globalThis.fetch, simd = wasmSimd()) {
  if (binary) return binary;
  const res = await fetchFn(wasmUrlFor(simd));
  if (!res.ok) throw new Error(`rnnoise: ${res.status} for ${wasmUrlFor(simd)}`);
  binary = await res.arrayBuffer();
  return binary;
}

/**
 * The microphone, cleaned. wrap() takes the device's track and returns a
 * Denoiser whose .track is what to publish; stop() ends the device track,
 * the processed track and the audio graph, so the microphone light goes out
 * with the call. The device track ending on its own (unplugged) ends the
 * processed track too, so the app sees one dead track, not a silent live one.
 */
export class Denoiser {
  constructor({ ctx, node, src, dest, raw, track, simd }) {
    this.ctx = ctx;
    this.node = node;
    this.src = src;
    this.dest = dest;
    this.raw = raw;
    this.track = track;
    this.simd = simd;
    this.stopped = false;
    this._onEnded = () => {
      try {
        this.track.stop();
      } catch {}
    };
    raw.addEventListener?.('ended', this._onEnded);
  }

  // suppress: include RNNoise (Strong). Without it the chain is the
  // leveler alone (Standard): the browser's own suppression, made loud enough.
  static async wrap(raw, { suppress = true, fetchFn = globalThis.fetch, AudioContextCtor = globalThis.AudioContext, workletUrl = WORKLET_URL } = {}) {
    const simd = suppress && wasmSimd();
    const wasmBinary = suppress ? await loadWasm(fetchFn, simd) : null;
    const ctx = new AudioContextCtor({ sampleRate: RATE, latencyHint: 'interactive' });
    try {
      if (suppress) await ctx.audioWorklet.addModule(workletUrl);
      await ctx.audioWorklet.addModule(LEVELER_URL);
      const mono = { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1], channelCount: 1, channelCountMode: 'explicit' };
      const src = ctx.createMediaStreamSource(new MediaStream([raw]));
      const node = suppress ? new AudioWorkletNode(ctx, PROCESSOR, { ...mono, processorOptions: { maxChannels: 1, wasmBinary } }) : null;
      const lev = new AudioWorkletNode(ctx, LEVELER, mono);
      const dest = ctx.createMediaStreamDestination();
      dest.channelCount = 1;
      dest.channelCountMode = 'explicit';
      (node ? src.connect(node) : src).connect(lev).connect(dest);
      await ctx.resume();
      // Proven working, or not used: see awaitReady.
      if (node) await awaitReady(node.port);
      const track = dest.stream.getAudioTracks()[0];
      try {
        track.contentHint = 'speech';
      } catch {}
      const d = new Denoiser({ ctx, node, src, dest, raw, track, simd });
      d.lev = lev;
      d.suppress = suppress;
      return d;
    } catch (e) {
      ctx.close().catch(() => {});
      throw e;
    }
  }

  stop() {
    if (this.stopped) return;
    this.stopped = true;
    this.raw.removeEventListener?.('ended', this._onEnded);
    try {
      this.node?.port.postMessage('destroy');
    } catch {}
    try {
      this.lev?.port.postMessage('destroy');
    } catch {}
    try {
      this.src.disconnect();
    } catch {}
    try {
      this.raw.stop();
    } catch {}
    try {
      this.track.stop();
    } catch {}
    this.ctx.close().catch(() => {});
  }
}
