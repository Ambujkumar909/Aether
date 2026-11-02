// Q&A in a meeting among four real browsers:
//   - B asks; it appears for A and C at once (C, panel closed, gets a dot);
//   - C asks anonymously: nobody, the organizer included, sees C's name;
//   - Like and Reply reach the others;
//   - the host turns on review: B's next question waits ("Pending (1)" for
//     the host, invisible to C) until the host publishes it;
//   - Pin and "Mark as answered" show for everyone; "Close Q&A" stops posting;
//   - rule 7: someone signed in but not in the meeting, an anonymous caller,
//     a missing CSRF header, an attendee moderating and a question one over
//     the length bound are all refused.
//
//   node tools/e2e/qna.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C', 'D']);
const browser = await launch();
const log = [];
const room = 'qna-' + Math.random().toString(36).slice(2, 8);
const openQnA = (p) => p.evaluate(() => {
  const open = !document.getElementById('shell').classList.contains('panel-closed') && document.getElementById('view-qna').classList.contains('active');
  if (!open) document.getElementById('btn-qna').click();
});
const cards = (p) => p.evaluate(() => [...document.querySelectorAll('#view-qna .qna-q')].map((c) => c.textContent.replace(/\s+/g, ' ').trim()));
const has = (p, text) => until(p, (t) => [...document.querySelectorAll('#view-qna .qna-q')].some((c) => c.textContent.includes(t)), text, 8000);
const ask = async (p, text, anon = false) => {
  await p.evaluate((text, anon) => {
    document.getElementById('qna-text').value = text;
    const a = document.getElementById('qna-anon');
    if (a) a.checked = anon;
    document.getElementById('qna-post').click();
  }, text, anon);
};
const call = (p, method, path, body, csrf = true) => p.evaluate(async (method, path, body, csrf) => {
  const h = { 'content-type': 'application/json' };
  if (csrf) h['X-Aether-CSRF'] = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
  const r = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  return r.status;
}, method, path, body, csrf);
const card = (p, text) => p.evaluate((t) => [...document.querySelectorAll('#view-qna .qna-q')].find((c) => c.textContent.includes(t))?.dataset.q, text);
try {
  const [A, B, C, D] = [await signedIn(browser, acc.A, 'A', log), await signedIn(browser, acc.B, 'B', log), await signedIn(browser, acc.C, 'C', log), await signedIn(browser, acc.D, 'D', log)];
  await enter(A, room);
  await enter(B, room);
  await enter(C, room);
  await sleep(2500);
  await openQnA(A);
  await openQnA(B);
  ok(await until(B, () => /No questions yet/.test(document.getElementById('view-qna').textContent), null), 'B opens Q&A: "No questions yet."');

  await ask(B, 'What is the plan for Q3?');
  ok(await has(A, 'What is the plan for Q3?'), 'B\'s question reaches A at once');
  ok(await until(C, () => !document.getElementById('qna-dot').hidden, null), 'C, with the panel closed, gets a dot on Q&A');
  await openQnA(C);
  ok(await has(C, 'What is the plan for Q3?'), 'and sees it on opening');

  await ask(C, 'Is the budget fixed?', true);
  ok(await has(A, 'Is the budget fixed?'), 'C\'s anonymous question reaches the organizer');
  const anonCard = (await cards(A)).find((c) => c.includes('Is the budget fixed?'));
  ok(anonCard.startsWith('Anonymous') && !anonCard.includes('E2E C'), `nobody sees who asked, the organizer included: "${anonCard.slice(0, 40)}"`);

  // Like and reply.
  const q1 = await card(C, 'What is the plan for Q3?');
  await C.evaluate((id) => document.querySelector(`[data-like="${id}"]`).click(), q1);
  ok(await until(B, (id) => /Like · 1/.test(document.querySelector(`[data-q="${id}"]`)?.textContent ?? ''), q1), 'C\'s like reaches B: "Like · 1"');
  await C.evaluate((id) => document.querySelector(`[data-reply="${id}"]`).click(), q1);
  await C.evaluate(() => { document.getElementById('qna-reply-text').value = 'Following this'; });
  await C.evaluate((id) => document.querySelector(`[data-send-reply="${id}"]`).click(), q1);
  ok(await has(B, 'Following this'), 'C\'s reply reaches B');

  // Review before publishing.
  await A.evaluate(() => document.getElementById('qna-settings').click());
  await until(A, () => !!document.querySelector('#dialog [name="moderation"]'), null);
  await A.evaluate(() => { document.querySelector('#dialog [name="moderation"]').checked = true; document.querySelector('#dialog [data-yes]').click(); });
  ok(await until(B, () => /reviewed before they are published/.test(document.getElementById('view-qna').textContent), null), 'B is told questions are reviewed first');
  await ask(B, 'Can we hire two more?');
  ok(await has(B, 'Waiting for the organizer to publish it'), 'B sees their question waiting');
  await sleep(800);
  ok(!(await cards(C)).some((c) => c.includes('Can we hire two more?')), 'C does not see it');
  ok(await until(A, () => /Pending \(1\)/.test(document.querySelector('.qna-tabs')?.textContent ?? ''), null), 'the host sees "Pending (1)"');
  await A.evaluate(() => document.querySelector('[data-qtab="pending"]').click());
  const q3 = await card(A, 'Can we hire two more?');
  await A.evaluate((id) => document.querySelector(`[data-qmore="${id}"]`).click(), q3);
  await sleep(150);
  await A.evaluate(() => [...document.querySelectorAll('#tile-menu button')].find((b) => b.textContent === 'Publish').click());
  ok(await has(C, 'Can we hire two more?'), 'published: C sees it');

  // Pin and answered.
  await A.evaluate(() => document.querySelector('[data-qtab="published"]').click());
  for (const label of ['Pin', 'Mark as answered']) {
    await A.evaluate((id) => document.querySelector(`[data-qmore="${id}"]`).click(), q1);
    await sleep(150);
    await A.evaluate((l) => [...document.querySelectorAll('#tile-menu button')].find((b) => b.textContent === l).click(), label);
    await sleep(400);
  }
  ok(await until(C, (id) => { const t = document.querySelector(`[data-q="${id}"]`)?.textContent ?? ''; return t.includes('Pinned') && t.includes('Answered'); }, q1), 'C sees it pinned and answered');

  // Rule 7, at the HTTP edge.
  const path = `/api/scheduling/rooms/${room}/qna`;
  ok((await call(D, 'GET', path)) === 403, 'someone not in the meeting cannot read it');
  ok((await fetch(BASE + path)).status === 401, 'an anonymous caller cannot');
  ok((await call(B, 'POST', path, { body: 'no csrf' }, false)) === 403, 'a post without the CSRF header is refused');
  ok((await call(B, 'POST', `${path}/${q1}/moderate`, { action: 'dismiss' })) === 403, 'an attendee cannot moderate');
  ok((await call(B, 'POST', path, { body: 'x'.repeat(1001) })) === 400, 'a question one over 1000 characters is refused');

  // Close.
  await A.evaluate(() => document.getElementById('qna-settings').click());
  await until(A, () => !!document.querySelector('#dialog [name="closed"]'), null);
  await A.evaluate(() => { document.querySelector('#dialog [name="moderation"]').checked = false; document.querySelector('#dialog [name="closed"]').checked = true; document.querySelector('#dialog [data-yes]').click(); });
  ok(await until(B, () => /Q&A is closed/.test(document.getElementById('view-qna').textContent) && !document.getElementById('qna-text'), null), 'Close Q&A: B is told, and cannot post');
  ok((await call(B, 'POST', path, { body: 'after close' })) === 409, 'and the server refuses a post');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
