// Mute, in every way a person reaches it, measured on the OTHER side's
// loudspeaker -- not on the button's colour. Two real browsers, a synthesised
// continuous tone as each microphone, and for each scenario: the other
// side's sink must fall silent within a moment of the press and STAY silent
// (no window above the floor over three seconds), and come back on unmute.
//
// Scenarios:
//   1. A mutes; A unmutes.                       (the basic press)
//   2. Both mute; nothing flows either way; A unmutes; B still silent on A.
//   3. Camera off and on while muted stays muted. (a re-acquired camera
//                                                 must not restart audio)
//   4. Muted A stays muted for a late joiner.     (B leaves and rejoins)
//   5. Mic OFF in the pre-join, then ON inside.   (a microphone that was never
//                                                 opened must open on unmute)
//   6. Ten rapid presses ending unmuted: heard; ending muted: silent.
//   7. The camera hangs (getUserMedia never returns); mute must still work.
//                                                 (mute used to queue behind
//                                                 camera work and wait too)
//
//   node tools/e2e/mute.e2e.mjs [https://localhost:8443] [--media ws|webrtc] [--ring]
//
// --ring drives the one-to-one call instead of a room link: A rings B from
// the chat header, B accepts, both handle the pre-join.
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const BASE = argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const arg = (name, dflt) => {
  const i = argv.indexOf(name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : dflt;
};
const MEDIA = arg('--media', 'ws');
const RING = argv.includes('--ring');
const host = new URL(BASE).host;
let accounts = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'))[host] ?? {};
if (!accounts.A || !accounts.B) {
  const mk = async (label) => {
    const email = `e2e-${label.toLowerCase()}-${Math.random().toString(36).slice(2, 10)}@example.test`;
    const password = 'e2e-' + Math.random().toString(36).slice(2, 14);
    const r = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email, displayName: `E2E ${label}`, password }) });
    if (r.status !== 201) throw new Error(`signup ${label}: ${r.status}`);
    return { email, password };
  };
  accounts = { A: await mk('A'), B: await mk('B') };
  console.log(`(made throwaway accounts on ${host})`);
  // Kept for the next run: signups are rate-limited, and a run that makes two
  // new accounts each time hits the limit on the third run.
  try {
    const all = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'));
    all[host] = accounts;
    writeFileSync(join(here, '.accounts.json'), JSON.stringify(all, null, 2));
  } catch {}
}
const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
].find((p) => existsSync(p));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (cond, what) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  if (!cond) failures++;
};

// The microphone is a continuous tone (see audio.e2e.mjs for why not the
// fake device); the sink is read through an analyser.
const INSTRUMENT = `
  (() => {
    const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    window.__gumAudio = 0;
    window.__hangVideo = false;
    window.__streams = [];
    navigator.mediaDevices.getUserMedia = async (c) => {
      // Scenario 7: a camera that never answers, as a device held by another
      // program or a permission prompt nobody sees behaves.
      if (c && c.video && !c.audio && window.__hangVideo) await new Promise(() => {});
      const st = await gum(c);
      if (c && c.audio) {
        window.__gumAudio++;
        if (!window.__tone) {
          const ctx = new AudioContext({ sampleRate: 48000 });
          const osc = ctx.createOscillator();
          osc.frequency.value = 440;
          const gain = ctx.createGain();
          gain.gain.value = 0.5;
          const dest = ctx.createMediaStreamDestination();
          dest.channelCount = 1;
          dest.channelCountMode = 'explicit';
          osc.connect(gain).connect(dest);
          osc.start();
          ctx.resume().catch(() => {});
          window.__tone = { ctx, osc, gain, dest };
        }
        for (const t of st.getAudioTracks()) { t.stop(); st.removeTrack(t); }
        // A fresh track per acquisition, as a device gives: a clone of the tone.
        const tone = window.__tone.dest.stream.getAudioTracks()[0].clone();
        const real = tone.getSettings.bind(tone);
        tone.getSettings = () => ({ ...real(), echoCancellation: true, noiseSuppression: true, autoGainControl: true, sampleRate: 48000, channelCount: 1 });
        st.addTrack(tone);
      }
      window.__streams.push(st);
      return st;
    };
    window.__diag = () => ({
      sinks: document.querySelectorAll('#audio-sinks audio').length,
      sinkLevel: window.__sinkLevel(),
      tracks: window.__streams.flatMap((s) => s.getAudioTracks().map((t) => t.readyState + '/' + (t.enabled ? 'on' : 'off'))),
      denoise: document.documentElement.dataset.denoise ?? 'unset',
      micAec: document.documentElement.dataset.micAec ?? 'unset',
      toast: document.querySelector('#toast.show')?.textContent ?? '',
    });
    window.__sinkLevel = () => {
      const a = document.querySelector('#audio-sinks audio');
      if (!a?.srcObject) return null;
      if (!window.__an || window.__anFor !== a.srcObject) {
        window.__ac?.close?.();
        window.__ac = new AudioContext();
        const src = window.__ac.createMediaStreamSource(a.srcObject);
        window.__an = window.__ac.createAnalyser();
        window.__an.fftSize = 1024;
        src.connect(window.__an);
        window.__anFor = a.srcObject;
        window.__buf = new Float32Array(1024);
      }
      window.__an.getFloatTimeDomainData(window.__buf);
      let s = 0;
      for (const v of window.__buf) s += v * v;
      return Math.sqrt(s / window.__buf.length);
    };
    window.__waitLevel = (above, thr, timeoutMs) => new Promise((res) => {
      const t0 = performance.now();
      const tick = () => {
        const l = window.__sinkLevel();
        if (l != null && (above ? l > thr : l < thr)) return res(performance.now() - t0);
        if (performance.now() - t0 > timeoutMs) return res(-1);
        setTimeout(tick, 5);
      };
      tick();
    });
    // Over ms, how many 50 ms windows carried sound above thr.
    window.__loudWindows = async (ms, thr) => {
      let n = 0, w = 0;
      const at = [];
      const t0 = performance.now();
      while (performance.now() - t0 < ms) {
        const l = window.__sinkLevel();
        if (l != null) { w++; if (l > thr) { n++; at.push(Math.round(performance.now() - t0)); } }
        await new Promise((r) => setTimeout(r, 50));
      }
      return { loud: n, windows: w, at };
    };
  })();
`;

async function signedIn(browser, acct, label, log) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(INSTRUMENT);
  page.on('console', (m) => {
    const t = m.text();
    if (/media:|wsmedia|audio playback|microphone|ice|noise|denoise|operation failed|rnnoise|wasm/i.test(t)) log.push(`${label}: ${t}`);
  });
  page.on('pageerror', (e) => log.push(`${label} PAGE ERROR: ${e}`));
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  const homeShown = () => getComputedStyle(document.getElementById('auth')).display === 'none' && getComputedStyle(document.getElementById('home')).display !== 'none';
  if (!(await page.evaluate(homeShown))) {
    await page.evaluate(
      async (email, password) => {
        const m = await import('/src/api.js');
        await m.login(email, password);
      },
      acct.email,
      acct.password
    );
    await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
    await page.waitForFunction(homeShown, { timeout: 15000 });
  }
  return page;
}
const visible = (id) => `getComputedStyle(document.getElementById('${id}')).display !== 'none'`;
const until = async (page, expr, budgetMs) => {
  const t0 = performance.now();
  for (;;) {
    let v = null;
    try {
      v = await page.evaluate(expr);
    } catch {}
    if (v) return true;
    if (performance.now() - t0 > budgetMs) return false;
    await sleep(80);
  }
};

async function joinRoom(page, room, { micOff = false } = {}) {
  const q = new URLSearchParams({ room, audio: '1' });
  q.set('media', MEDIA); // explicit either way: the app's default is WebRTC
  await page.goto(`${BASE}/?${q}`, { waitUntil: 'networkidle2', timeout: 30000 });
  await page.waitForFunction(visible('prejoin'), { timeout: 20000 });
  await sleep(600);
  if (micOff) {
    await page.evaluate(() => document.getElementById('pj-mic')?.click());
    await sleep(500);
  }
  await page.evaluate(() => document.getElementById('pj-join')?.click());
  await page.waitForFunction(visible('app'), { timeout: 20000 });
}

// The receiving side hears the sender: level above thr within budget.
const heard = (page, budget = 4000) => page.evaluate((b) => window.__waitLevel(true, 0.02, b), budget);
// The receiving side falls silent within budget, then stays silent for holdMs.
// Silence is judged from the press, not from the first quiet moment: a
// quiet moment can be a gap between chunks of audio that left before the
// press and are still in the pipe (capture, encode, the wire, the jitter
// buffer: ~250 ms on the socket path). So: from the press, the level must
// fall within budget, and after SETTLE ms from the press nothing may be
// heard for holdMs. Anything loud after SETTLE was sent after the mute --
// a leak -- and is reported with its time.
const SETTLE = 450;
async function silent(page, budget = 2500, holdMs = 3000) {
  const t0 = Date.now();
  const fell = await page.evaluate((b) => window.__waitLevel(false, 0.004, b), budget);
  if (fell < 0) return { fell, loud: -1, windows: 0 };
  const wait = SETTLE - (Date.now() - t0);
  if (wait > 0) await sleep(wait);
  const w = await page.evaluate((ms) => window.__loudWindows(ms, 0.01), holdMs);
  return { fell, ...w };
}
const press = (page, id) => page.evaluate((i) => document.getElementById(i)?.click(), id);
const micState = (page) => page.evaluate(() => ({ off: document.getElementById('btn-mic')?.classList.contains('off') ?? null, enabled: window.__streams?.at(-1)?.getAudioTracks?.()[0]?.enabled ?? null }));

const room = 'e2e-mute-' + Math.random().toString(36).slice(2, 8);
const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: ['--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
});
const log = [];
try {
  console.log(`== media asked: ${MEDIA}; ${RING ? 'one-to-one call by ring' : 'room ' + room}`);
  const pageA = await signedIn(browser, accounts.A, 'A', log);
  const pageB = await signedIn(browser, accounts.B, 'B', log);
  if (RING) {
    const lb = await (await fetch(BASE + '/api/identity/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: accounts.B.email, password: accounts.B.password }) })).json();
    const bName = lb.user.displayName;
    await pageA.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
    await sleep(700);
    await pageA.evaluate((name) => {
      const rows = [...document.querySelectorAll('.conv-row .conv')];
      rows.find((x) => x.textContent.replace(/\s+/g, ' ').includes(name) && !x.textContent.includes(' and '))?.click();
    }, bName);
    await sleep(800);
    const rang = await pageA.evaluate(() => {
      const b = document.querySelector('.thread-head [data-call][data-call-audio="1"], .thread-head [data-call]');
      b?.click();
      return !!b;
    });
    ok(rang, 'A rings B from the chat header');
    ok(await until(pageB, `!!document.querySelector('#dialog [data-accept]')`, 8000), 'B sees the incoming call');
    await pageB.evaluate(() => document.querySelector('#dialog [data-accept]')?.click());
    for (const p of [pageA, pageB]) {
      const t0 = performance.now();
      for (;;) {
        const where = await p.evaluate(() => (getComputedStyle(document.getElementById('prejoin')).display !== 'none' ? 'prejoin' : getComputedStyle(document.getElementById('app')).display !== 'none' ? 'app' : ''));
        if (where === 'app') break;
        if (where === 'prejoin') {
          await sleep(400);
          await press(p, 'pj-join');
        }
        if (performance.now() - t0 > 20000) break;
        await sleep(150);
      }
    }
    ok(await until(pageA, visible('app'), 5000) && (await until(pageB, visible('app'), 5000)), 'both are in the call');
  } else {
    await joinRoom(pageA, room);
    await joinRoom(pageB, room);
    ok(true, 'both in the room');
  }
  await sleep(MEDIA === 'webrtc' ? 5000 : 2500);
  const hb = await heard(pageB, 8000);
  const ha = await heard(pageA, 8000);
  ok(hb >= 0, 'B hears A at the start');
  ok(ha >= 0, 'A hears B at the start');
  if (hb < 0 || ha < 0) {
    console.log('    diag A ' + JSON.stringify(await pageA.evaluate(() => window.__diag())));
    console.log('    diag B ' + JSON.stringify(await pageB.evaluate(() => window.__diag())));
  }
  console.log(`    noise suppression on the pages: A ${await pageA.evaluate(() => document.documentElement.dataset.denoise ?? 'unset')}, B ${await pageB.evaluate(() => document.documentElement.dataset.denoise ?? 'unset')}`);

  // 1. The basic press.
  await press(pageA, 'btn-mic');
  let s = await silent(pageB);
  ok(s.fell >= 0 && s.loud === 0, `1. A mutes: B silent after ${s.fell.toFixed(0)} ms and stays silent (${s.loud} loud of ${s.windows} windows) ${JSON.stringify(await micState(pageA))}`);
  await press(pageA, 'btn-mic');
  let h = await heard(pageB);
  ok(h >= 0, `1. A unmutes: B hears again after ${h.toFixed(0)} ms`);

  // 2. Both muted.
  await press(pageA, 'btn-mic');
  await press(pageB, 'btn-mic');
  const sB = await silent(pageB);
  const sA = await silent(pageA, 2500, 1500);
  ok(sB.fell >= 0 && sB.loud === 0 && sA.fell >= 0 && sA.loud === 0, `2. both mute: B silent (${sB.loud}/${sB.windows} loud), A silent (${sA.loud}/${sA.windows} loud)`);
  await press(pageA, 'btn-mic');
  h = await heard(pageB);
  const stillA = await pageA.evaluate((ms) => window.__loudWindows(ms, 0.01), 1500);
  ok(h >= 0 && stillA.loud === 0, `2. A unmutes: B hears (${h.toFixed(0)} ms); A still hears nothing from muted B (${stillA.loud}/${stillA.windows} loud)`);
  await press(pageB, 'btn-mic');
  ok((await heard(pageA)) >= 0, '2. B unmutes: A hears B again');

  // 3. Camera toggled while muted.
  await press(pageA, 'btn-mic');
  s = await silent(pageB, 2500, 500);
  ok(s.fell >= 0, '3. A mutes');
  await press(pageA, 'btn-cam');
  await sleep(900);
  await press(pageA, 'btn-cam');
  await sleep(1500);
  let w = await pageB.evaluate((ms) => window.__loudWindows(ms, 0.01), 3000);
  ok(w.loud === 0, `3. camera off and on while muted: B still silent (${w.loud}/${w.windows} loud)`);
  await press(pageA, 'btn-mic');
  ok((await heard(pageB)) >= 0, '3. A unmutes after the camera toggles: heard');

  // 4. Late joiner meets a muted A.
  if (!RING) {
    await press(pageA, 'btn-mic');
    s = await silent(pageB, 2500, 500);
    await joinRoom(pageB, room);
    await sleep(MEDIA === 'webrtc' ? 5000 : 3000);
    w = await pageB.evaluate((ms) => window.__loudWindows(ms, 0.01), 3000);
    ok(w.loud === 0, `4. B rejoins while A is muted: B hears nothing (${w.loud}/${w.windows} loud)`);
    await press(pageA, 'btn-mic');
    ok((await heard(pageB, 6000)) >= 0, '4. A unmutes: the rejoined B hears A');
  }

  // 5. Mic off in the pre-join, on inside.
  if (!RING) {
    await joinRoom(pageA, room, { micOff: true });
    await sleep(MEDIA === 'webrtc' ? 5000 : 3000);
    w = await pageB.evaluate((ms) => window.__loudWindows(ms, 0.01), 2000);
    ok(w.loud === 0, `5. A joined with the mic off in the pre-join: B hears nothing (${w.loud}/${w.windows} loud)`);
    const before = await pageA.evaluate(() => window.__gumAudio);
    await press(pageA, 'btn-mic');
    h = await heard(pageB, 6000);
    const after = await pageA.evaluate(() => window.__gumAudio);
    ok(h >= 0, `5. A unmutes inside: B hears A after ${h.toFixed(0)} ms (microphone opened ${after - before} time(s) on unmute) ${JSON.stringify(await micState(pageA))}`);
    await press(pageA, 'btn-mic');
    s = await silent(pageB);
    ok(s.fell >= 0 && s.loud === 0, `5. and mutes again: B silent (${s.loud}/${s.windows} loud)`);
    await press(pageA, 'btn-mic');
    ok((await heard(pageB)) >= 0, '5. and is heard again');
  }

  // 6. Rapid presses.
  for (let i = 0; i < 10; i++) {
    await press(pageA, 'btn-mic');
    await sleep(70);
  }
  await sleep(600);
  ok((await heard(pageB)) >= 0, '6. ten rapid presses ending unmuted: A is heard');
  for (let i = 0; i < 9; i++) {
    await press(pageA, 'btn-mic');
    await sleep(70);
  }
  s = await silent(pageB);
  ok(s.fell >= 0 && s.loud === 0, `6. nine rapid presses ending muted: B silent (${s.loud}/${s.windows} loud${s.at?.length ? ' at ' + s.at.join(',') + ' ms' : ''})`);
  await press(pageA, 'btn-mic');
  ok((await heard(pageB)) >= 0, '6. and heard once more');

  // 7. The camera hangs; the mute must not wait for it.
  await press(pageA, 'btn-cam'); // off (fast)
  await sleep(600);
  await pageA.evaluate(() => { window.__hangVideo = true; });
  await press(pageA, 'btn-cam'); // on: hangs in getUserMedia for good
  await sleep(400);
  await press(pageA, 'btn-mic');
  s = await silent(pageB);
  ok(s.fell >= 0 && s.loud === 0, `7. A mutes while the camera acquisition hangs: B silent after ${s.fell >= 0 ? s.fell.toFixed(0) : 'never'} ms (${s.loud}/${s.windows} loud)`);
  await press(pageA, 'btn-mic');
  ok((await heard(pageB)) >= 0, '7. and unmutes: heard, the camera still hung');

  for (const p of [pageA, pageB]) {
    try {
      await p.evaluate(() => document.getElementById('btn-leave')?.click());
      await sleep(150);
      await p.evaluate(() => [...document.querySelectorAll('button')].find((b) => /^(leave|end|yes)/i.test(b.textContent.trim()))?.click());
    } catch {}
  }
  const bad = log.filter((l) => /PAGE ERROR/.test(l));
  ok(bad.length === 0, `no page errors${bad.length ? ': ' + bad.slice(0, 2).join(' | ') : ''}`);
  if (failures && log.length) console.log('    media log:\n      ' + log.slice(0, 30).join('\n      '));
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
