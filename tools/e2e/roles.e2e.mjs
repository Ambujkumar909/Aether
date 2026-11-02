// Roles, Meeting options, hands and removal, among three real browsers --
// Teams' People panel as a person uses it:
//   - the panel names roles: Organizer, Attendee; an attendee's "..." on
//     someone else offers only "Pin for me";
//   - Meeting options (More): "Allow mic for attendees?" and "Allow reactions"
//     off -> an attendee's Mic and React are unavailable;
//   - "Make a presenter": that person is told and may unmute with attendee
//     mics off; "Make an attendee" takes it back;
//   - two raise hands: "Raised hands (2)" in the order they went up; "Lower
//     all hands" lowers both and each is told "Your hand was lowered";
//   - "Remove from meeting": they see "You've been removed from this meeting"
//     and no device is left on;
//   - the organizer leaves: the one who takes over is told, and has the
//     host's controls -- before, they never knew and the controls stayed grey.
//
//   node tools/e2e/roles.e2e.mjs [https://localhost:8443]
import {
  BASE, accounts, launch, signedIn, enter, openPeople, personMenu, pickPerson, pickMeeting,
  confirmYes, toastSeen, until, ok, fail, failed, sleep,
} from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const room = 'roles-' + Math.random().toString(36).slice(2, 8);
const sub = (page, name) => page.evaluate((name) => {
  const row = [...document.querySelectorAll('#view-people .person')].find((r) => r.querySelector('.person-name')?.textContent.includes(name));
  return row?.querySelector('.person-sub')?.textContent ?? null;
}, name);
const micBlocked = (page) => page.evaluate(() => document.getElementById('btn-mic').disabled);
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  await enter(A, room);
  await enter(B, room);
  await enter(C, room);
  await sleep(2500);
  for (const p of [A, B, C]) await openPeople(p);
  ok(await until(A, () => document.querySelectorAll('#view-people .person').length >= 3, null), 'host sees three people');

  ok((await sub(A, 'You'))?.startsWith('Organizer'), `the organizer is labelled (${await sub(A, 'You')})`);
  ok((await sub(A, 'E2E B'))?.startsWith('Attendee'), `an attendee is labelled (${await sub(A, 'E2E B')})`);
  const bOnC = await personMenu(B, 'E2E C');
  ok(JSON.stringify(bOnC) === JSON.stringify(['Pin for me']), `an attendee's menu on someone else is only "Pin for me" (${bOnC})`);
  const aOnB = await personMenu(A, 'E2E B');
  ok(aOnB?.includes('Make a presenter') && aOnB.includes('Remove from meeting') && !aOnB.includes('Make an attendee'), `the host's menu on an attendee (${aOnB})`);
  const aOnSelf = await personMenu(A, 'You');
  ok(!aOnSelf.includes('Remove from meeting') && !aOnSelf.includes('Make a presenter'), `nothing re-roles or removes the organizer (${aOnSelf})`);
  ok(await C.evaluate(() => document.getElementById('more-options').hidden), 'an attendee has no "Meeting options"');

  // Meeting options: attendee mics and reactions off.
  await A.evaluate(() => { document.getElementById('btn-more').click(); });
  await sleep(150);
  await A.evaluate(() => document.getElementById('more-options').click());
  ok(await until(A, () => document.getElementById('mo-title')?.textContent === 'Meeting options', null), 'Meeting options opens from More');
  await A.evaluate(() => {
    document.getElementById('mo-mic').checked = false;
    document.getElementById('mo-react').checked = false;
    document.querySelector('#dialog [data-yes]').click();
  });
  ok(await until(C, () => document.getElementById('btn-mic').disabled, null), 'an attendee\'s Mic is unavailable with "Allow mic for attendees?" off');
  ok(await until(C, () => document.querySelector('#btn-react > button').disabled, null), 'an attendee\'s React is unavailable with "Allow reactions" off');
  ok(!(await micBlocked(A)), 'the organizer is not bound by attendee controls');

  // Make a presenter, and back.
  ok(await pickPerson(A, 'E2E B', 'Make a presenter'), 'host picks "Make a presenter" on B');
  ok(await toastSeen(B, 'Your role has been changed'), 'B is told "Your role has been changed"');
  ok(await until(B, () => !document.getElementById('btn-mic').disabled, null), 'a presenter may unmute with attendee mics off');
  // ...and stays unmuted when the controls are re-sent: the client used to
  // apply attendee controls to presenters and mute them on its own.
  if (await B.evaluate(() => document.getElementById('btn-mic').classList.contains('off'))) await B.evaluate(() => document.getElementById('btn-mic').click());
  ok(await until(B, () => !document.getElementById('btn-mic').classList.contains('off'), null), 'the presenter unmutes');
  await A.evaluate(() => { document.getElementById('btn-more').click(); });
  await sleep(150);
  await A.evaluate(() => document.getElementById('more-options').click());
  await until(A, () => document.getElementById('mo-title')?.textContent === 'Meeting options', null);
  await A.evaluate(() => document.querySelector('#dialog [data-yes]').click());
  await sleep(1200);
  ok(!(await B.evaluate(() => document.getElementById('btn-mic').classList.contains('off'))), 'the presenter is still unmuted after the controls are re-sent');
  ok(await until(A, (n) => [...document.querySelectorAll('#view-people .person')].some((r) => r.textContent.includes(n) && r.querySelector('.person-sub').textContent.startsWith('Presenter')), 'E2E B'), 'the host sees B as Presenter');
  ok(await pickPerson(A, 'E2E B', 'Make an attendee'), 'host picks "Make an attendee" on B');
  ok(await toastSeen(B, 'Your role has been changed to attendee'), 'B is told "Your role has been changed to attendee"');
  ok(await until(B, () => document.getElementById('btn-mic').disabled, null), 'back to attendee: Mic unavailable again');

  // Hands, in order, and Lower all hands.
  await B.evaluate(() => document.getElementById('btn-hand').click());
  await sleep(400);
  await C.evaluate(() => document.getElementById('btn-hand').click());
  ok(await until(A, () => [...document.querySelectorAll('.pp-head strong')].some((s) => s.textContent === 'Raised hands (2)'), null), 'the host sees "Raised hands (2)"');
  const order = await A.evaluate(() => {
    const sec = [...document.querySelectorAll('.pp-section')].find((s) => s.querySelector('strong')?.textContent.startsWith('Raised hands'));
    return [...sec.querySelectorAll('.person')].map((r) => r.querySelector('.person-name').firstChild.textContent.trim() + ':' + r.querySelector('.hand-rank')?.textContent.trim());
  });
  ok(JSON.stringify(order) === JSON.stringify(['E2E B:1', 'E2E C:2']), `in the order they went up (${order})`);
  await A.evaluate(() => document.getElementById('pp-lower-all').click());
  await confirmYes(A, 'Lower all hands?');
  ok(await toastSeen(B, 'Your hand was lowered'), 'B is told "Your hand was lowered"');
  ok(await toastSeen(C, 'Your hand was lowered'), 'C is told "Your hand was lowered"');
  ok(await until(A, () => ![...document.querySelectorAll('.pp-head strong')].some((s) => s.textContent.startsWith('Raised hands')), null), 'no raised hands left on the host\'s panel');
  ok(!(await B.evaluate(() => document.getElementById('btn-hand').classList.contains('on'))), 'B\'s Raise button is down again');

  // "Disable mic" / "Allow mic" on one attendee.
  ok(await pickPerson(A, 'E2E C', 'Disable mic'), 'host picks "Disable mic" on C');
  ok(await toastSeen(C, 'Your mic has been disabled'), 'C is told "Your mic has been disabled"');
  ok(await until(C, () => document.getElementById('btn-mic').disabled, null), 'C\'s Mic is unavailable');
  ok(await until(A, () => [...document.querySelectorAll('#view-people .person')].some((r) => r.textContent.includes('E2E C') && r.querySelector('.person-sub').textContent.includes('Mic disabled')), null), 'the host\'s list says "Mic disabled"');
  ok((await personMenu(A, 'E2E C'))?.includes('Allow mic'), 'the host is offered "Allow mic"');
  ok(await pickPerson(A, 'E2E C', 'Allow mic'), 'host picks "Allow mic"');
  ok(await toastSeen(C, "Your mic has been enabled. Unmute if you'd like to speak."), 'C is told the mic is enabled');
  ok(!(await personMenu(B, 'E2E C'))?.includes('Disable mic'), 'an attendee is not offered "Disable mic"');

  // Download attendance list: the host gets the CSV as an attachment; an
  // attendee, an anonymous caller, a foreign site and an over-long room id
  // get nothing (CLAUDE.md rule 7: stranger / anonymous / CSRF / bound).
  const path = `/api/scheduling/rooms/${room}/attendance`;
  const fetchIn = (page, u) => page.evaluate(async (u) => {
    const r = await fetch(u);
    return { s: r.status, ct: r.headers.get('content-type') ?? '', cd: r.headers.get('content-disposition') ?? '', body: await r.text() };
  }, u);
  const csv = await fetchIn(A, path);
  ok(csv.s === 200 && csv.ct.startsWith('text/csv') && csv.cd.startsWith('attachment'), `host gets a CSV attachment (${csv.s} ${csv.ct} ${csv.cd})`);
  ok(csv.body.includes('Attended participants,3') && csv.body.includes('E2E B') && csv.body.includes('E2E C') && csv.body.includes('In the meeting'), 'the list names all three, still in the meeting');
  ok((await fetchIn(B, path)).s === 403, 'an attendee cannot download it');
  ok((await fetch(BASE + path)).status === 401, 'an anonymous caller cannot');
  const cookie = (await A.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
  const foreign = await fetch(BASE + path, { headers: { cookie, origin: 'https://evil.example' } });
  ok(!foreign.headers.get('access-control-allow-origin'), 'a foreign site is given no cross-origin read (a GET with no side effects; nothing to forge)');
  ok((await fetchIn(A, `/api/scheduling/rooms/${'r'.repeat(129)}/attendance`)).s === 403, 'a room id one over the bound is refused');
  ok(await pickMeeting(A, 'Download attendance list'), 'host picks "Download attendance list"');
  await sleep(800);
  ok(!(await A.evaluate(() => document.getElementById('toast')?.classList.contains('bad') && document.getElementById('toast')?.classList.contains('show'))), 'no error shown for the host');

  // Remove from meeting.
  ok(await pickPerson(A, 'E2E C', 'Remove from meeting'), 'host picks "Remove from meeting" on C');
  await confirmYes(A, 'Remove from meeting?');
  ok(await until(C, () => document.getElementById('lobby-head')?.textContent === "You've been removed from this meeting", null), 'C sees "You\'ve been removed from this meeting"');
  ok((await C.evaluate(() => window.__liveTracks())) === 0, 'C: no camera or microphone left on');
  ok(await until(A, () => document.getElementById('mb-people-count').textContent === '2', null), 'the host\'s People count drops to 2');
  ok((await fetchIn(C, path)).s === 403, 'a removed person -- now a stranger to the meeting -- cannot download it');

  // The organizer leaves: B takes over, and knows it.
  await A.evaluate(() => document.getElementById('btn-leave').click());
  ok(await until(B, () => !document.getElementById('btn-leave-more').hidden, null, 10000), 'the new host gets "End meeting" on Leave');
  ok(await until(B, () => !document.getElementById('more-options').hidden && !!document.getElementById('hc-mute-all'), null), 'the new host has Meeting options and "Mute all"');
  ok((await sub(B, 'You'))?.startsWith('Organizer'), `the new host is the organizer (${await sub(B, 'You')})`);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
