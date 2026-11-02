// The audio path between two real browsers in one meeting, measured, not
// assumed -- on either transport. Each side publishes a microphone
// (Chrome's fake device, a tone) and the check reads what actually flows:
//   - what each side sends and receives (RTP stats on WebRTC; frames and
//     bytes on the meeting socket for the socket path);
//   - that each side plays exactly one sink, the other's, with real energy
//     in it, and that the two sides' sink ids differ -- a participant played
//     their own audio would make both pages name the same id;
//   - no <video> element is unmuted; the published microphone has echo
//     cancellation on; both are warned they share a device;
//   - glass-to-glass latency: A unmutes, and the time until B's sink
//     carries sound is measured on B's own clock, several times, median.
//
//   node tools/e2e/audio.e2e.mjs [https://localhost:8443] [--media webrtc|ws] [--audio-only] [--runs N]
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const BASE = argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const arg = (name, dflt) => {
  const i = argv.indexOf(name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : dflt;
};
const MEDIA = arg('--media', 'ws'); // what the page is asked for; 'ws' is the app default
const AUDIO_ONLY = argv.includes('--audio-only');
const NO_WEBCODECS = argv.includes('--no-webcodecs'); // emulate Safari, iOS, Firefox: no socket media path
const RUNS = Number(arg('--runs', '5'));
const TOGGLES = Number(arg('--toggles', '20')); // rapid mute/unmute presses before the latency is re-measured
// Speech mode: a WAV is every microphone (Chrome plays it as its fake device,
// with the same processing a real one gets), and what B plays of A is recorded
// and compared with the clean reference. --ref is the clean file when --speech
// is a noisy version of it. The tone sections (latency, mute) are skipped.
const SPEECH = arg('--speech', '');
const REF = arg('--ref', SPEECH);
const RECORD_S = Number(arg('--record', '12'));
// --dump <file.wav>: also save what B heard, and list every cut with its time.
const DUMP = arg('--dump', '');
// "8803:8802": the browser's RTP is sent through an impairment proxy on
// 127.0.0.1:8803 that forwards to the media port 8802 (tools/impair). The
// socket path is impaired by pointing BASE at a TCP proxy instead.
const UDP_PROXY = arg('--udp-proxy', '');
// --expect socket: WebRTC was asked for but UDP is made unreachable, so the
// automatic fallback must carry the call.
const EXPECT = arg('--expect', '');
// --no-denoise turns the neural noise suppressor off on both pages (the
// remembered setting), for the before side of a before/after.
const NO_DENOISE = argv.includes('--no-denoise');
// --apm ns0,agc0,aec0: switch off the browser's own capture processing
// stages, to see what each contributes (experiments only).
const APM = arg('--apm', '');
// --b edge: B runs in Microsoft Edge -- the engine the Windows app embeds
// (WebView2) -- while A stays in Chrome: the app-to-web call.
const B_ENGINE = arg('--b', 'chrome');
const A_ENGINE = arg('--a', 'chrome');
const ENGINES = { chrome: CHROME_PATH(), edge: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe' };
function CHROME_PATH() { return 'C:/Program Files/Google/Chrome/Application/chrome.exe'; }
// --no-chain: the app sees no AudioWorklet, so the microphone goes out raw
// (the pre-leveler behaviour) -- the baseline for the voice chain's own cost.
const NO_CHAIN = argv.includes('--no-chain');
const host = new URL(BASE).host;
let accounts = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'))[host] ?? {};
if (!accounts.A || !accounts.B) {
  // A server with no stored accounts (a plaintext one for a tshark capture):
  // make two throwaway ones.
  const mk = async (label) => {
    const email = `e2e-${label.toLowerCase()}-${Math.random().toString(36).slice(2, 10)}@example.test`;
    const password = 'e2e-' + Math.random().toString(36).slice(2, 14);
    const r = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email, displayName: `E2E ${label}`, password }) });
    if (r.status !== 201) throw new Error(`signup ${label}: ${r.status}`);
    return { email, password };
  };
  accounts = { A: await mk('A'), B: await mk('B') };
  console.log(`(made throwaway accounts on ${host})`);
  // Kept for the next run: signups are rate-limited, and a run that makes two
  // new accounts each time hits the limit on the third run.
  try {
    const all = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'));
    all[host] = accounts;
    writeFileSync(join(here, '.accounts.json'), JSON.stringify(all, null, 2));
  } catch {}
}
const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
].find((p) => existsSync(p));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const ok = (cond, what) => {
  console.log(`${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  if (!cond) failures++;
};
const median = (xs) => {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.floor(s.length / 2)] : NaN;
};

// Every RTCPeerConnection and every WebSocket the page makes is kept, so
// what flows can be read from outside without touching the app's internals.
const INSTRUMENT = `
  (() => {
    window.__AWN = window.AudioWorkletNode;
    if (${NO_CHAIN}) { try { delete window.AudioWorkletNode; } catch {} window.AudioWorkletNode = undefined; }
    const APM = '${APM}'.split(',').filter(Boolean);
    if (APM.length) {
      const g0 = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
      navigator.mediaDevices.getUserMedia = (c) => {
        if (c && c.audio) {
          const a = typeof c.audio === 'object' ? { ...c.audio } : {};
          if (APM.includes('ns0')) a.noiseSuppression = false;
          if (APM.includes('agc0')) a.autoGainControl = false;
          if (APM.includes('aec0')) a.echoCancellation = false;
          c = { ...c, audio: a };
        }
        return g0(c);
      };
    }
    try { localStorage.setItem('aether.denoise', ${NO_DENOISE ? "'off'" : "'on'"}); } catch {}
    const Orig = window.RTCPeerConnection;
    window.__pcs = [];
    window.RTCPeerConnection = function (...a) {
      const pc = new Orig(...a);
      window.__pcs.push(pc);
      return pc;
    };
    window.RTCPeerConnection.prototype = Orig.prototype;
    Object.setPrototypeOf(window.RTCPeerConnection, Orig);
    const OW = window.WebSocket;
    window.__ws = [];
    window.WebSocket = function (url, protocols) {
      const ws = protocols === undefined ? new OW(url) : new OW(url, protocols);
      const rec = { url: String(url), sentBytes: 0, sentCount: 0, recvBytes: 0, recvCount: 0 };
      window.__ws.push(rec);
      rec.sentLog = [];
      rec.recvLog = [];
      const send = ws.send.bind(ws);
      ws.send = (d) => {
        const n = d?.byteLength ?? d?.size ?? (typeof d === 'string' ? d.length : 0);
        rec.sentCount++;
        rec.sentBytes += n;
        rec.sentLog.push([performance.now(), n]);
        if (rec.sentLog.length > 600) rec.sentLog.shift();
        return send(d);
      };
      ws.addEventListener('message', (e) => {
        const n = e.data?.byteLength ?? e.data?.size ?? (typeof e.data === 'string' ? e.data.length : 0);
        rec.recvCount++;
        rec.recvBytes += n;
        rec.recvLog.push([performance.now(), n]);
        if (rec.recvLog.length > 600) rec.recvLog.shift();
      });
      return ws;
    };
    window.WebSocket.prototype = OW.prototype;
    Object.setPrototypeOf(window.WebSocket, OW);
    for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED']) window.WebSocket[k] = OW[k];
` + (SPEECH ? '' : `
    // The microphone is a synthesised, continuous 440 Hz tone rather than
    // Chrome's fake device: that device beeps with silence between, which
    // made a continuity check read dropouts that were the source, and a
    // latency check wait for the next beep. The tone's gain is the switch
    // for the latency measurement: it steps in one sample, no re-acquisition.
    window.__streams = [];
    const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = async (c) => {
      const st = await gum(c);
      if (c && c.audio) {
        if (!window.__tone) {
          const ctx = new AudioContext({ sampleRate: 48000 });
          const osc = ctx.createOscillator();
          osc.frequency.value = 440;
          const gain = ctx.createGain();
          gain.gain.value = 0.5;
          const dest = ctx.createMediaStreamDestination();
          dest.channelCount = 1; // a microphone is mono; the destination defaults to stereo
          dest.channelCountMode = 'explicit';
          osc.connect(gain).connect(dest);
          osc.start();
          ctx.resume().catch(() => {});
          window.__tone = { ctx, osc, gain, dest };
        }
        for (const t of st.getAudioTracks()) { t.stop(); st.removeTrack(t); }
        const tone = window.__tone.dest.stream.getAudioTracks()[0];
        // The app reads the device's processing settings; a synthesised
        // track has none, so answer as the real device would.
        const real = tone.getSettings.bind(tone);
        tone.getSettings = () => ({ ...real(), echoCancellation: true, noiseSuppression: true, autoGainControl: true, sampleRate: 48000, channelCount: 1 });
        st.addTrack(tone);
      }
      window.__streams.push(st);
      return st;
    };
`) + (UDP_PROXY ? `
    // Every remote candidate is pointed at the impairment proxy, and every
    // local candidate the page tells the server about is made unreachable
    // (port 9), so the only pair that can form is the one through the proxy:
    // the server learns the page's real address as peer-reflexive from the
    // checks that arrive through it. TCP and relay candidates are dropped.
    const [PPORT, MPORT] = '${UDP_PROXY}'.split(':');
    const PHOST = '${arg('--udp-proxy-host', '127.0.0.1')}';
    const origAdd = Orig.prototype.addIceCandidate;
    Orig.prototype.addIceCandidate = function (c) {
      if (c && c.candidate) {
        const m = c.candidate.match(/^candidate:(\\S+) (\\d+) (udp|tcp) (\\d+) (\\S+) (\\d+) typ (\\w+)/i);
        if (!m) return Promise.resolve();
        if (m[3].toLowerCase() !== 'udp' || m[7] !== 'host' || m[6] !== MPORT) return Promise.resolve();
        const s = c.candidate.replace(' ' + m[5] + ' ' + m[6] + ' typ', ' ' + PHOST + ' ' + PPORT + ' typ');
        c = { candidate: s, sdpMid: c.sdpMid, sdpMLineIndex: c.sdpMLineIndex, usernameFragment: c.usernameFragment };
      }
      return origAdd.call(this, c);
    };
    // The server also puts its candidates in the description itself.
    const origSRD = Orig.prototype.setRemoteDescription;
    Orig.prototype.setRemoteDescription = function (d) {
      if (d && d.sdp) {
        const lines = d.sdp.split('\\r\\n').filter((l) => {
          if (!l.startsWith('a=candidate:')) return true;
          const m = l.slice(2).match(/^candidate:(\\S+) (\\d+) (udp|tcp) (\\d+) (\\S+) (\\d+) typ (\\w+)/i);
          return m && m[3].toLowerCase() === 'udp' && m[7] === 'host' && m[6] === MPORT;
        }).map((l) => (l.startsWith('a=candidate:') ? l.replace(/ (\\S+) (\\d+) typ /, ' ' + PHOST + ' ' + PPORT + ' typ ') : l));
        d = { type: d.type, sdp: lines.join('\\r\\n') };
      }
      return origSRD.call(this, d);
    };
    const desc = Object.getOwnPropertyDescriptor(Orig.prototype, 'onicecandidate');
    Object.defineProperty(Orig.prototype, 'onicecandidate', {
      configurable: true,
      get() { return desc.get.call(this); },
      set(fn) {
        desc.set.call(this, (e) => {
          if (!e.candidate) return fn.call(this, e);
          const j = e.candidate.toJSON();
          j.candidate = j.candidate.replace(/ (\\d+) typ /, ' 9 typ ');
          fn.call(this, { candidate: new RTCIceCandidate(j) });
        });
      },
    });
` : '') + `
    // Record the given seconds of the sink into memory and reduce it: a 10 ms RMS
    // envelope, and the average spectrum of the louder half of the frames in
    // three bands (0-4, 4-8, 8-20 kHz), so a narrowed voice shows as a number.
    // The recorder runs on the audio thread (an AudioWorklet), not the page's
    // main thread: a ScriptProcessor is serviced by the main thread, and
    // under CPU load it drops input -- which read as the call cutting out
    // when it was the instrument. Every 128-sample block carries the audio
    // clock's frame number, so a block the recorder itself missed is counted
    // (recorderGaps) and never mistaken for silence in the call.
    window.__record = (seconds) => new Promise(async (res, rej) => {
      const a = document.querySelector('#audio-sinks audio');
      if (!a?.srcObject) return rej(new Error('no sink'));
      const ctx = new AudioContext({ sampleRate: 48000 });
      const code = "class R extends AudioWorkletProcessor { process(i) { const c = i[0] && i[0][0]; if (c) this.port.postMessage({ f: currentFrame, d: c.slice() }); return true; } } registerProcessor('rec', R);";
      await ctx.audioWorklet.addModule(URL.createObjectURL(new Blob([code], { type: 'text/javascript' })));
      const src = ctx.createMediaStreamSource(a.srcObject);
      const node = new window.__AWN(ctx, 'rec', { numberOfInputs: 1, numberOfOutputs: 0, channelCount: 1, channelCountMode: 'explicit' });
      const want = Math.round(seconds * 48000);
      const buf = new Float32Array(want);
      let n = 0, first = null, gaps = 0;
      node.port.onmessage = (e) => {
        const { f, d } = e.data;
        if (first == null) first = f;
        const at = f - first;
        if (at !== n) gaps++;
        if (at < want) buf.set(d.subarray(0, Math.min(d.length, want - at)), at);
        n = at + d.length;
        if (n >= want) {
          node.port.onmessage = null;
          try { src.disconnect(); node.disconnect(); ctx.close(); } catch {}
          const r = window.__analyse(buf, 48000);
          r.recorderGaps = gaps;
          if (window.__wantSamples) r.samples = Array.from(buf, (v) => Math.round(v * 32767));
          res(r);
        }
      };
      src.connect(node);
      ctx.resume();
    });
    window.__analyse = (x, sr) => {
      const F = Math.round(sr / 100);
      const frames = Math.floor(x.length / F);
      const env = new Array(frames);
      for (let f = 0; f < frames; f++) {
        let s = 0;
        for (let i = 0; i < F; i++) { const v = x[f * F + i]; s += v * v; }
        env[f] = Math.sqrt(s / F);
      }
      const sorted = [...env].sort((a, b) => a - b);
      const thr = sorted[Math.floor(sorted.length * 0.5)];
      const N = 2048;
      const re = new Float64Array(N), im = new Float64Array(N);
      const fft = () => {
        for (let i = 1, j = 0; i < N; i++) {
          let bit = N >> 1;
          for (; j & bit; bit >>= 1) j ^= bit;
          j ^= bit;
          if (i < j) { let t = re[i]; re[i] = re[j]; re[j] = t; t = im[i]; im[i] = im[j]; im[j] = t; }
        }
        for (let len = 2; len <= N; len <<= 1) {
          const ang = -2 * Math.PI / len, wr = Math.cos(ang), wi = Math.sin(ang);
          for (let i = 0; i < N; i += len) {
            let cr = 1, ci = 0;
            for (let k = 0; k < len / 2; k++) {
              const ur = re[i + k], ui = im[i + k];
              const vr = re[i + k + len / 2] * cr - im[i + k + len / 2] * ci;
              const vi = re[i + k + len / 2] * ci + im[i + k + len / 2] * cr;
              re[i + k] = ur + vr; im[i + k] = ui + vi;
              re[i + k + len / 2] = ur - vr; im[i + k + len / 2] = ui - vi;
              const nr = cr * wr - ci * wi; ci = cr * wi + ci * wr; cr = nr;
            }
          }
        }
      };
      const bands = [0, 0, 0];
      let cnt = 0;
      for (let f = 0; f + 5 <= frames; f += 5) {
        if (env[f] <= thr) continue;
        const off = f * F;
        if (off + N > x.length) break;
        for (let i = 0; i < N; i++) { const w = 0.5 - 0.5 * Math.cos((2 * Math.PI * i) / N); re[i] = x[off + i] * w; im[i] = 0; }
        fft();
        for (let k = 1; k < N / 2; k++) { const hz = (k * sr) / N; const p = re[k] * re[k] + im[k] * im[k]; bands[hz < 4000 ? 0 : hz < 8000 ? 1 : 2] += p; }
        cnt++;
      }
      const dB = (p) => (cnt && p > 0 ? 10 * Math.log10(p / cnt) : -200);
      return { env, bandsDb: bands.map(dB), spectra: cnt };
    };
    window.__mute = (on) => {
      if (!window.__tone) return 0;
      window.__tone.gain.gain.setValueAtTime(on ? 0 : 0.5, window.__tone.ctx.currentTime);
      return 1;
    };
    // An analyser on the first sink, kept, so a level can be read at once.
    window.__sinkLevel = () => {
      const a = document.querySelector('#audio-sinks audio');
      if (!a?.srcObject) return null;
      if (!window.__an || window.__anFor !== a.srcObject) {
        window.__ac?.close?.();
        window.__ac = new AudioContext();
        const src = window.__ac.createMediaStreamSource(a.srcObject);
        window.__an = window.__ac.createAnalyser();
        window.__an.fftSize = 1024;
        src.connect(window.__an);
        window.__anFor = a.srcObject;
        window.__buf = new Float32Array(1024);
      }
      window.__an.getFloatTimeDomainData(window.__buf);
      let s = 0;
      for (const v of window.__buf) s += v * v;
      return Math.sqrt(s / window.__buf.length);
    };
    // Wait until the sink carries sound (or falls silent); returns the page
    // clock at the moment it did, or -1 on timeout.
    window.__waitLevel = (above, thr, timeoutMs) => new Promise((res) => {
      const t0 = performance.now();
      const tick = () => {
        const l = window.__sinkLevel();
        if (l != null && (above ? l > thr : l < thr)) return res(performance.now());
        if (performance.now() - t0 > timeoutMs) return res(-1);
        setTimeout(tick, 4);
      };
      tick();
    });
  })();
`;

async function signedIn(browser, acct, label, log) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.setBypassCSP(true); // the recorder worklet is a blob: module
  await page.evaluateOnNewDocument(INSTRUMENT);
  if (NO_WEBCODECS) await page.evaluateOnNewDocument(() => { delete window.MediaStreamTrackProcessor; delete window.AudioEncoder; delete window.VideoEncoder; });
  page.on('console', (m) => {
    const t = m.text();
    if (/media:|wsmedia|audio playback|echo|microphone|ice/i.test(t)) log.push(`${label}: ${t}`);
  });
  page.on('pageerror', (e) => log.push(`${label} PAGE ERROR: ${e}`));
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  const homeShown = () => getComputedStyle(document.getElementById('auth')).display === 'none' && getComputedStyle(document.getElementById('home')).display !== 'none';
  if (!(await page.evaluate(homeShown))) {
    await page.evaluate(
      async (email, password) => {
        const m = await import('/src/api.js');
        await m.login(email, password);
      },
      acct.email,
      acct.password
    );
    await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
    await page.waitForFunction(homeShown, { timeout: 15000 });
  }
  return page;
}

async function joinRoom(page, room) {
  const q = new URLSearchParams({ room });
  q.set('media', MEDIA); // explicit either way: the app's default is WebRTC
  if (AUDIO_ONLY) q.set('audio', '1');
  await page.goto(`${BASE}/?${q}`, { waitUntil: 'networkidle2', timeout: 30000 });
  await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 20000 });
  await sleep(700);
  await page.evaluate(() => document.getElementById('pj-join')?.click());
  await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
}

// What flows: per page, RTP audio in and out from every peer connection,
// the meeting socket's bytes, the sink elements, the level, the mic report.
async function measure(page) {
  return page.evaluate(async () => {
    const out = { inbound: [], outbound: [], rtt: null, sinks: [], videosUnmuted: 0, micAec: document.documentElement.dataset.micAec ?? null };
    for (const pc of window.__pcs ?? []) {
      let stats;
      try {
        stats = await pc.getStats();
      } catch {
        continue;
      }
      for (const s of stats.values()) {
        if (s.type === 'inbound-rtp' && s.kind === 'audio') {
          out.inbound.push({
            packets: s.packetsReceived ?? 0, lost: s.packetsLost ?? 0, bytes: s.bytesReceived ?? 0, energy: s.totalAudioEnergy ?? 0,
            jitterMs: (s.jitter ?? 0) * 1000,
            bufferMs: s.jitterBufferEmittedCount ? ((s.jitterBufferDelay ?? 0) / s.jitterBufferEmittedCount) * 1000 : null,
            concealed: s.concealedSamples ?? 0, total: s.totalSamplesReceived ?? 0,
          });
        }
        if (s.type === 'outbound-rtp' && s.kind === 'audio') out.outbound.push({ packets: s.packetsSent ?? 0, bytes: s.bytesSent ?? 0 });
        if (s.type === 'candidate-pair' && s.state === 'succeeded' && s.currentRoundTripTime != null) out.rtt = s.currentRoundTripTime * 1000;
      }
    }
    out.sinks = [...document.querySelectorAll('#audio-sinks audio')].map((a) => a.dataset.id);
    out.videosUnmuted = [...document.querySelectorAll('video')].filter((v) => !v.muted).length;
    // Video actually decoding: elements with a frame size. Camera on, each
    // side should have its own tile and the other's -- the other's proves a
    // keyframe reached a late joiner.
    out.videosDecoding = [...document.querySelectorAll('video')].filter((v) => v.videoWidth > 0 && v.srcObject).length;
    out.at = performance.now();
    const rtc = (window.__ws ?? []).filter((w) => /\/rtc\?/.test(w.url));
    out.ws = { sentBytes: rtc.reduce((n, w) => n + w.sentBytes, 0), sentCount: rtc.reduce((n, w) => n + w.sentCount, 0), recvBytes: rtc.reduce((n, w) => n + w.recvBytes, 0), recvCount: rtc.reduce((n, w) => n + w.recvCount, 0) };
    // What the received messages were, by size: keyframes (> 8 KB), video
    // deltas, audio-sized. From the per-message log, application level, so
    // a keyframe split over many TCP segments counts once at its true size.
    const log = rtc.flatMap((w) => w.recvLog ?? []);
    const recent = log.filter((e) => e[0] > performance.now() - 6000);
    const bucket = (lo, hi) => {
      const xs = recent.filter((e) => e[1] > lo && e[1] <= hi);
      return { n: xs.length, bytes: xs.reduce((a, e) => a + e[1], 0), avg: xs.length ? Math.round(xs.reduce((a, e) => a + e[1], 0) / xs.length) : 0 };
    };
    out.mix = { secs: 6, key: bucket(8000, Infinity), video: bucket(300, 8000), audio: bucket(0, 300) };
    let peak = 0;
    const t0 = performance.now();
    while (performance.now() - t0 < 500) {
      peak = Math.max(peak, window.__sinkLevel() ?? 0);
      await new Promise((r) => setTimeout(r, 25));
    }
    const a = document.querySelector('#audio-sinks audio');
    // Continuity: over two seconds of a steady tone, how many 50 ms windows
    // fell silent. Zero is a clean stream; each one is an audible dropout.
    let windows = 0;
    let silent = 0;
    const t1 = performance.now();
    while (performance.now() - t1 < 2000) {
      const l = window.__sinkLevel();
      if (l != null) {
        windows++;
        if (l < 0.01) silent++;
      }
      await new Promise((r) => setTimeout(r, 50));
    }
    out.sink = { rms: peak, paused: a?.paused ?? null, windows, silent };
    return out;
  });
}

// The clean reference as a 10 ms envelope: RIFF walk, mono s16 48 kHz.
function refEnvelope(path) {
  const buf = readFileSync(path);
  let p = 12, fmt = null, data = null;
  while (p + 8 <= buf.length) {
    const id = buf.toString('ascii', p, p + 4), sz = buf.readUInt32LE(p + 4);
    if (id === 'fmt ') fmt = { ch: buf.readUInt16LE(p + 10), rate: buf.readUInt32LE(p + 12), bits: buf.readUInt16LE(p + 22) };
    if (id === 'data') { data = buf.subarray(p + 8, p + 8 + sz); break; }
    p += 8 + sz + (sz & 1);
  }
  if (!fmt || !data || fmt.ch !== 1 || fmt.rate !== 48000 || fmt.bits !== 16) throw new Error('reference must be 48 kHz mono s16: ' + JSON.stringify(fmt));
  const n = data.length / 2, F = 480, frames = Math.floor(n / F);
  const env = new Array(frames);
  let peak = 0;
  for (let f = 0; f < frames; f++) {
    let s = 0;
    for (let i = 0; i < F; i++) { const v = data.readInt16LE((f * F + i) * 2) / 32768; s += v * v; }
    env[f] = Math.sqrt(s / F);
    peak = Math.max(peak, env[f]);
  }
  return { env, peak };
}
const mean = (xs) => xs.reduce((a, b) => a + b, 0) / (xs.length || 1);

// Speech in, speech out. B records RECORD_S seconds of what it plays of A and
// the recording is judged against the clean reference, frame by 10 ms frame:
//   - alignment: the lag that best correlates the two log envelopes (the file
//     loops in the fake device, so the reference is tiled);
//   - dropouts: reference frames with speech (above its peak - 35 dB) where
//     the received frame is 25 dB or more under the received speech level;
//     total ms, longest run;
//   - the floor: the received level in the reference's pauses at least
//     300 ms from any speech; against the speech level it is the SNR the
//     listener actually gets;
//   - bandwidth: 0-4 / 4-8 / 8-20 kHz energy over the louder frames.
async function speechQuality(page, report) {
  const ref = refEnvelope(REF);
  if (DUMP) await page.evaluate(() => { window.__wantSamples = true; });
  const got = await page.evaluate((s) => window.__record(s), RECORD_S);
  if (DUMP && got.samples) {
    const n = got.samples.length, b = Buffer.alloc(44 + n * 2);
    b.write('RIFF', 0); b.writeUInt32LE(36 + n * 2, 4); b.write('WAVE', 8); b.write('fmt ', 12); b.writeUInt32LE(16, 16); b.writeUInt16LE(1, 20); b.writeUInt16LE(1, 22); b.writeUInt32LE(48000, 24); b.writeUInt32LE(96000, 28); b.writeUInt16LE(2, 32); b.writeUInt16LE(16, 34); b.write('data', 36); b.writeUInt32LE(n * 2, 40);
    for (let i = 0; i < n; i++) b.writeInt16LE(Math.max(-32768, Math.min(32767, got.samples[i])), 44 + i * 2);
    writeFileSync(DUMP, b);
  }
  const env = got.env;
  const logE = (e) => e.map((v) => 20 * Math.log10(v + 1e-6));
  const r = logE(ref.env), g = logE(env);
  const rm = mean(r), gm = mean(g);
  let best = { lag: 0, c: -Infinity };
  for (let lag = 0; lag < r.length; lag++) {
    let s = 0, sa = 0, sb = 0;
    for (let i = 0; i < g.length; i++) { const a = g[i] - gm, b = r[(i + lag) % r.length] - rm; s += a * b; sa += a * a; sb += b * b; }
    const c = s / Math.sqrt(sa * sb || 1);
    if (c > best.c) best = { lag, c };
  }
  const active = ref.peak * Math.pow(10, -35 / 20);
  const refAt = (i) => ref.env[(i + best.lag) % ref.env.length];
  const speech = [], pause = [];
  for (let i = 0; i < env.length; i++) {
    if (refAt(i) > active) { speech.push(i); continue; }
    let far = true;
    for (let k = -30; k <= 30 && far; k++) if (refAt(i + k + ref.env.length) > active) far = false;
    if (far) pause.push(i);
  }
  const speechLevel = median(speech.map((i) => env[i]));
  const floor = pause.length ? median(pause.map((i) => env[i])) : 0;
  const holeThr = speechLevel * Math.pow(10, -25 / 20);
  let holes = 0, longest = 0, run = 0, prev = -2;
  for (const i of speech) {
    if (env[i] < holeThr) { holes++; run = i === prev + 1 ? run + 1 : 1; longest = Math.max(longest, run); } else run = 0;
    prev = i;
  }
  // Strong speech: reference frames within 12 dB of the reference's median
  // speech level. A gap there is a syllable a listener would miss; a gap in
  // the -35 dB tail of a word is often below the input's own noise floor.
  const refSpeechMed = median(speech.map((i) => refAt(i)));
  const strong = speech.filter((i) => refAt(i) > refSpeechMed * Math.pow(10, -12 / 20));
  const strongHoles = strong.filter((i) => env[i] < holeThr).length;
  // Cut: strong-speech frames more than 10 dB below the chain's own median
  // gain -- the voice dipping out, whatever the overall level (same measure
  // as the offline voice bench).
  const gainsDb = strong.map((i) => 20 * Math.log10((env[i] + 1e-9) / (refAt(i) + 1e-9)));
  const gMed = median(gainsDb);
  let cutN = 0, cutRun = 0, cutLongest = 0, cutPrev = -2;
  const cutList = [];
  strong.forEach((i, k) => { if (gainsDb[k] < gMed - 10) { cutN++; cutRun = i === cutPrev + 1 ? cutRun + 1 : 1; cutLongest = Math.max(cutLongest, cutRun); if (cutRun === 1) cutList.push({ at: i * 10, ms: 10, depth: +(gainsDb[k] - gMed).toFixed(0), outDb: +(20 * Math.log10(env[i] + 1e-9)).toFixed(0) }); else { cutList.at(-1).ms += 10; cutList.at(-1).depth = Math.min(cutList.at(-1).depth, +(gainsDb[k] - gMed).toFixed(0)); } } else cutRun = 0; cutPrev = i; });
  console.log(`    recorder: ${got.recorderGaps ?? '?'} missed blocks`);
  if (DUMP) console.log('    cuts (ms into recording, length, depth vs median gain, output dBFS): ' + cutList.filter((c) => c.ms >= 40).map((c) => `@${c.at} ${c.ms}ms ${c.depth}dB ${c.outDb}dBFS`).join(', '));
  const dB = (v) => 20 * Math.log10(v + 1e-9);
  const out = { cutMs: cutN * 10, longestCutMs: cutLongest * 10, strongMs: strong.length * 10, strongHoleMs: strongHoles * 10, alignment: +best.c.toFixed(3), speechDbfs: +dB(speechLevel).toFixed(1), floorDbfs: +dB(floor).toFixed(1), snrDb: +(dB(speechLevel) - dB(floor)).toFixed(1), holeMs: holes * 10, longestHoleMs: longest * 10, speechMs: speech.length * 10, pauseMs: pause.length * 10, bandsDb: got.bandsDb.map((x) => +x.toFixed(1)) };
  console.log(`    speech on B over ${RECORD_S} s (alignment ${out.alignment}): speech ${out.speechDbfs} dBFS, floor in pauses ${out.floorDbfs} dBFS -> SNR ${out.snrDb} dB; dropouts ${out.holeMs} ms of ${out.speechMs} ms speech (longest ${out.longestHoleMs} ms), in strong speech ${out.strongHoleMs} of ${out.strongMs} ms; CUT ${out.cutMs} ms (longest ${out.longestCutMs}); bands 0-4k ${out.bandsDb[0]}, 4-8k ${out.bandsDb[1]}, 8-20k ${out.bandsDb[2]} dB`);
  ok(best.c > 0.5, `the received speech aligns with the reference (correlation ${out.alignment})`);
  ok(speech.length >= 100 && pause.length >= 20, `enough speech and pause to judge (${out.speechMs} ms speech, ${out.pauseMs} ms pause)`);
  report.speech = out;
}

const room = 'e2e-audio-' + Math.random().toString(36).slice(2, 8);
const browser = await puppeteer.launch({
  executablePath: ENGINES[A_ENGINE],
  headless: true,
  args: [
    '--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required',
    '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
    ...(SPEECH ? ['--use-file-for-fake-audio-capture=' + SPEECH] : []),
  ],
});
const browserB = B_ENGINE === A_ENGINE ? browser : await puppeteer.launch({
  executablePath: ENGINES[B_ENGINE],
  headless: true,
  args: [
    '--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required',
    '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
    ...(SPEECH ? ['--use-file-for-fake-audio-capture=' + SPEECH] : []),
  ],
});
console.log(`== B runs in ${B_ENGINE} ${await browserB.version()}; A in ${await browser.version()}`);
const log = [];
try {
  console.log(`== media asked: ${MEDIA}${AUDIO_ONLY ? ', audio only' : ', camera on'}${NO_WEBCODECS ? ', no WebCodecs' : ''}; room ${room}`);
  const pageA = await signedIn(browser, accounts.A, 'A', log);
  const pageB = await signedIn(browserB, accounts.B, 'B', log);
  await joinRoom(pageA, room);
  await joinRoom(pageB, room);
  ok(true, 'both in the room');
  for (const [label, p] of [['A', pageA], ['B', pageB]]) {
    const t0 = performance.now();
    let told = null;
    while (performance.now() - t0 < 3000) {
      told = await p.evaluate(() => ({ n: document.documentElement.dataset.sameDevice ?? null, toast: document.querySelector('#toast.show')?.textContent ?? '' }));
      if (told.n && /same device/.test(told.toast)) break;
      await sleep(80);
    }
    ok(told?.n === '1' && /same device/.test(told?.toast ?? ''), `${label} was warned of the shared device ${Math.round(performance.now() - t0)} ms after the join`);
  }
  // Let media settle (WebRTC needs ICE; the fallback fires at 6 s), then read
  // twice 3 s apart: growth proves flow, not a one-off number.
  await sleep(MEDIA === 'webrtc' || NO_WEBCODECS ? 7000 : 4000);
  const a1 = await measure(pageA);
  const b1 = await measure(pageB);
  await sleep(3000);
  const a2 = await measure(pageA);
  const b2 = await measure(pageB);

  if (UDP_PROXY) {
    const dump = await pageA.evaluate(async () => {
      const out = [];
      for (const [i, pc] of (window.__pcs ?? []).entries()) {
        const st = await pc.getStats();
        const byId = new Map([...st.values()].map((x) => [x.id, x]));
        for (const x of st.values()) {
          if (x.type !== 'candidate-pair') continue;
          const l = byId.get(x.localCandidateId), r = byId.get(x.remoteCandidateId);
          out.push(`pc${i} ${x.state} ${l?.address}:${l?.port}/${l?.candidateType} -> ${r?.address}:${r?.port}/${r?.candidateType} req ${x.requestsSent} resp ${x.responsesReceived}`);
        }
        out.push(`pc${i} ice=${pc.iceConnectionState}`);
      }
      return out;
    });
    console.log('    A pairs: ' + dump.join(' || '));
  }
  const sum = (xs, k) => xs.reduce((n, x) => n + (x[k] || 0), 0);
  const path = (m) => (m.inbound.some((x) => x.packets > 0) ? 'WebRTC' : 'socket');
  const report = {};
  for (const [label, m1, m2] of [['A', a1, a2], ['B', b1, b2]]) {
    const got = path(m2);
    console.log(`    ${label} media path in use: ${got}`);
    ok(got === (EXPECT === 'socket' ? 'socket' : MEDIA === 'webrtc' || NO_WEBCODECS ? 'WebRTC' : 'socket'), `${label} is on the expected transport (${got}${NO_WEBCODECS ? ', browser without WebCodecs' : ''})`);
    let bytesIn = 0;
    const secs = (m2.at - m1.at) / 1000;
    if (got === 'WebRTC') {
      const inb = m2.inbound.filter((x) => x.packets > 0);
      ok(inb.length === 1, `${label} receives exactly one inbound audio stream (${inb.length} with packets)`);
      const grew = sum(m2.inbound, 'packets') - sum(m1.inbound, 'packets');
      bytesIn = sum(m2.inbound, 'bytes') - sum(m1.inbound, 'bytes');
      ok(grew > 50, `${label}: inbound audio +${grew} packets, ${Math.round(bytesIn / secs)} B/s over ${secs.toFixed(1)} s`);
      const x = inb[0] ?? {};
      console.log(`    ${label}: jitter ${x.jitterMs?.toFixed(1)} ms, jitter buffer ${x.bufferMs?.toFixed(0)} ms, lost ${x.lost}, concealed ${x.total ? ((100 * x.concealed) / x.total).toFixed(2) : '?'}%, rtt ${m2.rtt?.toFixed(1) ?? '?'} ms`);
      report[label] = { transport: got, bytesPerSec: Math.round(bytesIn / secs), jitterMs: x.jitterMs, bufferMs: x.bufferMs, lost: x.lost, rttMs: m2.rtt };
    } else {
      const sentF = m2.ws.sentCount - m1.ws.sentCount;
      const recvF = m2.ws.recvCount - m1.ws.recvCount;
      bytesIn = m2.ws.recvBytes - m1.ws.recvBytes;
      ok(sentF > 50, `${label}: sent ${sentF} socket messages, ${Math.round((m2.ws.sentBytes - m1.ws.sentBytes) / secs)} B/s over ${secs.toFixed(1)} s`);
      ok(recvF > 50, `${label}: received ${recvF} socket messages, ${Math.round(bytesIn / secs)} B/s`);
      report[label] = { transport: got, bytesPerSec: Math.round(bytesIn / secs) };
    }
    if (!SPEECH) ok(m2.sink.rms > 0.01 && m2.sink.paused === false, `${label}: the one sink is playing real audio (peak RMS ${m2.sink.rms.toFixed(3)})`);
    if (!SPEECH) ok(m2.sink.silent === 0, `${label}: continuous over 2 s (${m2.sink.silent} silent of ${m2.sink.windows} windows)`);
    report[label] = { ...(report[label] ?? {}), silentWindows: m2.sink.silent, windows: m2.sink.windows };
    ok(m2.sinks.length === 1, `${label} has exactly one audio sink element (${JSON.stringify(m2.sinks)})`);
    ok(m2.videosUnmuted === 0, `${label}: no <video> element is unmuted`);
    if (!AUDIO_ONLY && m2.mix) {
      const x = m2.mix;
      const all = x.key.bytes + x.video.bytes + x.audio.bytes;
      console.log(`    ${label} inbound mix over ${x.secs} s: keyframes ${x.key.n} (${(x.key.bytes / x.secs / 1000).toFixed(1)} KB/s, avg ${(x.key.avg / 1000).toFixed(1)} KB, ${all ? Math.round((100 * x.key.bytes) / all) : 0}% of bytes); video deltas ${x.video.n} (${(x.video.bytes / x.secs / 1000).toFixed(1)} KB/s, avg ${x.video.avg} B); audio ${x.audio.n} (${(x.audio.bytes / x.secs / 1000).toFixed(1)} KB/s)`);
      report[label] = { ...(report[label] ?? {}), mix: x };
    }
    if (!AUDIO_ONLY) ok(m2.videosDecoding >= 2, `${label}: ${m2.videosDecoding} video element(s) decoding (own tile and the other's: a keyframe reached the late joiner)`);
    console.log(`    ${label}: mic processing reported ${m2.micAec} (synthesised tone here; the real device is asserted in livestatus.e2e.mjs)`);
  }
  ok(a2.sinks[0] && b2.sinks[0] && a2.sinks[0] !== b2.sinks[0], `the two sinks name different people (A plays ${a2.sinks[0]}, B plays ${b2.sinks[0]}): nobody is played their own audio`);

  if (SPEECH) {
    await speechQuality(pageB, report);
  } else {
  // Glass to glass, transport only: A's microphone track is silenced at the
  // source (track.enabled, no button, no re-acquisition); B's sink falls
  // silent; the track is restored, and B's own clock says when sound is
  // back. The two evaluate() hops cost a few ms, the same for both paths.
  const lat = [];
  const muted = await pageA.evaluate(() => window.__mute(true));
  ok(muted >= 1, `A's tone is reachable for the measurement (${muted})`);
  await pageA.evaluate(() => window.__mute(false));
  for (let i = 0; i < RUNS; i++) {
    await pageA.evaluate(() => window.__mute(true));
    const quiet = await pageB.evaluate(() => window.__waitLevel(false, 0.004, 4000));
    if (quiet < 0) {
      console.log(`    run ${i + 1}: B never fell silent after A's track was silenced`);
      await pageA.evaluate(() => window.__mute(false));
      await sleep(800);
      continue;
    }
    await sleep(600);
    const waiting = pageB.evaluate(() => window.__waitLevel(true, 0.02, 4000));
    const before = await pageB.evaluate(() => performance.now());
    await pageA.evaluate(() => window.__mute(false));
    const at = await waiting;
    if (at > 0) lat.push(at - before);
    else console.log(`    run ${i + 1}: B never heard A again within 4 s`);
    await sleep(500);
  }
  const med = median(lat);
  ok(lat.length >= Math.max(1, RUNS - 1), `glass-to-glass latency (transport only) measured ${lat.length}/${RUNS} times: median ${med.toFixed(0)} ms, all [${lat.map((x) => x.toFixed(0)).join(', ')}] ms`);
  report.latencyMs = { median: med, runs: lat };
  // Where the time goes on the socket path: a silent Opus frame is a few
  // bytes, a tone's is dozens, so the moment the tone leaves A and the
  // moment it reaches B are visible in the socket logs. One run, split
  // three ways: A's capture+encode+batching, the wire and the server, B's
  // decode+scheduling.
  if (MEDIA !== 'webrtc') {
    await pageA.evaluate(() => window.__mute(true));
    await pageB.evaluate(() => window.__waitLevel(false, 0.004, 4000));
    await sleep(800);
    const sizeQuiet = await pageA.evaluate(() => {
      const w = (window.__ws ?? []).find((x) => /\/rtc\?/.test(x.url));
      const last = w.sentLog.slice(-20).map((e) => e[1]);
      return Math.max(...last);
    });
    const markA = await pageA.evaluate(() => performance.now());
    const markB = await pageB.evaluate(() => performance.now());
    const hostMark = performance.now();
    const waiting = pageB.evaluate(() => window.__waitLevel(true, 0.02, 4000));
    await pageA.evaluate(() => window.__mute(false));
    const heard = await waiting;
    await sleep(300);
    const thr = sizeQuiet + 8;
    const firstSent = await pageA.evaluate((m, t) => {
      const w = (window.__ws ?? []).find((x) => /\/rtc\?/.test(x.url));
      const e = w.sentLog.find((x) => x[0] > m && x[1] > t);
      return e ? e[0] - m : -1;
    }, markA, thr);
    const firstRecv = await pageB.evaluate((m, t) => {
      const w = (window.__ws ?? []).find((x) => /\/rtc\?/.test(x.url));
      const e = w.recvLog.find((x) => x[0] > m && x[1] > t);
      return e ? e[0] - m : -1;
    }, markB, thr);
    const total = heard > 0 ? heard - markB : -1;
    console.log(`    socket path breakdown (one run, silent frame ${sizeQuiet} B, threshold ${thr} B): tone left A after ${firstSent.toFixed(0)} ms; reached B's socket after ${firstRecv.toFixed(0)} ms; heard on B after ${total.toFixed(0)} ms -> capture+encode+batch ${firstSent.toFixed(0)} ms, wire+server ${(firstRecv - firstSent).toFixed(0)} ms, decode+jitter buffer ${(total - firstRecv).toFixed(0)} ms`);
    report.breakdownMs = { sender: firstSent, wire: firstRecv - firstSent, receiver: total - firstRecv, total };
    void hostMark;
  }
  // And the mute button as a person presses it: how long from press to
  // being heard again, which includes whatever the button does.
  const btn = [];
  for (let i = 0; i < Math.min(RUNS, 3); i++) {
    await pageA.click('#btn-mic');
    const quiet = await pageB.evaluate(() => window.__waitLevel(false, 0.004, 4000));
    if (quiet < 0) {
      await pageA.click('#btn-mic');
      await sleep(800);
      continue;
    }
    await sleep(600);
    const waiting = pageB.evaluate(() => window.__waitLevel(true, 0.02, 5000));
    const before = await pageB.evaluate(() => performance.now());
    await pageA.click('#btn-mic');
    const at = await waiting;
    if (at > 0) btn.push(at - before);
    await sleep(700);
  }
  console.log(`    unmute button to being heard: median ${median(btn).toFixed(0)} ms, all [${btn.map((x) => x.toFixed(0)).join(', ')}] ms`);
  report.unmuteButtonMs = { median: median(btn), runs: btn };

  // Stress: the button hammered -- TOGGLES presses 80 ms apart, ending
  // unmuted -- must leave the pipeline exactly as it was: audio flowing,
  // continuous, and the unmute latency unchanged. (A signalling chain that
  // wedges on quick toggles was a real bug once; see rtc.js _enqueue.)
  for (let i = 0; i < TOGGLES; i++) {
    await pageA.click('#btn-mic');
    await sleep(80);
  }
  const micOn = await pageA.evaluate(() => !document.getElementById('btn-mic')?.classList.contains('off') && !document.getElementById('btn-mic')?.classList.contains('muted'));
  const stateMic = await pageA.evaluate(() => document.getElementById('mic-icon')?.getAttribute('href'));
  console.log(`    after ${TOGGLES} rapid presses: mic icon ${stateMic}`);
  if (TOGGLES % 2 === 1) await pageA.click('#btn-mic'); // an odd count ends muted; end unmuted
  await sleep(400);
  const back = await pageB.evaluate(() => window.__waitLevel(true, 0.02, 4000));
  ok(back > 0, `after ${TOGGLES} rapid mute/unmute presses A is heard again on B`);
  const after = await measure(pageB);
  ok(after.sink.silent === 0, `and B's playout is continuous afterwards (${after.sink.silent} silent of ${after.sink.windows} windows)`);
  const post = [];
  for (let i = 0; i < 3; i++) {
    await pageA.click('#btn-mic');
    const quiet = await pageB.evaluate(() => window.__waitLevel(false, 0.004, 4000));
    if (quiet < 0) {
      await pageA.click('#btn-mic');
      await sleep(800);
      continue;
    }
    await sleep(600);
    const waiting = pageB.evaluate(() => window.__waitLevel(true, 0.02, 5000));
    const before = await pageB.evaluate(() => performance.now());
    await pageA.click('#btn-mic');
    const at = await waiting;
    if (at > 0) post.push(at - before);
    await sleep(500);
  }
  ok(post.length === 3 && median(post) < median(btn) * 1.5 + 60, `unmute after the stress: median ${median(post).toFixed(0)} ms, all [${post.map((x) => x.toFixed(0)).join(', ')}] ms (before the stress ${median(btn).toFixed(0)} ms)`);
  report.unmuteAfterStressMs = { median: median(post), runs: post, toggles: TOGGLES };
  void micOn;
  }
  console.log('    REPORT ' + JSON.stringify({ media: MEDIA, audioOnly: AUDIO_ONLY, speech: SPEECH || undefined, ...report }));

  for (const p of [pageA, pageB]) {
    try {
      await p.evaluate(() => document.getElementById('btn-leave')?.click());
      await sleep(150);
      await p.evaluate(() => [...document.querySelectorAll('button')].find((b) => /^(leave|end|yes)/i.test(b.textContent.trim()))?.click());
    } catch {}
  }
  const bad = log.filter((l) => /PAGE ERROR/.test(l));
  ok(bad.length === 0, `no page errors${bad.length ? ': ' + bad.slice(0, 2).join(' | ') : ''}`);
  if (log.length) console.log('    media log:\n      ' + log.slice(0, 10).join('\n      '));
} finally {
  await browser.close();
  if (browserB !== browser) await browserB.close();
}
console.log(failures ? `\n${failures} FAILED` : '\nall passed');
process.exit(failures ? 1 : 0);
