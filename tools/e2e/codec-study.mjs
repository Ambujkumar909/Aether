// Camera encoding study: what the same frames cost, and how good they look,
// under the socket path's encoder today (VP8, 600 kbit/s target) and under
// VP9 at a constant quality (WebCodecs bitrateMode 'quantizer').
//
// Three kinds of picture, 150 frames each at 1280x720, 30 fps:
//   moving  the fake camera as it is (a moving pattern and a clock)
//   still   one frame of it, repeated (a person sitting still is close)
//   flat    a plain background colour (a replaced background, nobody in it)
// For each: bytes per second, and PSNR of the decoded frames against the
// originals (luma, from the drawn RGB). Equal PSNR is equal quality.
//
//   node tools/e2e/codec-study.mjs http://localhost:8444
import puppeteer from 'puppeteer-core';

const BASE = process.argv.find((a) => a.startsWith('http')) || 'http://localhost:8444';
const b = await puppeteer.launch({
  executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
  headless: true,
  args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
});
const pg = await b.newPage();
await pg.goto(BASE + '/healthz');
const out = await pg.evaluate(async () => {
  const W = 1280, H = 720, N = 150, FPS = 30;
  const cam = await navigator.mediaDevices.getUserMedia({ video: { width: W, height: H, frameRate: 30 } });
  const reader = new MediaStreamTrackProcessor({ track: cam.getVideoTracks()[0] }).readable.getReader();
  // Capture N frames as ImageBitmaps (so every encoder sees identical input).
  const moving = [];
  while (moving.length < N) {
    const { value: f } = await reader.read();
    moving.push(await createImageBitmap(f, { resizeWidth: W, resizeHeight: H }));
    f.close();
  }
  cam.getTracks().forEach((t) => t.stop());
  const flatCanvas = new OffscreenCanvas(W, H);
  const fc = flatCanvas.getContext('2d');
  fc.fillStyle = '#0f3d3a';
  fc.fillRect(0, 0, W, H);
  const flatBmp = await createImageBitmap(flatCanvas);
  const sets = { moving, still: Array(N).fill(moving[40]), flat: Array(N).fill(flatBmp) };

  const lumaOf = (src) => {
    const c = new OffscreenCanvas(320, 180);
    const g = c.getContext('2d', { willReadFrequently: true });
    g.drawImage(src, 0, 0, 320, 180);
    const d = g.getImageData(0, 0, 320, 180).data;
    const y = new Float32Array(320 * 180);
    for (let i = 0, j = 0; j < y.length; i += 4, j++) y[j] = 0.299 * d[i] + 0.587 * d[i + 1] + 0.114 * d[i + 2];
    return y;
  };
  const psnr = (a, bb) => {
    let se = 0;
    for (let i = 0; i < a.length; i++) se += (a[i] - bb[i]) ** 2;
    const mse = se / a.length;
    return mse === 0 ? 99 : 10 * Math.log10((255 * 255) / mse);
  };

  async function run(frames, cfg, perFrame) {
    let bytes = 0;
    const decoded = [];
    const dec = new VideoDecoder({ output: (f) => { decoded.push(lumaOf(f)); f.close(); }, error: (e) => { throw e; } });
    dec.configure({ codec: cfg.codec });
    const enc = new VideoEncoder({
      output: (chunk) => { bytes += chunk.byteLength; const d = new Uint8Array(chunk.byteLength); chunk.copyTo(d); dec.decode(new EncodedVideoChunk({ type: chunk.type, timestamp: chunk.timestamp, data: d })); },
      error: (e) => { throw e; },
    });
    enc.configure({ width: W, height: H, framerate: FPS, latencyMode: 'realtime', ...cfg });
    for (let i = 0; i < frames.length; i++) {
      const vf = new VideoFrame(frames[i], { timestamp: Math.round((i * 1e6) / FPS) });
      enc.encode(vf, { keyFrame: i === 0, ...perFrame });
      vf.close();
    }
    await enc.flush();
    await dec.flush();
    let ps = 0;
    const n = Math.min(decoded.length, frames.length);
    for (let i = 0; i < n; i++) ps += psnr(lumaOf(frames[i]), decoded[i]);
    enc.close();
    dec.close();
    return { kbps: Math.round((bytes * 8) / (frames.length / FPS) / 1000), psnr: +(ps / n).toFixed(2), frames: n };
  }

  const results = {};
  for (const [name, frames] of Object.entries(sets)) {
    results[name] = { 'vp8 600k (today)': await run(frames, { codec: 'vp8', bitrate: 600_000 }, {}) };
    for (const q of [20, 28, 36, 44]) {
      results[name][`vp9 q${q}`] = await run(frames, { codec: 'vp09.00.10.08', bitrateMode: 'quantizer' }, { vp9: { quantizer: q } });
    }
  }
  return results;
});
for (const [name, r] of Object.entries(out)) {
  console.log(`\n${name}`);
  for (const [k, v] of Object.entries(r)) console.log(`  ${k.padEnd(18)} ${String(v.kbps).padStart(6)} kbit/s   PSNR ${v.psnr} dB   (${v.frames} frames)`);
}
await b.close();
