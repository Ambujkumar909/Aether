// Teams among three real browsers:
//   - Teams in the rail; "Join or create a team" > Create team;
//   - a public team: B finds it and joins, and is in General and the
//     standard channel but not the private one;
//   - a post in a team's channel reaches B as an unread channel, and opens;
//   - a private team: invisible to B; C joins it with its team code;
//   - leaving a team takes it away; deleting one takes it from everyone;
//   - every new endpoint refuses a stranger, nobody signed in, and a request
//     without the CSRF token; the name is bounded.
//
//   node tools/e2e/teams.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const tag = Math.random().toString(36).slice(2, 6);
const api = (p, fn, ...args) => p.evaluate(async (fn, args) => {
  const m = await import('/src/api.js');
  try { return { ok: true, v: await m.messaging[fn](...args) }; } catch (e) { return { ok: false, status: e.status, msg: e.message }; }
}, fn, args);
const teamsShown = (p) => p.evaluate(() => [...document.querySelectorAll('#teams-list .team-name')].map((x) => x.textContent));
const chansOf = (p, team) => p.evaluate((team) => {
  const t = [...document.querySelectorAll('#teams-list .team')].find((x) => x.querySelector('.team-name')?.textContent === team);
  return t ? [...t.querySelectorAll('.tch .tch-name')].map((x) => x.textContent) : null;
}, team);
const openTeams = (p) => p.evaluate(() => document.querySelector('[data-home="teams"]').click());
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  for (const p of [A, B, C]) await openTeams(p);
  await sleep(1200);

  // Create a public team through the interface.
  const design = `Design ${tag}`;
  await A.evaluate(() => document.getElementById('t-join').click());
  await until(A, () => !!document.querySelector('#dialog [data-create]'), null);
  await A.evaluate(() => document.querySelector('#dialog [data-create]').click());
  await until(A, () => !!document.getElementById('tc-name'), null);
  await A.evaluate((n) => {
    const i = document.getElementById('tc-name'); i.value = n; i.dispatchEvent(new Event('input'));
    document.querySelector('#dialog [data-seg="public"]').click();
    document.querySelector('#dialog [data-confirm]').click();
  }, design);
  ok(await until(A, (n) => [...document.querySelectorAll('#teams-list .team-name')].some((x) => x.textContent === n), design), 'Create team: the team appears');
  ok(JSON.stringify(await chansOf(A, design)) === '["General"]', `with its General channel: ${JSON.stringify(await chansOf(A, design))}`);
  const tid = (await api(A, 'teams')).v.teams.find((t) => t.name === design).id;
  const reviews = (await api(A, 'createTeamChannel', tid, 'Reviews', '', false)).v;
  const leads = (await api(A, 'createTeamChannel', tid, 'Leads', '', true)).v;
  ok(reviews?.id && leads?.id, 'a standard and a private channel');

  // B finds it and joins.
  await B.evaluate(() => document.getElementById('t-join').click());
  ok(await until(B, (n) => [...document.querySelectorAll('#tj-list .tj-team b')].some((x) => x.textContent === n), design), 'B finds the public team');
  await B.evaluate((n) => [...document.querySelectorAll('#tj-list .tj-team')].find((x) => x.querySelector('b').textContent === n).querySelector('[data-join]').click(), design);
  ok(await until(B, (n) => [...document.querySelectorAll('#teams-list .team-name')].some((x) => x.textContent === n), design), 'and joins it');
  const bChans = await chansOf(B, design);
  ok(bChans && bChans.includes('General') && bChans.includes('Reviews') && !bChans.includes('Leads'), `B is in General and Reviews, not the private Leads: ${JSON.stringify(bChans)}`);

  // A post reaches B as unread, and opens.
  await api(A, 'send', reviews.id, `Ship the review build ${tag}`);
  await sleep(800);
  await B.evaluate(async () => (await import('/src/chat.js')).refresh());
  ok(await until(B, (id) => !!document.querySelector(`#teams-list [data-tch="${id}"].unread`), reviews.id), 'the post shows Reviews as unread for B');
  await B.evaluate((id) => document.querySelector(`#teams-list [data-tch="${id}"]`).click(), reviews.id);
  ok(await until(B, (t) => document.getElementById('thread')?.textContent.includes(t), `Ship the review build ${tag}`), 'B opens the channel and reads it');
  ok(await B.evaluate((n) => document.getElementById('thread').textContent.includes(`${n} › Reviews`), design), "the channel is headed with its team's name");

  // A private team, and its code.
  const lead = (await api(A, 'createTeam', `Leadership ${tag}`, '', 'private')).v;
  ok((await api(B, 'browseTeams', 'Leadership')).v.teams.every((t) => t.id !== lead.id), 'B cannot browse the private team');
  const joinB = await api(B, 'joinTeam', lead.id);
  ok(!joinB.ok && joinB.status === 403, `nor join it by id: ${joinB.status}`);
  const membersB = await api(B, 'teamMembers', lead.id);
  ok(!membersB.ok && membersB.status === 403, `nor list its members: ${membersB.status}`);
  const code = (await api(A, 'teamCode', lead.id)).v.code;
  ok(/^[a-z2-9]{7}$/.test(code), `a seven-character team code: ${code}`);
  await C.evaluate(() => document.getElementById('t-join').click());
  await until(C, () => !!document.querySelector('#tj-code input'), null);
  await C.evaluate((c) => { document.querySelector('#tj-code input').value = c; document.querySelector('#tj-code [type="submit"]').click(); }, code);
  ok(await until(C, (n) => [...document.querySelectorAll('#teams-list .team-name')].some((x) => x.textContent === n), `Leadership ${tag}`), 'C joins the private team with the code');

  // The new endpoints refuse nobody, and a request without the CSRF token.
  const anon = await fetch(BASE + '/api/messaging/teams');
  ok(anon.status === 401, `nobody signed in: ${anon.status}`);
  const csrf = await A.evaluate(async () => (await fetch('/api/messaging/teams', { method: 'POST', credentials: 'same-origin', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name: 'x', visibility: 'public' }) })).status);
  ok(csrf === 403, `a request without the CSRF token: ${csrf}`);
  const long = await api(A, 'createTeam', 'n'.repeat(81), '', 'private');
  ok(long.ok && [...long.v.name].length === 80, `a name past 80 characters is cut to 80: ${long.v?.name?.length}`);
  await api(A, 'deleteTeam', long.v.id);

  // Leaving and deleting.
  await api(B, 'removeTeamMember', tid, (await B.evaluate(async () => (await import('/src/api.js')).session.user.id)));
  await openTeams(B);
  ok(await until(B, (n) => ![...document.querySelectorAll('#teams-list .team-name')].some((x) => x.textContent === n), design), 'B leaves the team: it is gone from the list');
  ok((await api(A, 'deleteTeam', lead.id)).ok, 'A deletes the private team');
  await openTeams(C);
  ok(await until(C, (n) => ![...document.querySelectorAll('#teams-list .team-name')].some((x) => x.textContent === n), `Leadership ${tag}`), 'and it is gone for C');
  await api(A, 'deleteTeam', tid);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
