// Together mode among three real browsers, read on the pixels of the scene:
//   - only the organizer chooses it for everyone; an attendee is told so;
//   - everyone sees the scene, in the scene chosen;
//   - A appears in it cut out of A's own room: A's face is there, A's wall
//     is not (the outline did the cutting);
//   - a person sitting still sends almost no outlines;
//   - C, arriving late, is sent everyone's outline;
//   - turning it off takes it away for everyone.
//
// The cameras are a drawn head and shoulders on a plain wall (#c9d6df), the
// figure the segmenter recognises as a person (effects.e2e.mjs).
//
//   node tools/e2e/together.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const CAMERA = `(() => {
  const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (c) => {
    if (!c || !c.video) return real(c);
    const cv = document.createElement('canvas');
    cv.width = 640; cv.height = 360;
    const g = cv.getContext('2d');
    let t = 0;
    const draw = () => {
      t++;
      g.fillStyle = '#c9d6df'; g.fillRect(0, 0, 640, 360);
      for (let x = 432; x < 640; x += 16) { g.fillStyle = (x / 16) % 2 ? '#c9d6df' : '#6f7c88'; /* a still room */ g.fillRect(x, 0, 16, 360); }
      g.fillStyle = '#2a2f3a'; g.beginPath(); g.ellipse(160, 175, 120, 70, 0, Math.PI, 0); g.fill(); g.fillRect(40, 175, 240, 185);
      g.fillStyle = '#e0ac8a'; g.beginPath(); g.ellipse(160, 80, 45, 58, 0, 0, 2 * Math.PI); g.fill();
      g.fillStyle = '#3b2a20'; g.beginPath(); g.ellipse(160, 45, 48, 28, 0, Math.PI, 0); g.fill();
    };
    draw();
    setInterval(draw, 33);
    const s = cv.captureStream(30);
    if (c.audio) (await real({ audio: c.audio })).getAudioTracks().forEach((tr) => s.addTrack(tr));
    (window.__streams = window.__streams || []).push(s);
    return s;
  };
})();`;

const acc = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
const room = 'tog-' + Math.random().toString(36).slice(2, 8);
const shown = (p) => p.evaluate(() => !document.getElementById('together').hidden);
// Pixels of the scene canvas: how many look like skin, and like A's wall.
const census = (p) => p.evaluate(() => {
  const c = document.getElementById('together');
  const d = c.getContext('2d').getImageData(0, 0, c.width, c.height).data;
  let skin = 0, wall = 0;
  for (let i = 0; i < d.length; i += 16) {
    const r = d[i], g = d[i + 1], b = d[i + 2];
    if (Math.abs(r - 224) < 22 && Math.abs(g - 172) < 22 && Math.abs(b - 138) < 22) skin++;
    if (Math.abs(r - 201) < 12 && Math.abs(g - 214) < 12 && Math.abs(b - 223) < 12) wall++;
  }
  return { skin, wall };
});
const outlines = (p) => p.evaluate(() => Number(document.documentElement.dataset.togOutlines || 0));
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  for (const p of [A, B, C]) await p.evaluateOnNewDocument(CAMERA);
  await enter(A, room);
  await sleep(800);
  await enter(B, room);
  await sleep(3000);

  await B.evaluate(() => document.getElementById('view-together').click());
  ok(await until(B, () => document.body.textContent.includes('Only the organizer can choose Together mode for everyone.'), null), 'an attendee is told only the organizer can choose it');
  ok(!(await shown(A)) && !(await shown(B)), 'and nothing changes');

  await A.evaluate(() => document.getElementById('view-together').click());
  await until(A, () => !!document.querySelector('#dialog select[name="scene"]'), null);
  await A.evaluate(() => { document.querySelector('#dialog select[name="scene"]').value = '1'; document.querySelector('#dialog [data-yes]').click(); });
  ok(await until(A, () => !document.getElementById('together').hidden, null) && await until(B, () => !document.getElementById('together').hidden, null), 'the organizer starts it: both see the scene');
  ok(await until(B, () => document.body.textContent.includes('selected together mode for everyone'), null), 'B is told who selected it');

  // A cut out: A's face in B's scene, A's wall not.
  ok(await until(B, () => Number(document.documentElement.dataset.togOutlines || 0) > 0, null, 15000), "B receives A's outline");
  await sleep(1500);
  const seen = await census(B);
  ok(seen.skin > 30, `A's face is in B's scene: ${JSON.stringify(seen)}`);
  ok(seen.wall < seen.skin, `and A's own wall is cut away: ${JSON.stringify(seen)}`);

  // Still people: almost nothing on the wire.
  const n0 = await outlines(B);
  await sleep(5000);
  const n1 = await outlines(B);
  ok(n1 - n0 <= 3, `a person sitting still sends almost no outlines: ${n1 - n0} in 5 s (10 a second allowed)`);

  // A late arrival.
  await enter(C, room);
  ok(await until(C, () => !document.getElementById('together').hidden, null, 15000), 'C, arriving late, sees the scene');
  ok(await until(C, () => Number(document.documentElement.dataset.togOutlines || 0) >= 2, null, 15000), `and is sent A's and B's outlines (${await outlines(C)})`);

  // Off.
  await A.evaluate(() => document.getElementById('view-together').click());
  await until(A, () => !!document.querySelector('#dialog input[name="stop"]'), null);
  await A.evaluate(() => { document.querySelector('#dialog input[name="stop"]').checked = true; document.querySelector('#dialog [data-yes]').click(); });
  for (const [p, l] of [[A, 'A'], [B, 'B'], [C, 'C']]) ok(await until(p, () => document.getElementById('together').hidden, null), `turned off for ${l}`);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
