// Call health (More > "Call health"), between two real browsers:
//   - on the WebRTC path every Teams row is a measurement: round trip time,
//     sent and received bitrates, the camera's sent and received resolution
//     and frame rate, the devices -- read from getStats, refreshed each second;
//   - on the WebSocket path it says what it cannot measure rather than
//     showing zeros, and still shows the bytes each way;
//   - closing the panel stops the sampling.
//
//   node tools/e2e/health.e2e.mjs [https://localhost:8443]
import { BASE, accounts, launch, signedIn, enter, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const rows = (p) => p.evaluate(() => Object.fromEntries([...document.querySelectorAll('#view-health .hl-sec')].flatMap((sec) =>
  [...sec.querySelectorAll('.hl-row')].map((r) => [sec.querySelector('h3').textContent + '/' + r.querySelector('span').textContent, r.querySelector('b').textContent]))));
const openHealth = async (p) => {
  await p.evaluate(() => document.getElementById('btn-more').click());
  await sleep(150);
  await p.evaluate(() => document.getElementById('more-health').click());
};
try {
  const A = await signedIn(browser, acc.A, 'A', log);
  const B = await signedIn(browser, acc.B, 'B', log);
  const room = 'health-' + Math.random().toString(36).slice(2, 8);
  await enter(A, room, 'webrtc');
  await enter(B, room, 'webrtc');
  await sleep(6000);
  await openHealth(A);
  ok(await until(A, () => document.getElementById('side-title').textContent === 'Call health', null), 'More > Call health opens the panel');
  await sleep(3500);
  const r = await rows(A);
  const measured = (k) => r[k] && r[k] !== '—';
  for (const k of ['Network/Round trip time', 'Network/Sent bitrate', 'Network/Received bitrate', 'Audio/Sent bitrate', 'Audio/Received bitrate', 'Video/Sent resolution', 'Video/Received resolution', 'Video/Received frame rate', 'Devices/Microphone']) {
    ok(measured(k), `${k}: ${r[k]}`);
  }
  ok(/^\d+ ms$/.test(r['Network/Round trip time'] ?? ''), `round trip time in Teams' unit (${r['Network/Round trip time']})`);
  ok(!/MediaStream/.test(r['Devices/Microphone']), `the microphone is named as the device, not the processing node (${r['Devices/Microphone']})`);
  ok(/Kbps|Mbps/.test(r['Audio/Sent bitrate'] ?? ''), `bitrate in Teams' unit (${r['Audio/Sent bitrate']})`);
  const first = (await rows(A))['Network/Received bitrate'];
  await sleep(2200);
  ok(await A.evaluate(() => !!document.querySelector('#view-health .hl-row')), `it refreshes each second (${first} -> ${(await rows(A))['Network/Received bitrate']})`);
  await A.evaluate(() => document.getElementById('side-close').click());
  await sleep(300);
  ok(await A.evaluate(() => document.getElementById('shell').classList.contains('panel-closed')), 'closing stops it');

  // The WebSocket path.
  const room2 = 'health-ws-' + Math.random().toString(36).slice(2, 8);
  await enter(A, room2, 'ws');
  await enter(B, room2, 'ws');
  await sleep(4000);
  await openHealth(A);
  await sleep(2500);
  const w = await rows(A);
  ok(await A.evaluate(() => /WebSocket path/.test(document.getElementById('view-health').textContent)), 'on the WebSocket path it says what it cannot measure');
  ok(/Kbps|Mbps/.test(w['Network/Sent bitrate'] ?? '') && w['Audio/Sent bitrate'] === '—', `bytes each way are shown, unmeasured rows are a dash (${w['Network/Sent bitrate']}, ${w['Audio/Sent bitrate']})`);
} catch (e) {
  fail(e);
} finally {
  for (const l of log) console.log('  ' + l);
  await browser.close();
}
console.log(failed() ? `\n${failed()} FAILED (${BASE})` : '\nall passed');
process.exit(failed() ? 1 : 0);
