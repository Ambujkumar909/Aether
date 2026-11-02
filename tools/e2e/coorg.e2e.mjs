// "Choose co-organizers": a meeting scheduled with B as a co-organizer and C
// as a plain invitee. B arrives as Co-organizer -- labelled so, with the
// host's controls -- and C as Attendee. A co-organizer who was not invited
// is refused when scheduling.
//
//   node tools/e2e/coorg.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, openPeople, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const sub = (page, name) => page.evaluate((name) => {
  const row = [...document.querySelectorAll('#view-people .person')].find((r) => r.querySelector('.person-name')?.textContent.includes(name));
  return row?.querySelector('.person-sub')?.textContent ?? null;
}, name);
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  const room = 'coorg-' + Math.random().toString(36).slice(2, 8);
  const schedule = (body) => A.evaluate(async (body) => {
    const csrf = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
    const r = await fetch('/api/scheduling/meetings', { method: 'POST', headers: { 'content-type': 'application/json', 'X-Aether-CSRF': csrf }, body: JSON.stringify(body) });
    return { status: r.status, body: await r.json().catch(() => null) };
  }, body);
  const now = Date.now();
  const base = { title: 'Planning', category: 'internal', startsAt: new Date(now - 60000).toISOString(), endsAt: new Date(now + 3600000).toISOString() };
  const refused = await schedule({ ...base, room: room + 'x', invitees: [acc.C.email], coOrganizers: ['nobody-invited@example.test'] });
  ok(refused.status === 400 && /co-organizers must be people you invited/.test(JSON.stringify(refused.body)), `a co-organizer who was not invited is refused (${refused.status} ${JSON.stringify(refused.body)})`);
  const made = await schedule({ ...base, room, invitees: [acc.B.email, acc.C.email], coOrganizers: [acc.B.email] });
  ok(made.status === 201, `scheduled with B as co-organizer (${made.status})`);

  await enter(A, room);
  await enter(B, room);
  await enter(C, room);
  await sleep(2500);
  for (const p of [A, B, C]) await openPeople(p);
  ok(await until(A, () => document.querySelectorAll('#view-people .person').length >= 3, null), 'three people');
  ok((await sub(A, 'E2E B'))?.startsWith('Co-organizer'), `the organizer sees B as Co-organizer (${await sub(A, 'E2E B')})`);
  ok((await sub(A, 'E2E C'))?.startsWith('Attendee'), `and C as Attendee (${await sub(A, 'E2E C')})`);
  ok((await sub(B, 'You'))?.startsWith('Co-organizer'), `B sees themselves as Co-organizer (${await sub(B, 'You')})`);
  ok(await B.evaluate(() => !document.getElementById('more-options').hidden && !!document.getElementById('hc-mute-all') && !document.getElementById('btn-leave-more').hidden), 'B has the host controls');
  ok(await C.evaluate(() => document.getElementById('more-options').hidden && !document.getElementById('hc-mute-all')), 'C does not');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
