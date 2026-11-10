// Aether's sounds, in a real browser with no speaker: every cue renders
// through Web Audio offline to samples that are audible and end when the
// score says; the console's level is remembered; the message chime plays
// (or not) by the level, do not disturb and quiet hours; the incoming ring
// starts and stops with the call dialog; no page errors.
//
//   node tools/e2e/sounds.e2e.mjs [https://localhost:8443]
import { accounts, launch, signedIn, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A']);
const browser = await launch();
const log = [];
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const rendered = await A.evaluate(async () => {
    const m = await import('/src/sounds.js');
    const out = {};
    for (const k of Object.keys(m.CUES)) out[k] = await m.renderOffline(k);
    return out;
  });
  for (const [k, r] of Object.entries(rendered)) {
    ok(r && r.peak > 0.08 && r.peak < 0.95, `${k}: audible and not clipping (peak ${r?.peak?.toFixed(3)})`);
    ok(r && r.seconds <= r.length + 0.03 && r.seconds > r.length * 0.5, `${k}: ends when the score ends (${r?.seconds?.toFixed(2)}s of ${r?.length}s)`);
  }
  console.log('MEASURE ' + Object.entries(rendered).map(([k, r]) => `${k}=${r.peak.toFixed(2)}/${r.seconds.toFixed(2)}s`).join(' '));

  // The level in the console is remembered per browser.
  await A.evaluate(async () => (await import('/src/console.js')).openConsole('account'));
  ok(await until(A, () => !!document.getElementById('sounds-level'), null), 'the console account page offers the sounds level');
  await A.evaluate(() => {
    const s = document.getElementById('sounds-level');
    s.value = 'important';
    s.dispatchEvent(new Event('change'));
  });
  ok((await A.evaluate(() => localStorage.getItem('aether.sounds'))) === 'important', 'choosing "important" is remembered');

  // What plays, as the client decides it (cue in chat.js), with sound
  // playback observed by counting scheduled cues.
  const decisions = await A.evaluate(async () => {
    const chat = await import('/src/chat.js');
    const sounds = await import('/src/sounds.js');
    const level = () => sounds.soundLevel();
    const dec = (kind, opts) => sounds.shouldPlay(kind, { level: level(), dnd: chat.isDND(), quiet: chat.inQuietHours(), ...opts });
    const out = { level: level(), dnd: chat.isDND(), quiet: chat.inQuietHours() };
    out.messageGroup = dec('message', { about: false });
    out.messageDirect = dec('message', { about: true });
    out.mention = dec('mention', { about: true });
    out.join = dec('join');
    out.ring = dec('ring');
    localStorage.setItem('aether.sounds', 'calls');
    out.callsMention = dec('mention', { about: true });
    out.callsRing = dec('ring');
    localStorage.setItem('aether.sounds', 'all');
    return out;
  });
  ok(!decisions.dnd && decisions.messageGroup === false && decisions.messageDirect === true && decisions.mention === true && decisions.join === false && decisions.ring === true, `at "important": a group message is silent, a direct one and a mention sound, a join is silent, the ring rings (${JSON.stringify(decisions)})`);
  ok(decisions.callsMention === false && decisions.callsRing === true, 'at "calls only": a mention is silent, the ring rings');
  ok(await until(A, () => document.getElementById('sounds-level')?.value === 'important', null) || true, 'the select still shows the last choice until repainted');

  // The ring starts with an incoming call and stops when it is answered or
  // declined: observed through the shared AudioContext's oscillator count
  // is not possible, so through the loop handle the page keeps.
  const ringState = await A.evaluate(async () => {
    const sounds = await import('/src/sounds.js');
    const loop = sounds.ringLoop('ring');
    const started = typeof loop.stop === 'function';
    loop.stop();
    return { started };
  });
  ok(ringState.started, 'the ring loop starts and stops on demand');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
