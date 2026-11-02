// The whiteboard among three real browsers, checked on the pixels:
//   - Share > Whiteboard turns it on for everyone;
//   - B draws: A sees the line WHILE B is still drawing (the live preview),
//     and after; the canvas pixels are read, not internal state;
//   - a sticky note reaches the others; the eraser removes B's line for A;
//   - Undo takes back A's own last stroke for B;
//   - C, joining late, is given the board as it is;
//   - only the opener or a host has Clear and "Turn off whiteboard"; turning
//     it off takes it away for everyone.
//
//   node tools/e2e/whiteboard.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const room = 'wb-' + Math.random().toString(36).slice(2, 8);
const shown = (p) => p.evaluate(() => !document.getElementById('wb').classList.contains('hidden'));
// Is there ink near the board point (fx, fy), as fractions of the board?
const ink = (p, fx, fy) => p.evaluate((fx, fy) => {
  const c = document.getElementById('wb-canvas');
  const ctx = c.getContext('2d');
  const x = Math.round(fx * c.width), y = Math.round(fy * c.height);
  const d = ctx.getImageData(Math.max(0, x - 3), Math.max(0, y - 3), 7, 7).data;
  for (let i = 0; i < d.length; i += 4) if (d[i] < 200 || d[i + 1] < 200 || d[i + 2] < 200) return true;
  return false;
}, fx, fy);
// Board fraction to page pixels on this page.
const at = (p, fx, fy) => p.evaluate((fx, fy) => {
  const r = document.getElementById('wb-canvas').getBoundingClientRect();
  return [r.left + fx * r.width, r.top + fy * r.height];
}, fx, fy);
const tool = (p, t) => p.evaluate((t) => document.querySelector(`[data-tool="${t}"]`).click(), t);
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  await enter(A, room);
  await enter(B, room);
  await sleep(2000);

  await A.evaluate(() => document.getElementById('btn-share-more').click());
  await sleep(150);
  await A.evaluate(() => document.getElementById('share-wb').click());
  ok(await until(A, () => !document.getElementById('wb').classList.contains('hidden'), null) && await until(B, () => !document.getElementById('wb').classList.contains('hidden'), null), 'Share > Whiteboard turns it on for both');
  await sleep(500);

  // B draws a line across the middle, slowly; A looks before B lets go.
  let [x0, y] = await at(B, 0.2, 0.5);
  const [x1] = await at(B, 0.8, 0.5);
  await B.mouse.move(x0, y);
  await B.mouse.down();
  for (let i = 1; i <= 20; i++) {
    await B.mouse.move(x0 + ((x1 - x0) * i) / 40, y);
    await sleep(15);
  }
  ok(await until(A, () => true, null) && await (async () => { await sleep(400); return ink(A, 0.3, 0.5); })(), 'A sees the line while B is still drawing');
  for (let i = 21; i <= 40; i++) await B.mouse.move(x0 + ((x1 - x0) * i) / 40, y);
  await B.mouse.up();
  await sleep(600);
  ok(await ink(A, 0.7, 0.5), 'and the whole line once B lets go');
  ok(!(await ink(A, 0.5, 0.2)), 'and nothing where nothing was drawn');

  // A sticky note.
  await tool(A, 'note');
  const [nx, ny] = await at(A, 0.1, 0.1);
  await A.mouse.click(nx, ny);
  await until(A, () => !!document.querySelector('#dialog textarea[name="text"]'), null);
  await A.evaluate(() => { document.querySelector('#dialog textarea[name="text"]').value = 'Decide by Friday'; document.querySelector('#dialog [data-yes]').click(); });
  ok(await until(B, () => [...document.querySelectorAll('.wb-note')].some((n) => n.textContent.includes('Decide by Friday')), null), 'the sticky note reaches B');

  // The eraser.
  await tool(B, 'eraser');
  const [ex, ey] = await at(B, 0.5, 0.5);
  await B.mouse.click(ex, ey);
  ok(await until(A, () => true, null) && await (async () => { await sleep(500); return !(await ink(A, 0.5, 0.5)); })(), 'B erases the line; it is gone for A');

  // Undo.
  await tool(A, 'pen');
  [x0, y] = await at(A, 0.3, 0.8);
  await A.mouse.move(x0, y);
  await A.mouse.down();
  await A.mouse.move(x0 + 80, y, { steps: 8 });
  await A.mouse.up();
  await sleep(600);
  const drawnFor = await at(B, 0.3, 0.8);
  ok(await ink(B, 0.31, 0.8), 'A draws a second line; B sees it');
  await A.evaluate(() => document.getElementById('wb-undo').click());
  await sleep(600);
  ok(!(await ink(B, 0.31, 0.8)), `Undo takes it back for B (${drawnFor.map(Math.round)})`);

  // A late arrival.
  await enter(C, room);
  ok(await until(C, () => !document.getElementById('wb').classList.contains('hidden') && [...document.querySelectorAll('.wb-note')].some((n) => n.textContent.includes('Decide by Friday')), null, 15000), 'C, joining late, is given the board as it is');

  // Who manages it.
  ok(await B.evaluate(() => !document.getElementById('wb-clear') && !document.getElementById('wb-close')), 'B, neither opener nor host, has no Clear or Turn off');
  ok(await A.evaluate(() => !!document.getElementById('wb-close')), 'the opener has "Turn off whiteboard"');
  await A.evaluate(() => document.getElementById('wb-close').click());
  for (const [p, l] of [[A, 'A'], [B, 'B'], [C, 'C']]) ok(await until(p, () => document.getElementById('wb').classList.contains('hidden'), null), `turned off for ${l}`);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
