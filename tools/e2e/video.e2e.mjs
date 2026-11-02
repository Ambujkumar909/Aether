// What video a person actually receives, measured on the receiver.
//
// Two real browsers in one room, both cameras on at 1280x720. After the call
// settles, read on B what arrives from A: resolution, frames per second,
// bitrate, freezes, and on A what the sender is doing (which encodings are
// active, and whether it says it is limited by bandwidth or CPU). Samples
// every second for WINDOW seconds, so a ramp-up or a collapse shows.
//
//   node tools/e2e/video.e2e.mjs [https://localhost:8443] [--media webrtc|ws] [--secs 20]
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const BASE = argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const arg = (n, d) => { const i = argv.indexOf(n); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const MEDIA = arg('--media', 'webrtc');
const SECS = Number(arg('--secs', '20'));
const accounts = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'))[new URL(BASE).host];
const CHROME = ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'].find((p) => existsSync(p));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (c, w) => { console.log(`${c ? 'ok  ' : 'FAIL'}  ${w}`); if (!c) failures++; };

const INSTRUMENT = `(() => {
  const O = window.RTCPeerConnection; window.__pcs = [];
  window.RTCPeerConnection = function (...a) { const pc = new O(...a); window.__pcs.push(pc); return pc; };
  window.RTCPeerConnection.prototype = O.prototype;
})();`;

async function signedIn(acct) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(INSTRUMENT);
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  const home = () => getComputedStyle(document.getElementById('auth')).display === 'none';
  if (!(await page.evaluate(home))) {
    await page.evaluate(async (e, p) => { const m = await import('/src/api.js'); await m.login(e, p); }, acct.email, acct.password);
  }
  return page;
}
async function joinRoom(page, room) {
  await page.goto(`${BASE}/?room=${room}&media=${MEDIA}`, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 20000 });
  await sleep(800);
  await page.evaluate(() => document.getElementById('pj-join').click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
}
// One sample: inbound camera video on this page (largest by bytes), outbound per encoding.
const sample = (page) => page.evaluate(async () => {
  const o = { in: null, out: [], tiles: [] };
  for (const pc of window.__pcs) {
    const st = await pc.getStats();
    for (const s of st.values()) {
      if (s.type === 'inbound-rtp' && s.kind === 'video' && (!o.in || s.bytesReceived > o.in.bytes)) {
        o.in = { codec: st.get(s.codecId)?.mimeType, w: s.frameWidth, h: s.frameHeight, fps: s.framesPerSecond, bytes: s.bytesReceived, frames: s.framesDecoded, freezes: s.freezeCount, freezeS: s.totalFreezesDuration, qp: s.qpSum && s.framesDecoded ? s.qpSum / s.framesDecoded : null };
      }
      if (s.type === 'outbound-rtp' && s.kind === 'video') {
        o.out.push({ rid: s.rid, w: s.frameWidth, h: s.frameHeight, fps: s.framesPerSecond, bytes: s.bytesSent, active: s.active, limit: s.qualityLimitationReason, target: s.targetBitrate });
      }
    }
  }
  // What the page shows: the other person's tile, as drawn.
  o.tiles = [...document.querySelectorAll('video')].filter((v) => v.videoWidth).map((v) => ({ vw: v.videoWidth, vh: v.videoHeight, cw: Math.round(v.getBoundingClientRect().width), ch: Math.round(v.getBoundingClientRect().height) }));
  o.t = performance.now();
  return o;
});

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ['--ignore-certificate-errors', '--no-first-run', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] });
try {
  const room = 'e2e-video-' + Math.random().toString(36).slice(2, 7);
  const A = await signedIn(accounts.A);
  const B = await signedIn(accounts.B);
  await joinRoom(A, room);
  await joinRoom(B, room);
  const cam = await A.evaluate(() => { const t = [...document.querySelectorAll('video')].map((v) => v.srcObject?.getVideoTracks?.()[0]).find((t) => t && t.label); return t ? t.getSettings() : null; });
  const srcFps = cam?.frameRate ?? 30;
  console.log(`== ${MEDIA}, room ${room}, sampling ${SECS} s; A's camera ${cam?.width}x${cam?.height} @ ${cam?.frameRate} fps`);
  const rows = [];
  let prev = await sample(B), prevA = await sample(A);
  for (let i = 0; i < SECS; i++) {
    await sleep(1000);
    const cur = await sample(B), curA = await sample(A);
    const dt = (cur.t - prev.t) / 1000;
    const kbps = cur.in && prev.in ? Math.round(((cur.in.bytes - prev.in.bytes) * 8) / dt / 1000) : 0;
    const outKbps = curA.out.map((e) => { const p = prevA.out.find((x) => x.rid === e.rid); return `${e.rid ?? '-'}:${e.w ?? 0}x${e.h ?? 0}@${Math.round(e.fps ?? 0)} ${p ? Math.round(((e.bytes - p.bytes) * 8) / dt / 1000) : 0}k ${e.limit ?? ''}`; }).join(' | ');
    const upKbps = curA.out.reduce((a, e) => { const p = prevA.out.find((x) => x.rid === e.rid); return a + (p ? ((e.bytes - p.bytes) * 8) / dt / 1000 : 0); }, 0);
    rows.push({ i, w: cur.in?.w ?? 0, h: cur.in?.h ?? 0, fps: cur.in?.fps ?? 0, kbps, up: Math.round(upKbps), freezes: cur.in?.freezes ?? 0, qp: cur.in?.qp });
    console.log(`  t=${String(i + 1).padStart(2)}s  B gets ${cur.in?.w ?? 0}x${cur.in?.h ?? 0} @ ${Math.round(cur.in?.fps ?? 0)} fps, ${kbps} kbit/s, qp ${cur.in?.qp?.toFixed(0) ?? '-'}, freezes ${cur.in?.freezes ?? 0}   | A sends ${outKbps}`);
    prev = cur; prevA = curA;
  }
  // Switch streams: shrink B's window so the tile wants a lower rung, then
  // grow it back. Every switch must land on a keyframe without a freeze.
  const f0 = (await sample(B)).in?.freezes ?? 0;
  const sizes = [];
  for (const [w, h] of [[640, 480], [1280, 800], [480, 400], [1280, 800]]) {
    await B.setViewport({ width: w, height: h });
    await sleep(3500);
    const s1 = await sample(B);
    sizes.push(`${w}x${h} window -> ${s1.in?.w}x${s1.in?.h}`);
  }
  const f1 = (await sample(B)).in?.freezes ?? 0;
  console.log('  layer switches: ' + sizes.join('; ') + `; freezes during switching: ${f1 - f0}`);
  ok(f1 - f0 === 0, `no freeze across ${sizes.length} stream switches (${f1 - f0})`);
  console.log(`  codec in use: ${(await sample(B)).in?.codec}`);
  const last = rows.slice(-5);
  const avg = (k) => last.reduce((a, r) => a + r[k], 0) / last.length;
  const tiles = (await sample(B)).tiles;
  console.log(`  B's video elements: ${JSON.stringify(tiles)}`);
  console.log(`    REPORT ${JSON.stringify({ media: MEDIA, last5: { w: avg('w'), h: avg('h'), fps: +avg('fps').toFixed(1), downKbps: Math.round(avg('kbps')), upKbps: Math.round(avg('up')) }, freezes: rows.at(-1).freezes })}`);
  ok(avg('h') >= 700, `B receives HD from A in steady state (${avg('w')}x${avg('h')})`);
  ok(avg('fps') >= srcFps * 0.9, `at the camera's frame rate (${avg('fps').toFixed(1)} of ${srcFps} fps)`);
  ok((rows.at(-1).freezes ?? 0) === 0, `with no freezes (${rows.at(-1).freezes})`);
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
