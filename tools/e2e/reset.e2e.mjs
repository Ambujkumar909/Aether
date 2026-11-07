// Password reset, end to end: the sign-in card in a phone-sized browser, a
// real SMTP server (Mailpit, STARTTLS required -- tools/e2e/mailpit.sh), the
// emailed link followed, the new password used.
//
// The server must run with mail on and links pointing at BASE:
//   -smtp-addr 127.0.0.1:1025 -smtp-from noreply@aether.test
//   -smtp-ca tools/e2e/.mail/cert.pem -public-url <BASE>
//
//   node tools/e2e/reset.e2e.mjs https://localhost:9443
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, launch, sleep, ok, fail, failed, BASE, until } from './meet.lib.mjs';

const MAIL = process.env.MAILPIT || 'http://127.0.0.1:8025';
const log = [];
const acct = (await accounts(['Reset'])).Reset;
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
const post = (path, body, type = 'application/json') =>
  fetch(BASE + path, { method: 'POST', headers: { 'content-type': type }, body: typeof body === 'string' ? body : JSON.stringify(body) });
const mailFor = async (to) => {
  for (let i = 0; i < 40; i++) {
    const r = await (await fetch(`${MAIL}/api/v1/search?query=${encodeURIComponent('to:' + to)}`)).json();
    if (r.messages?.length) return r.messages;
    await sleep(250);
  }
  return [];
};
const phoneFits = (page) =>
  page.evaluate(() => {
    const vw = innerWidth;
    const wide = document.documentElement.scrollWidth > vw;
    const small = [...document.querySelectorAll('#auth button, #auth input')]
      .filter((e) => e.getClientRects().length && !e.closest('.hidden'))
      .filter((e) => e.type !== 'checkbox')
      .map((e) => [e.id || e.className, Math.round(e.getBoundingClientRect().height)])
      .filter(([, h]) => h < 44);
    return { wide, small };
  });

try {
  await fetch(`${MAIL}/api/v1/messages`, { method: 'DELETE' });
  const page = await open();
  await page.goto(BASE, { waitUntil: 'networkidle2' });
  await until(page, () => !document.getElementById('auth').classList.contains('hidden'));

  // The swap between signing in and creating an account works both ways.
  await page.click('#swap-btn');
  ok((await shown(page, 'signup-form')) && (await text(page, '.auth-tagline')) === 'Create your account' &&
    (await text(page, '#swap-btn')) === 'Back to sign in', 'Create an account opens the sign-up form, with its own title and a way back');
  await page.click('#swap-btn');
  ok((await shown(page, 'login-form')) && (await text(page, '#swap-btn')) === 'Create an account', 'and Back returns to sign-in');

  // Asking for a link.
  await page.click('#li-forgot');
  ok((await shown(page, 'forgot-form')) && (await text(page, '.auth-tagline')) === 'Reset your password', 'Forgot your password? opens the reset request');
  const fit = await phoneFits(page);
  ok(!fit.wide && fit.small.length === 0, `the request fits a 390 px phone with 44 px targets (${fit.wide ? 'overflows' : 'fits'}; small: ${JSON.stringify(fit.small)})`);
  await typeInto(page, 'fp-email', 'nobody-' + Date.now() + '@example.test');
  await page.click('#fp-submit');
  await until(page, () => !document.getElementById('fp-done').classList.contains('hidden'));
  const forNobody = await text(page, '#fp-done');
  await typeInto(page, 'fp-email', acct.email);
  await page.click('#fp-submit');
  await sleep(600);
  const forSomebody = await text(page, '#fp-done');
  ok(forNobody && forNobody === forSomebody, `the same answer with and without an account ("${forSomebody}")`);

  const msgs = await mailFor(acct.email);
  const all = await (await fetch(`${MAIL}/api/v1/messages`)).json();
  ok(msgs.length === 1 && all.total === 1, `one email, to the account only (${msgs.length} to it, ${all.total} in all)`);
  const full = msgs[0] ? await (await fetch(`${MAIL}/api/v1/message/${msgs[0].ID}`)).json() : {};
  const m = /(\S+)\/#reset=([A-Za-z0-9_-]+)/.exec(full.Text || '');
  ok(full.Subject === 'Reset your Aether password' && m && m[1] === new URL(BASE).origin,
    `it carries a link to this server with the token in the fragment (${m?.[1] ?? 'no link'})`);
  const link = m ? `${m[1]}/#reset=${m[2]}` : BASE;

  // Following it.
  const p2 = await open();
  await p2.goto(link, { waitUntil: 'networkidle2' });
  await until(p2, () => !document.getElementById('reset-form').classList.contains('hidden'));
  ok((await text(p2, '.auth-tagline')) === 'Choose a new password', 'the link opens Choose a new password');
  ok(!(await p2.evaluate(() => location.href)).includes('reset='), 'and the token is wiped from the address bar');
  const fit2 = await phoneFits(p2);
  ok(!fit2.wide && fit2.small.length === 0, `the reset form fits a phone with 44 px targets (small: ${JSON.stringify(fit2.small)})`);
  const next = 'e2e-new-' + Math.random().toString(36).slice(2, 12);
  await typeInto(p2, 'rp-pass', next);
  await typeInto(p2, 'rp-pass2', next + 'x');
  await p2.click('#rp-submit');
  ok((await shown(p2, 'rp-error')) && /different/.test(await text(p2, '#rp-error')), 'two different passwords are caught before sending');
  await typeInto(p2, 'rp-pass2', next);
  await p2.click('#rp-submit');
  await until(p2, () => !document.getElementById('li-done').classList.contains('hidden'));
  ok((await shown(p2, 'login-form')) && /Password changed/.test(await text(p2, '#li-done')), 'a new password returns to sign-in, saying so');

  // Keep the stored account usable for the other harnesses.
  const store = join(dirname(fileURLToPath(import.meta.url)), '.accounts.json');
  const saved = JSON.parse(readFileSync(store, 'utf8'));
  saved[new URL(BASE).host].Reset.password = next;
  writeFileSync(store, JSON.stringify(saved, null, 2));

  await typeInto(p2, 'li-email', acct.email);
  await typeInto(p2, 'li-pass', next);
  await p2.click('#li-submit');
  await until(p2, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  ok(await shown(p2, 'home'), 'and the new password signs in');
  const old = await post('/api/identity/login', { email: acct.email, password: acct.password });
  ok(old.status === 401, `the old password does not (${old.status})`);

  // The link works once.
  const p3 = await open();
  await p3.goto(link, { waitUntil: 'networkidle2' });
  await until(p3, () => !document.getElementById('reset-form').classList.contains('hidden'));
  await typeInto(p3, 'rp-pass', next + 'y');
  await typeInto(p3, 'rp-pass2', next + 'y');
  await p3.click('#rp-submit');
  await until(p3, () => !document.getElementById('fp-error').classList.contains('hidden'));
  ok((await shown(p3, 'forgot-form')) && /expired or was already used/.test(await text(p3, '#fp-error')),
    'used again, the link says so and offers a new one');

  // Bounds, a cross-site form, and the limit.
  const longMail = await post('/api/identity/password/forgot', { email: 'a'.repeat(330) + '@example.test' });
  ok(longMail.status === 400, `an address over 320 characters is refused (${longMail.status})`);
  const longTok = await post('/api/identity/password/reset', { token: 'x'.repeat(200), password: next + 'z' });
  ok(longTok.status === 400 && /reset_invalid/.test(await longTok.text()), `a token over the bound is refused (${longTok.status})`);
  const form = await post('/api/identity/password/forgot', `{"email":"${acct.email}","x":"=`, 'text/plain');
  ok(form.status === 415, `a cross-site form (text/plain) is refused before it is read (${form.status})`);
  let limited = 0;
  for (let i = 0; i < 8 && !limited; i++) {
    const r = await post('/api/identity/password/forgot', { email: 'nobody@example.test' });
    if (r.status === 429) limited = i + 1;
  }
  ok(limited > 0, `requests from one address are limited (429 after ${limited} more)`);

  ok(log.length === 0, `no page errors (${log.join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
