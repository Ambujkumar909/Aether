// Live behaviour between two real browsers, the things a person notices in
// the first minute: a message lands in the open thread without switching
// chats; the sender's tick turns into "seen" when the other side reads; my
// own status glyph in the corner follows my pick; the peer's state line in
// the header follows theirs. Each check is timed. Page errors fail the run.
//
//   node tools/e2e/live.e2e.mjs [https://localhost:8443]
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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (cond, what) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  if (!cond) failures++;
};

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
const A = new Client();
const B = new Client();
const la = await A.req('POST', '/api/identity/login', { email: accounts.A.email, password: accounts.A.password });
const lb = await B.req('POST', '/api/identity/login', { email: accounts.B.email, password: accounts.B.password });
if (la.status !== 200 || lb.status !== 200) {
  console.error('sign-in failed', la.status, lb.status);
  process.exit(2);
}
const aId = la.json.user.id;
const bId = lb.json.user.id;
const conv = (await A.req('POST', '/api/messaging/direct', { email: accounts.B.email })).json;
await A.req('PUT', '/api/messaging/status', { status: 'available' });
await B.req('PUT', '/api/messaging/status', { status: 'available' });

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: ['--ignore-certificate-errors', '--no-first-run', '--disable-gpu'],
});
const errors = { A: [], B: [] };
async function signedInPage(ctx, acct, label) {
  const page = await ctx.newPage();
  // Errors count from the signed-in home onwards: the session probe before
  // sign-in answers 401 by design.
  let counting = false;
  page.on('pageerror', (e) => counting && errors[label].push(String(e)));
  page.on('console', (m) => {
    if (counting && m.type() === 'error') errors[label].push(m.text());
  });
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
  counting = true;
  await page.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
  await sleep(800);
  return page;
}
const openDirectWith = async (page, otherName) => {
  const opened = await page.evaluate((name) => {
    const rows = [...document.querySelectorAll('.conv-row .conv')];
    const r = rows.find((x) => {
      const t = x.textContent.replace(/\s+/g, ' ').trim();
      return t.includes(name) && !t.includes(' and ') && !t.includes('(You)');
    });
    r?.click();
    return !!r;
  }, otherName);
  await sleep(900);
  return opened;
};
const until = async (page, fn, arg, budgetMs) => {
  const t0 = performance.now();
  for (;;) {
    const v = await page.evaluate(fn, arg);
    if (v) return { v, ms: Math.round(performance.now() - t0) };
    if (performance.now() - t0 > budgetMs) return { v: null, ms: Math.round(performance.now() - t0), timedOut: true };
    await sleep(80);
  }
};

try {
  const ctxA = await browser.createBrowserContext();
  const ctxB = await browser.createBrowserContext();
  const pageA = await signedInPage(ctxA, accounts.A, 'A');
  const pageB = await signedInPage(ctxB, accounts.B, 'B');
  ok(await openDirectWith(pageA, 'E2E B'), 'A opens the chat with B');
  ok(await openDirectWith(pageB, 'E2E A'), 'B opens the chat with A');
  await pageA.bringToFront();

  // 1. A message from B lands in A's open thread, no switching.
  const body = 'live-' + Math.random().toString(36).slice(2, 8);
  const sent = await B.req('POST', `/api/messaging/conversations/${conv.id}/messages`, { body });
  ok(sent.status === 201, 'B sends through the API');
  const landed = await until(pageA, (b) => [...document.querySelectorAll('#thread .bubble')].some((el) => el.textContent.includes(b)), body, 5000);
  ok(!landed.timedOut, `the message is in A's open thread ${landed.ms} ms after the send (no chat switch)`);

  // 2. A sends; B is looking at the thread, so A's tick becomes "seen".
  // A sends from the page, as a person does: the composer adds the message
  // with its receipt. (A send through the API with no device id is not
  // pushed back to A's tabs -- the older contract, kept for such clients.)
  const mine = 'mine-' + Math.random().toString(36).slice(2, 8);
  await pageA.focus('#c-input');
  await pageA.keyboard.type(mine);
  await pageA.keyboard.press('Enter');
  ok(true, 'A sends from the composer');
  const drawn = await until(pageA, (b) => [...document.querySelectorAll('#thread .msg')].find((el) => el.textContent.includes(b))?.querySelector('.receipt')?.className ?? null, mine, 5000);
  ok(!drawn.timedOut, `A's own message is drawn with a receipt (${drawn.v})`);
  if (drawn.timedOut) {
    const html = await pageA.evaluate((b) => [...document.querySelectorAll('#thread .msg')].find((el) => el.textContent.includes(b))?.outerHTML.slice(0, 600) ?? '(no .msg)', mine);
    console.log('    DEBUG own message markup:', html);
    const members = (await A.req('GET', '/api/messaging/conversations')).json.conversations?.find((c) => c.id === conv.id)?.members;
    console.log('    DEBUG members as A lists them:', JSON.stringify(members));
  }
  await pageB.bringToFront();
  const inB = await until(pageB, (b) => [...document.querySelectorAll('#thread .bubble')].some((el) => el.textContent.includes(b)), mine, 5000);
  ok(!inB.timedOut, `B sees it in the open thread ${inB.ms} ms later`);
  await pageA.bringToFront();
  const seen = await until(
    pageA,
    (b) => {
      const el = [...document.querySelectorAll('#thread .msg')].find((x) => x.textContent.includes(b));
      const r = el?.querySelector('.receipt');
      return r && /seen|all/.test(r.className) ? r.className : null;
    },
    mine,
    8000
  );
  ok(!seen.timedOut, `A's tick turned to seen ${seen.ms} ms after B had it on screen (${seen.v})`);

  // 2b. The report, exactly: B's window is behind another. A sends. A's
  // tick must stay single (B has not seen it) -- and turn double the moment
  // B's window comes to the front, with no chat switch.
  // Headless Chrome reports every page as focused, so "behind another
  // window" is emulated the way the client sees it: document.hasFocus()
  // false, then true again with a focus event, exactly what a real window
  // reports when it is covered and then clicked.
  await pageB.evaluate(() => { window.__realHasFocus = document.hasFocus.bind(document); document.hasFocus = () => false; });
  await pageA.bringToFront();
  const later = 'later-' + Math.random().toString(36).slice(2, 8);
  await pageA.focus('#c-input');
  await pageA.keyboard.type(later);
  await pageA.keyboard.press('Enter');
  ok(true, 'A sends from the composer while B is in the background');
  const receiptOf = (b) => {
    const el = [...document.querySelectorAll('#thread .msg')].find((x) => x.textContent.includes(b));
    return el?.querySelector('.receipt')?.className ?? null;
  };
  await sleep(1500);
  const single = await pageA.evaluate(receiptOf, later);
  ok(single && /receipt sent/.test(single) && !/seen|all/.test(single), `while B is behind, A's tick is single (${single})`);
  const unreadOnB = await pageB.evaluate(() => [...document.querySelectorAll('.conv-row')].find((r) => r.textContent.includes('E2E A') && !r.textContent.includes(' and '))?.textContent.replace(/\s+/g, ' ').trim().slice(0, 60) ?? null);
  console.log(`    (B's row while behind: "${unreadOnB}")`);
  await pageB.evaluate(() => { document.hasFocus = window.__realHasFocus; window.dispatchEvent(new Event('focus')); });
  await pageB.bringToFront();
  const turned = await until(pageA, (b) => {
    const el = [...document.querySelectorAll('#thread .msg')].find((x) => x.textContent.includes(b));
    const r = el?.querySelector('.receipt');
    return r && /seen|all/.test(r.className) ? r.className : null;
  }, later, 6000);
  ok(!turned.timedOut, `B comes to the front: A's tick turns double ${turned.ms} ms later, no chat switch (${turned.v})`);
  await pageA.bringToFront();

  // 2c. Typing: B types; A sees it at once; it goes away by itself; and a
  // message from B ends it immediately.
  await pageB.bringToFront();
  await pageB.focus('#c-input');
  await pageB.keyboard.type('hel');
  const typing = await until(pageA, () => (document.querySelector('#thread .typing:not(.hidden)')?.textContent ?? '').trim() || null, null, 3000);
  ok(!typing.timedOut && /E2E B is typing/.test(typing.v), `A sees "${typing.v}" ${typing.ms} ms after B's first key`);
  await pageB.evaluate(() => { const i = document.getElementById('c-input'); i.value = ''; i.dispatchEvent(new Event('input', { bubbles: true })); });
  const gone = await until(pageA, () => (document.querySelector('#thread .typing.hidden') ? 'hidden' : null), null, 3000);
  ok(!gone.timedOut, `B clears the box: the indicator is gone on A ${gone.ms} ms later`);
  await pageB.keyboard.type('hey');
  await until(pageA, () => document.querySelector('#thread .typing:not(.hidden)') ? 1 : null, null, 3000);
  const t1 = performance.now();
  const lapsed = await until(pageA, () => (document.querySelector('#thread .typing.hidden') ? 'hidden' : null), null, 9000);
  ok(!lapsed.timedOut && lapsed.ms >= 3500, `B pauses: the indicator lapses by itself after ${Math.round(performance.now() - t1)} ms`);
  await pageB.keyboard.type(' there');
  await until(pageA, () => document.querySelector('#thread .typing:not(.hidden)') ? 1 : null, null, 3000);
  await pageB.keyboard.press('Enter');
  const ended = await until(pageA, () => (document.querySelector('#thread .typing.hidden') && [...document.querySelectorAll('#thread .bubble')].some((el) => el.textContent.includes('hey there')) ? 1 : null), null, 4000);
  ok(!ended.timedOut, `B sends: the message is in A's thread and the indicator is gone, ${ended.ms} ms`);
  await pageA.bringToFront();

  // 3. My own corner follows my pick, on the same device that picked it.
  await A.req('PUT', '/api/messaging/status', { status: 'busy', note: 'corner check' });
  const corner = await until(pageA, () => document.querySelector('#top-avatar')?.closest('.av-wrap')?.querySelector('.presence')?.className ?? null, null, 5000);
  ok(!corner.timedOut && /pi-busy/.test(corner.v), `A's own corner glyph shows busy ${corner.ms} ms after the pick (${corner.v})`);

  // 4. The peer's header line follows their pick.
  await B.req('PUT', '/api/messaging/status', { status: 'dnd', note: 'header check' });
  const header = await until(pageA, () => document.querySelector('.thread-head .st-word')?.textContent ?? null, null, 5000);
  const headerText = await until(pageA, () => (document.querySelector('.thread-head .st-word')?.textContent === 'Do not disturb' ? document.querySelector('.thread-head h2 + span, .thread-head div > span')?.textContent : null), null, 5000);
  ok(!headerText.timedOut, `A's header shows B's new state ${headerText.ms} ms after B's pick ("${headerText.v}")`);
  void header;
  // and the row in the list
  const row = await until(pageA, () => [...document.querySelectorAll('.conv-row .conv')].find((r) => r.textContent.includes('E2E B') && !r.textContent.includes(' and '))?.querySelector('.presence')?.className ?? null, null, 3000);
  ok(!row.timedOut && /pi-dnd/.test(row.v), `and B's row glyph is dnd (${row.v})`);

  await A.req('PUT', '/api/messaging/status', { status: 'available' });
  await B.req('PUT', '/api/messaging/status', { status: 'available' });
  ok(errors.A.length === 0, `no page errors on A${errors.A.length ? ': ' + errors.A.slice(0, 3).join(' | ') : ''}`);
  ok(errors.B.length === 0, `no page errors on B${errors.B.length ? ': ' + errors.B.slice(0, 3).join(' | ') : ''}`);
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
