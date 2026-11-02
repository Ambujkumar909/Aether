// Meeting notes among four real browsers:
//   - before the organizer adds them, attendees read "The organizer hasn't
//     created meeting notes yet."; "Add meeting notes" opens Agenda, Notes and
//     Follow-up tasks for everyone at once;
//   - items added, edited, ticked off, assigned and deleted reach the others;
//   - two people editing the same item: the second is refused with the
//     first's text (409 + the item), never overwritten;
//   - rule 7: someone outside the meeting, an anonymous caller, no CSRF
//     header, an attendee creating the notes, one over 2000 characters.
//
//   node tools/e2e/notes.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C', 'D']);
const browser = await launch();
const log = [];
const room = 'notes-' + Math.random().toString(36).slice(2, 8);
const open = (p) => p.evaluate(() => {
  const showing = !document.getElementById('shell').classList.contains('panel-closed') && document.getElementById('view-notes').classList.contains('active');
  if (!showing) document.getElementById('btn-notes').click();
});
const items = (p, sec) => p.evaluate((sec) => [...document.querySelectorAll(`#view-notes [data-sec="${sec}"] .nt-item`)].map((li) => li.textContent.replace(/\s+/g, ' ').trim()), sec);
const add = (p, sec, text) => p.evaluate((sec, text) => {
  const el = document.querySelector(`#view-notes [data-add="${sec}"]`);
  el.value = text;
  el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
}, sec, text);
const idOf = (p, text) => p.evaluate((t) => [...document.querySelectorAll('#view-notes .nt-item')].find((li) => li.textContent.includes(t))?.dataset.nid, text);
const call = (p, method, path, body, csrf = true) => p.evaluate(async (method, path, body, csrf) => {
  const h = { 'content-type': 'application/json' };
  if (csrf) h['X-Aether-CSRF'] = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
  const r = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, method, path, body, csrf);
try {
  const [A, B, C, D] = [await signedIn(browser, acc.A, 'A', log), await signedIn(browser, acc.B, 'B', log), await signedIn(browser, acc.C, 'C', log), await signedIn(browser, acc.D, 'D', log)];
  await enter(A, room);
  await enter(B, room);
  await enter(C, room);
  await sleep(2500);
  await open(B);
  ok(await until(B, () => /hasn't created meeting notes yet/.test(document.getElementById('view-notes').textContent), null), 'B reads "The organizer hasn\'t created meeting notes yet."');
  await open(A);
  ok(await until(A, () => !!document.getElementById('nt-create'), null), 'the organizer is offered "Add meeting notes"');
  await A.evaluate(() => document.getElementById('nt-create').click());
  ok(await until(B, () => ['Agenda', 'Notes', 'Follow-up tasks'].every((t) => [...document.querySelectorAll('#view-notes h3')].some((h) => h.textContent === t)), null), 'B sees Agenda, Notes and Follow-up tasks appear');

  await add(B, 'agenda', 'Budget review');
  ok(await until(A, () => /Budget review/.test(document.getElementById('view-notes').textContent), null), 'B\'s agenda item reaches A');
  await open(C);
  ok(await until(C, () => /Budget review/.test(document.getElementById('view-notes').textContent), null), 'and C on opening');

  await add(A, 'tasks', 'Send the deck');
  ok(await until(B, () => /Send the deck/.test(document.getElementById('view-notes').textContent), null), 'A\'s task reaches B');
  const task = await idOf(A, 'Send the deck');
  await A.evaluate((id) => { const s = document.querySelector(`[data-assign="${id}"]`); s.value = 'E2E B'; s.dispatchEvent(new Event('change')); }, task);
  await sleep(500);
  await A.evaluate((id) => { const c = document.querySelector(`[data-done="${id}"]`); c.checked = true; c.dispatchEvent(new Event('change')); }, task);
  ok(await until(B, (id) => { const li = document.querySelector(`[data-nid="${id}"]`); return li?.classList.contains('done') && li.querySelector('select').value === 'E2E B'; }, task), 'B sees it assigned to E2E B and done');

  // Same item, two editors: the second is refused with the first's text.
  await add(C, 'notes', 'Draft line');
  await until(A, () => /Draft line/.test(document.getElementById('view-notes').textContent), null);
  const nid = await idOf(A, 'Draft line');
  const path = `/api/scheduling/rooms/${room}/notes`;
  const first = await call(A, 'PATCH', `${path}/items/${nid}`, { body: 'Draft line, by A', version: 1 });
  const second = await call(B, 'PATCH', `${path}/items/${nid}`, { body: 'Draft line, by B', version: 1 });
  ok(first.status === 200 && second.status === 409 && second.body?.item?.body === 'Draft line, by A', `the second editor is refused with the first's text (${first.status}, ${second.status}, "${second.body?.item?.body}")`);
  ok(await until(C, () => /Draft line, by A/.test(document.getElementById('view-notes').textContent) && !/by B/.test(document.getElementById('view-notes').textContent), null), 'everyone sees A\'s version, nobody sees B\'s');

  // Delete.
  await C.evaluate((id) => document.querySelector(`[data-del="${id}"]`).click(), nid);
  ok(await until(A, () => !/Draft line/.test(document.getElementById('view-notes').textContent), null), 'C deletes it; it is gone for A');

  // Rule 7 at the HTTP edge.
  ok((await call(D, 'GET', path)).status === 403, 'someone outside the meeting cannot read them');
  ok((await fetch(BASE + path)).status === 401, 'an anonymous caller cannot');
  ok((await call(B, 'POST', `${path}/items`, { section: 'notes', body: 'x' }, false)).status === 403, 'no CSRF header: refused');
  ok((await call(B, 'POST', path, {})).status === 403, 'an attendee cannot create the notes');
  ok((await call(B, 'POST', `${path}/items`, { section: 'notes', body: 'x'.repeat(2001) })).status === 400, 'one over 2000 characters: refused');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
