// Out of office, in two real browsers:
//   - A turns it on from the account page (reached from the status menu);
//     what the page shows is read back from the server each time;
//   - B, with the chat with A already open, is told without reloading: a
//     line above the message box, the words beside A's name, A's card;
//   - a reload shows the same (the read path), at 390, 820 and 1280 wide
//     with nothing cut off or pushed off screen (screenshots kept);
//   - moving it into the future takes the line away again; an empty message
//     is refused and nothing changes;
//   - a window that ends while everybody sits still is noticed by the
//     server's status loop: the line goes with no one touching anything.
//
//   node tools/e2e/ooo.e2e.mjs [https://localhost:8443]
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, launch, signedIn, until, ok, fail, failed, sleep, offOrigin } from './meet.lib.mjs';

const SHOTS = join(dirname(fileURLToPath(import.meta.url)), '.shots');
mkdirSync(SHOTS, { recursive: true });
const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const MESSAGE = 'Back on Monday. Asha is covering the release.';

// A datetime-local value for a moment, in the page's own zone.
const localValue = (ms) => {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
};
const stateLine = (page) => page.evaluate(() => document.getElementById('ooo-state')?.textContent ?? '');
const note = (page) => page.evaluate(() => document.querySelector('.ooo-note')?.textContent.replace(/\s+/g, ' ').trim() ?? '');
const openThread = async (page, peerId) => {
  await page.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
  await page.evaluate(async (id) => (await import('/src/chat.js')).openChatWith(id), peerId);
  return until(page, () => !!document.querySelector('.composer'), null);
};
const save = async (page, { message, from = '', to = '' }) => {
  await page.evaluate(
    (m, f, t) => {
      document.getElementById('ooo-msg').value = m;
      document.getElementById('ooo-from').value = f;
      document.getElementById('ooo-until').value = t;
      document.getElementById('ooo-save').click();
    },
    message,
    from,
    to,
  );
};

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const aId = await A.evaluate(async () => (await import('/src/api.js')).session.user.id);
  await A.evaluate(async () => (await import('/src/api.js')).messaging.clearOOO()); // a known start

  ok(await openThread(B, aId), 'B has the chat with A open');
  ok((await note(B)) === '', 'nothing is said above the box when A is not away');

  // A: the status menu has the row; it leads to the card on the account page.
  await A.evaluate(() => document.getElementById('top-avatar')?.click());
  ok(await until(A, () => !!document.getElementById('st-ooo'), null), "A's status menu has an out of office row");
  ok(!(await A.evaluate(() => document.getElementById('st-ooo').classList.contains('on'))), 'which does not say "on" yet');
  await A.evaluate(() => document.getElementById('st-ooo').click());
  ok(await until(A, () => document.getElementById('ooo-state')?.textContent === 'Off.', null), 'it opens the account page, whose card reads the server: Off.');
  ok(await A.evaluate(() => getComputedStyle(document.getElementById('ooo-off')).display === 'none' && document.getElementById('ooo-save').textContent === 'Turn on'), 'while it is off the card offers Turn on, and no Turn off');

  await save(A, { message: '   ' });
  ok(await until(A, () => /message of 280/.test(document.getElementById('toast')?.textContent ?? ''), null), 'an empty message is refused, in words');
  await sleep(300);
  ok((await stateLine(A)) === 'Off.', 'and nothing changed');

  const end = Date.now() + 3 * 24 * 3600e3;
  await save(A, { message: MESSAGE, to: localValue(end) });
  ok(await until(A, () => /^On until /.test(document.getElementById('ooo-state')?.textContent ?? ''), null), `turned on: the card says so (${await stateLine(A)})`);
  ok(await A.evaluate(() => getComputedStyle(document.getElementById('ooo-off')).display !== 'none' && document.getElementById('ooo-save').textContent === 'Save'), 'and now offers Save and Turn off');

  // B is told without reloading.
  ok(await until(B, (m) => (document.querySelector('.ooo-note')?.textContent ?? '').includes(m), MESSAGE), 'B sees the line above the message box, pushed');
  const said = await note(B);
  ok(/^E2E A is out of office until \S+/.test(said) && said.endsWith(MESSAGE), `it names A, the end and the message (${said})`);
  ok(await until(B, () => /Out of office until/.test(document.querySelector('.thread-head, .chat-head, header')?.textContent ?? document.body.textContent), null), "and A's line in the header says it too");
  await B.evaluate(async (id) => (await import('/src/chat.js')).openPersonCard(id), aId);
  ok(await until(B, (m) => document.querySelector('.person-card .pc-ooo')?.textContent.includes(m), MESSAGE), "A's card carries the message");
  ok(await B.evaluate(() => /Out of office until/.test(document.querySelector('.person-card .pc-status')?.textContent ?? '')), 'and the words beside the status');
  await B.keyboard.press('Escape');
  await B.evaluate(() => document.querySelector('.person-card')?.closest('.dialog, #dialog, .dialog-host')?.click());

  // The read path, at three widths: reload, reopen, look.
  for (const vp of [
    { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 },
    { name: 'tablet', width: 820, height: 1180, isMobile: true, hasTouch: true, deviceScaleFactor: 2 },
    { name: 'desktop', width: 1280, height: 800 },
  ]) {
    await B.setViewport(vp);
    await B.reload({ waitUntil: 'networkidle2' });
    await openThread(B, aId);
    ok(await until(B, (m) => (document.querySelector('.ooo-note')?.textContent ?? '').includes(m), MESSAGE), `${vp.name}: the line is there after a reload`);
    const g = await B.evaluate(() => {
      const n = document.querySelector('.ooo-note');
      const c = document.querySelector('.composer');
      const r = n.getBoundingClientRect();
      const cr = c.getBoundingClientRect();
      return {
        vw: innerWidth,
        vh: innerHeight,
        page: document.documentElement.scrollWidth,
        left: Math.round(r.left),
        right: Math.round(r.right),
        clipped: n.scrollWidth > n.clientWidth + 1,
        above: r.bottom <= cr.top + 1,
        composerBottom: Math.round(cr.bottom),
      };
    });
    ok(g.page <= g.vw && g.left >= 0 && g.right <= g.vw && !g.clipped, `${vp.name}: inside the viewport, nothing cut off (${JSON.stringify(g)})`);
    ok(g.above && g.composerBottom <= g.vh, `${vp.name}: it sits above the box, and the box is still on screen`);
    await B.screenshot({ path: join(SHOTS, `ooo-thread-${vp.name}.png`) });
  }
  await B.setViewport({ width: 1280, height: 800 });
  await B.reload({ waitUntil: 'networkidle2' });
  await openThread(B, aId);

  // A's own menu now says so.
  await A.evaluate(async () => (await import('/src/console.js')).closeConsole());
  await A.evaluate(() => document.getElementById('top-avatar')?.click());
  ok(await until(A, () => document.getElementById('st-ooo')?.classList.contains('on'), null), 'A\'s status menu row says "on"');
  await sleep(500); // the menu fades in; the picture is for looking at
  await A.screenshot({ path: join(SHOTS, 'ooo-menu-desktop.png') });
  await A.evaluate(() => document.getElementById('st-ooo').click());
  await until(A, () => /^On until /.test(document.getElementById('ooo-state')?.textContent ?? ''), null);
  await A.evaluate(() => document.getElementById('ooo-card').scrollIntoView());
  await A.screenshot({ path: join(SHOTS, 'ooo-card-desktop.png') });

  // Moved into the future: stored, and the line goes from B's screen.
  await save(A, { message: MESSAGE, from: localValue(Date.now() + 24 * 3600e3), to: localValue(end) });
  ok(await until(A, () => /^Starts /.test(document.getElementById('ooo-state')?.textContent ?? ''), null), `planned for tomorrow: the card says when it starts (${await stateLine(A)})`);
  ok(await until(B, () => !document.querySelector('.ooo-note'), null), 'and B is no longer told A is away');

  await A.evaluate(() => document.getElementById('ooo-off').click());
  ok(await until(A, () => document.getElementById('ooo-state')?.textContent === 'Off.', null), 'turned off: Off.');

  // A window that ends by itself. Nobody clicks anything after this call.
  await A.evaluate(async (ms) => (await import('/src/api.js')).messaging.setOOO('Back in a moment', null, new Date(Date.now() + ms).toISOString()), 6000);
  ok(await until(B, () => (document.querySelector('.ooo-note')?.textContent ?? '').includes('Back in a moment'), null), 'a short one is shown');
  const t0 = Date.now();
  ok(await until(B, () => !document.querySelector('.ooo-note'), null, 30000), `and goes when its window ends, with nobody touching anything (${((Date.now() - t0) / 1000).toFixed(1)} s after it was set)`);
  ok((await A.evaluate(async () => (await (await import('/src/api.js')).messaging.ooo()).ooo)) === null, "A's own read agrees: none");

  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
  ok(offOrigin(log).length === 0, `nothing left the server's host (${offOrigin(log).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
