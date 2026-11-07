// A meeting joined over a slow link still negotiates WebRTC.
//
// The client queued every frame for 5 ms and then sent the batch -- or threw
// it away if the meeting socket was not open yet. The publisher's offer is
// written moments after joining, so wherever the socket's handshake was slower
// than the camera (a server in a container, a loaded machine, a far-away
// user) the offer was discarded: the server refused the candidates that
// followed ("remote description is not set"), and after 6 s every call fell
// back to sending media over the socket. Found by running the browser
// harnesses against the Docker image.
//
// Here the handshake is slowed on purpose (DevTools network emulation adds
// latency to HTTP and the WebSocket upgrade, not to WebRTC's UDP), and the
// join must still end in an answered offer, connected ICE and video arriving
// over WebRTC, with no fallback.
//
//   node tools/e2e/joinrace.e2e.mjs https://localhost:9443
import { accounts, launch, signedIn, sleep, ok, fail, failed, BASE } from './meet.lib.mjs';

const acct = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
// The meeting socket opens 2.5 s late while everything else runs at full
// speed: the media plane is up and offering before the socket can carry it,
// which is what a container's port proxy did. Test-only, in the page.
const SLOW_SOCKET = () => {
  const Orig = window.WebSocket;
  function Delayed(url, protos) {
    if (!String(url).includes('/rtc')) return new Orig(url, protos);
    const fake = { readyState: 0, binaryType: 'blob', onopen: null, onmessage: null, onclose: null, onerror: null };
    let real = null;
    Object.defineProperty(fake, 'bufferedAmount', { get: () => (real ? real.bufferedAmount : 0) });
    fake.send = (d) => {
      if (fake.readyState !== 1) throw new DOMException('Still in CONNECTING state.', 'InvalidStateError');
      real.send(d);
    };
    fake.close = (...a) => { if (real) real.close(...a); fake.readyState = 3; };
    setTimeout(() => {
      real = new Orig(url, protos);
      real.binaryType = fake.binaryType;
      real.onopen = (e) => { fake.readyState = 1; fake.onopen?.(e); };
      real.onmessage = (e) => fake.onmessage?.(e);
      real.onclose = (e) => { fake.readyState = 3; fake.onclose?.(e); };
      real.onerror = (e) => fake.onerror?.(e);
    }, 2500);
    return fake;
  }
  Object.assign(Delayed, { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 });
  window.WebSocket = Delayed;
};

async function attempt(name, prepare, { stagger = 0 } = {}) {
  const pages = [];
  for (const k of ['A', 'B']) {
    const p = await signedIn(browser, acct[k], k, log, { width: 1280, height: 800 });
    await prepare(p, k);
    await p.evaluateOnNewDocument(() => {
      const O = window.RTCPeerConnection; window.__pcs = [];
      window.RTCPeerConnection = function (...a) { const pc = new O(...a); window.__pcs.push(pc); return pc; };
      window.RTCPeerConnection.prototype = O.prototype;
    });
    const fellBack = [];
    p.on('console', (m) => { if (/WebSocket media on/.test(m.text())) fellBack.push(m.text()); });
    p.__fellBack = fellBack;
    pages.push(p);
  }
  const room = 'joinrace-' + name.replace(/\W/g, '') + Math.random().toString(36).slice(2, 7);
  for (const [i, p] of pages.entries()) {
    if (i > 0 && stagger) await sleep(stagger); // the second person arrives later
    await p.goto(`${BASE}/?room=${room}&media=webrtc`, { waitUntil: 'networkidle2', timeout: 60000 });
    await sleep(1500);
    await p.evaluate(() => document.getElementById('pj-join')?.click());
  }
  const stats = (p) => p.evaluate(async () => {
    const out = [];
    for (const pc of window.__pcs) {
      let inb = 0, local = 0;
      (await pc.getStats()).forEach((s) => {
        if (s.type === 'inbound-rtp') inb += s.bytesReceived || 0;
        if (s.type === 'local-candidate') local++;
      });
      out.push({ sig: pc.signalingState, ice: pc.iceConnectionState, gather: pc.iceGatheringState, local, remote: !!pc.remoteDescription, inb });
    }
    return out;
  });
  // Wait for video to arrive rather than a fixed time. Where the emulated
  // latency also reaches the media path (it does through Docker's port proxy:
  // a measured 1.8 s round trip), each leg is ICE plus a two-round-trip DTLS
  // handshake, ~5.5 s, and a newcomer's subscriber starts only once the other
  // publisher is up. A fallback still fails: it is caught on the console, and
  // media over the socket never reaches these counters.
  for (const t0 = Date.now(); Date.now() - t0 < 45000; await sleep(1000)) {
    const all = await Promise.all(pages.map(stats));
    if (all.every((r) => r[1]?.inb > 10000)) break;
  }
  const checks = [];
  let wedged = false;
  for (const [i, p] of pages.entries()) {
    const who = `${name}: ${'AB'[i]}`;
    const r = await stats(p);
    const pub = r[0], sub = r[1];
    if (sub && sub.ice === 'new' && sub.gather === 'gathering' && sub.local === 0 && sub.remote) wedged = true;
    checks.push([pub && pub.remote && pub.sig === 'stable', `${who}: the publisher's offer was answered (${pub?.sig}, remote ${pub?.remote})`]);
    checks.push([pub && (pub.ice === 'connected' || pub.ice === 'completed'), `${who}: publisher ICE connected (${pub?.ice})`]);
    checks.push([sub && sub.inb > 10000, `${who}: media arrives over WebRTC (${sub?.inb ?? 0} bytes; subscriber ${sub?.sig}/${sub?.ice}, remote ${sub?.remote})`]);
    checks.push([p.__fellBack.length === 0, `${who}: no fallback to the socket (${p.__fellBack[0] ?? 'none'})`]);
  }
  for (const p of pages) await p.browserContext().close();
  return { checks, wedged };
}

// Under DevTools network emulation Chrome occasionally wedges a newcomer's
// subscriber connection whose offer lands the instant it is created:
// gathering "in progress" forever, not one local candidate, and not revived
// by an ICE restart (measured: about one slow-link run in twenty; never in
// the ~150 runs without emulation on the newcomer; not reproduced in 640
// isolated trials). That signature, and only it, earns one retry, said
// aloud; anything else fails as it is. ARCHITECTURE 6by.
async function scenario(name, prepare, opts) {
  let r = await attempt(name, prepare, opts);
  if (r.wedged && r.checks.some(([c]) => !c)) {
    console.log(`note  ${name}: a subscriber wedged in Chrome (gathering, no local candidate); retrying once`);
    r = await attempt(name, prepare, opts);
  }
  for (const [c, msg] of r.checks) ok(c, msg);
}

try {
  // A long round trip for every request and the socket upgrade.
  await scenario('slow link', async (p) => {
    const cdp = await p.target().createCDPSession();
    await cdp.send('Network.enable');
    await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 900, downloadThroughput: -1, uploadThroughput: -1 });
  });
  // Only the meeting socket slow.
  await scenario('slow socket', async (p) => { await p.evaluateOnNewDocument(SLOW_SOCKET); });
  // Somebody joins 8 s after A, whose link is slow: A's first subscriber
  // offer arrives long after A started, and must be given its own window.
  // A's answers are held 1.8 s, as their trip over a slow link would be: on
  // loopback the offer-to-connected gap is milliseconds and a watchdog timed
  // from A's own start rarely looks inside it (it did in the container).
  await scenario('late joiner', async (p, k) => {
    if (k !== 'A') return;
    const cdp = await p.target().createCDPSession();
    await cdp.send('Network.enable');
    await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 900, downloadThroughput: -1, uploadThroughput: -1 });
    await p.evaluateOnNewDocument(() => {
      const set = RTCPeerConnection.prototype.setLocalDescription;
      RTCPeerConnection.prototype.setLocalDescription = async function (d) {
        if (d?.type === 'answer') await new Promise((r) => setTimeout(r, 1800));
        return set.call(this, d);
      };
    });
  }, { stagger: 8000 });
  ok(log.filter((l) => l.includes('PAGE ERROR')).length === 0, `no page errors (${log.filter((l) => l.includes('PAGE ERROR')).join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
