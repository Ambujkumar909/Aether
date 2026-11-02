// Recording, transcription and live captions among real browsers:
//   - only organizers and presenters are offered Record and transcribe;
//   - Start recording: everyone sees "This meeting is being recorded." and
//     the red dot; Stop: the file is made on the recorder's own computer;
//   - Start transcription: each speaker is asked before their speech becomes
//     text; one allows, one does not; the allowed speaker's lines reach the
//     transcript panel, attributed, and are kept by the server;
//   - live captions show another person's words; muting stops recognition;
//   - the transcript endpoint: a late arrival reads it, a stranger and
//     nobody do not;
//   - the recorder leaving stops the recording for everyone.
//
// Headless Chrome has no speech recogniser, so the browser API is replaced
// by a fake the test speaks into: everything from the recogniser's result
// onward -- the client, the socket, the server, the database -- is real.
//
// The deployment's "Recording allowed" is off by default, so this needs an
// administrator to turn it on: run against a fresh server started with
// -admin-email/-admin-password and pass the same in the environment.
//
//   AETHER_ADMIN_EMAIL=.. AETHER_ADMIN_PASSWORD=.. node tools/e2e/recording.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const FAKES = `(() => {
  window.__speech = [];
  class FakeSR {
    constructor() { this.running = false; window.__speech.push(this); }
    start() { this.running = true; }
    stop() { this.running = false; setTimeout(() => this.onend?.(), 0); }
    abort() { this.stop(); }
  }
  FakeSR.available = async () => 'unavailable';
  window.SpeechRecognition = FakeSR;
  window.webkitSpeechRecognition = FakeSR;
  window.__listening = () => window.__speech.some((r) => r.running);
  window.__say = (text, isFinal) => {
    const r = [...window.__speech].reverse().find((x) => x.running);
    if (!r) return false;
    const res = [{ transcript: text }];
    res.isFinal = isFinal;
    r.onresult?.({ results: [res] });
    return true;
  };
  // No file picker here: the recording is held in memory and handed over
  // as a download, which is recorded rather than performed.
  window.showSaveFilePicker = undefined;
  window.__downloads = [];
  // The page's CSP does not let it fetch a blob: URL, so sizes are noted
  // where the URL is made.
  const sizes = new Map();
  const make = URL.createObjectURL;
  URL.createObjectURL = (b) => { const u = make(b); sizes.set(u, b.size); return u; };
  const click = HTMLAnchorElement.prototype.click;
  HTMLAnchorElement.prototype.click = function () {
    if (this.download && this.href.startsWith('blob:')) {
      window.__downloads.push({ name: this.download, href: this.href, size: sizes.get(this.href) ?? -1 });
      return;
    }
    return click.call(this);
  };
})();`;

const acc = await accounts(['A', 'B', 'C', 'D']);
const browser = await launch();
const log = [];
const room = 'rec-' + Math.random().toString(36).slice(2, 8);
const banner = (p) => p.evaluate(() => document.getElementById('rec-banner').textContent.replace(/\s+/g, ' ').trim());
const more = async (p, id) => {
  await p.evaluate(() => document.getElementById('btn-more').click());
  await sleep(150);
  return p.evaluate((id) => { const b = document.getElementById(id); return b && !b.hidden ? (b.click(), true) : false; }, id);
};
const consent = (p, yes) => p.evaluate((yes) => document.querySelector(`#rec-banner [data-speech="${yes ? 'yes' : 'no'}"]`)?.click(), yes);
const lines = (p) => p.evaluate(() => [...document.querySelectorAll('#view-transcript .tr-line')].map((l) => l.textContent.replace(/\s+/g, ' ').trim()));
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const C = await signedIn(browser, acc.C, 'C', log);
  const D = await signedIn(browser, acc.D, 'D', log);
  for (const p of [A, B, C, D]) await p.evaluateOnNewDocument(FAKES);
  // Off, as a deployment starts (and as an earlier run may not have left it).
  const admin = await signedIn(browser, { email: process.env.AETHER_ADMIN_EMAIL, password: process.env.AETHER_ADMIN_PASSWORD }, 'admin', log);
  const setRecording = (v) => admin.evaluate(async (v) => { try { await (await import('/src/api.js')).api.put('/api/settings/meeting.recording.enabled', { value: v }); return 'ok'; } catch (e) { return String(e?.message ?? e); } }, v);
  ok((await setRecording('false')) === 'ok', 'recording is off for the deployment');
  await enter(A, room);
  await sleep(800);
  await enter(B, room);
  await enter(C, room);
  await sleep(1500);

  // Who is offered it.
  ok(await B.evaluate(() => document.getElementById('more-rec').hidden && document.getElementById('more-tr').hidden), 'an attendee is not offered Start recording or Start transcription');
  ok(await A.evaluate(() => !document.getElementById('more-rec').hidden), 'the organizer is');

  // Off for this deployment: refused, and said so in Teams' words.
  ok(await more(A, 'more-rec'), 'A: More > Start recording, with recording off for the deployment');
  ok(await until(A, () => document.body.textContent.includes('Recording not started. Your IT admin has disabled Recording.'), null), 'A is told the IT admin has disabled Recording');
  ok(await A.evaluate(() => document.getElementById('more-rec').textContent.includes('Your IT admin has disabled Recording')), 'and the menu item says why');
  ok(await B.evaluate(() => document.getElementById('rec-live').hidden), 'nobody else hears of it');
  const set = await setRecording('true');
  ok(set === 'ok', `an administrator turns "Recording allowed" on: ${set}`);

  // Recording.
  ok(await more(A, 'more-rec'), 'A: More > Start recording');
  for (const [p, l] of [[A, 'A'], [B, 'B'], [C, 'C']]) {
    ok(await until(p, () => !document.getElementById('rec-live').hidden, null), `${l} sees the red dot`);
  }
  ok((await banner(B)).includes('This meeting is being recorded.'), `B is told: "${await banner(B)}"`);
  ok(await A.evaluate(() => document.getElementById('more-rec').textContent === 'Stop recording'), 'A is offered Stop recording');

  // Transcription, and each speaker's own choice.
  ok(await more(A, 'more-tr'), 'A: More > Start transcription');
  ok(await until(B, () => !!document.querySelector('#rec-banner [data-speech]'), null), 'B is asked whether their speech may become text');
  ok((await banner(B)).includes('recorded and transcribed'), 'and told the meeting is recorded and transcribed');
  ok(/online speech service/.test(await banner(B)), 'and told the recognition is not on this device');
  ok(!(await B.evaluate(() => window.__listening())), 'nothing is recognised before B answers');
  await consent(B, true);
  await consent(C, false);
  ok(await until(B, () => window.__listening(), null), 'B allows: B is recognised');
  await sleep(300);
  ok(!(await C.evaluate(() => window.__listening())), 'C does not: C is not');

  await A.evaluate(() => document.getElementById('more-transcript').click());
  await C.evaluate(() => { document.getElementById('btn-more').click(); document.getElementById('more-transcript').click(); });
  await sleep(800);
  ok(await B.evaluate(() => window.__say('Budget is', false)), 'B speaks (a partial)');
  ok(await B.evaluate(() => window.__say('Budget is approved', true)), 'and finishes the sentence');
  ok(await until(A, () => [...document.querySelectorAll('#view-transcript .tr-line')].some((l) => l.textContent.includes('Budget is approved')), null), "the line reaches A's transcript panel");
  const aLines = await lines(A);
  ok(aLines.length === 1 && aLines[0].includes('E2E B'), `once, attributed to B: ${JSON.stringify(aLines)}`);
  ok(await until(C, () => [...document.querySelectorAll('#view-transcript .tr-line')].some((l) => l.textContent.includes('Budget is approved')), null), 'and C\'s, though C chose not to be included');
  await C.evaluate(() => window.__say('C should not be heard', true));

  // Captions: another person's words, as they speak.
  await A.evaluate(() => document.getElementById('btn-cc').click());
  await sleep(500);
  await B.evaluate(() => window.__say('Next item is hiring', false));
  ok(await until(A, () => document.getElementById('caption').textContent.includes('Next item is hiring'), null), "A, with captions on, sees B's words as B speaks");

  // Muted is not speaking.
  await B.evaluate(() => document.getElementById('btn-mic').click());
  ok(await until(B, () => !window.__listening(), null), "muting stops B's recognition");
  await B.evaluate(() => document.getElementById('btn-mic').click());
  ok(await until(B, () => window.__listening(), null), 'unmuting starts it again');

  // Kept by the server; who may read it.
  const api = (p, path) => p.evaluate(async (path) => { const r = await fetch(path, { credentials: 'same-origin' }); return { status: r.status, body: r.status === 200 ? await r.text() : '' }; }, path);
  const path = `/api/scheduling/rooms/${room}/transcript`;
  const mine = await api(A, path);
  ok(mine.status === 200 && JSON.parse(mine.body).lines.map((l) => l.text).join('|') === 'Budget is approved', `the server kept exactly the allowed line: ${mine.body.slice(0, 160)}`);
  const vtt = await api(A, path + '?format=vtt');
  ok(vtt.status === 200 && /^WEBVTT\n\n1\n\d\d:\d\d:\d\d\.\d{3} --> /.test(vtt.body) && vtt.body.includes('<v E2E B>Budget is approved</v>'), 'Download .vtt');
  ok((await api(D, path)).status === 403, 'a stranger cannot read it');
  const anon = await fetch(BASE + path);
  ok(anon.status === 401, `nobody signed in cannot: ${anon.status}`);

  // A late arrival is told, asked, and given the transcript so far.
  await enter(D, room);
  ok(await until(D, () => !document.getElementById('rec-live').hidden && !!document.querySelector('#rec-banner [data-speech]'), null, 12000), 'D, arriving late, sees the recording and is asked');
  await D.evaluate(() => { document.getElementById('btn-more').click(); document.getElementById('more-transcript').click(); });
  ok(await until(D, () => [...document.querySelectorAll('#view-transcript .tr-line')].some((l) => l.textContent.includes('Budget is approved')), null), 'and reads the transcript so far');

  // Stop: the file is made on A's computer.
  ok(await more(A, 'more-rec'), 'A: Stop recording');
  ok(await until(A, () => window.__downloads.length === 1 && window.__downloads[0].size > 0, null, 15000), 'the recording is handed over as a file');
  const dl = await A.evaluate(() => window.__downloads[0]);
  ok(/^Meeting recording \d{4}-\d\d-\d\d \d{4}\.(webm|mp4)$/.test(dl.name) && dl.size > 5000, `named and non-empty: ${dl.name}, ${dl.size} B`);
  ok(await until(B, () => document.getElementById('rec-live').hidden, null), 'B sees the recording stop');

  // The recorder leaving stops it for everyone.
  ok(await more(A, 'more-rec'), 'A starts recording again');
  ok(await until(C, () => !document.getElementById('rec-live').hidden, null), 'C sees it');
  await A.evaluate(() => document.getElementById('btn-leave').click());
  ok(await until(C, () => document.getElementById('rec-live').hidden, null, 12000), 'A leaves: the recording stops for C');
  ok(await until(C, () => /transcribed/.test(document.getElementById('rec-banner').textContent), null), 'and transcription carries on');
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
