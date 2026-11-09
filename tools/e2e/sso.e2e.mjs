// Single sign-on, end to end in a phone-sized browser: the sign-in card's
// "Sign in with Example Org" button, the provider's page (tools/e2e/
// fakeidp.mjs), Continue, and back signed in -- as a new member, then as the
// same account; a cancelled sign-in; an address the provider did not verify;
// a domain the server does not allow; a forged state; a callback with no
// start. The server runs with -sso-issuer http://127.0.0.1:9460 and
// -sso-domains example.test (the runners do this).
//
//   node tools/e2e/sso.e2e.mjs https://localhost:9443
import { launch, ok, fail, failed, BASE, until } from './meet.lib.mjs';

const IDP = process.env.AETHER_E2E_IDP || 'http://127.0.0.1:9460';
const browser = await launch();
const log = [];
const open = async () => {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  page.on('pageerror', (e) => log.push('PAGE ERROR ' + e.message));
  await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
  return page;
};
const setPerson = (p) => fetch(IDP + '/_control/person', { method: 'POST', body: JSON.stringify(p) });
const text = (page, sel) => page.evaluate((s) => document.querySelector(s)?.textContent.trim() ?? '', sel);
const shown = (page, id) => page.evaluate((id) => { const e = document.getElementById(id); return !!e && !e.classList.contains('hidden') && e.getClientRects().length > 0; }, id);
const me = (page) => page.evaluate(async () => { const r = await fetch('/api/identity/me', { credentials: 'same-origin' }); return r.status === 200 ? (await r.json()).user : null; });

// Start at the sign-in card, press the button, land on the provider's page.
const toProvider = async (page) => {
  await page.goto(BASE + '/?room=standup', { waitUntil: 'networkidle2' });
  await until(page, () => !document.getElementById('auth').classList.contains('hidden'));
  await until(page, () => !document.getElementById('li-sso').classList.contains('hidden'), undefined, 5000);
  await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('#li-sso')]);
};

try {
  const email = `sso-${Date.now().toString(36)}@example.test`;
  await setPerson({ email, name: 'Priya SSO', verified: true });

  const p = await open();
  await p.goto(BASE, { waitUntil: 'networkidle2' });
  await until(p, () => !document.getElementById('li-sso').classList.contains('hidden'), undefined, 5000);
  ok((await text(p, '#li-sso')).includes('Sign in with Example Org'), `the card offers "${await text(p, '#li-sso')}"`);
  const fit = await p.evaluate(() => ({ wide: document.documentElement.scrollWidth > innerWidth, h: Math.round(document.getElementById('li-sso').getBoundingClientRect().height) }));
  ok(!fit.wide && fit.h >= 44, `the button fits a 390 px phone at ${fit.h} px tall`);

  await toProvider(p);
  ok(p.url().startsWith(IDP + '/authorize') && /code_challenge_method=S256/.test(p.url()) && /nonce=/.test(p.url()), 'the button leads to the provider with PKCE and a nonce');
  ok((await text(p, '#who')) === email, 'the provider shows who is signing in');
  await Promise.all([p.waitForNavigation({ waitUntil: 'networkidle2' }), p.click('#continue')]);
  await until(p, () => !document.getElementById('home').classList.contains('hidden') || !document.getElementById('prejoin')?.classList.contains('hidden'), undefined, 10000);
  const u1 = await me(p);
  ok(u1?.email === email && u1?.displayName === 'Priya SSO' && u1?.role === 'member', `Continue creates the member and signs them in (${u1?.email}, ${u1?.role})`);
  ok(p.url().includes('room=standup'), `and lands where the sign-in started (${new URL(p.url()).search})`);

  // Again: the same account, not a second one.
  const p2 = await open();
  await toProvider(p2);
  await Promise.all([p2.waitForNavigation({ waitUntil: 'networkidle2' }), p2.click('#continue')]);
  await until(p2, () => !!document.querySelector('#home:not(.hidden), #prejoin:not(.hidden)'), undefined, 10000);
  const u2 = await me(p2);
  ok(u2?.id === u1?.id, 'signing in again is the same account');

  // Cancelled at the provider.
  const p3 = await open();
  await toProvider(p3);
  await Promise.all([p3.waitForNavigation({ waitUntil: 'networkidle2' }), p3.click('#deny')]);
  await until(p3, () => !document.getElementById('li-sso-error').classList.contains('hidden'), undefined, 8000);
  ok(!(await me(p3)) && /cancelled/.test(await text(p3, '#li-sso-error')), `cancelling signs nobody in and says so ("${await text(p3, '#li-sso-error')}")`);

  // An address the provider did not verify.
  await setPerson({ email: 'unverified@example.test', name: 'U', verified: false });
  const p4 = await open();
  await toProvider(p4);
  await Promise.all([p4.waitForNavigation({ waitUntil: 'networkidle2' }), p4.click('#continue')]);
  await until(p4, () => !document.getElementById('li-sso-error').classList.contains('hidden'), undefined, 8000);
  ok(!(await me(p4)) && /cannot sign in here/.test(await text(p4, '#li-sso-error')), 'an unverified address is refused');

  // A domain the server does not allow.
  await setPerson({ email: 'outsider@elsewhere.example', name: 'O', verified: true });
  const p5 = await open();
  await toProvider(p5);
  await Promise.all([p5.waitForNavigation({ waitUntil: 'networkidle2' }), p5.click('#continue')]);
  await until(p5, () => !document.getElementById('li-sso-error').classList.contains('hidden'), undefined, 8000);
  ok(!(await me(p5)), 'an address outside the allowed domains is refused');
  const stranger = await fetch(BASE + '/api/identity/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: 'outsider@elsewhere.example', password: 'anything-at-all-12' }) });
  ok(stranger.status === 401, `and no account was made for it (${stranger.status})`);

  // A callback that did not start here, and one with a forged state.
  const p6 = await open();
  await p6.goto(BASE + '/api/identity/sso/callback?code=x&state=y', { waitUntil: 'networkidle2' });
  await until(p6, () => !document.getElementById('li-sso-error').classList.contains('hidden'), undefined, 8000);
  ok(!(await me(p6)) && /too long/.test(await text(p6, '#li-sso-error')), 'a callback without a start is refused');
  await setPerson({ email, name: 'Priya SSO', verified: true });
  const p7 = await open();
  await toProvider(p7);
  const forged = new URL(await p7.evaluate(() => document.getElementById('continue').href));
  forged.searchParams.set('state', 'forged-' + forged.searchParams.get('state'));
  await p7.goto(forged.href, { waitUntil: 'networkidle2' });
  await until(p7, () => !document.getElementById('li-sso-error').classList.contains('hidden'), undefined, 8000);
  ok(!(await me(p7)) && /did not start from this browser/.test(await text(p7, '#li-sso-error')), 'a forged state is refused');

  const stats = await (await fetch(IDP + '/_control/stats')).json();
  ok(stats.tokenCalls >= 4, `codes were exchanged server to server (${stats.tokenCalls} token calls)`);
  ok(log.length === 0, `no page errors (${log.join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
