// Voice selection on the socket media path, in real browsers: with more
// microphones open than the room relays, a listener is sent six voices, not
// all of them, and somebody who starts to speak is among them.
//
// Two browsers, because Chrome's fake microphone is one per browser:
//   - "quiet": its microphone is a file of silence. Seven tabs join one room
//     with the microphone open and say nothing; an eighth tab, muted, listens.
//   - "loud": its microphone is Chrome's test tone, a beep a second. One tab
//     joins and so "speaks".
// The listener counts the audio frames it is sent per publisher and reads the
// level each frame carries, which is the sender's own measure of real
// AudioData (wsmedia.js frameLevel): silence must read as silence and the
// beep as speech, or the server has nothing to choose by.
//
//   node tools/e2e/voices.e2e.mjs [https://localhost:8443]
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';
import { accounts, launch, until, ok, fail, failed, sleep, BASE } from './meet.lib.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const SHOTS = join(here, '.shots');
mkdirSync(SHOTS, { recursive: true });
const MAX = 6; // room.MaxVoices
const OPEN = MAX + 1;

// Two seconds of silence, 48 kHz mono 16-bit: the quiet browser's microphone.
const silence = join(SHOTS, 'silence.wav');
{
  const n = 48000 * 2 * 2;
  const h = Buffer.alloc(44);
  h.write('RIFF', 0); h.writeUInt32LE(36 + n, 4); h.write('WAVEfmt ', 8); h.writeUInt32LE(16, 16); h.writeUInt16LE(1, 20); h.writeUInt16LE(1, 22);
  h.writeUInt32LE(48000, 24); h.writeUInt32LE(96000, 28); h.writeUInt16LE(2, 32); h.writeUInt16LE(16, 34); h.write('data', 36); h.writeUInt32LE(n, 40);
  writeFileSync(silence, Buffer.concat([h, Buffer.alloc(n)]));
}

const acc = await accounts(['A', 'B']);
const loud = await launch();
const quiet = await puppeteer.launch({
  executablePath: loud.process().spawnfile,
  headless: true,
  args: ['--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--use-file-for-fake-audio-capture=' + silence],
});
const log = [];
const room = 'voices-' + Math.random().toString(36).slice(2, 8);

async function signIn(ctx, a, label) {
  const page = await ctx.newPage();
  page.on('pageerror', (e) => log.push(`${label} PAGE ERROR: ${e}`));
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  await page.evaluate(async (e, p) => {
    const api = await import('/src/api.js');
    if (!api.session.user) await api.login(e, p);
  }, a.email, a.password);
  return page;
}
async function joinAudio(page) {
  await page.goto(`${BASE}/?room=${room}&media=ws&audio=1`, { waitUntil: 'networkidle2', timeout: 30000 });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 20000 });
  await sleep(500);
  await page.evaluate(() => document.getElementById('pj-join')?.click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
}
// Per publisher (one receiver each): frames since the last read, and the
// loudest level among them.
const COUNT = `(async () => {
  const { WSMediaReceiver } = await import('/src/wsmedia.js');
  const seen = (window.__voices = new Map());
  const push = WSMediaReceiver.prototype.push;
  WSMediaReceiver.prototype.push = function (f) {
    if (f.kind === 0) {
      const v = seen.get(this) ?? { n: 0, level: 0, id: seen.size };
      v.n++;
      v.level = Math.max(v.level, f.level ?? 0);
      seen.set(this, v);
    }
    return push.call(this, f);
  };
})()`;
const window1s = async (page, ms = 1000) => {
  await page.evaluate(() => window.__voices.forEach((v) => { v.n = 0; v.level = 0; }));
  await sleep(ms);
  return page.evaluate(() => [...window.__voices.values()].map((v) => ({ id: v.id, n: v.n, level: v.level })));
};

try {
  // The quiet browser: one sign-in, eight tabs of the same person (a person
  // may be in a meeting from several devices; each tab is a participant).
  const qctx = await quiet.createBrowserContext();
  const listener = await signIn(qctx, acc.A, 'listener');
  await listener.evaluateOnNewDocument(COUNT);
  await joinAudio(listener);
  await listener.evaluate(() => document.getElementById('btn-mic').click());
  ok(await until(listener, () => document.getElementById('btn-mic').classList.contains('off'), null), 'the listener is in the room, muted');
  const tabs = [];
  for (let i = 0; i < OPEN; i++) {
    const p = await qctx.newPage();
    p.on('pageerror', (e) => log.push(`quiet${i} PAGE ERROR: ${e}`));
    await joinAudio(p);
    tabs.push(p);
  }
  const open = (await Promise.all(tabs.map((p) => p.evaluate(() => !document.getElementById('btn-mic').classList.contains('off'))))).filter(Boolean).length;
  ok(open === OPEN, `${open} more tabs joined with the microphone open`);
  await sleep(2500);

  // Seven open, silent microphones: six are relayed, in full; one is not.
  const c0 = Date.now();
  const calm = await window1s(listener, 2000);
  console.log(`MARK calm ${c0} ${Date.now()}`); // for a capture taken alongside (ARCHITECTURE 6cd)
  const flowing = calm.filter((v) => v.n > 0);
  ok(flowing.length === MAX, `${OPEN} silent microphones open: the listener is sent ${flowing.length} voices, want ${MAX} (${JSON.stringify(calm)})`);
  ok(flowing.every((v) => v.n >= 60 && v.n <= 125),  // a loaded runner encodes late; a voice not relayed reads 0
     `each of them whole: about 100 frames in two seconds (${flowing.map((v) => v.n).join(', ')})`);
  ok(calm.every((v) => v.level < 4), `silence reads as silence in a real browser: the loudest level seen is ${Math.max(0, ...calm.map((v) => v.level))} of 63, under the floor of 4`);

  // Somebody speaks.
  const lctx = await loud.createBrowserContext();
  const speaker = await signIn(lctx, acc.B, 'speaker');
  await joinAudio(speaker);
  let heard = null;
  const t0 = Date.now();
  while (!heard && Date.now() - t0 < 12000) {
    const w = await window1s(listener, 500);
    heard = w.find((v) => v.level >= 4 && v.n > 0) ?? null;
  }
  ok(!!heard, `the one who speaks is heard: frames marked level ${heard?.level ?? '-'} arrive (${Date.now() - t0} ms after joining)`);
  // And still six at a time, over three seconds of beeps and gaps.
  let most = 0;
  let speakerFrames = 0;
  const s0 = Date.now();
  for (let i = 0; i < 6; i++) {
    const w = await window1s(listener, 500);
    most = Math.max(most, w.filter((v) => v.n > 0).length);
    speakerFrames += w.find((v) => v.id === heard?.id)?.n ?? 0;
  }
  console.log(`MARK speaking ${s0} ${Date.now()}`);
  ok(most <= MAX, `with eight microphones open the listener is never sent more than ${MAX} at once (most: ${most})`);
  ok(speakerFrames >= 100, `the speaker keeps the place through the gaps between beeps: ${speakerFrames} of about 150 frames in three seconds`);
  await listener.screenshot({ path: join(SHOTS, 'voices-listener.png') });

  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await quiet.close();
  await loud.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
