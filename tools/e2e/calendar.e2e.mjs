// The calendar, in two real browsers:
//   - A schedules a weekly series with B invited, from the dialog; the week
//     view shows one occurrence a week, marked as repeating, week after week;
//   - B's invitation appears once (not once per occurrence) and says the
//     rule; B confirms it;
//   - A cancels one occurrence from the entry detail (asked twice in the
//     same button); that week empties for A and for B, the next does not;
//   - who is free: B has a meeting at the time A proposes; the assistant
//     shows B busy, and "Find the next free time" moves the proposal past it;
//   - rule 7 at the HTTP edge: an invitee cannot cancel an occurrence, an
//     anonymous caller cannot read free/busy, no CSRF header is refused,
//     21 people or 15 days are refused, a bad rule is refused by name;
//   - A cancels the series; it is gone from both calendars.
//
//   node tools/e2e/calendar.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, until, ok, fail, failed, sleep, toastSeen } from './meet.lib.mjs';

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const title = 'Weekly sync ' + Math.random().toString(36).slice(2, 7);

// Next Monday 10:00 local, at least two days out so "today" never collides.
const monday = (() => {
  const d = new Date();
  d.setDate(d.getDate() + ((8 - d.getDay()) % 7 || 7));
  d.setHours(10, 0, 0, 0);
  return d;
})();
const plus = (d, days, h = d.getHours(), m = d.getMinutes()) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + days, h, m);
const iso = (d) => d.toISOString();

const call = (p, method, path, body, csrf = true) => p.evaluate(async (method, path, body, csrf) => {
  const h = { 'content-type': 'application/json' };
  if (csrf) h['X-Aether-CSRF'] = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
  const r = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, method, path, body, csrf);
const openCalendar = async (p) => {
  await p.evaluate(() => document.querySelector('[data-home="meet"]')?.click());
  await sleep(200);
  await p.evaluate(() => document.querySelector('[data-home="calendar"]')?.click());
  await until(p, () => !!document.getElementById('cv-week'), null);
};
// Select a day in the month grid (moving the month first) and a view.
const goTo = async (p, d, view) => {
  await p.evaluate(async (ymd, view) => {
    const want = new Date(ymd);
    for (let i = 0; i < 24; i++) {
      const label = document.getElementById('month-name').textContent;
      const shown = new Date(label);
      if (shown.getFullYear() === want.getFullYear() && shown.getMonth() === want.getMonth()) break;
      document.getElementById(shown < want ? 'm-next' : 'm-prev').click();
    }
    const btn = [...document.querySelectorAll('#days [data-d]')].find((b) => {
      const x = new Date(b.dataset.d);
      return x.getFullYear() === want.getFullYear() && x.getMonth() === want.getMonth() && x.getDate() === want.getDate();
    });
    btn.click();
    document.getElementById('cv-' + view).click();
  }, d.toISOString(), view);
  await sleep(150);
};
const setLocal = (p, id, d) => p.evaluate((id, ms) => {
  const x = new Date(ms);
  const pad = (n) => String(n).padStart(2, '0');
  const el = document.getElementById(id);
  el.value = `${x.getFullYear()}-${pad(x.getMonth() + 1)}-${pad(x.getDate())}T${pad(x.getHours())}:${pad(x.getMinutes())}`;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}, id, d.getTime());
const weekEvents = (p) => p.evaluate((t) => [...document.querySelectorAll('.wev')].filter((e) => e.textContent.includes(t)).map((e) => ({ rep: !!e.querySelector('.rep-mark'), time: e.querySelector('.wev-time').textContent.trim() })), title);
const dayEvents = (p) => p.evaluate((t) => [...document.querySelectorAll('#day-body .event')].filter((e) => e.textContent.includes(t)).length, title);

try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);

  // A schedules the series from the dialog.
  await openCalendar(A);
  await A.evaluate(() => document.getElementById('cal-new').click());
  await until(A, () => !!document.getElementById('s-rep'), null);
  await A.evaluate((t, inv) => {
    document.getElementById('s-title').value = t;
    document.getElementById('s-inv').value = inv;
  }, title, acc.B.email);
  await setLocal(A, 's-start', monday);
  await setLocal(A, 's-end', plus(monday, 0, 10, 30));
  await A.evaluate(() => {
    const s = document.getElementById('s-rep');
    s.value = 'weekly';
    s.dispatchEvent(new Event('change'));
  });
  const repUI = await A.evaluate(() => ({
    shown: !document.getElementById('s-rep-opts').hidden,
    monday: document.querySelector('#s-rep-days [data-wd="1"]').getAttribute('aria-pressed') === 'true',
    until: document.getElementById('s-rep-until').value,
  }));
  ok(repUI.shown && repUI.monday && repUI.until, `choosing "Every week" shows the rule with Monday pressed and an end date (${JSON.stringify(repUI)})`);
  await A.evaluate(() => document.querySelector('#dialog [data-confirm]').click());
  ok(await toastSeen(A, 'repeating'), 'A is told it is scheduled, repeating');
  await sleep(600);

  // The week view: one occurrence per week, marked as repeating.
  await goTo(A, monday, 'week');
  let evs = await weekEvents(A);
  ok(evs.length === 1 && evs[0].rep, `the week shows one occurrence with the repeat mark (${JSON.stringify(evs)})`);
  const labelW1 = await A.evaluate(() => document.getElementById('day-label').textContent);
  await A.evaluate(() => document.getElementById('d-next').click());
  await sleep(150);
  const labelW2 = await A.evaluate(() => document.getElementById('day-label').textContent);
  evs = await weekEvents(A);
  ok(labelW1 !== labelW2 && evs.length === 1, `the next week (${labelW2}) has the next occurrence`);
  await A.evaluate(() => document.getElementById('d-next').click());
  await sleep(150);
  ok((await weekEvents(A)).length === 1, 'and the week after');
  await goTo(A, monday, 'day');
  ok((await dayEvents(A)) === 1, 'the day view shows it too');

  // B: one invitation, saying the rule; confirm it.
  await openCalendar(B);
  const invB = await until(B, (t) => {
    const cards = [...document.querySelectorAll('#inv-list .inv')].filter((c) => c.textContent.includes(t));
    return cards.length === 1 && /Every week/.test(cards[0].textContent) ? cards[0].textContent : false;
  }, title);
  ok(!!invB, 'B sees exactly one invitation for the series, saying "Every week"');
  await B.evaluate((t) => [...document.querySelectorAll('#inv-list .inv')].find((c) => c.textContent.includes(t)).querySelector('[data-accept]').click(), title);
  ok(await toastSeen(B, 'Confirmed'), 'B confirms');
  ok(await until(B, (t) => ![...document.querySelectorAll('#inv-list .inv')].some((c) => c.textContent.includes(t) && c.querySelector('[data-accept]')), title), 'the invitation is answered');

  // A cancels the second occurrence from the entry detail.
  await goTo(A, plus(monday, 7), 'week');
  await A.evaluate((t) => [...document.querySelectorAll('.wev')].find((e) => e.textContent.includes(t)).click(), title);
  ok(await until(A, () => /Every week on Mon/.test(document.getElementById('dialog').textContent) && !!document.querySelector('[data-cancel-occ]'), null), 'the detail names the rule and offers "Cancel this occurrence"');
  await A.evaluate(() => document.querySelector('[data-cancel-occ]').click());
  const asked = await A.evaluate(() => document.querySelector('[data-cancel-occ]').textContent);
  ok(/just this one/.test(asked), `the first press asks (${asked})`);
  await A.evaluate(() => document.querySelector('[data-cancel-occ]').click());
  ok(await toastSeen(A, 'occurrence is cancelled'), 'the second press cancels that occurrence');
  await sleep(500);
  await goTo(A, plus(monday, 7), 'week');
  ok((await weekEvents(A)).length === 0, 'that week is empty for A');
  await goTo(A, plus(monday, 14), 'week');
  ok((await weekEvents(A)).length === 1, 'the week after still has its occurrence');
  await openCalendar(B);
  await goTo(B, plus(monday, 7), 'day');
  ok(await until(B, () => /Nothing scheduled/.test(document.getElementById('day-body').textContent), null), 'and it is gone from B\'s day');
  await goTo(B, monday, 'day');
  ok((await dayEvents(B)) === 1, 'B still has the first');

  // Who is free. B is busy next Tuesday 10:00-11:00; A proposes 10:00.
  const tue = plus(monday, 1);
  const made = await call(B, 'POST', '/api/scheduling/meetings', { title: 'B busy block', category: 'internal', startsAt: iso(tue), endsAt: iso(plus(tue, 0, 11, 0)), invitees: [] });
  ok(made.status === 201, `B's busy block is made (${made.status})`);
  await A.evaluate(() => document.getElementById('cal-new').click());
  await until(A, () => !!document.getElementById('s-assist'), null);
  await A.evaluate((inv) => {
    const el = document.getElementById('s-inv');
    el.value = inv;
    el.dispatchEvent(new Event('input', { bubbles: true }));
  }, acc.B.email);
  await setLocal(A, 's-start', tue);
  await setLocal(A, 's-end', plus(tue, 0, 10, 30));
  const busyShown = await until(A, (b) => {
    const a = document.getElementById('s-assist');
    if (a.hidden) return false;
    const row = [...a.querySelectorAll('.assist-row')].find((r) => r.textContent.includes(b));
    return !!row && row.classList.contains('clash') && row.querySelectorAll('.assist-bar i').length >= 1;
  }, acc.B.email);
  const note = await A.evaluate(() => document.getElementById('s-assist-note').textContent);
  ok(busyShown && /busy/.test(note), `the assistant shows B busy at that time ("${note}")`);
  await A.evaluate(() => document.getElementById('s-find').click());
  const movedOk = await until(A, () => /everyone is free/i.test(document.getElementById('s-assist-note').textContent), null);
  const moved = await A.evaluate(() => document.getElementById('s-start').value);
  ok(movedOk && new Date(moved).getTime() >= plus(tue, 0, 11, 0).getTime(), `"Find the next free time" moves the proposal past B's meeting (${moved})`);
  await A.evaluate(() => document.querySelector('#dialog [data-cancel]').click());

  // Rule 7 at the edge.
  const list = await call(A, 'GET', `/api/scheduling/meetings?from=${encodeURIComponent(iso(plus(monday, -1)))}&to=${encodeURIComponent(iso(plus(monday, 30)))}`);
  const series = (list.body?.meetings ?? []).find((m) => m.title === title);
  ok(!!series?.recurrence && !!series?.occurrence, `the list carries the rule and the occurrence (${!!series?.recurrence}, ${series?.occurrence})`);
  const third = plus(monday, 14).getTime();
  ok((await call(B, 'POST', `/api/scheduling/meetings/${series.id}/cancel`, { occurrence: third })).status === 403, 'an invitee cannot cancel an occurrence');
  ok((await call(A, 'POST', `/api/scheduling/meetings/${series.id}/cancel`, { occurrence: third + 60000 })).status === 404, 'a time that is not an occurrence is refused');
  ok((await call(A, 'POST', `/api/scheduling/meetings/${series.id}/cancel`, { occurrence: third }, false)).status === 403, 'no CSRF header: refused');
  ok((await fetch(`${BASE}/api/scheduling/freebusy?emails=${acc.B.email}&from=${iso(tue)}&to=${iso(plus(tue, 1))}`)).status === 401, 'an anonymous caller cannot read free/busy');
  const many = Array.from({ length: 21 }, (_, i) => `p${i}@example.test`).join(',');
  ok((await call(A, 'GET', `/api/scheduling/freebusy?emails=${many}&from=${encodeURIComponent(iso(tue))}&to=${encodeURIComponent(iso(plus(tue, 1)))}`)).status === 400, '21 people: refused');
  ok((await call(A, 'GET', `/api/scheduling/freebusy?emails=${acc.B.email}&from=${encodeURIComponent(iso(tue))}&to=${encodeURIComponent(iso(plus(tue, 15)))}`)).status === 400, '15 days: refused');
  const fb = await call(A, 'GET', `/api/scheduling/freebusy?emails=${acc.B.email}&from=${encodeURIComponent(iso(tue))}&to=${encodeURIComponent(iso(plus(tue, 1)))}`);
  const spans = fb.body?.busy?.[acc.B.email.toLowerCase()] ?? [];
  // At least one span (a rerun on the same database has more than one), and
  // each is from and to, nothing else.
  ok(fb.status === 200 && spans.length >= 1 && spans.every((x) => Object.keys(x).sort().join() === 'from,to'), `free/busy answers spans and nothing else (${JSON.stringify(spans)})`);
  const badRule = await call(A, 'POST', '/api/scheduling/meetings', { title: 'x', category: 'internal', startsAt: iso(tue), endsAt: iso(plus(tue, 0, 11, 0)), invitees: [], recurrence: { freq: 'yearly', until: iso(plus(tue, 30)) } });
  ok(badRule.status === 400 && badRule.body?.code === 'bad_recurrence', `a bad rule is refused by name (${badRule.status} ${badRule.body?.code})`);
  const offDay = await call(A, 'POST', '/api/scheduling/meetings', { title: 'x', category: 'internal', startsAt: iso(tue), endsAt: iso(plus(tue, 0, 11, 0)), invitees: [], recurrence: { freq: 'weekly', days: [1], until: iso(plus(tue, 30)) } });
  ok(offDay.status === 400, `a weekly rule that excludes its own first meeting is refused (${offDay.status})`);
  const noEnd = await call(A, 'POST', '/api/scheduling/meetings', { title: 'x', category: 'internal', startsAt: iso(tue), endsAt: iso(plus(tue, 0, 11, 0)), invitees: [], recurrence: { freq: 'daily' } });
  ok(noEnd.status === 400, `a rule without an end is refused (${noEnd.status})`);

  // The series is cancelled for everyone.
  await goTo(A, monday, 'week');
  await A.evaluate((t) => [...document.querySelectorAll('.wev')].find((e) => e.textContent.includes(t)).click(), title);
  await until(A, () => !!document.querySelector('[data-cancel-all]'), null);
  await A.evaluate(() => document.querySelector('[data-cancel-all]').click());
  await A.evaluate(() => document.querySelector('[data-cancel-all]').click());
  ok(await toastSeen(A, 'series is cancelled'), 'A cancels the series');
  await sleep(500);
  await goTo(A, monday, 'week');
  ok((await weekEvents(A)).length === 0, 'it is gone from A\'s week');
  await openCalendar(B);
  await goTo(B, monday, 'day');
  ok(await until(B, () => /Nothing scheduled/.test(document.getElementById('day-body').textContent), null), 'and from B\'s');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
