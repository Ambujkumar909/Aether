// The README's pictures, made from the real app with fictional demo people.
//
// Against a FRESH throwaway server (signup is limited to three an hour, and
// this makes three accounts):
//
//   aether -addr :9445 -tls -db <scratch>/show.db -web ../web -media-port 8813
//   node tools/e2e/showcase.mjs https://localhost:9445
//
// Writes docs/media/*.png. Everything shown is driven through the product the
// way a person would, apart from speech, which headless Chrome cannot hear:
// the browser's recogniser is replaced by one the script speaks into, exactly
// as tools/e2e/recording.e2e.mjs does.
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const BASE = process.argv.find((a) => a.startsWith('http')) || 'https://localhost:9445';
const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'docs', 'media');
mkdirSync(OUT, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PEOPLE = {
  maya: { name: 'Maya Chen', email: 'maya.chen@northwind.example', look: ['#e8b896', '#2b1d16', '#1f2d33'] },
  arjun: { name: 'Arjun Rao', email: 'arjun.rao@northwind.example', look: ['#b98563', '#1a1410', '#2b2e4a'] },
  sofia: { name: 'Sofia Lind', email: 'sofia.lind@northwind.example', look: ['#e9bfa0', '#3b2a20', '#2a2f3a'] },
};

// Each person's camera: a drawn head and shoulders in their own colours, on a
// plain wall -- the figure the segmenter takes for a person (effects.e2e.mjs).
const cameraFor = ([skin, hair, shirt]) => `(() => {
  const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (c) => {
    if (!c || !c.video) return real(c);
    const cv = document.createElement('canvas');
    cv.width = 640; cv.height = 360;
    const g = cv.getContext('2d');
    const draw = () => {
      g.fillStyle = '#c9d6df'; g.fillRect(0, 0, 640, 360);
      g.fillStyle = '${shirt}'; g.beginPath(); g.ellipse(320, 230, 135, 75, 0, Math.PI, 0); g.fill(); g.fillRect(185, 230, 270, 130);
      g.fillStyle = '${skin}'; g.fillRect(298, 150, 44, 40);
      g.beginPath(); g.ellipse(320, 115, 48, 60, 0, 0, 2 * Math.PI); g.fill();
      g.fillStyle = '${hair}'; g.beginPath(); g.ellipse(320, 78, 52, 32, 0, Math.PI, 0); g.fill();
    };
    draw(); setInterval(draw, 100);
    const s = cv.captureStream(30);
    if (c.audio) (await real({ audio: c.audio })).getAudioTracks().forEach((t) => s.addTrack(t));
    return s;
  };
})();`;
const PASSWORD = 'Showcase-' + Math.random().toString(36).slice(2, 10) + '!';

const FAKE_SPEECH = `(() => {
  window.__speech = [];
  class FakeSR { constructor() { this.running = false; window.__speech.push(this); } start() { this.running = true; } stop() { this.running = false; setTimeout(() => this.onend?.(), 0); } abort() { this.stop(); } }
  FakeSR.available = async () => 'available';
  window.SpeechRecognition = FakeSR; window.webkitSpeechRecognition = FakeSR;
  window.__say = (text, isFinal) => { const r = [...window.__speech].reverse().find((x) => x.running); if (!r) return false; const res = [{ transcript: text }]; res.isFinal = isFinal; r.onresult?.({ results: [res] }); return true; };
})();`;

for (const p of Object.values(PEOPLE)) {
  const r = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: p.email, displayName: p.name, password: PASSWORD }) });
  if (r.status !== 201) throw new Error(`signup ${p.name}: ${r.status} (use a fresh server)`);
}

const browser = await puppeteer.launch({
  executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
  headless: true,
  args: ['--ignore-certificate-errors', '--no-first-run', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--hide-scrollbars', '--font-render-hinting=none'],
});

async function open(who, viewport = { width: 1440, height: 900, deviceScaleFactor: 2 }) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.evaluateOnNewDocument(FAKE_SPEECH);
  await page.evaluateOnNewDocument(cameraFor(PEOPLE[who].look));
  await page.evaluateOnNewDocument(() => { try { localStorage.setItem('aether.theme', 'dark'); } catch {} });
  await page.setViewport(viewport);
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  await page.evaluate(async (e, p) => (await import('/src/api.js')).login(e, p), PEOPLE[who].email, PASSWORD);
  await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('home')).display !== 'none', { timeout: 15000 });
  return page;
}
const api = (page, fn, ...args) => page.evaluate(fn, ...args);
const shot = async (page, name) => {
  await sleep(700);
  await page.screenshot({ path: join(OUT, name + '.png') });
  console.log('wrote', name);
};

const maya = await open('maya');
const arjun = await open('arjun');
const sofia = await open('sofia');

// --- Conversations ---------------------------------------------------------
const dm = await api(maya, async (email) => (await (await import('/src/api.js')).messaging.openDirect(email)).id, PEOPLE.arjun.email);
const say = async (page, conv, text) => {
  await api(page, async (c, t) => (await import('/src/api.js')).messaging.send(c, t), conv, text);
  await sleep(350);
};
await say(maya, dm, 'Morning! Did the new onboarding flow land on staging?');
await say(arjun, dm, 'It did, about twenty minutes ago. Sign-up to first meeting is down to three screens.');
await say(maya, dm, 'Three! That was seven last month 🎉');
await say(arjun, dm, 'And the invite link now opens straight into the pre-join, so people can check their camera first.');
await say(maya, dm, 'Love it. Can you walk Sofia through it in the design review?');
await say(arjun, dm, 'Yes. I will bring the before and after.');
const dm2 = await api(maya, async (email) => (await (await import('/src/api.js')).messaging.openDirect(email)).id, PEOPLE.sofia.email);
await say(sofia, dm2, 'The new icons are in the shared folder whenever you have a minute.');
await say(maya, dm2, 'Looking now, thank you!');
const group = await api(maya, async (a, s) => (await (await import('/src/api.js')).messaging.createGroup('Launch crew', [a, s])).id, PEOPLE.arjun.email, PEOPLE.sofia.email);
await say(sofia, group, 'Launch checklist is 9 of 11 done. Waiting on legal and the release notes.');
await say(arjun, group, 'Release notes are drafted, I will share them after the review.');

// --- Meetings ----------------------------------------------------------------
const at = (h, m = 0) => { const d = new Date(); d.setHours(d.getHours() + h, m, 0, 0); return d; };
const sched = (page, m) => api(page, async (m) => (await import('/src/api.js')).scheduling.create(m), m);
await sched(maya, { title: 'Design review: onboarding v2', description: 'Walk through the new flow before launch.', category: 'design', startsAt: at(1).toISOString(), endsAt: at(1, 45).toISOString(), invitees: [PEOPLE.arjun.email, PEOPLE.sofia.email], coOrganizers: [] });
await sched(maya, { title: 'Weekly sync', description: '', category: 'internal', startsAt: at(3).toISOString(), endsAt: at(3, 30).toISOString(), invitees: [PEOPLE.arjun.email, PEOPLE.sofia.email], coOrganizers: [] });
await sched(maya, { title: 'Customer call: Northwind', description: '', category: 'external', startsAt: at(26).toISOString(), endsAt: at(27).toISOString(), invitees: [PEOPLE.sofia.email], coOrganizers: [] });

// --- Home, chat, calendar ------------------------------------------------------
await maya.goto(BASE + '/', { waitUntil: 'networkidle2' });
await sleep(1200);
await shot(maya, 'home');
await maya.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
await sleep(900);
await maya.evaluate(() => [...document.querySelectorAll('button.conv')].find((x) => x.querySelector('.conv-name')?.textContent.trim() === 'Arjun Rao')?.click());
await sleep(1200);
await shot(maya, 'chat');
await maya.evaluate(() => document.querySelector('[data-home="calendar"]')?.click());
await sleep(1200);
await shot(maya, 'calendar');

// --- A meeting -------------------------------------------------------------------
const room = 'design-review-' + Math.random().toString(36).slice(2, 6);
async function enterRoom(page, camOn = false) {
  await page.goto(`${BASE}/?room=${room}`, { waitUntil: 'networkidle2' });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 20000 });
  await sleep(600);
  if (!camOn) await page.evaluate(() => document.getElementById('pj-cam').click());
  await sleep(300);
  await page.evaluate(() => document.getElementById('pj-join').click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
}
await enterRoom(maya);
await sleep(800);
await enterRoom(arjun);
await enterRoom(sofia);
await sleep(2500);
await shot(maya, 'meeting');

// The whiteboard: a flow sketched by Arjun, two sticky notes.
await maya.evaluate(() => { document.getElementById('btn-share-more').click(); document.getElementById('share-wb').click(); });
await sleep(1500);
const board = (p, fx, fy) => p.evaluate((fx, fy) => { const r = document.getElementById('wb-canvas').getBoundingClientRect(); return [r.left + fx * r.width, r.top + fy * r.height]; }, fx, fy);
async function stroke(p, pts) {
  const [x0, y0] = await board(p, ...pts[0]);
  await p.mouse.move(x0, y0);
  await p.mouse.down();
  for (const pt of pts.slice(1)) {
    const [x, y] = await board(p, ...pt);
    await p.mouse.move(x, y, { steps: 12 });
  }
  await p.mouse.up();
  await sleep(250);
}
const box = (x, y, w, h) => [[x, y], [x + w, y], [x + w, y + h], [x, y + h], [x, y]];
await stroke(arjun, box(0.08, 0.38, 0.18, 0.2));
await stroke(arjun, box(0.41, 0.38, 0.18, 0.2));
await stroke(arjun, box(0.74, 0.38, 0.18, 0.2));
await stroke(arjun, [[0.27, 0.48], [0.4, 0.48], [0.37, 0.45], [0.4, 0.48], [0.37, 0.51]]);
await stroke(arjun, [[0.6, 0.48], [0.73, 0.48], [0.7, 0.45], [0.73, 0.48], [0.7, 0.51]]);
async function note(p, fx, fy, text) {
  await p.evaluate(() => document.querySelector('[data-tool="note"]').click());
  const [x, y] = await board(p, fx, fy);
  await p.mouse.click(x, y);
  await p.waitForFunction(() => !!document.querySelector('#dialog textarea[name="text"]'), { timeout: 8000 });
  await p.evaluate((t) => { document.querySelector('#dialog textarea[name="text"]').value = t; document.querySelector('#dialog [data-yes]').click(); }, text);
  await sleep(500);
}
await note(sofia, 0.1, 0.72, 'Sign up → Pre-join → In the meeting');
await note(maya, 0.62, 0.12, 'Ship v2 on Friday');
await sleep(1500);
await shot(maya, 'whiteboard');
await maya.evaluate(() => document.getElementById('wb-close')?.click());
await sleep(800);

// Transcription, and everyone's words in the panel.
await maya.evaluate(() => { document.getElementById('btn-more').click(); document.getElementById('more-tr').click(); });
await sleep(1500);
for (const p of [maya, arjun, sofia]) await p.evaluate(() => document.querySelector('#rec-banner [data-speech="yes"]')?.click());
await sleep(800);
await maya.evaluate(() => { document.getElementById('btn-more').click(); document.getElementById('more-transcript').click(); });
await sleep(800);
const lines = [
  [arjun, 'Sign-up to first meeting is three screens now, down from seven.'],
  [sofia, 'And the camera check happens before anyone sees you. That was the top request.'],
  [maya, 'Great. Let us ship it on Friday and watch the drop-off numbers.'],
  [arjun, 'I will add the release notes to the Launch crew chat after this.'],
];
for (const [p, t] of lines) {
  await p.evaluate((t) => window.__say(t, true), t);
  await sleep(900);
}
await maya.evaluate(() => document.querySelector('#rec-banner [data-close]')?.click());
await sleep(1200);
await shot(maya, 'transcript');

// Live captions as Sofia sees them.
await sofia.evaluate(() => document.getElementById('btn-cc').click());
await sleep(800);
await arjun.evaluate(() => window.__say('Next, the pricing page. I think we can drop the third tier', false));
const seen = await sofia.waitForFunction(() => document.getElementById('caption')?.textContent.includes('pricing page'), { timeout: 8000 }).then(() => true, () => false);
if (!seen) console.log('captions not shown:', await sofia.evaluate(() => ({ bar: document.getElementById('caption')?.outerHTML.slice(0, 200), fx: document.documentElement.dataset })), await arjun.evaluate(() => window.__speech.map((r) => r.running)));
await shot(sofia, 'captions');

// --- Together mode: cameras on, everyone in one scene ------------------------------
await sofia.evaluate(() => document.getElementById('btn-cc').click()); // captions off again
for (const p of [maya, arjun, sofia]) await p.evaluate(() => { document.getElementById('btn-cam').click(); document.getElementById('side-close')?.click(); document.querySelector('#rec-banner [data-close]')?.click(); });
await sleep(3000);
await maya.evaluate(() => { document.getElementById('btn-view').click(); document.getElementById('view-together').click(); });
await maya.waitForFunction(() => !!document.querySelector('#dialog select[name="scene"]'), { timeout: 8000 });
await maya.evaluate(() => document.querySelector('#dialog [data-yes]').click());
await maya.waitForFunction(() => Number(document.documentElement.dataset.togOutlines || 0) >= 2, { timeout: 30000 }).catch(() => console.log('together: outlines did not all arrive'));
await sleep(3500);
await shot(maya, 'together');
await maya.evaluate(() => { document.getElementById('btn-view').click(); document.getElementById('view-together').click(); });
await maya.waitForFunction(() => !!document.querySelector('#dialog input[name="stop"]'), { timeout: 8000 });
await maya.evaluate(() => { document.querySelector('#dialog input[name="stop"]').checked = true; document.querySelector('#dialog [data-yes]').click(); });
await sleep(800);
for (const p of [maya, arjun, sofia]) await p.evaluate(() => document.getElementById('btn-cam').click());
await sleep(800);

// --- On a phone ---------------------------------------------------------------------
const phone = { width: 390, height: 844, deviceScaleFactor: 3, isMobile: true, hasTouch: true };
await sofia.evaluate(() => document.getElementById('btn-leave')?.click());
await sleep(1500);
const sofiaPhone = await open('sofia', phone);
await enterRoom(sofiaPhone);
await sleep(1000);
await sofiaPhone.evaluate(() => document.querySelector('#rec-banner [data-speech="yes"]')?.click());
await sleep(7000); // the same-device notice (all three are on this machine) fades
await sofiaPhone.evaluate(() => document.querySelector('#rec-banner [data-close]')?.click());
await shot(sofiaPhone, 'phone-meeting');
const arjunPhone = await open('arjun', phone);
await arjunPhone.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
await sleep(900);
await arjunPhone.evaluate(() => [...document.querySelectorAll('button.conv')].find((x) => x.querySelector('.conv-name')?.textContent.trim() === 'Maya Chen')?.click());
await sleep(1200);
await shot(arjunPhone, 'phone-chat');

await browser.close();
