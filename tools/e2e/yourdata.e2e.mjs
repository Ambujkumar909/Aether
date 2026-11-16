// "Your data" on the account page, in a real browser: the button fetches
// the signed-in person's own export and hands it to the browser as a file;
// what is handed over is read back here and is theirs; a refusal (six an
// hour, two at once) is said on the page; the card fits a phone.
//
//   node tools/e2e/yourdata.e2e.mjs [https://localhost:8443]
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, launch, signedIn, until, ok, fail, failed, offOrigin } from './meet.lib.mjs';

const SHOTS = join(dirname(fileURLToPath(import.meta.url)), '.shots');
mkdirSync(SHOTS, { recursive: true });
const acc = await accounts(['A']);
const browser = await launch();
const log = [];

const openAccount = async (page) => {
  await page.evaluate(async () => {
    document.getElementById('console-body').innerHTML = '';
    (await import('/src/console.js')).openConsole('account');
  });
  return until(page, () => !!document.getElementById('data-export'), null);
};
// What the page hands the browser to save: the blob behind the object URL
// and the name it is saved under.
const CATCH = () => {
  window.__saved = [];
  const make = URL.createObjectURL.bind(URL);
  URL.createObjectURL = (blob) => {
    const entry = { text: null, name: null };
    window.__saved.push(entry);
    blob.text().then((t) => (entry.text = t));
    return make(blob);
  };
  const click = HTMLAnchorElement.prototype.click;
  HTMLAnchorElement.prototype.click = function () {
    if (this.download && window.__saved.length) window.__saved.at(-1).name = this.download;
    else click.call(this);
  };
};
const state = (page) => page.evaluate(() => document.getElementById('data-state').textContent);
const press = (page) => page.evaluate(() => document.getElementById('data-export').click());

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  ok(await openAccount(A), "A's account page has the card");
  await A.evaluate(CATCH);
  await press(A);
  ok(await until(A, () => document.getElementById('data-state').textContent === 'Downloaded.', null), 'pressing the button ends in "Downloaded."');
  ok(await until(A, () => window.__saved[0]?.text != null, null), 'a file was handed to the browser');
  const saved = await A.evaluate(() => window.__saved[0]);
  ok(/^aether-export-\d{4}-\d\d-\d\d\.json$/.test(saved.name ?? ''), `under a dated name (${saved.name})`);
  let doc = null;
  try {
    doc = JSON.parse(saved.text);
  } catch {}
  ok(doc?.account?.[0]?.email === acc.A.email, 'it is JSON and the account in it is the one signed in');
  ok(Array.isArray(doc?.messages) && Array.isArray(doc?.sessions) && doc.sessions.length >= 1, 'with the messages and sessions sections');
  ok(await A.evaluate(() => !document.getElementById('data-export').disabled), 'the button is ready again');

  // Two at once are allowed; the third is refused, and the page says so.
  await press(A);
  await until(A, () => window.__saved.length === 2 && document.getElementById('data-state').textContent === 'Downloaded.', null);
  await press(A);
  ok(await until(A, () => /Try again later/.test(document.getElementById('data-state').textContent), null), `a third copy straight away is refused in words (${await state(A)})`);
  ok((await A.evaluate(() => window.__saved.length)) === 2, 'and nothing is saved for it');

  // On a phone.
  await A.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('data-card').scrollIntoView({ block: 'center' }));
  const g = await A.evaluate(() => {
    const card = document.getElementById('data-card').getBoundingClientRect();
    const b = document.getElementById('data-export').getBoundingClientRect();
    const hint = document.querySelector('#data-card .hint');
    return { vw: innerWidth, page: document.documentElement.scrollWidth, left: Math.round(card.left), right: Math.round(card.right), bh: Math.round(b.height), bw: Math.round(b.width), cut: hint.scrollWidth > hint.clientWidth + 1 };
  });
  ok(g.page <= g.vw && g.left >= 0 && g.right <= g.vw && !g.cut, `phone: the card is inside the viewport, nothing cut off (${JSON.stringify(g)})`);
  ok(g.bh >= 44 && g.bw >= 44, 'phone: the button is at least 44px');
  await A.screenshot({ path: join(SHOTS, 'yourdata-phone.png') });
  await A.setViewport({ width: 1280, height: 800 });
  await A.reload({ waitUntil: 'networkidle2' });
  await openAccount(A);
  await A.evaluate(() => document.getElementById('data-card').scrollIntoView({ block: 'center' }));
  await A.screenshot({ path: join(SHOTS, 'yourdata-desktop.png') });

  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
  ok(offOrigin(log).length === 0, `nothing left the server's host (${offOrigin(log).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
