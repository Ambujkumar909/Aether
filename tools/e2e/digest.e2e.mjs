// Missed-activity email, end to end: two real browsers, the server's own
// loop, and a real SMTP server (Mailpit, STARTTLS required) the email is read
// back from. The runner starts the server with -mail-digest-minute 300ms, so
// "every 10 minutes" is three seconds.
//
//   - the account page offers a period because the server can send mail,
//     saves it, and shows the same after a reload; the row fits a phone;
//   - A closes Aether; B writes twice; nothing is sent before the period
//     ends, then one email reaches A's address saying who, how many unread
//     and the newest words, and no second email follows;
//   - missed and then read before the period ends: no email;
//   - previews off: the email says who and how many, never what;
//   - A is here (a socket open): nothing is noted, no email.
//
//   node tools/e2e/digest.e2e.mjs [https://localhost:8443]
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, launch, signedIn, until, ok, fail, failed, sleep, offOrigin, BASE, MAILPIT } from './meet.lib.mjs';

const SHOTS = join(dirname(fileURLToPath(import.meta.url)), '.shots');
mkdirSync(SHOTS, { recursive: true });
const SUBJECT = 'Unread messages in Aether';
const PERIOD = 3000; // "10m" at 300 ms a minute
const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const token = () => Math.random().toString(36).slice(2, 10);

// Every digest mailed to an address, oldest first, with its text.
const digests = async (to) => {
  const r = await (await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent(`to:${to} subject:"${SUBJECT}"`)}`)).json();
  const out = [];
  for (const m of r.messages ?? []) {
    const full = await (await fetch(`${MAILPIT}/api/v1/message/${m.ID}`)).json();
    out.push({ id: m.ID, at: Date.parse(m.Created), from: full.From?.Address, text: (full.Text || '').replace(/\r\n/g, '\n') });
  }
  return out.sort((a, b) => a.at - b.at);
};
const withWord = async (to, word, budget) => {
  const t0 = Date.now();
  for (;;) {
    const hit = (await digests(to)).filter((d) => d.text.includes(word));
    if (hit.length || Date.now() - t0 > budget) return hit;
    await sleep(200);
  }
};
const openAccount = async (page) => {
  await page.evaluate(async () => {
    document.getElementById('console-body').innerHTML = '';
    (await import('/src/console.js')).openConsole('account');
  });
  return until(page, () => document.getElementById('np-email') && !document.getElementById('np-email').disabled, null);
};
const stored = (page) => page.evaluate(async () => (await (await import('/src/api.js')).messaging.notifyPrefs()).notify);
const setPrefs = (page, change) =>
  page.evaluate(async (change) => {
    const { messaging } = await import('/src/api.js');
    return (await messaging.setNotifyPrefs({ ...(await messaging.notifyPrefs()).notify, ...change })).notify;
  }, change);

try {
  let A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const ctxA = A.browserContext();
  const aId = await A.evaluate(async () => (await import('/src/api.js')).session.user.id);
  const conv = await B.evaluate(async (id) => (await (await import('/src/api.js')).messaging.openDirectById(id)).id, aId);
  const say = (body) => B.evaluate(async (conv, body) => (await import('/src/api.js')).api.post(`/api/messaging/conversations/${conv}/messages`, { body }), conv, body);
  // A closes Aether (every socket of theirs goes) and comes back.
  const leave = async () => {
    await A.close();
    await sleep(700);
  };
  const back = async () => {
    A = await ctxA.newPage();
    A.on('pageerror', (e) => log.push(`A PAGE ERROR: ${e}`));
    await A.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  };
  await setPrefs(A, { direct: 'alert', mention: 'alert', preview: true, email: 'off' }); // a known start
  await A.evaluate(async (conv) => (await import('/src/api.js')).messaging.markRead(conv), conv);
  const before = (await digests(acc.A.email)).length;

  // The control.
  ok(await openAccount(A), "A's account page reads the settings");
  const row = await A.evaluate(() => {
    const r = document.getElementById('np-email-row');
    return { shown: getComputedStyle(r).display !== 'none' && r.getBoundingClientRect().height > 0, value: document.getElementById('np-email').value, options: [...document.getElementById('np-email').options].map((o) => o.value).join('/') };
  });
  ok(row.shown, 'a period is offered: this server can send mail');
  ok(row.value === 'off' && row.options === 'off/10m/hourly/8h/daily', `it starts off, and offers the five periods (${JSON.stringify(row)})`);
  await A.evaluate(() => {
    const el = document.getElementById('np-email');
    el.value = '10m';
    el.dispatchEvent(new Event('change', { bubbles: true }));
  });
  ok(await until(A, () => document.getElementById('np-state')?.textContent === 'Saved.', null), 'choosing every 10 minutes is saved');
  ok((await stored(A)).email === '10m', 'and is what the server holds');
  await A.reload({ waitUntil: 'networkidle2' });
  ok((await openAccount(A)) && (await A.evaluate(() => document.getElementById('np-email').value)) === '10m', 'after a reload it still says every 10 minutes');

  // On a phone.
  await A.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('np-email-row').scrollIntoView({ block: 'center' }));
  const g = await A.evaluate(() => {
    const r = document.getElementById('np-email-row').getBoundingClientRect();
    const sel = document.getElementById('np-email').getBoundingClientRect();
    const hint = document.querySelector('#np-email-row .hint');
    return { vw: innerWidth, page: document.documentElement.scrollWidth, left: Math.round(r.left), right: Math.round(r.right), selH: Math.round(sel.height), selW: Math.round(sel.width), cut: hint.scrollWidth > hint.clientWidth + 1 };
  });
  ok(g.page <= g.vw && g.left >= 0 && g.right <= g.vw && !g.cut, `phone: the row is inside the viewport, its words not cut off (${JSON.stringify(g)})`);
  ok(g.selH >= 44 && g.selW >= 44, 'phone: the control is at least 44px');
  await A.screenshot({ path: join(SHOTS, 'digest-row-phone.png') });
  await A.setViewport({ width: 1280, height: 800 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('np-email-row').scrollIntoView({ block: 'center' }));
  await A.screenshot({ path: join(SHOTS, 'digest-row-desktop.png') });

  // Missed: one email, when the period ends, saying what is unread.
  await leave();
  const one = token();
  const sentAt = Date.now();
  await say('are you there?');
  await say(`the build is red ${one}`);
  await sleep(1200);
  ok((await withWord(acc.A.email, one, 0)).length === 0, 'nothing is mailed before the period ends');
  const got = await withWord(acc.A.email, one, PERIOD + 6000);
  ok(got.length === 1, `one email reaches A's address after the period (${got.length})`);
  const mail = got[0] ?? { text: '', at: 0 };
  ok(mail.at - sentAt >= PERIOD - 400, `and not earlier: ${mail.at - sentAt} ms after the first message`);
  ok(mail.from === 'noreply@aether.test', `from the server's address (${mail.from})`);
  ok(mail.text.includes(`\nE2E B, 2 unread\n    "the build is red ${one}"\n`), `it says who, how many, and the newest words:\n${mail.text}`);
  ok(mail.text.includes(`\nOpen Aether: ${BASE}\n`) && mail.text.includes('every 10 minutes'), 'with the link back and the period it was sent under');
  const headers = mail.id ? await (await fetch(`${MAILPIT}/api/v1/message/${mail.id}/headers`)).json() : {};
  ok((headers['Auto-Submitted'] ?? [])[0] === 'auto-generated', 'marked as automatic, so no vacation responder answers it');
  await sleep(PERIOD + 1500);
  ok((await digests(acc.A.email)).length === before + 1, 'still unread, nothing new: no second email');

  // Missed, then read before the period ends.
  const two = token();
  await say(`standup moved ${two}`);
  await back();
  await A.evaluate(async (conv) => (await import('/src/api.js')).messaging.markRead(conv), conv);
  await leave();
  await sleep(PERIOD + 2000);
  ok((await withWord(acc.A.email, two, 0)).length === 0 && (await digests(acc.A.email)).length === before + 1, 'read before the period ended: no email');

  // Previews off: who and how many, never what.
  await back();
  ok((await setPrefs(A, { preview: false })).preview === false, 'A turns message previews off');
  await leave();
  const three = token();
  await say(`the door code is ${three}`);
  const t0 = Date.now();
  let hidden = [];
  while (Date.now() - t0 < PERIOD + 6000 && (hidden = await digests(acc.A.email)).length < before + 2) await sleep(200);
  ok(hidden.length === before + 2, 'a second email, for the new message');
  const quiet = hidden[hidden.length - 1]?.text ?? '';
  ok(quiet.includes('\nE2E B, 1 unread\n') && !quiet.includes(three) && !quiet.includes('door code'), `it says who and how many, not what:\n${quiet}`);

  // Here: the socket tells them, nothing is mailed.
  await back();
  await setPrefs(A, { preview: true });
  await A.evaluate(async (conv) => (await import('/src/api.js')).messaging.markRead(conv), conv);
  ok(await until(B, async (id) => (await (await import('/src/api.js')).messaging.statuses([id])).statuses?.[id]?.online === true, aId), 'A is back, and connected');
  const four = token();
  await say(`lunch? ${four}`);
  await sleep(PERIOD + 2000);
  ok((await digests(acc.A.email)).length === before + 2, 'a message to someone who is here is not mailed, though it stays unread');

  ok((await setPrefs(A, { email: 'off' })).email === 'off', 'left off, as found');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
  ok(offOrigin(log).length === 0, `nothing left the server's host (${offOrigin(log).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
