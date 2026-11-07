// Email-verified sign-up, end to end: the sign-in card in a phone-sized
// browser, a real SMTP server (Mailpit, tools/e2e/mailpit.sh), the emailed
// link opened, the password chosen at sign-up typed again.
//
// The server must run with mail on and links pointing at BASE (as for
// reset.e2e.mjs). Run after a harness that has made account A (console).
//
//   node tools/e2e/signup.e2e.mjs https://localhost:9443
import { accounts, launch, sleep, ok, fail, failed, BASE, until, MAILPIT, mailedToken } from './meet.lib.mjs';

const log = [];
const A = (await accounts(['A'])).A; // an address that already has an account
const browser = await launch();
const open = async () => {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  page.on('pageerror', (e) => log.push('PAGE ERROR ' + e.message));
  await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
  return page;
};
const shown = (page, id) =>
  page.evaluate((id) => {
    const e = document.getElementById(id);
    return !!e && !e.classList.contains('hidden') && e.getClientRects().length > 0;
  }, id);
const text = (page, sel) => page.evaluate((s) => document.querySelector(s)?.textContent.trim() ?? '', sel);
const typeInto = async (page, id, v) => {
  await page.evaluate((id) => (document.getElementById(id).value = ''), id);
  await page.type('#' + id, v);
};
const post = (path, body) => fetch(BASE + path, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
const mails = async (to) => (await (await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent('to:' + to)}`)).json()).messages ?? [];

try {
  const email = `e2e-new-${Date.now().toString(36)}@example.test`;
  const pwOwner = 'owner-' + Math.random().toString(36).slice(2, 12);
  const pwStranger = 'stranger-' + Math.random().toString(36).slice(2, 12);

  // Sign up in the browser.
  const page = await open();
  await page.goto(BASE, { waitUntil: 'networkidle2' });
  await until(page, () => !document.getElementById('auth').classList.contains('hidden'));
  await page.click('#swap-btn');
  await typeInto(page, 'su-name', 'New Person');
  await typeInto(page, 'su-email', email);
  await typeInto(page, 'su-pass', pwOwner);
  await page.click('#su-submit');
  await until(page, () => !document.getElementById('su-done').classList.contains('hidden'), undefined, 10000);
  ok(/Check your email/.test(await text(page, '#su-done')), `sign-up says to check email ("${await text(page, '#su-done')}")`);
  ok(!(await shown(page, 'home')), 'and nothing is signed in yet');
  const early = await post('/api/identity/login', { email, password: pwOwner });
  ok(early.status === 401, `the account does not exist before the link is used (${early.status})`);

  const ownerToken = await mailedToken(email, 'verify');
  const first = (await mails(email))[0];
  ok(first?.Subject === 'Finish creating your Aether account', `the email arrived (${first?.Subject})`);

  // A stranger signs up for the same address with their own password; their
  // link goes to the owner's inbox too.
  const r = await post('/api/identity/signup', { email, displayName: 'Not Them', password: pwStranger });
  const rb = await r.json();
  ok(r.status === 202 && rb.verify, `a second sign-up for the address answers the same (${r.status})`);
  let strangerToken = '';
  for (let i = 0; i < 40 && !strangerToken; i++) {
    for (const m of await mails(email)) {
      const full = await (await fetch(`${MAILPIT}/api/v1/message/${m.ID}`)).json();
      const t = /#verify=([A-Za-z0-9_-]+)/.exec(full.Text || '')?.[1];
      if (t && t !== ownerToken) strangerToken = t;
    }
    if (!strangerToken) await sleep(250);
  }
  ok(!!strangerToken, 'the stranger\'s attempt mailed its own link');

  // Opening the stranger's link with the owner's password does nothing.
  const p2 = await open();
  await p2.goto(`${BASE}/#verify=${strangerToken}`, { waitUntil: 'networkidle2' });
  await until(p2, () => !document.getElementById('verify-form').classList.contains('hidden'));
  ok((await text(p2, '.auth-tagline')) === 'Finish creating your account', 'a sign-up link opens Finish creating your account');
  ok(!(await p2.evaluate(() => location.href)).includes('verify='), 'and the token is wiped from the address bar');
  const fit = await p2.evaluate(() => ({
    wide: document.documentElement.scrollWidth > innerWidth,
    small: [...document.querySelectorAll('#auth button, #auth input')].filter((e) => e.getClientRects().length && !e.closest('.hidden') && e.type !== 'checkbox')
      .map((e) => [e.id || e.className, Math.round(e.getBoundingClientRect().height)]).filter(([, h]) => h < 44),
  }));
  ok(!fit.wide && fit.small.length === 0, `it fits a 390 px phone with 44 px targets (small: ${JSON.stringify(fit.small)})`);
  await typeInto(p2, 'vf-pass', pwOwner);
  await p2.click('#vf-submit');
  await until(p2, () => !document.getElementById('vf-error').classList.contains('hidden'));
  ok(/expired|password you chose/.test(await text(p2, '#vf-error')), 'the stranger\'s link refuses the owner\'s password');

  // The owner's own link finishes and signs in.
  const p3 = await open();
  await p3.goto(`${BASE}/#verify=${ownerToken}`, { waitUntil: 'networkidle2' });
  await until(p3, () => !document.getElementById('verify-form').classList.contains('hidden'));
  await typeInto(p3, 'vf-pass', pwOwner);
  await p3.click('#vf-submit');
  await until(p3, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  ok(await shown(p3, 'home'), 'the owner\'s link, with their password, creates the account and signs in');
  const asStranger = await post('/api/identity/login', { email, password: pwStranger });
  ok(asStranger.status === 401, `the stranger's password does not sign in (${asStranger.status})`);
  const again = await post('/api/identity/signup/verify', { token: strangerToken, password: pwStranger });
  ok(again.status === 400, `the stranger's link cannot make a second account (${again.status})`);

  // An address that already has an account: a notice, never a link.
  const before = (await mails(A.email)).length;
  const n = await post('/api/identity/signup', { email: A.email, displayName: 'X', password: pwStranger });
  ok(n.status === 202 && JSON.stringify(await n.json()) === JSON.stringify(rb), `sign-up for an existing address answers word for word the same (${n.status})`);
  let notice = null;
  for (let i = 0; i < 40 && !notice; i++) {
    notice = (await mails(A.email)).find((m) => m.Subject === 'Someone tried to sign up with your address');
    if (!notice) await sleep(250);
  }
  const noticeText = notice ? (await (await fetch(`${MAILPIT}/api/v1/message/${notice.ID}`)).json()).Text : '';
  ok(!!notice && !/#verify=/.test(noticeText) && (await mails(A.email)).length === before + 1,
    'its owner gets one notice, carrying no link');
  const still = await post('/api/identity/login', { email: A.email, password: A.password });
  ok(still.status === 200, `and the existing account is untouched (${still.status})`);

  ok(log.length === 0, `no page errors (${log.join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const nf = failed();
console.log(nf ? `${nf} FAILED` : 'all passed');
process.exit(nf ? 1 : 0);
