// The activity feed among three real browsers:
//   - A has the Activity view open; B mentions A in a group: the row and the
//     badge arrive by push, without a reload;
//   - B and C react to A's message: one row names them both, and taking a
//     reaction back takes that name away;
//   - B replies to A: a reply row; the Mentions filter shows only mentions;
//   - opening a row opens the conversation at the message and reads it; the
//     badge follows; "mark all as read" clears it;
//   - nobody signed in, a request without the CSRF token, 101 ids, and
//     somebody else's ids are all refused or do nothing.
//
//   node tools/e2e/activity.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const tag = Math.random().toString(36).slice(2, 6);
const api = (p, fn, ...args) => p.evaluate(async (fn, args) => {
  const m = await import('/src/api.js');
  try { return { ok: true, v: await m.messaging[fn](...args) }; } catch (e) { return { ok: false, status: e.status, msg: e.message }; }
}, fn, args);
const me = (p) => p.evaluate(async () => (await import('/src/api.js')).session.user.id);
const badge = (p) => p.evaluate(() => {
  const b = document.getElementById('activity-badge');
  return b && !b.classList.contains('hidden') ? b.textContent : '0';
});
const rows = (p) => p.evaluate(() => [...document.querySelectorAll('#act-list .act-item')].map((x) => ({
  line: x.querySelector('.act-line').textContent.replace(/\s+/g, ' ').trim(),
  snippet: x.querySelector('.act-snippet')?.textContent ?? '',
  unread: x.classList.contains('is-unread'),
})));
const rowWith = (p, text) => until(p, (t) => [...document.querySelectorAll('#act-list .act-item')].some((x) => x.textContent.includes(t)), text);

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  const [idA] = [await me(A)];
  // A starts from nothing unread, with the feed open.
  await api(A, 'readActivity', [], true);
  await A.evaluate(() => document.querySelector('[data-home="activity"]').click());
  await sleep(1000);
  ok((await badge(A)) === '0', `A starts with nothing unread: ${await badge(A)}`);

  // B mentions A in a new group: pushed.
  const g = (await api(B, 'createGroup', `Launch ${tag}`, [acc.A.email, acc.C.email])).v;
  const mention = `Can you check the build ${tag}?`;
  await api(B, 'sendFull', g.id, `@E2E A ${mention}`, '', [idA]);
  ok(await rowWith(A, mention), 'the mention arrives in the open feed by push');
  ok(await until(A, () => document.getElementById('activity-badge')?.textContent === '1'), `and the badge reads 1: ${await badge(A)}`);
  let r = (await rows(A)).find((x) => x.snippet.includes(mention));
  ok(r && /E2E B mentioned you/.test(r.line) && r.unread, `"B mentioned you", unread: ${JSON.stringify(r)}`);

  // A writes; B and C react: one row naming both.
  const mine = (await api(A, 'send', g.id, `Shipping at five ${tag}`)).v;
  await api(B, 'react', mine.id, '👍', true);
  await api(C, 'react', mine.id, '🎉', true);
  ok(await until(A, (t) => [...document.querySelectorAll('#act-list .act-item')].some((x) => x.textContent.includes(t) && /E2E B/.test(x.textContent) && /E2E C/.test(x.textContent)), `Shipping at five ${tag}`),
    'two reactions to one message are one row naming both');
  r = (await rows(A)).find((x) => x.snippet.includes(`Shipping at five ${tag}`));
  ok(r && /reacted/.test(r.line) && r.line.includes('👍') && r.line.includes('🎉'), `with both reactions: ${r?.line}`);
  await api(C, 'react', mine.id, '🎉', false);
  ok(await until(A, (t) => {
    const x = [...document.querySelectorAll('#act-list .act-item')].find((y) => y.textContent.includes(t));
    return x && !/E2E C/.test(x.textContent) && !x.textContent.includes('🎉');
  }, `Shipping at five ${tag}`), 'a reaction taken back leaves the row');
  ok(await until(A, () => document.getElementById('activity-badge')?.textContent === '2'), `badge after the take-back: ${await badge(A)}`);

  // A reply.
  await api(B, 'send', g.id, `Five works ${tag}`, mine.id);
  ok(await rowWith(A, `Five works ${tag}`), 'a reply arrives');
  r = (await rows(A)).find((x) => x.snippet.includes(`Five works ${tag}`));
  ok(r && /replied to you/.test(r.line), `"replied to you": ${r?.line}`);

  // Filters answer from what is loaded, at once.
  await A.evaluate(() => document.querySelector('[data-act-filter="mention"]').click());
  const onlyMentions = await rows(A);
  ok(onlyMentions.length >= 1 && onlyMentions.every((x) => /mentioned you/.test(x.line)), `Mentions shows only mentions: ${onlyMentions.length}`);
  await A.evaluate(() => document.querySelector('[data-act-filter="all"]').click());
  await sleep(300);

  // Opening the mention opens the conversation at it and reads it.
  await A.evaluate((t) => [...document.querySelectorAll('#act-list .act-item')].find((x) => x.textContent.includes(t)).click(), mention);
  ok(await until(A, (t) => !!document.querySelector('#thread .flash, #thread')?.textContent.includes(t), mention), 'opening the row opens the conversation at the message');
  // Reading the conversation reads its mention and the reply in it; the
  // reaction (not in the conversation's text) stays.
  ok(await until(A, () => document.getElementById('activity-badge')?.textContent === '1'), `the badge follows: ${await badge(A)}`);
  const server = (await api(A, 'activity', { limit: 50 })).v;
  ok(server.unread === 1 && server.items.find((x) => !x.read)?.kind === 'reaction', `the server agrees: ${server.unread}`);

  // Mark all as read.
  await A.evaluate(() => document.getElementById('c-back')?.click());
  await A.evaluate(() => document.getElementById('act-all-read').click());
  ok(await until(A, () => document.getElementById('activity-badge')?.classList.contains('hidden')), 'mark all as read clears the badge');
  ok((await api(A, 'activity', { limit: 1 })).v.unread === 0, 'and the server has none unread');

  // Refusals.
  const anon = await fetch(BASE + '/api/messaging/activity');
  ok(anon.status === 401, `nobody signed in: ${anon.status}`);
  const csrf = await A.evaluate(async () => (await fetch('/api/messaging/activity/read', { method: 'POST', credentials: 'same-origin', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ all: true }) })).status);
  ok(csrf === 403, `a request without the CSRF token: ${csrf}`);
  const tooMany = await api(A, 'readActivity', Array.from({ length: 101 }, (_, i) => `x${i}`));
  ok(!tooMany.ok && tooMany.status === 400, `101 ids: ${tooMany.status}`);
  ok((await api(A, 'readActivity', Array.from({ length: 100 }, (_, i) => `x${i}`))).ok, '100 ids is fine');
  const badKind = await api(A, 'activity', { kind: 'everything' });
  ok(!badKind.ok && badKind.status === 400, `an unknown kind: ${badKind.status}`);
  // B cannot read A's rows: a fresh mention of A stays unread after B names its id.
  // (In a conversation A does not have open: one A is looking at is read.)
  const g2 = (await api(C, 'createGroup', `Ops ${tag}`, [acc.A.email, acc.B.email])).v;
  await api(C, 'sendFull', g2.id, `@E2E A ping ${tag}`, '', [idA]);
  await sleep(500);
  const fresh = (await api(A, 'activity', { unread: true })).v.items.find((x) => x.snippet.includes(`ping ${tag}`));
  await api(B, 'readActivity', [fresh.id]);
  ok((await api(A, 'activity', { unread: true })).v.items.some((x) => x.id === fresh.id), "somebody else's id does nothing");
  // Leave A one unread mention for the layout harness's screenshot.
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
