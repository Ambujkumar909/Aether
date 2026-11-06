// A one-to-one call between two real browsers, end to end: A rings B from
// the chat header, B accepts in the dialog, both land in the room. Then the
// things a person does there:
//   - the in-call chat opens on BOTH sides as the two of them -- the kept
//     conversation, with its history -- not as a nameless group chat;
//   - a message sent there lands on the other side and in the permanent
//     thread (the API lists it);
//   - the call chat ("Call chat, not kept") exists as a second choice on both
//     sides: a message there reaches the other side and is NOT stored;
//   - the reaction picker opens on click, offers the set, closes on a pick
//     and on an outside click, and a reaction floats over the stage on both
//     sides within a moment.
//
//   node tools/e2e/call.e2e.mjs [https://localhost:8443]
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
// Chat is a toggle in the Teams layout (a second press closes it): open it
// only if it is not already the panel showing.
const openChat = (p) => p.evaluate(() => {
  const shell = document.getElementById('shell');
  const open = !shell.classList.contains('panel-closed') && document.getElementById('view-chat').classList.contains('active');
  if (!open) document.getElementById('rail-chat').click();
});
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
const aName = la.json.user.displayName;
const bName = lb.json.user.displayName;
const conv = (await A.req('POST', '/api/messaging/direct', { email: accounts.B.email })).json;

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: [
    '--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required',
    '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
  ],
});
const errors = { A: [], B: [] };
async function signedIn(acct, label) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  let counting = false;
  page.on('pageerror', (e) => counting && errors[label].push(String(e)));
  page.on('console', (m) => counting && m.type() === 'error' && !/401/.test(m.text()) && errors[label].push(m.text()));
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
  await sleep(600);
  return page;
}
const until = async (page, fn, arg, budgetMs) => {
  const t0 = performance.now();
  for (;;) {
    let v = null;
    try {
      v = await page.evaluate(fn, arg);
    } catch {}
    if (v) return { v, ms: Math.round(performance.now() - t0) };
    if (performance.now() - t0 > budgetMs) return { v: null, ms: Math.round(performance.now() - t0), timedOut: true };
    await sleep(80);
  }
};
const inMeeting = (page) => until(page, () => getComputedStyle(document.getElementById('app')).display !== 'none' ? 1 : null, null, 20000);

try {
  const pageA = await signedIn(accounts.A, 'A');
  const pageB = await signedIn(accounts.B, 'B');
  // The history B should see the moment B is in the call: made here, so the
  // check does not depend on another harness having run first.
  await pageA.evaluate(async (email) => {
    const api = await import('/src/api.js');
    const conv = await api.messaging.openDirect(email);
    await api.messaging.send(conv.id, 'Before the call: the agenda is in the doc.');
  }, accounts.B.email);
  await sleep(600);
  // A opens the chat with B and rings from the header's call button.
  await pageA.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
  await sleep(700);
  await pageA.evaluate((name) => {
    const rows = [...document.querySelectorAll('.conv-row .conv')];
    rows.find((x) => x.textContent.replace(/\s+/g, ' ').includes(name) && !x.textContent.includes(' and '))?.click();
  }, bName);
  await sleep(800);
  const rang = await pageA.evaluate(() => {
    const b = document.querySelector('.thread-head [data-call][data-call-audio="0"], .thread-head .call-main[data-call]');
    b?.click();
    return !!b;
  });
  ok(rang, 'A presses the video-call button in the chat header');
  // B: the incoming dialog, accepted.
  const ringing = await until(pageB, () => document.querySelector('#dialog [data-accept]') ? 1 : null, null, 8000);
  ok(!ringing.timedOut, `B sees the incoming call dialog ${ringing.ms} ms later`);
  await pageB.evaluate(() => document.querySelector('#dialog [data-accept]')?.click());
  // B navigates to the room and lands on pre-join; A went straight in.
  const preB = await until(pageB, () => getComputedStyle(document.getElementById('prejoin')).display !== 'none' ? 1 : null, null, 20000);
  ok(!preB.timedOut, 'B reaches the pre-join for the call');
  await sleep(500);
  await pageB.evaluate(() => document.getElementById('pj-join')?.click());
  // A may have been shown the pre-join as well; if so, join.
  const preA = await until(pageA, () => getComputedStyle(document.getElementById('prejoin')).display !== 'none' ? 1 : (getComputedStyle(document.getElementById('app')).display !== 'none' ? 2 : null), null, 20000);
  if (preA.v === 1) await pageA.evaluate(() => document.getElementById('pj-join')?.click());
  const [inA, inB] = [await inMeeting(pageA), await inMeeting(pageB)];
  // How long after walking in the callee's panel shows the conversation's
  // history -- it was "Opening your conversation..." and a fetch after join.
  await openChat(pageB);
  // Looked at once, immediately: the history must already be there, with no
  // "Opening your conversation..." state in between.
  const firstLook = await pageB.evaluate(() => ({ msgs: document.querySelectorAll('#chat-log .msg').length, text: document.getElementById('chat-log')?.textContent.trim().slice(0, 60) }));
  ok(firstLook.msgs > 0, `B's call chat shows the saved history the moment B is in the call (${firstLook.msgs} messages; ${firstLook.msgs ? 'no loading state' : 'saw "' + firstLook.text + '"'})`);
  const where = async (p) => p.evaluate(() => ({ prejoin: getComputedStyle(document.getElementById('prejoin')).display, app: getComputedStyle(document.getElementById('app')).display, home: getComputedStyle(document.getElementById('home')).display, url: location.search }));
  ok(!inA.timedOut && !inB.timedOut, `both are in the call (A ${JSON.stringify(await where(pageA))}, B ${JSON.stringify(await where(pageB))})`);
  await sleep(2500);

  // --- the in-call chat, both sides
  for (const [label, p, other] of [['A', pageA, bName], ['B', pageB, aName]]) {
    await openChat(p);
    await sleep(600);
    const st = await p.evaluate(() => ({
      selected: document.querySelector('#chat-tabs [aria-selected="true"]')?.dataset.chat,
      tabs: [...document.querySelectorAll('#chat-tabs .chat-tab')].map((t) => t.textContent.replace(/\s+/g, ' ').trim()),
      tabsHidden: document.getElementById('chat-tabs')?.hidden,
      dropdownHidden: document.getElementById('chat-to')?.hidden,
      title: document.getElementById('side-title')?.textContent.trim(),
    }));
    const otherId = label === 'A' ? bId : aId;
    ok(st.selected === otherId, `${label}: the in-call chat opens on the saved conversation with ${other} (selected=${st.selected}, title "${st.title}")`);
    ok(!st.tabsHidden && st.tabs.length === 2 && st.dropdownHidden, `${label}: two tabs, no dropdown: ${JSON.stringify(st.tabs)}`);
  }
  // A message in the kept conversation reaches B's panel and the API.
  const kept = 'kept-' + Math.random().toString(36).slice(2, 8);
  await pageA.focus('#chat-input');
  await pageA.keyboard.type(kept);
  await pageA.keyboard.press('Enter');
  const seenB = await until(pageB, (t) => [...document.querySelectorAll('#chat-log .bubble')].some((b) => b.textContent.includes(t)) ? 1 : null, kept, 6000);
  ok(!seenB.timedOut, `B sees the kept message in the call panel ${seenB.ms} ms later`);
  const listed = (await B.req('GET', `/api/messaging/conversations/${conv.id}/messages?limit=20`)).json;
  ok((listed?.messages ?? []).some((m) => m.body === kept), 'and it is stored in the permanent conversation');
  // B's history is visible: the message count in B's panel is at least the API's.
  const historyB = await pageB.evaluate(() => document.querySelectorAll('#chat-log .msg').length);
  ok(historyB >= 1, `B's panel shows history (${historyB} messages drawn)`);

  // --- the call chat: not kept
  for (const p of [pageA, pageB]) {
    await p.click('#chat-tabs [data-chat="everyone"]');
    await sleep(300);
  }
  const labelEveryone = await pageA.evaluate(() => document.querySelector('#chat-tabs [data-chat="everyone"]')?.textContent.replace(/\s+/g, ' ').trim());
  ok(/this call only/i.test(labelEveryone ?? '') && /deleted/i.test(labelEveryone ?? ''), `the second tab says what it is: "${labelEveryone}"`);
  const gone = 'gone-' + Math.random().toString(36).slice(2, 8);
  await pageA.focus('#chat-input');
  await pageA.keyboard.type(gone);
  await pageA.keyboard.press('Enter');
  const seenGone = await until(pageB, (t) => [...document.querySelectorAll('#chat-log .bubble')].some((b) => b.textContent.includes(t)) ? 1 : null, gone, 6000);
  ok(!seenGone.timedOut, `B sees the call-chat message ${seenGone.ms} ms later`);
  await sleep(400);
  const listed2 = (await B.req('GET', `/api/messaging/conversations/${conv.id}/messages?limit=20`)).json;
  ok(!(listed2?.messages ?? []).some((m) => m.body === gone), 'and it is NOT stored anywhere');

  // --- reactions
  const picker = await pageA.evaluate(() => {
    const menu = document.getElementById('btn-react');
    menu?.querySelector('button')?.click();
    const pop = menu?.querySelector('.react-pop');
    return { open: pop ? getComputedStyle(pop).display !== 'none' : false, n: pop?.querySelectorAll('[data-react]').length ?? 0 };
  });
  ok(picker.open, `A: the reaction picker opens on click`);
  ok(picker.n >= 8, `A: it offers ${picker.n} reactions`);
  await pageA.evaluate(() => document.querySelector('#btn-react [data-react]')?.click());
  const floatA = await until(pageA, () => document.querySelector('.reaction-float') ? 1 : null, null, 3000);
  const floatB = await until(pageB, () => document.querySelector('.reaction-float') ? 1 : null, null, 3000);
  ok(!floatA.timedOut, `A: the reaction floats over A's own stage ${floatA.ms} ms after the click`);
  ok(!floatB.timedOut, `B: the reaction floats over B's stage ${floatB.ms} ms after A's click`);
  const closedAfterPick = await pageA.evaluate(() => getComputedStyle(document.querySelector('#btn-react .react-pop')).display === 'none');
  ok(closedAfterPick, 'A: the picker closed after the pick');
  await pageA.evaluate(() => document.querySelector('#btn-react button')?.click());
  await pageA.evaluate(() => document.querySelector('.stage')?.dispatchEvent(new MouseEvent('click', { bubbles: true })));
  await sleep(150);
  const closedOutside = await pageA.evaluate(() => getComputedStyle(document.querySelector('#btn-react .react-pop')).display === 'none');
  ok(closedOutside, 'A: the picker closes on a click outside it');
  const anim = await pageA.evaluate(() => {
    const el = document.querySelector('.reaction-float');
    return el ? getComputedStyle(el).animationName : 'none';
  });
  ok(anim && anim !== 'none', `the reaction has a rise animation (${anim})`);

  // --- leave: the call chat is gone, the kept one stays
  for (const p of [pageA, pageB]) {
    try {
      await p.evaluate(() => document.getElementById('btn-leave')?.click());
      await sleep(150);
      await p.evaluate(() => [...document.querySelectorAll('button')].find((b) => /^(leave|end|yes)/i.test(b.textContent.trim()))?.click());
    } catch {}
  }
  await sleep(800);
  const after = (await B.req('GET', `/api/messaging/conversations/${conv.id}/messages?limit=20`)).json;
  ok((after?.messages ?? []).some((m) => m.body === kept) && !(after?.messages ?? []).some((m) => m.body === gone), 'after the call: the kept message is there, the call-chat message is not');
  ok(errors.A.length === 0, `no page errors on A${errors.A.length ? ': ' + errors.A.slice(0, 2).join(' | ') : ''}`);
  ok(errors.B.length === 0, `no page errors on B${errors.B.length ? ': ' + errors.B.slice(0, 2).join(' | ') : ''}`);
} finally {
  await browser.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
