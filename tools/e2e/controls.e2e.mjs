// The in-call controls, as a person uses them, on the other side's
// loudspeaker and on the screen:
//   - the audio panel opens from its button, lies inside the viewport at
//     desktop and phone widths, lists the microphone, and closes on Escape
//     and on an outside click;
//   - noise cancellation switched live (Strong -> Standard -> Strong): the
//     other side keeps hearing, exactly one microphone stays open;
//   - a muted person who switches stays muted; unmuting after is heard;
//   - re-picking the microphone mid-call: heard, one microphone open;
//   - the reaction picker is drawn on screen, not merely "open".
//
//   node tools/e2e/controls.e2e.mjs [http://127.0.0.2:8444] [--media ws|webrtc]
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


const OUT = process.env.SHOTS || join(here, '.shots');
const liveRaw = (p) => p.evaluate(() => window.__streams.flatMap((s) => s.getAudioTracks()).filter((t) => t.readyState === 'live').length);
const inView = (p, sel) => p.evaluate((sel) => {
  const el = document.querySelector(sel);
  if (!el) return { ok: false, why: 'missing' };
  const r = el.getBoundingClientRect();
  const cs = getComputedStyle(el);
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
  const hit = document.elementFromPoint(cx, cy);
  // Every corner too, 6 px in: a popover half under another element is not on screen.
  const corners = [[r.left + 6, r.top + 6], [r.right - 6, r.top + 6], [r.left + 6, r.bottom - 6], [r.right - 6, r.bottom - 6]];
  const covered = corners.filter(([x, y]) => { const h = document.elementFromPoint(x, y); return !h || !el.contains(h); }).length;
  return {
    ok: cs.display !== 'none' && r.width > 0 && r.top >= 0 && r.left >= 0 && r.bottom <= innerHeight + 0.5 && r.right <= innerWidth + 0.5 && !!hit && el.contains(hit) && covered === 0, covered,
    rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)], vw: innerWidth, vh: innerHeight, hit: hit?.id || String(hit?.className || hit?.tagName),
  };
}, sel);
const pick = (p, value) => p.evaluate((v) => {
  const r = document.querySelector(`input[name="ap-nc"][value="${v}"]`);
  r.checked = true;
  r.dispatchEvent(new Event('change', { bubbles: true }));
}, value);
const denoiseState = (p) => p.evaluate(() => document.documentElement.dataset.denoise);
const waitDenoise = async (p, want, ms = 6000) => {
  const t0 = Date.now();
  for (;;) {
    const d = await denoiseState(p);
    if (want.includes(d)) return d;
    if (Date.now() - t0 > ms) return d;
    await sleep(100);
  }
};
const isOpen = (p) => p.evaluate(() => document.getElementById('audio-menu').classList.contains('open'));

const room = 'e2e-ctl-' + Math.random().toString(36).slice(2, 8);
const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: ['--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
});
const log = [];
try {
  console.log(`== media asked: ${MEDIA}; room ${room}`);
  const pageA = await signedIn(browser, accounts.A, 'A', log);
  const pageB = await signedIn(browser, accounts.B, 'B', log);
  await joinRoom(pageA, room);
  await joinRoom(pageB, room);
  await sleep(MEDIA === 'webrtc' ? 5000 : 2500);
  ok((await heard(pageB, 8000)) >= 0, 'B hears A at the start');
  const d0 = await denoiseState(pageA);
  ok(d0 === 'simd' || d0 === 'on', `A starts with strong noise cancellation (${d0})`);
  ok((await liveRaw(pageA)) === 1, `one microphone open on A (${await liveRaw(pageA)})`);

  // The panel.
  await press(pageA, 'btn-audio');
  await sleep(300);
  let v = await inView(pageA, '#audio-pop');
  ok(v.ok, `desktop: the audio panel is on screen ${JSON.stringify(v)}`);
  const micOpts = await pageA.evaluate(() => document.querySelectorAll('#ap-mic option').length);
  ok(micOpts >= 1, `the panel lists ${micOpts} microphone(s)`);
  const checked = await pageA.evaluate(() => document.querySelector('input[name="ap-nc"]:checked')?.value);
  ok(checked === 'strong', `the panel shows Strong selected (${checked})`);
  await sleep(400);
  const meter = await pageA.evaluate(() => parseFloat(document.getElementById('ap-level').style.width) || 0);
  ok(meter > 0, `the level meter moves with the voice (${meter}%)`);
  await pageA.screenshot({ path: join(OUT, 'controls-audio-desktop.png') });

  // Strong -> Standard, live.
  await pick(pageA, 'standard');
  const d1 = await waitDenoise(pageA, ['off']);
  ok(d1 === 'off', `switched to Standard (${d1})`);
  await sleep(600);
  ok((await heard(pageB)) >= 0, 'B still hears A after the switch to Standard');
  ok((await liveRaw(pageA)) === 1, `still one microphone open (${await liveRaw(pageA)})`);
  // Standard -> Strong, live.
  await pick(pageA, 'strong');
  const d2 = await waitDenoise(pageA, ['simd', 'on']);
  ok(d2 === 'simd' || d2 === 'on', `switched back to Strong (${d2})`);
  await sleep(600);
  ok((await heard(pageB)) >= 0, 'B still hears A after the switch back to Strong');
  ok((await liveRaw(pageA)) === 1, `still one microphone open (${await liveRaw(pageA)})`);

  // Muted, then switch: stays muted.
  await pageA.keyboard.press('Escape');
  await sleep(200);
  ok(!(await isOpen(pageA)), 'Escape closes the panel');
  await press(pageA, 'btn-mic');
  let s = await silent(pageB, 2500, 800);
  ok(s.fell >= 0 && s.loud === 0, 'A mutes');
  await press(pageA, 'btn-audio');
  await sleep(200);
  await pick(pageA, 'standard');
  await waitDenoise(pageA, ['off']);
  let w = await pageB.evaluate((ms) => window.__loudWindows(ms, 0.01), 2500);
  ok(w.loud === 0, `muted A switches noise cancellation: B still hears nothing (${w.loud}/${w.windows} loud)`);
  await pageA.evaluate(() => document.getElementById('ap-mic').dispatchEvent(new Event('change', { bubbles: true })));
  await sleep(1500);
  w = await pageB.evaluate((ms) => window.__loudWindows(ms, 0.01), 2500);
  ok(w.loud === 0, `muted A re-picks the microphone: B still hears nothing (${w.loud}/${w.windows} loud)`);
  ok((await liveRaw(pageA)) === 1, `one microphone open after the re-pick (${await liveRaw(pageA)})`);
  await pageA.mouse.click(5, 5);
  await sleep(200);
  ok(!(await isOpen(pageA)), 'an outside click closes the panel');
  await press(pageA, 'btn-mic');
  ok((await heard(pageB)) >= 0, 'A unmutes after the switches: heard');
  await press(pageA, 'btn-audio');
  await sleep(200);
  await pick(pageA, 'strong');
  ok(['simd', 'on'].includes(await waitDenoise(pageA, ['simd', 'on'])), 'back to Strong for the rest');
  await pageA.keyboard.press('Escape');
  await sleep(600);
  ok((await heard(pageB)) >= 0, 'heard on Strong again');

  // The reaction picker, drawn on screen.
  await pageA.evaluate(() => document.querySelector('#btn-react > button')?.click());
  await sleep(300);
  v = await inView(pageA, '#btn-react .react-pop');
  ok(v.ok, `desktop: the reaction picker is on screen ${JSON.stringify(v)}`);
  await pageA.screenshot({ path: join(OUT, 'controls-react-desktop.png') });
  await pageA.keyboard.press('Escape');

  // Phone.
  // A mobile viewport reloads the page: back through the pre-join.
  await pageA.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  await sleep(1500);
  if (await pageA.evaluate(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none')) {
    await sleep(500);
    await press(pageA, 'pj-join');
  }
  ok(await until(pageA, visible('app'), 20000), 'phone: A is back in the meeting');
  await sleep(1500);
  await press(pageA, 'btn-audio');
  await sleep(300);
  v = await inView(pageA, '#audio-pop');
  ok(v.ok, `phone: the audio panel is on screen ${JSON.stringify(v)}`);
  const small = await pageA.evaluate(() => [...document.querySelectorAll('#audio-pop button, #audio-pop select, #audio-pop input')].filter((e) => e.offsetParent && e.getBoundingClientRect().height < 44).map((e) => e.id || e.tagName));
  ok(small.length === 0, `phone: every control in the panel is at least 44 px high (${JSON.stringify(small)})`);
  await pageA.screenshot({ path: join(OUT, 'controls-audio-phone.png') });
  await pageA.keyboard.press('Escape');
  await sleep(200);
  await pageA.evaluate(() => document.querySelector('#btn-react > button')?.click());
  await sleep(300);
  v = await inView(pageA, '#btn-react .react-pop');
  ok(v.ok, `phone: the reaction picker is on screen ${JSON.stringify(v)}`);
  await pageA.screenshot({ path: join(OUT, 'controls-react-phone.png') });
  const overflow = await pageA.evaluate(() => document.documentElement.scrollWidth - innerWidth);
  ok(overflow <= 0, `phone: no page-level horizontal overflow (${overflow}px)`);

  const bad = log.filter((l) => /PAGE ERROR/.test(l));
  ok(bad.length === 0, `no page errors${bad.length ? ': ' + bad.slice(0, 2).join(' | ') : ''}`);
  if (failures && log.length) console.log('    log:\n      ' + log.slice(0, 20).join('\n      '));
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
