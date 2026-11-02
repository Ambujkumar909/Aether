// What a video effect costs on the wire (CLAUDE.md rule 1): A's camera over
// the socket media path for 12 s each with no effect, Standard blur, Portrait
// blur and a plain background, against a PLAINTEXT server so tshark reads it:
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -db <scratch>.db -web ../web
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w fx.pcapng -a duration:90 &
//   node tools/e2e/measure-effects.mjs http://localhost:8444
//
// The script prints when each window starts. The fake camera shows a moving
// test pattern and no person, so the segmenter finds only background: this
// measures what blurring or replacing a frame does to the encoder, not a
// person against a room.
import { accounts, launch, signedIn, enter, sleep, BASE } from './meet.lib.mjs';

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;
const room = 'fxm-' + Math.random().toString(36).slice(2, 8);
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  await A.evaluate(() => localStorage.removeItem('aether.effects'));
  await enter(A, room);
  await enter(B, room);
  await A.evaluate(() => { document.getElementById('btn-cam-more').click(); document.getElementById('cam-effects').click(); });
  await sleep(6000);
  const pick = (label) => A.evaluate((label) => [...document.querySelectorAll('#view-effects .fx-tile')].find((x) => x.title === label)?.click(), label);
  for (const [label, name] of [['None', 'none'], ['Standard blur', 'blur'], ['Portrait blur', 'portrait'], ['Deep teal', 'teal'], ['None', 'none-again']]) {
    await pick(label);
    await sleep(3000); // the encoder settles on the new picture
    console.log(`${name.padEnd(11)} ${at()}  (12 s window)`);
    await sleep(12000);
  }
  console.log(`end         ${at()}  fx ${await A.evaluate(() => document.documentElement.dataset.fx)}`);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
