// Spotlight and "End meeting", between two real browsers, as people use them:
//   - only the host is offered "Spotlight for everyone"; the member is not;
//   - the host spotlights the member after a confirmation: both stages show
//     the member as "Spotlighted", the member is told "You're spotlighted";
//   - the spotlighted member is offered "Exit spotlight", which takes them off
//     both stages; a spotlighted person cannot be pinned ("Pin for me");
//   - "Stop all spotlights" from the meeting's "..." in People;
//   - the member has no arrow on Leave; the host ends the meeting from it:
//     both see "This meeting has ended", and no camera or microphone is left on.
// The rules themselves are proved at the server (signal/spotlight_test.go);
// this proves the screens obey them.
//
//   node tools/e2e/spotlight.e2e.mjs [https://localhost:8443]
import {
  BASE, accounts, launch, signedIn, enter, openPeople, personMenu, pickPerson, pickMeeting,
  confirmYes, toastSeen, chip, until, ok, fail, failed, sleep,
} from './meet.lib.mjs';

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const room = 'spot-' + Math.random().toString(36).slice(2, 8);
const role = (p) => p.evaluate(() => document.getElementById('chip-role').textContent);
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  await enter(A, room); // first in hosts
  await enter(B, room);
  await sleep(2500);
  await openPeople(A);
  await openPeople(B);
  ok(await until(A, () => document.querySelectorAll('#view-people .person').length >= 2, null), 'host sees both people');

  const bMenu = await personMenu(B, 'You');
  ok(bMenu && !bMenu.includes('Spotlight for everyone'), `the member is offered no spotlight (${bMenu})`);
  ok(await B.evaluate(() => document.getElementById('btn-leave-more').hidden), 'the member is offered no "End meeting" (no arrow on Leave)');

  ok(await pickPerson(A, 'E2E B', 'Spotlight for everyone'), 'host picks "Spotlight for everyone" on E2E B');
  await confirmYes(A, 'Spotlight for everyone?');
  ok(await toastSeen(B, "You're spotlighted"), 'the member is told "You\'re spotlighted"');
  ok(await until(A, () => document.getElementById('chip-role').textContent === 'Spotlighted', null), `host stage: ${JSON.stringify(await chip(A))}`);
  ok(await until(B, () => document.getElementById('chip-role').textContent === 'Spotlighted', null), `member stage: ${JSON.stringify(await chip(B))}`);

  ok((await personMenu(B, 'You'))?.includes('Exit spotlight'), 'the member is offered "Exit spotlight"');
  await pickPerson(B, 'You', 'Exit spotlight');
  await confirmYes(B, 'Exit spotlight?');
  ok(await until(A, () => document.getElementById('chip-role').textContent !== 'Spotlighted', null), `exit spotlight leaves the host stage (${await role(A)})`);
  ok(await toastSeen(B, "You're no longer spotlighted"), 'the member is told "You\'re no longer spotlighted"');

  // Two lit: one holds the stage, the other leads the strip and cannot be pinned.
  ok(await pickPerson(A, 'You', 'Spotlight for everyone'), 'host spotlights themselves');
  await confirmYes(A, 'Spotlight for everyone?');
  ok(await until(B, () => document.getElementById('chip-role').textContent === 'Spotlighted', null), 'the member sees the host spotlighted');
  ok(await pickPerson(A, 'E2E B', 'Spotlight for everyone'), 'host spotlights the member as well (two lit)');
  await confirmYes(A, 'Spotlight for everyone?');
  ok(await until(A, () => !!document.querySelector('#strip .strip-tile.spotlit'), null), 'a second spotlighted person leads the host strip, marked');
  await A.evaluate(() => document.querySelector('#strip .strip-tile.spotlit .tile-more')?.click());
  await sleep(200);
  await A.evaluate(() => document.querySelector('#tile-menu [data-k="pin"]')?.click());
  ok(await toastSeen(A, "Can't pin a participant that is spotlighted"), 'pinning a spotlighted person is refused with Teams\' words');
  ok(await pickMeeting(A, 'Stop all spotlights'), 'host picks "Stop all spotlights" from the meeting menu');
  await confirmYes(A, 'Stop spotlighting all videos?');
  ok(await toastSeen(B, "You're no longer spotlighted"), 'stop all: the spotlighted member is told they are no longer spotlighted');

  // End the meeting for everyone, from Leave's arrow.
  const liveBefore = await B.evaluate(() => window.__liveTracks());
  await A.evaluate(() => document.getElementById('btn-leave-more').click());
  await sleep(150);
  await A.evaluate(() => document.getElementById('hc-end').click());
  await confirmYes(A, 'End the meeting?');
  for (const [p, who] of [[A, 'host'], [B, 'member']]) {
    ok(await until(p, () => document.getElementById('lobby-head')?.textContent === 'This meeting has ended' && !document.getElementById('lobby-cover').classList.contains('hidden'), null), `${who} sees "This meeting has ended"`);
    const live = await p.evaluate(() => window.__liveTracks());
    ok(live === 0, `${who}: no camera or microphone left on (${live} live${who === 'member' ? `, ${liveBefore} before` : ''})`);
  }
  ok((await A.evaluate(() => document.getElementById('lobby-leave').textContent)) === 'Back to home', 'the cover offers "Back to home"');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
