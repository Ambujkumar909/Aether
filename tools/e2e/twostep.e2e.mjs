// Two-step sign-in, end to end in a phone-sized browser: set up in the
// console's account card (the server-drawn QR and the key), turn on with a
// code this harness computes the way an authenticator app does (RFC 6238,
// HMAC-SHA1, 30 s), sign out, sign in again through the code step, use a
// recovery code, and see a wrong code refused.
//
// Run after a harness that has made account A (console).
//   node tools/e2e/twostep.e2e.mjs https://localhost:9443
import { createHmac } from 'node:crypto';
import { accounts, launch, ok, fail, failed, BASE, until } from './meet.lib.mjs';

const A = (await accounts(['A'])).A;
const browser = await launch();
const log = [];

const b32 = (s) => {
  const abc = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const ch of s.replace(/=+$/, '')) bits += abc.indexOf(ch).toString(2).padStart(5, '0');
  const out = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(out);
};
const totp = (secretB32, at = Date.now()) => {
  const step = Math.floor(at / 1000 / 30);
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(step));
  const h = createHmac('sha1', b32(secretB32)).update(msg).digest();
  const off = h[h.length - 1] & 0x0f;
  const bin = ((h[off] & 0x7f) << 24) | (h[off + 1] << 16) | (h[off + 2] << 8) | h[off + 3];
  return String(bin % 1000000).padStart(6, '0');
};

const open = async () => {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  page.on('pageerror', (e) => log.push('PAGE ERROR ' + e.message));
  await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
  return page;
};
const shown = (page, id) => page.evaluate((id) => { const e = document.getElementById(id); return !!e && !e.classList.contains('hidden') && e.getClientRects().length > 0; }, id);
const text = (page, sel) => page.evaluate((s) => document.querySelector(s)?.textContent.trim() ?? '', sel);
const typeInto = async (page, id, v) => { await page.evaluate((id) => (document.getElementById(id).value = ''), id); await page.type('#' + id, v); };
const signIn = async (page, email, password) => {
  await page.goto(BASE, { waitUntil: 'networkidle2' });
  await until(page, () => !document.getElementById('auth').classList.contains('hidden'));
  await typeInto(page, 'li-email', email);
  await typeInto(page, 'li-pass', password);
  await page.click('#li-submit');
  // Home, the code step, or an error -- said, rather than a silent timeout.
  const done = await until(page, () => !document.getElementById('home').classList.contains('hidden') || !document.getElementById('code-form').classList.contains('hidden') || !document.getElementById('li-error').classList.contains('hidden'), undefined, 10000);
  const err = await text(page, '#li-error');
  if (!done || err) throw new Error(`sign-in of ${email} did not finish: ${err || 'timed out'}`);
};
const post = (path, body) => fetch(BASE + path, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });

try {
  // Set up, in the console.
  const p = await open();
  await signIn(p, A.email, A.password);
  await until(p, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  await p.evaluate(async () => { const m = await import('/src/console.js'); m.openConsole('account'); });
  await until(p, () => !!document.getElementById('ts-start'), undefined, 8000);
  ok(await shown(p, 'ts-card'), 'the account card offers two-step sign-in');
  await typeInto(p, 'ts-pw', A.password);
  await p.click('#ts-start');
  await until(p, () => !document.getElementById('ts-setup').classList.contains('hidden'), undefined, 8000);
  const svgOK = await p.evaluate(() => { const s = document.querySelector('#ts-qr svg'); return !!s && s.getBoundingClientRect().width > 100 && !document.querySelector('#ts-qr script'); });
  const secret = await text(p, '#ts-secret');
  ok(svgOK && /^[A-Z2-7]{32}$/.test(secret), `the QR is drawn and the key shown (${secret.length} chars)`);
  const fit = await p.evaluate(() => ({ wide: document.documentElement.scrollWidth > innerWidth, small: [...document.querySelectorAll('#ts-card button, #ts-card input')].filter((e) => e.getClientRects().length).map((e) => [e.id, Math.round(e.getBoundingClientRect().height)]).filter(([, h]) => h < 44) }));
  ok(!fit.wide && fit.small.length === 0, `the card fits a 390 px phone with 44 px targets (small: ${JSON.stringify(fit.small)})`);
  await typeInto(p, 'ts-code', '000000');
  await p.click('#ts-enable');
  await until(p, () => document.getElementById('toast')?.textContent.length > 0, undefined, 6000);
  ok(!(await shown(p, 'ts-codes')), 'a wrong code does not turn it on');
  await typeInto(p, 'ts-code', totp(secret));
  await p.click('#ts-enable');
  await until(p, () => !document.getElementById('ts-codes').classList.contains('hidden'), undefined, 8000);
  const codes = (await text(p, '#ts-codes-list')).split(/\s+/).filter(Boolean);
  ok(codes.length === 10 && codes.every((c) => /^[A-Z2-9]{5}-[A-Z2-9]{5}$/.test(c)), `ten recovery codes are shown once (${codes[0]} ...)`);

  // Sign in again: the code step.
  const q = await open();
  await signIn(q, A.email, A.password);
  await until(q, () => !document.getElementById('code-form').classList.contains('hidden'), undefined, 8000);
  ok((await text(q, '.auth-tagline')) === 'One more step' && !(await shown(q, 'home')), 'the right password now leads to the code step, not home');
  await typeInto(q, 'cd-code', '000000');
  await q.click('#cd-submit');
  await until(q, () => !document.getElementById('cd-error').classList.contains('hidden'), undefined, 6000);
  ok(/not right/.test(await text(q, '#cd-error')), 'a wrong code is refused, and the step stays');
  // The code that turned two-step on was this 30-second window's, and a
  // code is accepted once; the server allows one step of clock skew, so the
  // next window's code -- what the app will show in a moment -- is used.
  const code = totp(secret, Date.now() + 30000);
  await typeInto(q, 'cd-code', code);
  await q.click('#cd-submit');
  await until(q, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  ok(await shown(q, 'home'), 'the app code signs in');

  // The same code again, on a fresh sign-in, is a replay. (The per-ticket
  // bound of five wrong codes and the oversized-ticket case are unit tests:
  // here they would spend the sign-in rate limit this harness runs within.)
  const r1 = await post('/api/identity/login', { email: A.email, password: A.password });
  const t1 = (await r1.json()).ticket;
  const replay = await post('/api/identity/login/second', { ticket: t1, code });
  ok(r1.status === 200 && t1 && replay.status === 401, `a code is accepted once (replay: ${replay.status})`);
  const rec = await post('/api/identity/login/second', { ticket: t1, code: codes[3].toLowerCase() });
  ok(rec.status === 200, `a recovery code signs in, however typed (${rec.status})`);
  // (A recovery code refused the second time is a unit test: another sign-in
  // here would spend the rate limit this harness runs within.)

  // Every new endpoint: a stranger, an anonymous caller and a cross-site
  // form. The signed-in member's own fetches carry the session cookie.
  const anon = await open();
  await anon.goto(BASE, { waitUntil: 'domcontentloaded' });
  const a1 = await anon.evaluate(() => fetch('/api/identity/two-step/setup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{"password":"x"}' }).then((r) => r.status));
  ok(a1 === 401, `anonymous: two-step setup is refused (${a1})`);
  const noCsrf = await q.evaluate(() => fetch('/api/identity/two-step/disable', { method: 'POST', headers: { 'content-type': 'application/json' }, credentials: 'same-origin', body: '{"password":"x"}' }).then((r) => r.status));
  ok(noCsrf === 403, `a signed-in request without the CSRF header is refused (${noCsrf})`);
  const selfReset = await q.evaluate(async () => { const m = await import('/src/api.js'); try { await m.twoStep.reset((await m.api.get('/api/identity/me')).user.id); return 200; } catch (e) { return e.status; } });
  ok(selfReset === 403, `a member cannot use the administrator's reset, even on themselves (${selfReset})`);

  // An administrator removes it for a lost phone: the account's sessions end.
  const adm = await open();
  await signIn(adm, process.env.AETHER_ADMIN_EMAIL, process.env.AETHER_ADMIN_PASSWORD);
  await until(adm, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  const findA = `const m = await import('/src/api.js'); const me = (await m.api.get('/api/identity/users?q=' + encodeURIComponent(email))).users.find((u) => u.email === email);`;
  const reset = await adm.evaluate(new Function('email', `return (async () => { ${findA} try { await m.twoStep.reset(me.id); return 200; } catch (e) { return e.status; } })();`), A.email);
  const qMe = await q.evaluate(() => fetch('/api/identity/me', { credentials: 'same-origin' }).then((r) => r.status));
  ok(reset === 200 && qMe === 401, `an administrator's reset removes it and ends the account's sessions (${reset}, then /me ${qMe})`);
  const again2 = await adm.evaluate(new Function('email', `return (async () => { ${findA} try { await m.twoStep.reset(me.id); return 200; } catch (e) { return e.status + ' ' + e.code; } })();`), A.email);
  ok(again2 === '409 two_step_not_set_up', `a reset with nothing to remove is refused, so it cannot end sessions by mistake (${again2})`);

  // Turn it on again, then off from the console, in a signed-in page.
  await signIn(q, A.email, A.password);
  await until(q, () => !document.getElementById('home').classList.contains('hidden'), undefined, 10000);
  const secret2 = await q.evaluate(async (pw) => { const m = await import('/src/api.js'); return (await m.twoStep.setup(pw)).secret; }, A.password);
  const on2 = await q.evaluate(async (code) => { const m = await import('/src/api.js'); try { return (await m.twoStep.enable(code)).recoveryCodes.length; } catch (e) { return e.status; } }, totp(secret2));
  ok(on2 === 10, `turned on again through the API (${on2} recovery codes)`);

  // Turn it off from the console, in the signed-in page.
  await q.evaluate(async () => { const m = await import('/src/console.js'); m.openConsole('account'); });
  await until(q, () => !!document.getElementById('ts-off'), undefined, 8000);
  await typeInto(q, 'ts-off-pw', A.password);
  await q.click('#ts-off');
  await until(q, () => !!document.getElementById('ts-start'), undefined, 8000);
  const r3 = await post('/api/identity/login', { email: A.email, password: A.password });
  ok(r3.status === 200 && !(await r3.json()).ticket, `turned off from the console, the password alone signs in again (${r3.status})`);

  ok(log.length === 0, `no page errors (${log.join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
