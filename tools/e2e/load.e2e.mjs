// Five people in one meeting for ten minutes, measured on every receiver.
//
//   --mode video   cameras on, microphones muted
//   --mode audio   microphones on, cameras off
//   --mode both    both
//   --codec vp8    force VP8 (the codec before VP9 was preferred): the baseline
//
// Every 10 s, on each of the five pages: how many of the other four it is
// receiving video from and decoding, at what resolution and frame rate, how
// many freezes; how many audio streams it receives and what fraction of the
// audio had to be concealed; bytes in and out. Every 30 s: CPU and memory of
// the server and of the browser. At the end: pass/fail against fixed rules
// and a REPORT line with the totals.
//
//   node tools/e2e/load.e2e.mjs [https://localhost:8443] --mode both --secs 600 [--codec vp8]
import { existsSync, readFileSync } from 'node:fs';
import { execSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const BASE = argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const arg = (n, d) => { const i = argv.indexOf(n); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const MODE = arg('--mode', 'both');
const SECS = Number(arg('--secs', '600'));
const CODEC = arg('--codec', '');
const N = 5;
const all = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'))[new URL(BASE).host];
const accts = ['A', 'B', 'C', 'D', 'E'].map((k) => all[k]);
const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (c, w) => { console.log(`${c ? 'ok  ' : 'FAIL'}  ${w}`); if (!c) failures++; };

const INSTRUMENT = `(() => {
  const O = window.RTCPeerConnection; window.__pcs = [];
  window.RTCPeerConnection = function (...a) { const pc = new O(...a); window.__pcs.push(pc); return pc; };
  window.RTCPeerConnection.prototype = O.prototype;
  if (${JSON.stringify(CODEC)} === 'vp8') RTCRtpTransceiver.prototype.setCodecPreferences = function () {};
})();`;

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, protocolTimeout: 120000,
  args: ['--ignore-certificate-errors', '--no-first-run', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] });
const errors = [];
const room = `e2e-load-${MODE}-` + Math.random().toString(36).slice(2, 6);
async function joinOne(acct, label) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(INSTRUMENT);
  page.on('pageerror', (e) => errors.push(`${label}: ${e}`));
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  if (!(await page.evaluate(() => getComputedStyle(document.getElementById('auth')).display === 'none'))) {
    await page.evaluate(async (e, p) => { const m = await import('/src/api.js'); await m.login(e, p); }, acct.email, acct.password);
  }
  const q = new URLSearchParams({ room, media: 'webrtc' });
  if (MODE === 'audio') q.set('audio', '1');
  await page.goto(`${BASE}/?${q}`, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 30000 });
  await sleep(800);
  if (MODE === 'video') await page.evaluate(() => document.getElementById('pj-mic').click());
  await sleep(400);
  await page.evaluate(() => document.getElementById('pj-join').click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 30000 });
  return page;
}
const sample = (page) => page.evaluate(async () => {
  const o = { vin: [], ain: [], out: 0, inBytes: 0, t: performance.now(), transport: null };
  for (const pc of window.__pcs) {
    const st = await pc.getStats();
    for (const s of st.values()) {
      if (s.type === 'inbound-rtp' && s.kind === 'video' && s.bytesReceived > 0) o.vin.push({ id: s.trackIdentifier || s.id, w: s.frameWidth || 0, h: s.frameHeight || 0, fps: s.framesPerSecond || 0, frames: s.framesDecoded || 0, freezes: s.freezeCount || 0, keys: s.keyFramesDecoded || 0, plis: s.pliCount || 0, dropped: s.framesDropped || 0, codec: st.get(s.codecId)?.mimeType });
      if (s.type === 'inbound-rtp' && s.kind === 'audio' && s.packetsReceived > 0) o.ain.push({ id: s.trackIdentifier || s.id, total: s.totalSamplesReceived || 0, concealed: s.concealedSamples || 0, silentConcealed: s.silentConcealedSamples || 0 });
      if (s.type === 'inbound-rtp') o.inBytes += s.bytesReceived || 0;
      if (s.type === 'outbound-rtp') o.out += s.bytesSent || 0;
      if (s.type === 'candidate-pair' && s.state === 'succeeded' && s.nominated) o.transport = 'webrtc';
    }
  }
  o.stage = document.getElementById('chip-who')?.textContent + '/' + document.getElementById('chip-role')?.textContent + ' vis=' + document.visibilityState + ' focus=' + document.hasFocus() + ' wake=' + (document.documentElement.dataset.wake ?? 'none');
  o.wsOn = document.querySelector('#rate-readout, .rate')?.textContent?.includes('WS') ?? null;
  return o;
});
function procStats() {
  try {
    const srv = execSync(`powershell -NoProfile -Command "Get-Process aether-server | Select-Object -First 1 | ForEach-Object { '{0} {1}' -f $_.TotalProcessorTime.TotalSeconds, $_.WorkingSet64 }"`, { encoding: 'utf8' }).trim().split(/\s+/).map(Number);
    const pid = browser.process().pid;
    const ch = execSync(`powershell -NoProfile -Command "$ids = @(${pid}); $all = Get-CimInstance Win32_Process -Filter \\"name='chrome.exe'\\"; $more = $true; while ($more) { $more = $false; foreach ($p in $all) { if ($ids -contains $p.ParentProcessId -and -not ($ids -contains $p.ProcessId)) { $ids += $p.ProcessId; $more = $true } } }; $t = 0; $m = 0; foreach ($p in $all) { if ($ids -contains $p.ProcessId) { $t += ($p.KernelModeTime + $p.UserModeTime) / 1e7; $m += $p.WorkingSetSize } }; '{0} {1}' -f $t, $m"`, { encoding: 'utf8' }).trim().split(/\s+/).map(Number);
    const sys = Number(execSync(`powershell -NoProfile -Command "(Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average"`, { encoding: 'utf8' }).trim());
    return { srvCpu: srv[0], srvMem: srv[1], brCpu: ch[0], brMem: ch[1], sys, at: Date.now() };
  } catch (e) {
    return null;
  }
}

try {
  const pages = [];
  for (let i = 0; i < N; i++) {
    pages.push(await joinOne(accts[i], 'ABCDE'[i]));
    await sleep(1500);
  }
  console.log(`== ${N} people, mode ${MODE}${CODEC ? ', codec forced ' + CODEC : ''}, ${SECS} s, room ${room}`);
  await sleep(8000);
  const first = await Promise.all(pages.map(sample));
  const p0 = procStats();
  let prev = first;
  const series = [];
  let lastProc = p0;
  const procSeries = [];
  const t0 = Date.now();
  while (Date.now() - t0 < SECS * 1000) {
    await sleep(10000);
    const cur = await Promise.all(pages.map(sample));
    const dt = (cur[0].t - prev[0].t) / 1000;
    const row = cur.map((c, i) => ({
      v: c.vin.filter((x) => x.frames > 0).length,
      res: c.vin.map((x) => `${x.w}x${x.h}@${Math.round(x.fps)}`).join(','),
      minFps: c.vin.length ? Math.min(...c.vin.map((x) => x.fps)) : 0,
      a: c.ain.length,
      downKbps: Math.round(((c.inBytes - prev[i].inBytes) * 8) / dt / 1000),
      upKbps: Math.round(((c.out - prev[i].out) * 8) / dt / 1000),
      freezes: c.vin.reduce((s, x) => s + x.freezes, 0),
      concealPct: (() => { const tot = c.ain.reduce((s, x) => s + x.total, 0) - prev[i].ain.reduce((s, x) => s + x.total, 0); const con = c.ain.reduce((s, x) => s + x.concealed - x.silentConcealed, 0) - prev[i].ain.reduce((s, x) => s + x.concealed - x.silentConcealed, 0); return tot > 0 ? (100 * con) / tot : 0; })(),
      codec: c.vin[0]?.codec,
    }));
    series.push(row);
    // Anything short: which receiver, and what its inbound video looks like.
    const wantV0 = MODE === 'audio' ? 0 : N - 1;
    cur.forEach((c, i) => {
      const live = c.vin.filter((x) => x.frames > 0).length;
      const fzNow = c.vin.reduce((s2, x) => s2 + x.freezes, 0), fzPrev = prev[i].vin.reduce((s2, x) => s2 + x.freezes, 0);
      if ((wantV0 && live < wantV0) || fzNow > fzPrev) {
        console.log(`    ANOMALY t=${Math.round((Date.now() - t0) / 1000)}s machine CPU ${procSeries.at(-1)?.sysPct ?? '?'}% ${'ABCDE'[i]}: ${live}/${wantV0} decoding, +${fzNow - fzPrev} freezes; inbound: ` + c.vin.map((x) => { const p = prev[i].vin.find((y) => y.id === x.id); return `${x.w}x${x.h}@${Math.round(x.fps)} frames ${x.frames} (+${p ? x.frames - p.frames : 'new'}) key+${p ? x.keys - p.keys : x.keys} pli+${p ? x.plis - p.plis : x.plis} fz+${p ? x.freezes - p.freezes : x.freezes}`; }).join(' | ') + ` stage=${c.stage}`);
      }
    });
    if (argv.includes('--trace')) {
      const pi = 0; // A's view, tile by tile: resolution@fps, keyframes and PLIs sent in this window, freezes
      console.log('    A tiles: ' + cur[pi].vin.map((x) => { const p = prev[pi].vin.find((y) => y.id === x.id) || { keys: 0, plis: 0, freezes: 0, frames: 0 }; return `${x.w}x${x.h}@${Math.round(x.fps)} +${x.frames - p.frames}fr +${x.keys - p.keys}key +${x.plis - p.plis}pli +${x.freezes - p.freezes}fz`; }).join(' | ') + '   stage: ' + (cur[pi].stage || ''));
    }
    const el = Math.round((Date.now() - t0) / 1000);
    if (el % 30 < 10) {
      const ps = procStats();
      if (ps && lastProc) {
        const w = (ps.at - lastProc.at) / 1000;
        procSeries.push({ srvCpuPct: (100 * (ps.srvCpu - lastProc.srvCpu)) / w, srvMemMB: ps.srvMem / 1e6, brCpuPct: (100 * (ps.brCpu - lastProc.brCpu)) / w, brMemMB: ps.brMem / 1e6, sysPct: ps.sys });
        lastProc = ps;
      }
    }
    const pr = procSeries.at(-1);
    console.log(`  t=${String(el).padStart(3)}s  ` + row.map((r, i) => `${'ABCDE'[i]}: v${r.v} a${r.a} ${r.downKbps}↓${r.upKbps}↑ fz${r.freezes} con${r.concealPct.toFixed(2)}%`).join(' | ') + (pr ? `  | server ${pr.srvCpuPct.toFixed(1)}% ${pr.srvMemMB.toFixed(0)}MB, browsers ${pr.brCpuPct.toFixed(0)}% ${pr.brMemMB.toFixed(0)}MB` : ''));
    prev = cur;
  }
  const last = series.at(-1);
  const wantV = MODE === 'audio' ? 0 : N - 1;
  const wantA = MODE === 'video' ? 0 : N - 1;
  const avg = (f) => series.reduce((s, row) => s + row.reduce((a, r) => a + f(r), 0) / row.length, 0) / series.length;
  const worstConceal = Math.max(...series.flatMap((row) => row.map((r) => r.concealPct)));
  const lastFreezes = last.reduce((s, r) => s + r.freezes, 0);
  const firstFreezes = series[0].reduce((s, r) => s + r.freezes, 0);
  const dropouts = series.filter((row) => row.some((r) => r.v < wantV || (wantA && r.a < wantA))).length;
  if (wantV) ok(dropouts === 0 && last.every((r) => r.v === wantV), `every receiver decoded video from all ${wantV} others in every sample (${series.length - dropouts}/${series.length} samples complete)`);
  if (wantA) ok(last.every((r) => r.a >= wantA), `every receiver received audio from all ${wantA} others`);
  if (wantV) ok(lastFreezes - firstFreezes === 0, `no video freezes over the run (${lastFreezes - firstFreezes})`);
  if (wantV) ok(Math.min(...series.slice(1).flatMap((row) => row.map((r) => r.minFps))) >= 14, `every tile kept a watchable frame rate (lowest ${Math.min(...series.slice(1).flatMap((row) => row.map((r) => r.minFps))).toFixed(0)} fps)`);
  if (wantA) ok(worstConceal < 1, `concealed audio stayed under 1% in every 10 s window (worst ${worstConceal.toFixed(2)}%)`);
  ok(errors.length === 0, `no page errors${errors.length ? ': ' + errors.slice(0, 2).join(' | ') : ''}`);
  const cpu = (k) => (procSeries.length ? procSeries.reduce((s, x) => s + x[k], 0) / procSeries.length : 0);
  const memMax = (k) => (procSeries.length ? Math.max(...procSeries.map((x) => x[k])) : 0);
  console.log(`  tiles as received (last sample): ${last.map((r, i) => 'ABCDE'[i] + '[' + r.res + ']').join(' ')}`);
  console.log(`    REPORT ${JSON.stringify({ mode: MODE, codec: CODEC || last[0].codec || 'n/a', secs: SECS, samples: series.length, avgDownKbps: Math.round(avg((r) => r.downKbps)), avgUpKbps: Math.round(avg((r) => r.upKbps)), freezes: lastFreezes - firstFreezes, worstConcealPct: +worstConceal.toFixed(2), serverCpuPct: +cpu('srvCpuPct').toFixed(1), serverMemMaxMB: Math.round(memMax('srvMemMB')), browsersCpuPct: Math.round(cpu('brCpuPct')), browsersMemMaxMB: Math.round(memMax('brMemMB')), machineCpuAvgPct: Math.round(cpu('sysPct')), machineCpuMaxPct: Math.round(memMax('sysPct')) })}`);
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
