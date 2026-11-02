// "5 minutes left in your meeting." -- Teams' heads-up before a scheduled
// meeting's end. A meeting scheduled to end in four minutes is joined from
// its link: the notice appears at once, and the meeting carries on (nothing
// ends on the clock). An ad-hoc meeting, with no scheduled end, says nothing.
//
//   node tools/e2e/endnotice.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, toastSeen, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A']);
const browser = await launch();
const log = [];
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const room = 'endnotice-' + Math.random().toString(36).slice(2, 8);
  const made = await A.evaluate(async (room) => {
    const csrf = document.cookie.split('; ').find((c) => c.startsWith('aether_csrf='))?.split('=')[1];
    const now = Date.now();
    const r = await fetch('/api/scheduling/meetings', {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'X-Aether-CSRF': csrf },
      body: JSON.stringify({ room, title: 'Ends soon', category: 'internal', startsAt: new Date(now - 60000).toISOString(), endsAt: new Date(now + 4 * 60000).toISOString(), invitees: [] }),
    });
    return r.status;
  }, room);
  ok(made === 201 || made === 200, `scheduled a meeting ending in four minutes (${made})`);
  await enter(A, room);
  ok(await toastSeen(A, '5 minutes left in your meeting.'), 'the notice appears');
  await sleep(1500);
  ok(await A.evaluate(() => document.getElementById('lobby-cover').classList.contains('hidden') && !document.getElementById('app').classList.contains('hidden')), 'the meeting carries on');

  // Ad hoc: no scheduled end, no notice.
  await enter(A, 'adhoc-' + Math.random().toString(36).slice(2, 8));
  await sleep(4000);
  ok(!(await A.evaluate(() => document.getElementById('toast')?.textContent.includes('minutes left'))), 'an ad-hoc meeting says nothing');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
