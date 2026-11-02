// Shared pieces of the meeting harnesses: accounts, signing in, joining,
// and driving the People panel's menus the way a person does.
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
export const BASE = process.argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let failures = 0;
export const ok = (cond, what) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  if (!cond) failures++;
};
export const failed = () => failures;
export const fail = (e) => { console.log('FAIL  threw: ' + (e?.stack ?? e)); failures++; };

// Throwaway accounts named "E2E <label>", kept per host in .accounts.json:
// signups are rate-limited, so a run reuses what an earlier run made.
export async function accounts(labels) {
  const store = join(here, '.accounts.json');
  const all = existsSync(store) ? JSON.parse(readFileSync(store, 'utf8')) : {};
  const host = new URL(BASE).host;
  const mine = all[host] ?? {};
  for (const label of labels) {
    if (mine[label]) continue;
    const email = `e2e-${label.toLowerCase()}-${Math.random().toString(36).slice(2, 10)}@example.test`;
    const password = 'e2e-' + Math.random().toString(36).slice(2, 14);
    const r = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email, displayName: `E2E ${label}`, password }) });
    if (r.status !== 201) throw new Error(`signup ${label}: ${r.status}`);
    mine[label] = { email, password };
    // Kept at once: signup is limited to three an hour, and an account made
    // but not written down is one the next run cannot sign in to.
    all[host] = mine;
    writeFileSync(store, JSON.stringify(all, null, 2));
  }
  all[host] = mine;
  writeFileSync(store, JSON.stringify(all, null, 2));
  return mine;
}

export async function launch() {
  const CHROME = [
    'C:/Program Files/Google/Chrome/Application/chrome.exe',
    'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  ].find((p) => existsSync(p));
  return puppeteer.launch({
    executablePath: CHROME,
    headless: true,
    // AETHER_E2E_GPU=1 keeps the GPU on, to measure what a real machine pays
    // (video effects); by default it is off, as in CI.
    args: ['--ignore-certificate-errors', '--no-first-run', ...(process.env.AETHER_E2E_GPU ? ['--enable-gpu', '--use-angle=d3d11', '--ignore-gpu-blocklist'] : ['--disable-gpu']), '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
  });
}

export const until = async (page, fn, arg, budget = 8000) => {
  const t0 = Date.now();
  for (;;) {
    try { if (await page.evaluate(fn, arg)) return true; } catch {}
    if (Date.now() - t0 > budget) return false;
    await sleep(100);
  }
};

// Every getUserMedia stream, so "the camera light is off" can be checked.
const INSTRUMENT = `(() => {
  const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  window.__streams = [];
  navigator.mediaDevices.getUserMedia = async (c) => { const s = await gum(c); window.__streams.push(s); return s; };
  window.__liveTracks = () => window.__streams.flatMap((s) => s.getTracks()).filter((t) => t.readyState === 'live').length;
})();`;

export async function signedIn(browser, acct, label, log, viewport = { width: 1280, height: 800 }) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(INSTRUMENT);
  page.on('pageerror', (e) => log.push(`${label} PAGE ERROR: ${e}`));
  await page.setViewport(viewport);
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  const home = () => getComputedStyle(document.getElementById('auth')).display === 'none' && getComputedStyle(document.getElementById('home')).display !== 'none';
  if (!(await page.evaluate(home))) {
    await page.evaluate(async (e, p) => (await import('/src/api.js')).login(e, p), acct.email, acct.password);
    await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
    await page.waitForFunction(home, { timeout: 15000 });
  }
  return page;
}

const shown = (id) => getComputedStyle(document.getElementById(id)).display !== 'none' && !document.getElementById(id).classList.contains('hidden');
export async function enter(page, room, media = 'ws') {
  await page.goto(`${BASE}/?room=${room}&media=${media}`, { waitUntil: 'networkidle2', timeout: 30000 });
  await page.waitForFunction(shown, { timeout: 20000 }, 'prejoin');
  await sleep(500);
  await page.evaluate(() => document.getElementById('pj-join').click());
  await page.waitForFunction(shown, { timeout: 20000 }, 'app');
}

// Opens the People panel (a toggle: only if it is not already showing).
export const openPeople = (page) => page.evaluate(() => {
  const shell = document.getElementById('shell');
  const open = !shell.classList.contains('panel-closed') && document.getElementById('view-people').classList.contains('active');
  if (!open) document.getElementById('rail-people').click();
});

// The labels in a person's "..." menu in the People panel, or null if no row
// with that name. Leaves the menu closed.
export const personMenu = (page, name) => page.evaluate(async (name) => {
  const row = [...document.querySelectorAll('#view-people .person')].find((r) => r.querySelector('.person-name')?.textContent.includes(name));
  if (!row) return null;
  row.querySelector('.person-more').click();
  await new Promise((r) => setTimeout(r, 120));
  const labels = [...document.querySelectorAll('#tile-menu button')].map((b) => b.textContent.trim());
  document.getElementById('tile-menu')?.remove();
  return labels;
}, name);

// Picks `label` from the "..." menu of the row named `name`. True if found.
export const pickPerson = (page, name, label) => page.evaluate(async (name, label) => {
  const row = [...document.querySelectorAll('#view-people .person')].find((r) => r.querySelector('.person-name')?.textContent.includes(name));
  if (!row) return false;
  row.querySelector('.person-more').click();
  await new Promise((r) => setTimeout(r, 120));
  const b = [...document.querySelectorAll('#tile-menu button')].find((x) => x.textContent.trim() === label);
  if (!b) { document.getElementById('tile-menu')?.remove(); return false; }
  b.click();
  return true;
}, name, label);

// Picks `label` from the meeting "..." beside "Mute all".
export const pickMeeting = (page, label) => page.evaluate(async (label) => {
  const more = document.getElementById('pp-meeting-more');
  if (!more) return false;
  more.click();
  await new Promise((r) => setTimeout(r, 120));
  const b = [...document.querySelectorAll('#tile-menu button')].find((x) => x.textContent.trim() === label);
  if (!b) { document.getElementById('tile-menu')?.remove(); return false; }
  b.click();
  return true;
}, label);

export const confirmYes = async (page, title) => {
  const seen = await until(page, (t) => document.getElementById('takeover-title')?.textContent === t, title, 4000);
  ok(seen, `confirmation "${title}" shown`);
  await page.evaluate(() => document.querySelector('#dialog [data-yes]')?.click());
};

export const toastSeen = (page, text) => until(page, (t) => document.getElementById('toast')?.textContent.includes(t), text, 6000);
export const chip = (page) => page.evaluate(() => ({ who: document.getElementById('chip-who').textContent, role: document.getElementById('chip-role').textContent }));
