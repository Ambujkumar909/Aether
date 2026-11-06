// Product screenshots with three demo people: Test 1, Test 2 and Ambuj.
//
// Against a FRESH throwaway server (signup is limited to three an hour, and
// this makes three accounts):
//
//   aether -addr :9445 -tls -db <scratch>/screens.db -web ../web -media-port 8813
//   node tools/e2e/screens.mjs https://localhost:9445
//
// Writes screenshots/*.png at the repository root (not committed). Everything
// is driven through the product the way a person would; the only stand-ins are
// the cameras (a drawn figure) and the shared screen (a drawn slide), because a
// headless browser has neither a camera nor a screen to pick.
import { mkdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const BASE = process.argv.find((a) => a.startsWith('http')) || 'https://localhost:9445';
// A folder per run, so a picture open in a viewer never blocks the next run.
const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'screenshots', new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-'));
mkdirSync(OUT, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PEOPLE = {
  ambuj: { name: 'Ambuj', email: 'ambuj@aether-demo.example' },
  test1: { name: 'Test 1', email: 'test1@aether-demo.example' },
  test2: { name: 'Test 2', email: 'test2@aether-demo.example' },
};
const PASSWORD = 'Screens-' + Math.random().toString(36).slice(2, 10) + '!';

// What Ambuj shares: a slide exported from the PowerPoint deck and the first
// page of the PDF, rendered to images beforehand (PowerPoint's own Export, and
// PyMuPDF). They reach the meeting through the product's real screen-share
// path; only the capture is stood in for, because a headless browser has no
// screen to pick and capturing another program's window remotely is fragile.
//   SHARE_SLIDE=<png 1920x1080>  SHARE_PAGE=<png of a portrait page>
const SHARE_SLIDE = process.env.SHARE_SLIDE;
const SHARE_PAGE = process.env.SHARE_PAGE;
const dataURL = (f) => 'data:image/png;base64,' + readFileSync(f).toString('base64');
const SHARE = `(() => {
  navigator.mediaDevices.getDisplayMedia = async () => {
    const img = new Image();
    img.src = window.__shareSrc;
    await img.decode();
    const cv = document.createElement('canvas');
    cv.width = 1920; cv.height = 1080;
    const g = cv.getContext('2d');
    const page = window.__shareKind === 'page';
    const draw = () => {
      if (!page) { g.drawImage(img, 0, 0, 1920, 1080); return; }
      // A PDF viewer at fit-to-width: the page on a grey backdrop, its top.
      g.fillStyle = '#525659'; g.fillRect(0, 0, 1920, 1080);
      const w = 1180, h = img.height * w / img.width;
      g.drawImage(img, (1920 - w) / 2, 40, w, h);
    };
    draw(); setInterval(draw, 500);
    return cv.captureStream(5);
  };
})();`;

for (const p of Object.values(PEOPLE)) {
  const r = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: p.email, displayName: p.name, password: PASSWORD }) });
  if (r.status !== 201) throw new Error(`signup ${p.name}: ${r.status} (use a fresh server)`);
}

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const FLAGS = ['--ignore-certificate-errors', '--no-first-run', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--hide-scrollbars', '--font-render-hinting=none'];
const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: FLAGS });

const open = (who, theme, viewport) => openIn(browser, who, theme, viewport);
async function openIn(b, who, theme = 'light', viewport = { width: 1440, height: 900, deviceScaleFactor: 2 }) {
  const ctx = b === browser ? await b.createBrowserContext() : b.defaultBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(SHARE);
  await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('aether.theme', t); } catch {} }, theme);
  await page.setViewport(viewport);
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  await page.evaluate(async (e, p) => (await import('/src/api.js')).login(e, p), PEOPLE[who].email, PASSWORD);
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('home')).display !== 'none', { timeout: 15000 });
  return page;
}
const api = (page, fn, ...args) => page.evaluate(fn, ...args);
const shot = async (page, name) => {
  await sleep(800);
  await page.screenshot({ path: join(OUT, name + '.png') });
  console.log('wrote', join(OUT, name + '.png'));
};

const ambuj = await open('ambuj');
const test1 = await open('test1');
const test2 = await open('test2');

// --- Conversations: two direct chats and a group ------------------------------
const say = async (page, conv, text) => {
  await api(page, async (c, t) => (await import('/src/api.js')).messaging.send(c, t), conv, text);
  await sleep(350);
};
const direct = (page, email) => api(page, async (e) => (await (await import('/src/api.js')).messaging.openDirect(e)).id, email);
const dm1 = await direct(ambuj, PEOPLE.test1.email);
await say(test1, dm1, 'Hey Ambuj, good morning!');
await say(ambuj, dm1, 'Good morning! Testing the system like this.');
await say(test1, dm1, 'Messages are arriving instantly on my side.');
await say(ambuj, dm1, 'Great. Join the demo meeting at 11, I will share the slides there.');
await say(test1, dm1, 'Sure, see you there 👍');
const dm2 = await direct(ambuj, PEOPLE.test2.email);
await say(test2, dm2, 'Can you send me the meeting link?');
await say(ambuj, dm2, 'It is on your calendar: Aether demo.');
await say(test2, dm2, 'Found it, thanks!');
const group = await api(ambuj, async (a, b) => (await (await import('/src/api.js')).messaging.createGroup('Aether testing', [a, b])).id, PEOPLE.test1.email, PEOPLE.test2.email);
await say(ambuj, group, 'Hey team, good morning!');
await say(test1, group, 'Good morning! Testing the system like this.');
await say(test2, group, 'Hello everyone, all good from my side.');

// --- Meetings on the calendar ---------------------------------------------------
const at = (h, m = 0) => { const d = new Date(); d.setHours(d.getHours() + h, m, 0, 0); return d; };
const sched = (page, m) => api(page, async (m) => (await import('/src/api.js')).scheduling.create(m), m);
const demo = await sched(ambuj, { title: 'Aether demo', description: 'Walk through meetings, chat and screen sharing.', category: 'internal', startsAt: at(0, 0).toISOString(), endsAt: at(1, 0).toISOString(), invitees: [PEOPLE.test1.email, PEOPLE.test2.email], coOrganizers: [] });
await sched(ambuj, { title: 'Weekly sync', description: '', category: 'internal', startsAt: at(3).toISOString(), endsAt: at(3, 30).toISOString(), invitees: [PEOPLE.test1.email, PEOPLE.test2.email], coOrganizers: [] });
await sched(ambuj, { title: 'Design review', description: '', category: 'design', startsAt: at(26).toISOString(), endsAt: at(27).toISOString(), invitees: [PEOPLE.test2.email], coOrganizers: [] });
const room = demo.room;
if (!room) throw new Error('scheduled meeting has no room: ' + JSON.stringify(demo));

// --- Dashboard and chat, light ------------------------------------------------------
await ambuj.goto(BASE + '/', { waitUntil: 'networkidle2' });
await sleep(1500);
await shot(ambuj, '04-dashboard-home-light');
await ambuj.evaluate(() => document.querySelector('[data-home="calendar"]')?.click());
await sleep(1500);
await shot(ambuj, '05-dashboard-calendar-light');
await ambuj.evaluate(() => document.querySelector('[data-home="activity"]')?.click());
await sleep(1500);
await shot(ambuj, '06-dashboard-activity-light');
await ambuj.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
await sleep(1000);
await ambuj.evaluate(() => [...document.querySelectorAll('button.conv')].find((x) => x.querySelector('.conv-name')?.textContent.trim() === 'Test 1')?.click());
await sleep(1500);
await shot(ambuj, '08-chat-light');

// --- Dashboard, dark ------------------------------------------------------------------
const ambujDark = await open('ambuj', 'dark');
await sleep(1500);
await shot(ambujDark, '07-dashboard-home-dark');
await ambujDark.close();

// --- The meeting ------------------------------------------------------------------------
const live = await open('ambuj');
async function enterRoom(page, { cam, mic }) {
  // From the meeting on the home screen, as a person would: that is also what
  // carries the meeting's title into the meeting bar.
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  await sleep(1200);
  const viaHome = await page.evaluate((id) => {
    const b = document.querySelector(`[data-join="${CSS.escape(id)}"]`);
    if (b) b.click();
    return !!b;
  }, demo.id);
  if (!viaHome) await page.goto(`${BASE}/?room=${encodeURIComponent(room)}`, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 20000 });
  await sleep(1200);
  if (!cam) await page.evaluate(() => document.getElementById('pj-cam').click());
  await sleep(300);
  await page.evaluate(() => document.getElementById('pj-join').click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
  await sleep(1500);
  // The mic as asked, checked against what the meeting bar shows.
  const micOn = () => page.evaluate(() => document.getElementById('mic-icon')?.getAttribute('href') === '#i-mic');
  if ((await micOn()) !== mic) await page.evaluate(() => document.getElementById('btn-mic').click());
  await sleep(500);
  if ((await micOn()) !== mic) console.log('warning: mic state not as asked for one participant');
}
await enterRoom(live, { cam: false, mic: true });
await sleep(800);
await enterRoom(test1, { cam: false, mic: false });
await enterRoom(test2, { cam: false, mic: false });
// Invited people come straight in; anything still asking to be let in is admitted.
for (let i = 0; i < 5; i++) {
  await live.evaluate(() => document.querySelector('#dialog [data-yes], .lobby-admit, [data-admit]')?.click());
  await sleep(600);
}
await sleep(8000); // notices fade (the same-device one: all three are on this machine)
await live.evaluate(() => document.querySelector('#rec-banner [data-close]')?.click());
await shot(live, '01-meeting');

// Chat beside the meeting, with a message from each.
await live.evaluate(() => document.getElementById('rail-chat').click());
await sleep(1200);
const chat = async (page, text) => {
  await page.evaluate((t) => {
    const i = document.getElementById('chat-input');
    i.value = t;
    i.dispatchEvent(new Event('input', { bubbles: true }));
    document.getElementById('chat-send').click();
  }, text);
  await sleep(700);
};
for (const p of [test1, test2]) {
  await p.evaluate(() => document.getElementById('rail-chat').click());
  await sleep(600);
}
await chat(test1, 'Hey, good morning everyone!');
await chat(live, 'Good morning! Testing the system like this.');
await chat(test2, 'Audio is clear here 👍');
await chat(test1, 'Chat is instant too.');
await chat(live, 'Great, I will share the deck next.');
await sleep(1500);
await shot(live, '02-meeting-chat');
for (const p of [live, test1, test2]) await p.evaluate(() => document.getElementById('side-close')?.click());

// Ambuj presents the deck, then the PDF; Test 1 is the audience.
async function present(page, src, kind, name) {
  await page.evaluate((s, k) => { window.__shareSrc = s; window.__shareKind = k; }, dataURL(src), kind);
  await page.evaluate(() => document.getElementById('btn-screen').click());
  await sleep(6000);
  await shot(page, name); // every picture is Ambuj's own view
  await page.evaluate(() => document.getElementById('btn-screen').click()); // stop presenting
  await sleep(3000);
}
if (SHARE_SLIDE) await present(live, SHARE_SLIDE, 'slide', '03-presenting-powerpoint');
if (SHARE_PAGE) await present(live, SHARE_PAGE, 'page', '09-presenting-pdf');

await browser.close();
console.log('done');
