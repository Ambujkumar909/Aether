// Breakout rooms among four real browsers, as Teams runs them:
//   - only the host has "Rooms"; "Create rooms" automatically assigns the two
//     attendees to two rooms;
//   - "Open Rooms": each is told "We'll move you to your assigned room in N
//     seconds." and lands in their room, its name and "Return" in a banner;
//     the host's panel says "Breakout rooms are open" and counts them in;
//   - a stranger who opens a room's link directly is refused with Teams'
//     words; nobody gets in without a ticket;
//   - "Make an announcement" reaches both rooms;
//   - the meeting cannot be locked while the rooms are open;
//   - "Return": an attendee goes back to the main meeting by themselves;
//   - "Close Rooms": the other is told "Your breakout room is closing." and
//     brought back; "Delete rooms" clears them.
// The rules are proved at the server (signal/breakout_test.go: tickets,
// timer, bounds); this proves the screens and the moves.
//
//   node tools/e2e/breakout.e2e.mjs [https://localhost:8443]
import {
  BASE, accounts, launch, signedIn, enter, openPeople, pickMeeting,
  confirmYes, toastSeen, until, ok, fail, failed, sleep,
} from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C', 'D']);
const browser = await launch();
const log = [];
const main = 'brk-' + Math.random().toString(36).slice(2, 8);
const roomOf = (page) => page.evaluate(() => new URLSearchParams(location.search).get('room'));
const banner = (page) => page.evaluate(() => {
  const b = document.getElementById('bo-banner');
  return b && !b.classList.contains('hidden') ? b.textContent.replace(/\s+/g, ' ').trim() : '';
});
// Waits for the move cover, presses "Join now"/"Return now", waits to land.
async function moved(page, label, inRoom) {
  const shown = await until(page, () => !document.getElementById('lobby-cover').classList.contains('hidden') && /We'll move you/.test(document.getElementById('lobby-msg').textContent), null, 12000);
  const line = await page.evaluate(() => document.getElementById('lobby-msg').textContent);
  ok(shown, `${label}: told "${line}"`);
  await page.evaluate(() => document.getElementById('lobby-leave').click());
  await sleep(1500);
  return until(page, (inRoom) => {
    const app = document.getElementById('app');
    const b = document.getElementById('bo-banner');
    return app && !app.classList.contains('hidden') && (inRoom ? b && !b.classList.contains('hidden') : true);
  }, inRoom, 25000);
}
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  const D = await signedIn(browser, acc.D, 'D', log);
  await enter(A, main);
  await enter(B, main);
  await enter(C, main);
  await sleep(2500);
  ok(!(await A.evaluate(() => document.getElementById('btn-rooms').hidden)), 'the host has "Rooms"');
  ok(await B.evaluate(() => document.getElementById('btn-rooms').hidden), 'an attendee has no "Rooms"');

  await A.evaluate(() => document.getElementById('btn-rooms').click());
  ok(await until(A, () => !!document.getElementById('br-create'), null), 'the Rooms panel offers "Create rooms"');
  await A.evaluate(() => {
    document.getElementById('br-count').value = '2';
    document.getElementById('br-create').click();
  });
  ok(await until(A, () => document.querySelectorAll('#view-rooms .br-room').length === 2, null), 'two rooms are created');
  const assigned = await A.evaluate(() => [...document.querySelectorAll('#view-rooms .br-members')].map((m) => m.textContent.trim()));
  ok(assigned.join('|').includes('E2E B') && assigned.join('|').includes('E2E C'), `both attendees assigned automatically (${assigned})`);

  await A.evaluate(() => document.getElementById('br-open').click());
  ok(await moved(B, 'B', true), 'B lands in a room');
  ok(await moved(C, 'C', true), 'C lands in a room');
  const bRoom = await roomOf(B), cRoom = await roomOf(C);
  ok(bRoom !== main && bRoom.startsWith(main + '~br~') && cRoom !== bRoom, `two different rooms (${bRoom}, ${cRoom})`);
  ok(/^Room [12](?!\d).*Return/.test(await banner(B)), `B's banner: ${await banner(B)}`);
  ok(await until(A, () => document.querySelector('.br-status')?.textContent.startsWith('Breakout rooms are open') &&
    [...document.querySelectorAll('.br-count')].every((c) => c.textContent === '1 in room'), null, 10000), 'the host sees the rooms open, one person in each');

  // A stranger with the link.
  await D.goto(`${BASE}/?room=${encodeURIComponent(bRoom)}&media=ws`, { waitUntil: 'networkidle2' });
  await until(D, () => !!document.getElementById('pj-join') && getComputedStyle(document.getElementById('prejoin')).display !== 'none', null, 15000);
  await sleep(500);
  await D.evaluate(() => document.getElementById('pj-join').click());
  ok(await until(D, () => /assigned to a different one/.test(document.getElementById('lobby-msg')?.textContent ?? ''), null, 10000),
    `a stranger with the room's link is refused (${await D.evaluate(() => document.getElementById('lobby-msg')?.textContent)})`);

  // Announcement.
  await A.evaluate(() => document.getElementById('br-announce').click());
  await until(A, () => !!document.querySelector('#dialog textarea[name="text"]'), null);
  await A.evaluate(() => {
    document.querySelector('#dialog textarea[name="text"]').value = 'Two minutes left';
    document.querySelector('#dialog [data-yes]').click();
  });
  for (const [p, l] of [[B, 'B'], [C, 'C']]) {
    ok(await until(p, () => document.getElementById('takeover-title')?.textContent === 'Announcement' && document.getElementById('takeover-body').textContent.includes('Two minutes left'), null, 6000), `${l} sees the announcement`);
    await p.evaluate(() => document.querySelector('#dialog [data-yes]')?.click());
  }

  // No lock while open.
  await openPeople(A);
  ok(await pickMeeting(A, 'Lock the meeting'), 'host tries "Lock the meeting"');
  ok(await toastSeen(A, "You can't lock the meeting while breakout rooms are open."), 'refused with Teams\' words');

  // B returns by themselves.
  await B.evaluate(() => document.getElementById('bo-return').click());
  ok(await until(B, (m) => new URLSearchParams(location.search).get('room') === m && !document.getElementById('app').classList.contains('hidden'), main, 25000), 'B returns to the main meeting');
  ok(await until(B, () => document.getElementById('bo-banner').classList.contains('hidden'), null), 'no room banner in the main meeting');

  // Close: C is brought back.
  await A.evaluate(() => document.getElementById('rail-people').click()); // close People...
  await A.evaluate(() => document.getElementById('btn-rooms').click()); // ...open Rooms
  await until(A, () => !!document.getElementById('br-close'), null);
  await A.evaluate(() => document.getElementById('br-close').click());
  await confirmYes(A, 'Close Rooms?');
  ok(await until(C, () => document.getElementById('lobby-head')?.textContent === 'Your breakout room is closing.', null, 8000), 'C is told "Your breakout room is closing."');
  ok(await moved(C, 'C', false), 'C is moved back');
  ok((await roomOf(C)) === main, 'C is in the main meeting');
  ok(await until(A, () => !!document.getElementById('br-delete'), null), 'the host can delete closed rooms');
  await A.evaluate(() => document.getElementById('br-delete').click());
  await confirmYes(A, 'Delete rooms?');
  ok(await until(A, () => !!document.getElementById('br-create'), null), '"Delete rooms" clears them');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
