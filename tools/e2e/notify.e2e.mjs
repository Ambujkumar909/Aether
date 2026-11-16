// Notification settings, in two real browsers:
//   - the account page reads A's settings from the server before a control
//     can be touched, saves every change whole, and shows the same after a
//     reload;
//   - A sits on the home screen with the banner hook the Windows shell uses
//     (window.aetherNotify) recording what would be shown. B writes to A:
//     with direct messages quiet nothing is shown and the chat still counts
//     as unread; an urgent message is shown whatever is set; a mention is
//     its own setting; with previews off the banner says who, not what;
//   - at 390 wide the card is inside the viewport with 44px controls.
//
//   node tools/e2e/notify.e2e.mjs [https://localhost:8443]
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, launch, signedIn, until, ok, fail, failed, sleep, offOrigin } from './meet.lib.mjs';

const SHOTS = join(dirname(fileURLToPath(import.meta.url)), '.shots');
mkdirSync(SHOTS, { recursive: true });
const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const DEFAULTS = { direct: 'alert', group: 'alert', mention: 'alert', reply: 'alert', reaction: 'quiet', preview: true, email: 'off' };

const openAccount = async (page) => {
  // Emptied first: the page redraws after reading the server, and a card
  // left from the last visit would be found, enabled, and then replaced
  // under the next click (which is what the first run of this did).
  await page.evaluate(async () => {
    document.getElementById('console-body').innerHTML = '';
    (await import('/src/console.js')).openConsole('account');
  });
  return until(page, () => document.getElementById('np-direct') && !document.getElementById('np-direct').disabled, null);
};
const choose = (page, id, value) =>
  page.evaluate(
    (id, value) => {
      const el = document.getElementById(id);
      if (el.type === 'checkbox') el.checked = value;
      else el.value = value;
      el.dispatchEvent(new Event('change', { bubbles: true }));
    },
    id,
    value,
  );
const saved = (page) => until(page, () => document.getElementById('np-state')?.textContent === 'Saved.', null);
const stored = (page) => page.evaluate(async () => (await (await import('/src/api.js')).messaging.notifyPrefs()).notify);
const shown = (page) => page.evaluate(() => window.__notes.splice(0));

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const aId = await A.evaluate(async () => (await import('/src/api.js')).session.user.id);
  await A.evaluate(async (d) => (await import('/src/api.js')).messaging.setNotifyPrefs(d), DEFAULTS); // a known start
  await A.evaluate(async () => (await import('/src/api.js')).messaging.setStatus('available'));
  const conv = await B.evaluate(async (id) => (await (await import('/src/api.js')).messaging.openDirectById(id)).id, aId);
  const say = (body, extra = {}) =>
    B.evaluate(async (conv, body, extra) => (await import('/src/api.js')).api.post(`/api/messaging/conversations/${conv}/messages`, { body, ...extra }), conv, body, extra);

  // The card: nothing can be changed until the server has been read.
  ok(await openAccount(A), "A's account page has the notification card, enabled once read");
  const first = await A.evaluate(() => Object.fromEntries([...document.querySelectorAll('[data-np]')].map((s) => [s.dataset.np, s.value]).concat([['preview', document.getElementById('np-preview').checked], ['email', document.getElementById('np-email').value]])));
  ok(JSON.stringify(first) === JSON.stringify(DEFAULTS), `it shows what is stored (${JSON.stringify(first)})`);
  const offered = await A.evaluate(() => Object.fromEntries([...document.querySelectorAll('[data-np]')].map((s) => [s.dataset.np, [...s.options].map((o) => o.value).join('/')])));
  ok(offered.direct === 'alert/quiet' && offered.mention === 'alert/quiet' && offered.reply === 'alert/quiet/off' && offered.reaction === 'quiet/off', `each kind offers only the levels it has (${JSON.stringify(offered)})`);

  await choose(A, 'np-direct', 'quiet');
  ok(await saved(A), 'choosing Quiet for direct messages is saved');
  ok((await stored(A)).direct === 'quiet', 'and is what the server holds');
  await A.reload({ waitUntil: 'networkidle2' });
  ok(await openAccount(A), 'after a reload the card is read again');
  ok((await A.evaluate(() => document.getElementById('np-direct').value)) === 'quiet', 'and still says Quiet');

  // A at home, the banner hook recording. (The hook is the Windows shell's;
  // in a browser the same call goes to the Notification API.)
  await A.evaluate(async () => (await import('/src/console.js')).closeConsole());
  await A.evaluate(() => {
    window.__notes = [];
    window.aetherNotify = (title, body) => window.__notes.push({ title, body });
  });
  const unreadOf = () => A.evaluate(async (id) => ((await (await import('/src/api.js')).messaging.list()).conversations ?? []).find((c) => c.id === id)?.unread ?? 0, conv);
  const before = await unreadOf();
  await say('are you there?');
  await sleep(1200);
  ok((await shown(A)).length === 0, 'direct messages quiet: a message from B shows no banner');
  ok((await unreadOf()) === before + 1, 'and still counts as unread');
  await say('the build server is on fire', { priority: 'urgent' });
  ok(await until(A, () => window.__notes.length === 1, null), 'an urgent one is shown: urgent is not a setting');
  ok(/^URGENT · the build server is on fire$/.test((await shown(A))[0]?.body ?? ''), 'with its words');

  // A mention is its own setting, whatever the chat it is written in.
  await say('@E2E A could you look?', { mentions: [aId] });
  const named = await until(A, () => window.__notes.length === 1, null);
  ok(named && /mentioned you/.test((await shown(A))[0]?.title ?? ''), 'a mention in that quiet chat is shown: mentions are still on Alert');
  await openAccount(A);
  await choose(A, 'np-mention', 'quiet');
  ok(await saved(A), 'mentions set quiet');
  await say('@E2E A and again?', { mentions: [aId] });
  await sleep(1200);
  ok((await shown(A)).length === 0, 'and now a mention shows nothing');

  // Previews: who, not what.
  await choose(A, 'np-direct', 'alert');
  await saved(A);
  await choose(A, 'np-preview', false);
  ok(await until(A, async () => (await (await import('/src/api.js')).messaging.notifyPrefs()).notify.preview === false, null), 'previews turned off');
  const secret = 'the door code is ' + Math.random().toString(36).slice(2, 8);
  await say(secret);
  ok(await until(A, () => window.__notes.length === 1, null), 'a direct message is shown again');
  const hidden = (await shown(A))[0];
  ok(hidden?.body === 'New message' && !JSON.stringify(hidden).includes(secret), `without its words (${JSON.stringify(hidden)})`);
  await choose(A, 'np-preview', true);
  await until(A, async () => (await (await import('/src/api.js')).messaging.notifyPrefs()).notify.preview === true, null);
  await say('back to normal');
  ok(await until(A, () => window.__notes.some((n) => n.body === 'back to normal'), null), 'previews on again: the words are shown');

  // The card on a phone.
  await A.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('notify-card').scrollIntoView());
  const g = await A.evaluate(() => {
    const card = document.getElementById('notify-card').getBoundingClientRect();
    const small = [...document.querySelectorAll('#notify-card select, #notify-card input')].map((e) => e.getBoundingClientRect()).filter((r) => r.height < 44 || r.width < 44).length;
    const cut = [...document.querySelectorAll('#notify-card .np-row span, #notify-card select')].filter((e) => e.scrollWidth > e.clientWidth + 1).length;
    return { vw: innerWidth, page: document.documentElement.scrollWidth, left: Math.round(card.left), right: Math.round(card.right), small, cut };
  });
  ok(g.page <= g.vw && g.left >= 0 && g.right <= g.vw && g.cut === 0, `phone: the card is inside the viewport, nothing cut off (${JSON.stringify(g)})`);
  ok(g.small === 0, 'phone: every control on it is at least 44px');
  await A.screenshot({ path: join(SHOTS, 'notify-card-phone.png') });
  await A.setViewport({ width: 1280, height: 800 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('notify-card').scrollIntoView());
  await A.screenshot({ path: join(SHOTS, 'notify-card-desktop.png') });

  await A.evaluate(async (d) => (await import('/src/api.js')).messaging.setNotifyPrefs(d), DEFAULTS); // as found
  ok(JSON.stringify(await stored(A)) === JSON.stringify(DEFAULTS), 'left at the defaults');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
  ok(offOrigin(log).length === 0, `nothing left the server's host (${offOrigin(log).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
