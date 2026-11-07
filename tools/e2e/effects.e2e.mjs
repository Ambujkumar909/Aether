// Video effects between two real browsers, checked on the pixels B receives:
//   - Camera > Video effects opens Teams' panel: None, Standard blur,
//     Portrait blur, backgrounds, Add new, Soft focus, Adjust brightness,
//     Mirror my video;
//   - Standard blur: what B sees of A loses its detail; with nothing yet to
//     tell person from background, all of it is blurred, never shown sharp;
//   - a background: B sees its colour; your own image: B sees it, and the
//     image never leaves A's browser;
//   - None: A's own camera track is published again, detail back;
//   - camera off with blur on: the camera itself is released (its light goes
//     out), not just the canvas;
//   - Mirror my video changes only A's own view;
//   - the choice is remembered on this device.
//
// A's camera is a drawn person -- head and shoulders in the top-left quarter
// -- against a plain wall with stripes on its right-hand third (the detail a
// blur takes away). The segmenter finds the figure as a person (checked against
// the CPU mask in probing), so the checks read two regions of what B
// receives: the face must stay a face while the background changes.
//
//   node tools/e2e/effects.e2e.mjs [https://localhost:8443]
import { writeFileSync } from 'node:fs';
import { deflateSync } from 'node:zlib';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep, offOrigin } from './meet.lib.mjs';

// A 320x180 solid red PNG, written by hand (no image library here).
function redPNG() {
  const w = 320, h = 180;
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    raw[y * (w * 3 + 1)] = 0;
    for (let x = 0; x < w; x++) raw.set([220, 30, 30], y * (w * 3 + 1) + 1 + x * 3);
  }
  const crc = (b) => { let c = ~0; for (const x of b) { c ^= x; for (let k = 0; k < 8; k++) c = (c >>> 1) ^ (0xedb88320 & -(c & 1)); } return ~c >>> 0; };
  const chunk = (type, data) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const file = join(tmpdir(), `aether-red-${process.pid}.png`);
  writeFileSync(file, Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]));
  return file;
}

const CAMERA = `(() => {
  const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (c) => {
    if (!c || !c.video) return real(c);
    const cv = document.createElement('canvas');
    cv.width = 640; cv.height = 360;
    const g = cv.getContext('2d');
    let t = 0;
    const draw = () => {
      t++;
      g.fillStyle = '#c9d6df'; g.fillRect(0, 0, 640, 360);
      for (let x = 432; x < 640; x += 16) { g.fillStyle = ((x / 16 + (t >> 4)) % 2) ? '#c9d6df' : '#6f7c88'; g.fillRect(x, 0, 16, 360); }
      g.fillStyle = '#2a2f3a'; g.beginPath(); g.ellipse(160, 175, 120, 70, 0, Math.PI, 0); g.fill(); g.fillRect(40, 175, 240, 185);
      g.fillStyle = '#e0ac8a'; g.beginPath(); g.ellipse(160, 80, 45, 58, 0, 0, 2 * Math.PI); g.fill();
      g.fillStyle = '#3b2a20'; g.beginPath(); g.ellipse(160, 45, 48, 28, 0, Math.PI, 0); g.fill();
    };
    draw();
    setInterval(draw, 33);
    const s = cv.captureStream(30);
    if (c.audio) (await real({ audio: c.audio })).getAudioTracks().forEach((tr) => s.addTrack(tr));
    (window.__streams = window.__streams || []).push(s);
    return s;
  };
})();`;

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const room = 'fx-' + Math.random().toString(36).slice(2, 8);

// What B sees of A: detail (mean absolute difference between neighbouring
// pixels) and mean colour, read from the video element's pixels.
// region: [x0, y0, x1, y1] as fractions of the picture. The background is
// the right-hand third; the face is the middle of the head.
const BG = [0.68, 0.1, 0.98, 0.9], FACE = [0.22, 0.17, 0.28, 0.3];
const look = (p, region = BG) => p.evaluate((region) => {
  const v = [...document.querySelectorAll('#stage video')].find((x) => x.readyState >= 2 && x.videoWidth && !x.closest('.mirror'));
  if (!v) return null;
  const c = document.createElement('canvas');
  c.width = 320; c.height = 180;
  const g = c.getContext('2d', { willReadFrequently: true });
  g.drawImage(v, 0, 0, 320, 180);
  const [x0, y0, x1, y1] = [Math.round(region[0] * 320), Math.round(region[1] * 180), Math.round(region[2] * 320), Math.round(region[3] * 180)];
  const d = g.getImageData(x0, y0, x1 - x0, y1 - y0).data, w = x1 - x0, h = y1 - y0;
  let detail = 0, r = 0, gg = 0, b = 0;
  for (let y = 0; y < h; y++) for (let x = 0; x < w - 1; x++) {
    const i = (y * w + x) * 4;
    detail += Math.abs(d[i] - d[i + 4]) + Math.abs(d[i + 1] - d[i + 5]) + Math.abs(d[i + 2] - d[i + 6]);
    r += d[i]; gg += d[i + 1]; b += d[i + 2];
  }
  const n = h * (w - 1);
  return { detail: detail / n, r: r / n, g: gg / n, b: b / n };
}, region);
const skin = (f) => f && f.r > 170 && f.r > f.g + 25 && f.g > f.b;
const fx = (p) => p.evaluate(() => document.documentElement.dataset.fx ?? '');
const pick = (p, label) => p.evaluate((label) => {
  const t = [...document.querySelectorAll('#view-effects .fx-tile')].find((x) => x.title === label || x.textContent.trim() === label);
  t?.click();
  return !!t;
}, label);
// Polls what B sees until it satisfies `want`, up to 15 s; the last look
// either way.
const settle = async (p, want = () => true) => {
  let last = null;
  for (const t0 = Date.now(); Date.now() - t0 < 15000; await sleep(300)) {
    last = await look(p);
    if (last && want(last)) break;
  }
  return last;
};

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  await A.evaluateOnNewDocument(CAMERA);
  await A.evaluate(() => localStorage.removeItem('aether.effects'));
  const uploads = [];
  A.on('request', (r) => { if (['POST', 'PUT', 'PATCH'].includes(r.method()) && (r.postData()?.length ?? 0) > 2000) uploads.push(r.url()); });
  await enter(A, room);
  await enter(B, room);
  await sleep(4000);

  const before = await look(B);
  ok(before && before.detail > 10, `B sees A's background with its detail: ${JSON.stringify(before)}`);
  ok(skin(await look(B, FACE)), `and A's face: ${JSON.stringify(await look(B, FACE))}`);
  ok((await fx(A)) === 'off', 'no effect: no pipeline (data-fx=off)');

  await A.evaluate(() => { document.getElementById('btn-cam-more').click(); document.getElementById('cam-effects').click(); });
  ok(await until(A, () => document.getElementById('view-effects').classList.contains('active') && !!document.querySelector('#view-effects .fx-tile'), null), 'Camera > Video effects opens the panel');
  const labels = await A.evaluate(() => [...document.querySelectorAll('#view-effects .fx-tile-label, #view-effects .fx-switch span, #view-effects .fx-h')].map((x) => x.textContent.trim()));
  for (const w of ['Background effects', 'None', 'Standard blur', 'Portrait blur', 'Add new', 'Filters', 'Soft focus', 'Adjust brightness', 'Mirror my video']) ok(labels.includes(w), `the panel has "${w}"`);

  // Blur.
  ok(await pick(A, 'Standard blur'), 'A picks Standard blur');
  ok(await until(A, () => /^blur:/.test(document.documentElement.dataset.fx ?? ''), null), `A runs the pipeline: ${await fx(A)}`);
  const ready = await until(A, () => /:(ready|failed)$/.test(document.documentElement.dataset.fx ?? ''), null, 30000);
  ok(ready && (await fx(A)) === 'blur:ready', `the segmenter loads on this device: ${await fx(A)}`);
  const blurred = await settle(B, (x) => x.detail < before.detail * 0.5);
  ok(blurred && blurred.detail < before.detail * 0.5, `the background B sees has lost its detail: ${before.detail.toFixed(1)} -> ${blurred?.detail.toFixed(1)}`);
  const faceBlur = await look(B, FACE);
  ok(skin(faceBlur), `and the person is kept, not blurred away: face ${JSON.stringify(faceBlur)}`);


  // A background, then your own image.
  ok(await pick(A, 'Deep teal'), 'A picks the Deep teal background');
  const teal = await settle(B, (x) => Math.abs(x.g - 61) < 25 && x.r < 40);
  ok(teal && Math.abs(teal.r - 15) < 25 && Math.abs(teal.g - 61) < 25 && Math.abs(teal.b - 58) < 25, `B sees the background's colour: ${JSON.stringify(teal)}`);
  const faceTeal = await look(B, FACE);
  ok(skin(faceTeal), `in front of it, A's face: ${JSON.stringify(faceTeal)}`);
  const [input] = await A.$$('#view-effects input[data-add]');
  await input.uploadFile(redPNG());
  ok(await until(A, () => document.querySelectorAll('#view-effects .fx-mine').length === 1, null), 'Add new: A\'s image is added');
  const red = await settle(B, (x) => x.r > 150);
  ok(red && red.r > 150 && red.g < 90 && red.b < 90, `B sees A's own image: ${JSON.stringify(red)}`);
  ok(uploads.length === 0, `and the image never left A's browser: ${uploads.join(', ') || 'no upload'}`);

  // Filters work without the segmenter.
  ok(await pick(A, 'None'), 'A picks None');
  ok(await until(A, () => document.documentElement.dataset.fx === 'off', null), 'None: the pipeline is gone, the camera published as it is');
  const back = await settle(B, (x) => x.detail > blurred.detail * 1.8);
  ok(back && back.detail > blurred.detail * 1.8, `and B sees the detail again: ${blurred.detail.toFixed(2)} blurred -> ${back?.detail.toFixed(2)}`);
  await A.evaluate(() => document.querySelector('#view-effects [data-filter="brightness"]').click());
  ok(await until(A, () => document.documentElement.dataset.fx === 'none:none', null), `Adjust brightness alone runs without the segmenter: ${await fx(A)}`);
  await A.evaluate(() => document.querySelector('#view-effects [data-filter="brightness"]').click());
  ok(await until(A, () => document.documentElement.dataset.fx === 'off', null), 'and off again');

  // Mirror my video: A's view only.
  await A.evaluate(() => document.querySelector('#view-effects [data-mirror]').click());
  ok(await A.evaluate(() => !document.querySelector('#stage .mirror')), "Mirror my video off: A's own view is not mirrored");
  ok(await A.evaluate(() => localStorage.getItem('aether.mirror') === 'false'), 'remembered');
  await A.evaluate(() => document.querySelector('#view-effects [data-mirror]').click());

  // Camera off with blur on releases the camera itself.
  await pick(A, 'Standard blur');
  await until(A, () => /^blur:ready$/.test(document.documentElement.dataset.fx ?? ''), null, 15000);
  ok(await A.evaluate(() => window.__liveTracks() >= 1), 'with blur on, the camera is live');
  await A.evaluate(() => document.getElementById('btn-cam').click());
  ok(await until(A, () => window.__streams.flatMap((s) => s.getVideoTracks()).every((t) => t.readyState === 'ended'), null), 'camera off: the camera itself is released, its light goes out');
  ok(await until(A, () => document.documentElement.dataset.fx === 'off', null), 'and the pipeline with it');
  await A.evaluate(() => document.getElementById('btn-cam').click());
  ok(await until(A, () => /^blur:/.test(document.documentElement.dataset.fx ?? ''), null, 10000), 'camera on again: the blur comes back with it');
  ok(await A.evaluate(() => JSON.parse(localStorage.getItem('aether.effects')).background === 'blur'), 'the choice is kept on this device');
  // MediaPipe ran for the whole test: nothing it carries reached past the server.
  const away = offOrigin(log);
  ok(away.length === 0, `nothing left the server's host while the segmenter ran (${away.slice(0, 3).join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
