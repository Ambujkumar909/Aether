// Keyboard shortcuts, in a real browser:
//   - at home: Ctrl+2 opens the calendar, / focuses the people search,
//     ? opens the sheet (which names every binding) and Escape closes it;
//   - in a meeting: M mutes and unmutes, V toggles the camera, C opens the
//     chat; while typing in the chat, M is a letter and Ctrl+Shift+M still
//     mutes; H raises the hand; Ctrl+Space held while muted unmutes until
//     released;
//   - on a phone the sheet fits inside the viewport.
//
//   node tools/e2e/shortcuts.e2e.mjs [https://localhost:8443]
import { accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const room = 'keys-' + Math.random().toString(36).slice(2, 8);
const chord = async (page, mods, key) => {
  for (const m of mods) await page.keyboard.down(m);
  await page.keyboard.press(key);
  for (const m of [...mods].reverse()) await page.keyboard.up(m);
};
const micOff = (p) => p.evaluate(() => document.getElementById('btn-mic').classList.contains('off'));
const camOff = (p) => p.evaluate(() => document.getElementById('btn-cam').classList.contains('off'));
const sheetOpen = (p) => p.evaluate(() => !!document.querySelector('#dialog .sc-sheet'));

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  // Home.
  await A.evaluate(() => document.body.focus());
  await chord(A, ['Control'], '2');
  ok(await until(A, () => document.querySelector('[data-home="calendar"]')?.classList.contains('active'), null), 'Ctrl+2 opens the calendar');
  await chord(A, ['Control'], '1');
  ok(await until(A, () => document.querySelector('[data-home="meet"]')?.classList.contains('active'), null), 'Ctrl+1 is back to meetings');
  await A.keyboard.press('Slash');
  ok(await until(A, () => document.activeElement?.id === 'top-find', null), '/ focuses the people search');
  await A.keyboard.press('Escape');
  await A.evaluate(() => document.activeElement?.blur());
  await A.keyboard.press('?');
  ok(await until(A, () => !!document.querySelector('#dialog .sc-sheet'), null), '? opens the shortcuts sheet');
  const sheet = await A.evaluate(() => document.querySelector('#dialog .sc-sheet')?.textContent || '');
  ok(/Mute or unmute/.test(sheet) && /Ctrl \+ Shift \+ M/.test(sheet) && /Hold to talk/.test(sheet) && /Schedule a meeting/.test(sheet), 'the sheet names the bindings the way the keyboard is labelled');
  await A.keyboard.press('Escape');
  ok(await until(A, () => !document.querySelector('#dialog .sc-sheet'), null), 'Escape closes it');
  // Typing ? in the search box is typing.
  await A.evaluate(() => document.getElementById('top-find').focus());
  await A.keyboard.press('?');
  await sleep(300);
  ok(!(await sheetOpen(A)), 'typing ? in a box does not open the sheet');
  await A.evaluate(() => { const f = document.getElementById('top-find'); f.value = ''; f.blur(); });

  // In a meeting.
  await enter(A, room);
  await sleep(1500);
  const mic0 = await micOff(A);
  await A.evaluate(() => document.activeElement?.blur());
  await A.keyboard.press('m');
  ok(await until(A, (was) => document.getElementById('btn-mic').classList.contains('off') !== was, mic0), 'M toggles the microphone');
  await A.keyboard.press('m');
  await until(A, (was) => document.getElementById('btn-mic').classList.contains('off') === was, mic0);
  const cam0 = await camOff(A);
  await A.keyboard.press('v');
  ok(await until(A, (was) => document.getElementById('btn-cam').classList.contains('off') !== was, cam0), 'V toggles the camera');
  await A.keyboard.press('v');
  await sleep(300);
  await A.keyboard.press('c');
  ok(await until(A, () => !document.getElementById('shell').classList.contains('panel-closed') && document.getElementById('view-chat')?.classList.contains('active'), null), 'C opens the chat panel');
  // Typing in the chat: letters are letters; the chord still mutes.
  await A.evaluate(() => document.getElementById('chat-input').focus());
  const micBefore = await micOff(A);
  await A.keyboard.type('mmm');
  await sleep(300);
  ok((await micOff(A)) === micBefore && (await A.evaluate(() => document.getElementById('chat-input').value)) === 'mmm', 'typing m in the chat types m');
  await chord(A, ['Control', 'Shift'], 'M');
  ok(await until(A, (was) => document.getElementById('btn-mic').classList.contains('off') !== was, micBefore), 'Ctrl+Shift+M mutes even while typing');
  await A.evaluate(() => { const c = document.getElementById('chat-input'); c.value = ''; c.blur(); });
  // Back to muted for the hold-to-talk check.
  if (!(await micOff(A))) {
    await A.keyboard.press('m');
    await until(A, () => document.getElementById('btn-mic').classList.contains('off'), null);
  }
  await A.keyboard.down('Control');
  await A.keyboard.down('Space');
  ok(await until(A, () => !document.getElementById('btn-mic').classList.contains('off'), null), 'Ctrl+Space held while muted unmutes');
  await A.keyboard.up('Space');
  await A.keyboard.up('Control');
  ok(await until(A, () => document.getElementById('btn-mic').classList.contains('off'), null), 'and letting go mutes again');
  const hand0 = await A.evaluate(() => document.getElementById('btn-hand').outerHTML);
  await A.keyboard.press('h');
  ok(await until(A, (was) => document.getElementById('btn-hand').outerHTML !== was, hand0), 'H raises the hand');
  await A.keyboard.press('h');
  await sleep(300);

  // A phone: the sheet fits.
  const P = await signedIn(browser, acc.B, 'B', log, { width: 390, height: 844 });
  await P.evaluate(() => document.activeElement?.blur());
  await P.keyboard.press('?');
  const fit = await until(P, () => {
    const d = document.querySelector('#dialog .sc-sheet');
    if (!d) return false;
    const r = d.getBoundingClientRect();
    return r.right <= innerWidth + 1 && d.scrollWidth <= d.clientWidth + 2;
  }, null);
  ok(fit, 'on a phone the sheet is inside the viewport with nothing cut off');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
