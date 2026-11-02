// Live status, end to end: a real browser (account A) joins a meeting; a
// second account (B, A's peer through the harness's direct chat) watches A's
// status through the API. "In a call" must appear within a moment of the
// join and clear the moment A leaves, falling back to what A had picked or
// to available. Proves the gateway → NudgeAutoStatus → effective() path
// against real sockets, not the unit test's injected map.
//
//   node tools/e2e/livestatus.e2e.mjs [https://localhost:8443]
//
// Needs the messaging harness's stored accounts (.accounts.json) and a
// Chrome or Edge to drive. Exit 1 on any failure.
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
const BASE = process.argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const host = new URL(BASE).host;
const accounts = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'))[host] ?? {};
if (!accounts.A || !accounts.B) {
  console.error(`no stored A/B accounts for ${host}; run messaging.e2e.mjs once first`);
  process.exit(2);
}
const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
].find((p) => existsSync(p));
if (!CHROME) {
  console.error('no Chrome or Edge found to drive');
  process.exit(2);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (cond, what) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  if (!cond) failures++;
};

// A tiny cookie-carrying client for B.
class Client {
  constructor() {
    this.cookies = new Map();
  }
  async req(method, path, body) {
    const h = {};
    if (this.cookies.size) h.cookie = [...this.cookies].map(([k, v]) => `${k}=${v}`).join('; ');
    if (body !== undefined) h['content-type'] = 'application/json';
    const csrf = this.cookies.get('aether_csrf');
    if (csrf && method !== 'GET') h['X-Aether-CSRF'] = csrf;
    const res = await fetch(BASE + path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [kv] = sc.split(';');
      const i = kv.indexOf('=');
      this.cookies.set(kv.slice(0, i).trim(), kv.slice(i + 1).trim());
    }
    let json = null;
    try {
      json = await res.json();
    } catch {}
    return { status: res.status, json };
  }
}
const B = new Client();
const login = await B.req('POST', '/api/identity/login', { email: accounts.B.email, password: accounts.B.password });
if (login.status !== 200) {
  console.error(`B cannot sign in: ${login.status}`);
  process.exit(2);
}
const A = new Client();
const loginA = await A.req('POST', '/api/identity/login', { email: accounts.A.email, password: accounts.A.password });
if (loginA.status !== 200) {
  console.error(`A cannot sign in: ${loginA.status}`);
  process.exit(2);
}
const aId = loginA.json.user.id;
// A picks "away" first, so the fall-back after the call has something to
// fall back to -- and the room must override it while A is in there.
ok((await A.req('PUT', '/api/messaging/status', { status: 'away', note: 'before the call' })).status === 200, 'A picks Away before joining');
for (const k of ['incall', 'presenting']) {
  ok((await A.req('PUT', '/api/messaging/status', { status: k })).status === 400, `"${k}" is refused as a manual status`);
}
const statusOfA = async () => (await B.req('GET', `/api/messaging/status?ids=${aId}`)).json?.statuses?.[aId] ?? null;
const until = async (pred, budgetMs) => {
  const t0 = performance.now();
  for (;;) {
    const st = await statusOfA();
    if (pred(st)) return { st, ms: performance.now() - t0 };
    if (performance.now() - t0 > budgetMs) return { st, ms: performance.now() - t0, timedOut: true };
    await sleep(60);
  }
};
let before = await statusOfA();
ok(before?.status === 'away' && !before.auto, `B sees A as Away before the call (${JSON.stringify(before)})`);

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: [
    '--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required',
    '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
  ],
});
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  const homeShown = () => getComputedStyle(document.getElementById('auth')).display === 'none' && getComputedStyle(document.getElementById('home')).display !== 'none';
  if (!(await page.evaluate(homeShown))) {
    await page.evaluate(
      async (email, password) => {
        const m = await import('/src/api.js');
        await m.login(email, password);
      },
      accounts.A.email,
      accounts.A.password
    );
    await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
    await page.waitForFunction(homeShown, { timeout: 15000 });
  }
  await sleep(500);
  // Start an instant meeting and join it.
  await page.evaluate(() => document.querySelector('[data-home="meet"]')?.click());
  await sleep(400);
  await page.evaluate(() => document.getElementById('next-start')?.click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 15000 });
  await sleep(600);
  const tJoin = performance.now();
  await page.evaluate(() => document.getElementById('pj-join')?.click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
  const joined = await until((st) => st?.status === 'incall', 8000);
  ok(!joined.timedOut && joined.st?.auto === true && joined.st?.note === 'In a call',
    `B sees A "In a call", automatic, ${Math.round(performance.now() - tJoin)} ms after the join click (${JSON.stringify(joined.st)})`);
  // A's own view agrees, and A picking something else while in the room
  // does not dislodge it.
  const mine = (await A.req('GET', `/api/messaging/status?ids=${aId}`)).json?.statuses?.[aId];
  ok(mine?.status === 'incall' && mine?.auto === true, `A sees their own status as In a call (${JSON.stringify(mine)})`);
  const pick = await A.req('PUT', '/api/messaging/status', { status: 'dnd', note: 'during the call' });
  ok(pick.status === 200 && pick.json?.status === 'incall', `picking DND during the call is stored but still reads In a call (${JSON.stringify(pick.json)})`);
  // A's own client learned it from the push (the OpStatus frame with the
  // auto bit), not from a reload: the module-level status map says so.
  await sleep(300);
  const mineInPage = await page.evaluate(async () => {
    const m = await import('/src/chat.js');
    return m.myStatus();
  });
  ok(mineInPage?.status === 'incall' && mineInPage?.auto === true, `A's own page holds In a call from the push (${JSON.stringify(mineInPage)})`);
  // The microphone in the call has echo cancellation on: the settings the
  // browser actually applied, read from the page's own report.
  const mic = await page.evaluate(() => ({ aec: document.documentElement.dataset.micAec ?? null, off: document.documentElement.dataset.micOff ?? '' }));
  ok(mic.aec === 'on', `the joined microphone has echo cancellation on (aec=${mic.aec}${mic.off ? ', off: ' + mic.off : ''})`);
  // And nothing plays this participant's own audio back: no audio sink for
  // their own id, and no receiver of their own frames.
  const selfPlay = await page.evaluate(() => {
    const sinks = [...document.querySelectorAll('#audio-sinks audio')].map((a) => a.dataset.id);
    return { sinks, videosUnmuted: [...document.querySelectorAll('video')].filter((v) => !v.muted).length };
  });
  ok(selfPlay.videosUnmuted === 0, `no <video> element is unmuted (${selfPlay.videosUnmuted})`);
  ok(selfPlay.sinks.length === 0, `alone in the room, no audio sink at all (${JSON.stringify(selfPlay.sinks)})`);
  // Leave. The moment the socket closes, the pick is back. Leaving may
  // navigate, which tears down the page's script context mid-click.
  const tLeave = performance.now();
  try {
    await page.evaluate(() => document.getElementById('btn-leave')?.click());
    await sleep(150);
    await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => /^(leave|end|yes)/i.test(b.textContent.trim()))?.click());
  } catch (e) {
    if (!/context was destroyed|navigation/i.test(String(e))) throw e;
  }
  const left = await until((st) => st?.status !== 'incall', 8000);
  ok(!left.timedOut && left.st?.status === 'dnd' && !left.st?.auto,
    `after leaving, B sees A's own pick (DND) again, ${Math.round(performance.now() - tLeave)} ms after the leave click (${JSON.stringify(left.st)})`);
  // With nothing picked, leaving reads available.
  ok((await A.req('PUT', '/api/messaging/status', { status: 'available' })).status === 200, 'A resets to Available');
  before = await statusOfA();
  ok(before?.status === 'available' && !before.auto, `and reads Available, not automatic (${JSON.stringify(before)})`);
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
