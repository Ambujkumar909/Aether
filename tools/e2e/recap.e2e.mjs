// The recap, in real browsers: A and B meet, the organizer writes a note,
// both leave; the recap then shows both people with their time and the
// note (captions, which write the transcript, need a microphone and speech
// recognition headless Chrome has not; the unit test covers the transcript
// in the recap); it opens from the recent
// list and from the lobby's "See the recap"; it is refused to a stranger
// and to an anonymous caller; and at phone width it is one column with
// nothing cut off.
//
//   node tools/e2e/recap.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep, confirmYes } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const room = 'recap-' + Math.random().toString(36).slice(2, 8);
const call = (p, method, path, body, csrf = true) => p.evaluate(async (method, path, body, csrf) => {
  const h = { 'content-type': 'application/json' };
  if (csrf) h['X-Aether-CSRF'] = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
  const r = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, method, path, body, csrf);

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log, { width: 390, height: 844 });
  await enter(A, room);
  await enter(B, room);
  await sleep(2500);
  // The organizer makes notes and adds a task; a transcript line is written
  // the way captions write it.
  ok((await call(A, 'POST', `/api/scheduling/rooms/${room}/notes`, {})).status === 204, 'A creates the meeting notes');
  const item = await call(A, 'POST', `/api/scheduling/rooms/${room}/notes/items`, { section: 'tasks', body: 'Send the recap' });
  ok(item.status === 201, `A adds a follow-up task (${item.status})`);
  await sleep(1500);

  // A, the host, ends the meeting for everyone; B lands in the lobby, which
  // offers the recap.
  await A.evaluate(() => document.getElementById('btn-leave-more')?.click());
  await sleep(300);
  await A.evaluate(() => document.getElementById('hc-end')?.click());
  await confirmYes(A, 'End the meeting?');
  const lobbyRecap = await until(B, () => {
    const b = document.getElementById('lobby-recap');
    return !!b && !b.classList.contains('hidden') && getComputedStyle(b).display !== 'none';
  }, null, 10000);
  ok(lobbyRecap, 'after the meeting ends the lobby offers "See the recap"');
  if (lobbyRecap) {
    await B.evaluate(() => document.getElementById('lobby-recap').click());
    const shown = await until(B, (t) => {
      const d = document.querySelector('.dialog-card.recap');
      return !!d && /Who was there/.test(d.textContent) && d.textContent.includes(t) && !/Loading/.test(d.textContent);
    }, 'Send the recap', 10000);
    ok(shown, 'the recap opens from the lobby, with the task');
    const text = await B.evaluate(() => document.querySelector('.dialog-card.recap')?.textContent || '');
    ok(/E2E A/.test(text) && /E2E B/.test(text), 'both people are listed');
    ok(/No transcript: captions were not on/.test(text), 'with no captions, the recap says there is no transcript');
    ok(/Download the attendance list/.test(text), 'the attendance list download is offered');
    await B.evaluate(() => document.querySelector('.dialog-card.recap [data-close]')?.click());
  }
  await sleep(1000);

  // From the recent list on the home page.
  await A.evaluate(() => document.getElementById('lobby-leave')?.click());
  await until(A, () => getComputedStyle(document.getElementById('home')).display !== 'none', null);
  await A.evaluate(() => document.querySelector('[data-home="meet"]')?.click());
  const rowBtn = await until(A, (r) => !!document.querySelector(`[data-recap="${r}"]`), room, 10000);
  ok(rowBtn, 'the recent list has a Recap button for the meeting');
  await A.evaluate((r) => document.querySelector(`[data-recap="${r}"]`).click(), room);
  ok(await until(A, () => /Who was there/.test(document.querySelector('.dialog-card.recap')?.textContent || '') && !/Loading/.test(document.querySelector('.dialog-card.recap')?.textContent || ''), null, 10000), 'and it opens the same recap');
  await A.evaluate(() => document.querySelector('.dialog-card.recap [data-close]')?.click());

  // The API: shape, and who may read it.
  const api = await call(A, 'GET', `/api/scheduling/rooms/${room}/recap`);
  ok(api.status === 200 && api.body.people?.length === 2 && api.body.notes?.length === 1 && api.body.transcript?.length === 0 && api.body.seconds > 0 && !api.body.live, `the recap API: 2 people, 1 note, no transcript, ${api.body?.seconds}s, over (${api.status})`);
  ok((await call(C, 'GET', `/api/scheduling/rooms/${room}/recap`)).status === 403, 'a stranger is refused');
  ok((await fetch(`${BASE}/api/scheduling/rooms/${room}/recap`)).status === 401, 'an anonymous caller is refused');
  ok((await call(C, 'GET', `/api/scheduling/rooms/${'x'.repeat(129)}/recap`)).status === 403 || (await call(C, 'GET', `/api/scheduling/rooms/${'x'.repeat(129)}/recap`)).status === 400, 'a room name over the bound is refused');

  // At phone width: C was in no meeting, so C is invited to a scheduled one
  // in the room and reads its recap in one column with nothing cut off.
  const start = new Date(Date.now() - 3600_000).toISOString();
  const end = new Date(Date.now() - 1800_000).toISOString();
  const sched = await call(A, 'POST', '/api/scheduling/meetings', { room, title: 'Recap E2E', category: 'internal', startsAt: start, endsAt: end, invitees: [acc.C.email] });
  ok(sched.status === 201, `A schedules the room's meeting with C invited (${sched.status})`);
  await C.evaluate(() => document.querySelector('[data-home="meet"]')?.click());
  await C.evaluate(async (r) => (await import('/src/app.js')), room).catch(() => {});
  const phone = await call(C, 'GET', `/api/scheduling/rooms/${room}/recap`);
  ok(phone.status === 200 && phone.body.title === 'Recap E2E', `an invitee who missed it may read it (${phone.status}, "${phone.body?.title}")`);
  // Open the recap sheet on the phone through the calendar's detail of the past meeting.
  await C.evaluate(() => document.querySelector('[data-home="calendar"]')?.click());
  await until(C, () => !!document.getElementById('cv-day'), null);
  await C.evaluate(() => {
    const want = new Date(Date.now() - 3600_000);
    const btn = [...document.querySelectorAll('#days [data-d]')].find((b) => { const x = new Date(b.dataset.d); return x.getDate() === want.getDate() && x.getMonth() === want.getMonth(); });
    btn?.click();
    document.getElementById('cv-day')?.click();
  });
  const opened = await until(C, () => {
    const ev = [...document.querySelectorAll('#day-body .event')].find((e) => /Recap E2E/.test(e.textContent));
    if (!ev) return false;
    ev.click();
    return true;
  }, null, 6000);
  ok(opened, 'C finds the past meeting in the day view');
  ok(await until(C, () => !!document.querySelector('[data-recap]'), null, 4000), 'its detail offers Recap, the meeting being over');
  await C.evaluate(() => document.querySelector('[data-recap]')?.click());
  const fit = await until(C, () => {
    const d = document.querySelector('.dialog-card.recap');
    if (!d || /Loading/.test(d.textContent)) return false;
    const r = d.getBoundingClientRect();
    return r.right <= innerWidth + 1 && d.scrollWidth <= d.clientWidth + 2 && getComputedStyle(d.querySelector('.rc-grid')).gridTemplateColumns.split(' ').length === 1;
  }, null, 8000);
  ok(fit, 'on a phone the recap is one column, inside the viewport, nothing cut off');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
