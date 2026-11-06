// Stress and scale: N people (50 by default) through every live feature at
// once, against a server this script builds and starts on a throwaway
// database, with the server process sampled throughout.
//
//   node tools/e2e/stress.mjs [--n 50] [--msgs 10] [--ui] [--keep] [--bin <server exe>]
//
// Every person is a raw protocol client (no browser): their own account,
// their own address (the server runs with -trust-proxy and each person sends
// a distinct X-Forwarded-For, as fifty real people would arrive from fifty
// addresses), their own chat socket and their own meeting socket. Frames are
// decoded with the browser's own decoders and EVERY frame is read, because
// the strings on both sockets are interned: a client that skips one decodes
// everything after it wrong.
//
// Phases, each with its assertions and its numbers:
//   accounts   N accounts, N concurrent sign-ins (argon2id: the CPU cost)
//   sockets    N chat sockets at once
//   chat       an N-person group; everyone sends --msgs messages in a burst;
//              every socket must receive every other person's messages
//              exactly once, in each sender's order
//   reactions  everyone reacts to one message at once; every socket must
//              converge on the final count, the author's feed must hold N-1
//   everyone   one @everyone: N-1 activity rows, each pushed
//   teams      a public team; N-1 join at once; the count is exact; a post in
//              General reaches everyone
//   meeting    N join one room at once; everyone must see all N; hands up;
//              a reaction each; ten people draw on the whiteboard; Together
//              mode with an outline each; one person floods reactions
//   leave      everyone leaves; the room and the server go quiet; handles
//              return to their baseline
//   ui         (--ui) a real browser joins the full meeting at phone and
//              desktop sizes: the grid and People panel at N, screenshots
//
// Prints a report and writes it to tools/e2e/.stress/report.json.
import { spawn, spawnSync, execFileSync } from 'node:child_process';
import { connect } from 'node:net';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir, platform } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Reader, Writer, Op, newDictPair, splitBatch, encodeState, decodeState, decodeRoster, decodeWelcome, decodeSpeakers, decodeSDP, decodeICE, decodeMediaFrame, encodeMediaFrame, encodeViewport } from '../../web/src/wire.js';
import { decodeWhiteboard, encodeOpen, encodeStroke, W, TOOL } from '../../web/src/whiteboard.js';
import { decodeBreakout, encodeSet as encodeBrSet, encodeCmd as encodeBrCmd, encodeAnnounce as encodeBrAnnounce, K as BR } from '../../web/src/breakout.js';
import { decodeRecording, encodeSet as encodeRecSet, encodeCaption, WHAT as REC } from '../../web/src/recording.js';
import { decodeTogether, encodeSet as encodeTogSet, encodeOutline, encodeMask } from '../../web/src/together.js';
import { decodeChatFrame } from './chatframes.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..', '..');
const arg = (name, dflt) => {
  const i = process.argv.indexOf('--' + name);
  return i > 0 ? process.argv[i + 1] : dflt;
};
const N = Number(arg('n', 50));
const MSGS = Number(arg('msgs', 10));
const UI = process.argv.includes('--ui');
const MEDIA = !process.argv.includes('--no-media');
const MEDIA_SECONDS = Number(arg('media-seconds', 10));
const RTC = !process.argv.includes('--no-webrtc');
const RTC_SECONDS = Number(arg('rtc-seconds', 15));
const KEEP = process.argv.includes('--keep');
const PORT = 8460 + Math.floor(Math.random() * 30);
const MEDIA_PORT = PORT + 100;
const PPROF = `127.0.0.1:${PORT + 200}`;
const HOST = '127.0.0.1';
const BASE = `http://${HOST}:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const now = () => performance.now();

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------
const report = { n: N, msgs: MSGS, phases: [], failures: 0, startedAt: new Date().toISOString() };
let phase = null;
const ok = (cond, what) => {
  console.log(`  ${cond ? 'ok  ' : 'FAIL'}  ${what}`);
  phase.checks.push({ ok: !!cond, what });
  if (!cond) report.failures++;
};
const metric = (k, v) => {
  phase.metrics[k] = v;
  console.log(`        ${k}: ${typeof v === 'number' ? Math.round(v * 100) / 100 : JSON.stringify(v)}`);
};
const pct = (xs, p) => {
  if (!xs.length) return NaN;
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
};
const dist = (xs) => ({ n: xs.length, p50: pct(xs, 50), p95: pct(xs, 95), p99: pct(xs, 99), max: xs.reduce((m, x) => (x > m ? x : m), -Infinity) });
async function begin(name) {
  phase = { name, checks: [], metrics: {}, t0: now(), proc0: await sample(), tel0: admin?.ready ? await telemetry() : null };
  report.phases.push(phase);
  console.log(`\n== ${name}`);
}
async function end() {
  phase.ms = now() - phase.t0;
  phase.proc1 = await sample();
  phase.tel1 = admin?.ready ? await telemetry() : null;
  const cpu = phase.proc1 && phase.proc0 ? phase.proc1.cpuS - phase.proc0.cpuS : NaN;
  console.log(`        took ${Math.round(phase.ms)} ms; server cpu ${cpu.toFixed(2)} s, memory ${phase.proc1?.mb} MB, goroutines ${phase.tel1?.goroutines}, heap ${phase.tel1?.heapMB} MB, OS threads ${phase.proc1?.threads}`);
}

// ---------------------------------------------------------------------------
// The server: built from this checkout, on a throwaway database.
// ---------------------------------------------------------------------------
const dir = mkdtempSync(join(tmpdir(), 'aether-stress-'));
// --bin runs a server built elsewhere (to compare two versions).
let exe = arg('bin', '');
if (!exe) {
  // One fixed path, reused run after run: the operating system sees one
  // program, not a new one each time (each new one is a firewall prompt).
  mkdirSync(join(here, '.stress'), { recursive: true });
  exe = join(here, '.stress', platform() === 'win32' ? 'aether-stress.exe' : 'aether-stress');
  console.log(`building the server: ${exe}`);
  const built = spawnSync('go', ['build', '-o', exe, './cmd/aether'], { cwd: join(root, 'engine'), stdio: 'inherit' });
  if (built.status !== 0) process.exit(1);
}
const adminEmail = 'stress-admin@example.test';
const adminPass = 'stress-' + randomBytes(9).toString('base64url');
const logLines = [];
const server = spawn(exe, ['-addr', `${HOST}:${PORT}`, '-media-port', String(MEDIA_PORT), '-relay=false', '-trust-proxy', '-db', join(dir, 'stress.db'), '-web', join(root, 'web'), '-admin-email', adminEmail, '-admin-password', adminPass, '-pprof', PPROF], { cwd: join(root, 'engine') });
for (const s of [server.stdout, server.stderr]) s.on('data', (d) => logLines.push(...d.toString().split('\n').filter(Boolean)));
for (let i = 0; i < 60; i++) {
  try {
    if ((await fetch(BASE + '/healthz')).ok) break;
  } catch {}
  await sleep(250);
}

// The server process, sampled: CPU seconds, memory, OS handles, threads.
async function sample() {
  try {
    if (platform() === 'win32') {
      const out = execFileSync('powershell', ['-NoProfile', '-c', `Get-Process -Id ${server.pid} | Select-Object CPU,WorkingSet64,HandleCount,@{n='T';e={$_.Threads.Count}} | ConvertTo-Json`], { encoding: 'utf8' });
      const j = JSON.parse(out);
      return { cpuS: j.CPU, mb: Math.round(j.WorkingSet64 / 1048576), handles: j.HandleCount, threads: j.T };
    }
    const st = (await import('node:fs')).readFileSync(`/proc/${server.pid}/status`, 'utf8');
    const kb = Number(/VmRSS:\s+(\d+)/.exec(st)?.[1] ?? 0);
    const fds = (await import('node:fs')).readdirSync(`/proc/${server.pid}/fd`).length;
    const stat = (await import('node:fs')).readFileSync(`/proc/${server.pid}/stat`, 'utf8').split(' ');
    return { cpuS: (Number(stat[13]) + Number(stat[14])) / 100, mb: Math.round(kb / 1024), handles: fds, threads: Number(/Threads:\s+(\d+)/.exec(st)?.[1] ?? 0) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// HTTP, as one person from one address.
// ---------------------------------------------------------------------------
class Person {
  constructor(i, email, password) {
    this.i = i;
    this.email = email;
    this.password = password;
    this.ip = `10.${50 + (i >> 16)}.${(i >> 8) & 255}.${i & 255}`;
    this.jar = new Map();
  }
  get cookie() {
    return [...this.jar].map(([k, v]) => `${k}=${v}`).join('; ');
  }
  async req(method, path, body) {
    const headers = { 'x-forwarded-for': this.ip, cookie: this.cookie };
    if (body !== undefined) headers['content-type'] = 'application/json';
    if (method !== 'GET') headers['x-aether-csrf'] = this.jar.get('aether_csrf') ?? '';
    const r = await fetch(BASE + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    for (const c of r.headers.getSetCookie?.() ?? []) {
      const [kv] = c.split(';');
      const at = kv.indexOf('=');
      this.jar.set(kv.slice(0, at), kv.slice(at + 1));
    }
    const t = await r.text();
    let json = null;
    try {
      json = t ? JSON.parse(t) : null;
    } catch {}
    if (r.status >= 400) {
      const e = new Error(`${method} ${path}: ${r.status} ${t.slice(0, 120)}`);
      e.status = r.status;
      throw e;
    }
    return json;
  }
}

// ---------------------------------------------------------------------------
// A WebSocket over a raw TCP socket: masked client frames, pongs, batches.
// ---------------------------------------------------------------------------
function ws(person, path, onFrame) {
  return new Promise((resolve, reject) => {
    const dicts = newDictPair();
    const conn = { dicts, rxBytes: 0, rxMessages: 0, rxFrames: 0, batches: 0, closed: false, decodeErrors: 0 };
    let buf = Buffer.alloc(0);
    let up = false;
    const sock = connect({ host: HOST, port: PORT }, () =>
      sock.write(
        `GET ${path} HTTP/1.1\r\nHost: ${HOST}:${PORT}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
          `Sec-WebSocket-Key: ${randomBytes(16).toString('base64')}\r\nSec-WebSocket-Version: 13\r\nOrigin: ${BASE}\r\n` +
          `X-Forwarded-For: ${person.ip}\r\nCookie: ${person.cookie}\r\n\r\n`
      )
    );
    sock.setNoDelay(true);
    conn.sock = sock;
    const write = (opcode, body) => {
      const mask = randomBytes(4);
      const out = Buffer.alloc(body.length);
      for (let i = 0; i < body.length; i++) out[i] = body[i] ^ mask[i & 3];
      const n = body.length;
      const head = n < 126 ? Buffer.from([0x80 | opcode, 0x80 | n]) : n < 65536 ? Buffer.from([0x80 | opcode, 0x80 | 126, n >> 8, n & 255]) : null;
      if (!head) throw new Error('frame too large for this client');
      if (!sock.destroyed) sock.write(Buffer.concat([head, mask, out]));
    };
    conn.send = (enc) => {
      const w = new Writer(dicts.tx);
      enc(w);
      write(2, Buffer.from(w.bytes()));
    };
    conn.close = () => {
      if (conn.closed) return;
      conn.closed = true;
      try {
        write(8, Buffer.from([0x03, 0xe8]));
      } catch {}
      sock.end();
      setTimeout(() => sock.destroy(), 500);
    };
    sock.on('error', (e) => {
      if (!up) reject(e);
    });
    sock.on('close', () => (conn.closed = true));
    sock.on('data', (d) => {
      buf = Buffer.concat([buf, d]);
      if (!up) {
        const endAt = buf.indexOf('\r\n\r\n');
        if (endAt < 0) return;
        const head = buf.toString('latin1', 0, endAt);
        if (!/^HTTP\/1\.1 101/.test(head)) {
          reject(new Error(`${path}: ${head.split('\r\n')[0]}`));
          sock.destroy();
          return;
        }
        up = true;
        buf = buf.subarray(endAt + 4);
        resolve(conn);
      }
      for (;;) {
        if (buf.length < 2) return;
        const opcode = buf[0] & 0x0f;
        let len = buf[1] & 0x7f;
        let off = 2;
        if (len === 126) {
          if (buf.length < 4) return;
          len = buf.readUInt16BE(2);
          off = 4;
        } else if (len === 127) {
          if (buf.length < 10) return;
          len = Number(buf.readBigUInt64BE(2));
          off = 10;
        }
        if (buf.length < off + len) return;
        const payload = Buffer.from(buf.subarray(off, off + len));
        buf = buf.subarray(off + len);
        conn.rxBytes += off + len;
        if (opcode === 9) write(10, payload);
        else if (opcode === 8) {
          conn.closed = true;
          sock.end();
        } else if (opcode === 2) {
          conn.rxMessages++;
          const each = (bytes) => {
            const r = new Reader(new Uint8Array(bytes), dicts.rx);
            const h = r.header();
            if (!h) return;
            if (h.op === Op.Batch) {
              conn.batches++;
              splitBatch(r, (f) => each(f));
              return;
            }
            conn.rxFrames++;
            onFrame(h, r, conn);
            if (r.err) conn.decodeErrors++;
          };
          each(payload);
        }
      }
    });
  });
}

// Every meeting frame, read the way app.js reads it.
function decodeMeetingFrame(h, r) {
  const f = { op: h.op };
  switch (h.op) {
    case Op.Welcome: f.welcome = decodeWelcome(r); break;
    case Op.SameDevice: r.uvarint(); break;
    case Op.KeyframeReq: r.str(); break;
    case Op.Roster: f.roster = decodeRoster(r); break;
    case Op.StateDelta: f.state = decodeState(r); break;
    case Op.ScreenShare: r.str(); r.str(); break;
    case Op.LobbyStatus: r.str(); r.str(); break;
    case Op.LobbyList: { const n = Number(r.uvarint()); for (let i = 0; i < n && i < 512 && !r.err; i++) { r.str(); r.str(); r.bool(); r.bits(7); } break; }
    case Op.Permissions: r.bits(8); break;
    case Op.MeetingPolicy: r.str(); r.bool(); r.bits(7); break;
    case Op.Spotlight: { const n = Number(r.uvarint()); f.spot = []; for (let i = 0; i < n && i < 512 && !r.err; i++) f.spot.push(r.str()); break; }
    case Op.Whiteboard: f.wb = decodeWhiteboard(r); break;
    case Op.Notes: case Op.QnA: f.json = r.blob(); break;
    case Op.Breakout: f.br = decodeBreakout(r); break;
    case Op.SetRole: f.role = r.str(); r.bits(8); break;
    case Op.Block: r.str(); r.bits(8); break;
    case Op.Speakers: decodeSpeakers(r); break;
    case Op.Chat: r.str(); r.blob(); break;
    case Op.Together: f.tog = decodeTogether(r); break;
    case Op.Recording: f.rec = decodeRecording(r); break;
    case Op.Reaction: f.reaction = { who: r.str(), emoji: r.str() }; break;
    case Op.MediaFrame: f.media = decodeMediaFrame(r); break;
    case Op.Sdp: decodeSDP(r); break;
    case Op.Ice: decodeICE(r); break;
    default: break; // EndMeeting, Remove, MuteAll, LowerHands: no body
  }
  return f;
}

const until = async (fn, budget = 20000, step = 50) => {
  const t = now();
  while (now() - t < budget) {
    if (fn()) return now() - t;
    await sleep(step);
  }
  return -1;
};
const all = (xs, fn) => Promise.all(xs.map(fn));

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------
const people = [];
let admin = null;
// The server's own view (admin telemetry): goroutines, connections, rooms.
async function telemetry() {
  try {
    const t = await admin.req('GET', '/api/telemetry');
    return { goroutines: t.runtime.goroutines, heapMB: t.runtime.heapMB, threads: t.runtime.threads, connections: t.connections, rooms: (t.rooms ?? []).length, media: (t.media ?? []).length };
  } catch {
    return null;
  }
}
try {
  // ---- accounts -----------------------------------------------------------
  admin = new Person(65000, adminEmail, adminPass);
  await begin('accounts');
  await admin.req('POST', '/api/identity/login', { email: adminEmail, password: adminPass });
  admin.ready = true;
  const tag = randomBytes(2).toString('hex');
  for (let i = 0; i < N; i++) {
    const p = new Person(i, `stress-${tag}-${i}@example.test`, 'stress-' + randomBytes(9).toString('base64url'));
    await admin.req('POST', '/api/identity/users', { email: p.email, displayName: `Person ${i}`, password: p.password, role: 'member' });
    people.push(p);
  }
  const loginMs = [];
  await all(people, async (p) => {
    const t = now();
    const j = await p.req('POST', '/api/identity/login', { email: p.email, password: p.password });
    loginMs.push(now() - t);
    p.id = j.user.id;
  });
  ok(people.every((p) => p.id), `${N} accounts signed in at once`);
  metric('login ms (concurrent)', dist(loginMs));
  await end();

  // ---- chat sockets -------------------------------------------------------
  await begin('sockets');
  const t0 = now();
  const chatIn = new Map(); // person -> frames
  await all(people, async (p) => {
    p.chat = { msgs: [], activity: [], other: 0 };
    p.cs = await ws(p, `/ws?d=stress-${p.i}`, (h, r) => {
      const f = decodeChatFrame(h, r);
      if (f.msg) p.chat.msgs.push({ ...f.msg, at: now() });
      else if (f.activity) p.chat.activity.push(f.activity);
      else p.chat.other++;
    });
  });
  metric('all chat sockets open (ms)', now() - t0);
  ok(people.every((p) => p.cs && !p.cs.closed), `${N} chat sockets open`);
  await sleep(1500);
  await end();

  // ---- chat storm ---------------------------------------------------------
  await begin('chat');
  const host = people[0];
  const g = await host.req('POST', '/api/messaging/groups', { title: `Stress ${tag}`, members: people.slice(1, 50).map((p) => p.email) });
  for (let k = 50; k < N; k += 50) await host.req('POST', `/api/messaging/conversations/${g.id}/members`, { emails: people.slice(k, k + 50).map((p) => p.email) });
  await sleep(1000);
  for (const p of people) p.chat.msgs = [];
  const sentAt = new Map();
  const ackAt = new Map();
  const postMs = [];
  const tSend = now();
  let sendErrors = 0;
  await all(people, async (p) => {
    for (let k = 0; k < MSGS; k++) {
      await sleep(Math.random() * 150);
      const body = `s${p.i}#${k}`;
      const t = now();
      sentAt.set(body, t);
      try {
        await p.req('POST', `/api/messaging/conversations/${g.id}/messages`, { body });
        ackAt.set(body, now());
        postMs.push(now() - t);
      } catch {
        sendErrors++;
      }
    }
  });
  metric('all sends accepted (ms)', now() - tSend);
  ok(sendErrors === 0, `every send accepted (${N * MSGS} messages, errors ${sendErrors})`);
  const want = (N - 1) * MSGS;
  const settled = await until(() => people.every((p) => p.chat.msgs.filter((m) => /^s\d+#\d+$/.test(m.body)).length >= want), 30000);
  ok(settled >= 0, `every socket received all ${want} messages from the others (settled ${Math.round(settled)} ms after the last send)`);
  const lat = [];
  let dupes = 0;
  let outOfOrder = 0;
  let missing = 0;
  for (const p of people) {
    const got = p.chat.msgs.filter((m) => /^s\d+#\d+$/.test(m.body));
    const seen = new Set();
    const lastSeq = new Map();
    for (const m of got) {
      if (seen.has(m.body)) dupes++;
      seen.add(m.body);
      const [, s, k] = /^s(\d+)#(\d+)$/.exec(m.body);
      if ((lastSeq.get(s) ?? -1) > Number(k)) outOfOrder++;
      lastSeq.set(s, Number(k));
      lat.push(m.at - sentAt.get(m.body));
      if (Number(s) === p.i) dupes++; // your own message pushed to the device that did not send it via the socket
    }
    missing += Math.max(0, want - seen.size);
  }
  ok(dupes === 0, `no duplicates (${dupes})`);
  ok(outOfOrder === 0, `each sender's order kept on every socket (${outOfOrder} out of order)`);
  ok(missing === 0, `nothing missing (${missing})`);
  metric('delivery latency ms (send start to receipt, all sockets)', dist(lat));
  // Split: the send's own round trip (the database write and the fan-out,
  // which happen before the reply), and receipt measured from the reply.
  // Negative means the push beat the HTTP reply back to this process.
  metric('send round trip ms (POST to its reply)', dist(postMs));
  const afterAck = [];
  for (const p of people) for (const m of p.chat.msgs) if (ackAt.has(m.body)) afterAck.push(m.at - ackAt.get(m.body));
  metric('receipt after the reply ms', dist(afterAck));
  const rx = people.map((p) => p.cs.rxBytes);
  metric('chat socket bytes received per person', dist(rx));
  metric('chat socket messages / frames / batches (person 1)', { messages: people[1].cs.rxMessages, frames: people[1].cs.rxFrames, batches: people[1].cs.batches });
  ok(people.every((p) => p.cs.decodeErrors === 0), 'every frame decoded');
  await end();

  // ---- reaction storm -----------------------------------------------------
  await begin('reactions');
  const target = (await host.req('POST', `/api/messaging/conversations/${g.id}/messages`, { body: 'react to this' }));
  await sleep(500);
  const tReact = now();
  await all(people.slice(1), (p) => p.req('POST', `/api/messaging/messages/${target.id}/react`, { emoji: '👍', on: true }));
  metric('all reactions accepted (ms)', now() - tReact);
  const finalCount = (p) => {
    const last = [...p.chat.msgs].reverse().find((m) => m.id === target.id);
    return last?.reactions?.find((x) => x.emoji === '👍')?.count ?? 0;
  };
  const conv = await until(() => people.slice(1).every((p) => finalCount(p) === N - 1), 30000);
  ok(conv >= 0, `every socket converged on ${N - 1} reactions (${Math.round(conv)} ms)`);
  // Every page of the author's reactions (a page is at most 100).
  const feed = { items: [], unread: 0 };
  for (let before = '', k = 0; k < 20; k++) {
    const pg = await host.req('GET', `/api/messaging/activity?kind=reaction&limit=100${before ? '&before=' + encodeURIComponent(before) : ''}`);
    feed.items.push(...pg.items);
    feed.unread = pg.unread;
    if (pg.items.length < 100) break;
    const last = pg.items[pg.items.length - 1];
    before = `${new Date(last.at).getTime()}:${last.id}`;
  }
  ok(feed.items.filter((x) => x.messageId === target.id).length === N - 1, `the author's feed holds ${N - 1} reaction rows, paged (${feed.items.filter((x) => x.messageId === target.id).length})`);
  ok(host.chat.activity.filter((a) => a.kind === 'item' && a.item.messageId === target.id).length === N - 1, `and ${N - 1} were pushed to the author`);
  ok(feed.unread >= N - 1, `the author's unread count includes them (${feed.unread})`);
  await end();

  // ---- @everyone ----------------------------------------------------------
  await begin('everyone');
  for (const p of people) p.chat.activity = [];
  const shout = await people[1].req('POST', `/api/messaging/conversations/${g.id}/messages`, { body: '@everyone standup in five', mentions: ['*'] });
  const gotShout = await until(() => people.filter((p) => p.i !== 1).every((p) => p.chat.activity.some((a) => a.kind === 'item' && a.item.messageId === shout.id && a.item.kind === 'mention')), 20000);
  ok(gotShout >= 0, `@everyone: an activity row pushed to each of the other ${N - 1} (${Math.round(gotShout)} ms)`);
  ok(!people[1].chat.activity.some((a) => a.kind === 'item' && a.item.messageId === shout.id), 'and none to the sender');
  const sampleFeed = await people[N - 1].req('GET', '/api/messaging/activity?kind=mention&limit=5');
  ok(sampleFeed.items.some((x) => x.messageId === shout.id), 'the row is in the feed from the source too');
  // Everyone reads the conversation at once: their mention rows go read.
  await all(people, (p) => p.req('POST', `/api/messaging/conversations/${g.id}/read`, {}));
  const afterRead = await people[N - 1].req('GET', '/api/messaging/activity?unread=1&kind=mention&limit=5');
  ok(!afterRead.items.some((x) => x.messageId === shout.id), 'reading the conversation read the mention');
  await end();

  // ---- teams --------------------------------------------------------------
  await begin('teams');
  const team = await host.req('POST', '/api/messaging/teams', { name: `Stress team ${tag}`, description: '', visibility: 'public' });
  const tJoin = now();
  let joinErrors = 0;
  await all(people.slice(1), (p) => p.req('POST', `/api/messaging/teams/${team.id}/join`, {}).catch(() => joinErrors++));
  metric('all joins (ms)', now() - tJoin);
  ok(joinErrors === 0, `${N - 1} joined at once without an error (${joinErrors})`);
  const members = await host.req('GET', `/api/messaging/teams/${team.id}/members`);
  ok((members.members ?? members).length === N, `the team has exactly ${N} members (${(members.members ?? members).length})`);
  const mine = (await host.req('GET', '/api/messaging/teams')).teams.find((t) => t.id === team.id);
  const general = mine.channels.find((c) => c.general || c.title === 'General');
  for (const p of people) p.chat.msgs = [];
  const post = await host.req('POST', `/api/messaging/conversations/${general.id}/messages`, { body: 'welcome, everyone' });
  const reached = await until(() => people.slice(1).every((p) => p.chat.msgs.some((m) => m.id === post.id)), 20000);
  ok(reached >= 0, `a post in General reached all ${N - 1} (${Math.round(reached)} ms)`);
  await end();

  // ---- meeting ------------------------------------------------------------
  await begin('meeting');
  const room = `stress-${tag}`;
  const tMeet = now();
  for (const p of people) {
    p.m = { id: '', roster: new Map(), hands: new Set(), reactions: [], strokes: 0, masks: 0, togOn: false, count: new Map(), br: [], finals: 0, spot: [], roles: 0 };
  }
  // The first in hosts: join them, then everyone else at once.
  const joinOne = async (p) => {
    p.ms = await ws(p, `/rtc?room=${room}&name=Person%20${p.i}`, (h, r) => {
      const f = decodeMeetingFrame(h, r);
      p.m.count.set(h.op, (p.m.count.get(h.op) ?? 0) + 1);
      if (f.welcome && !p.m.id) p.m.id = f.welcome.id;
      if (f.roster) {
        if (f.roster.present) p.m.roster.set(f.roster.id, f.roster);
        else p.m.roster.delete(f.roster.id);
      }
      if (f.state) {
        if (f.state.handRaised) p.m.hands.add(f.state.participant);
        else p.m.hands.delete(f.state.participant);
      }
      if (f.reaction) p.m.reactions.push(f.reaction);
      if (f.wb && f.wb.kind === 'stroke') p.m.strokes++;
      if (f.wb && f.wb.kind === 'state') p.m.board = f.wb;
      if (f.tog?.kind === 'state') p.m.togOn = f.tog.on;
      if (f.tog?.kind === 'mask') p.m.masks++;
      if (f.media && p.m.onMedia) p.m.onMedia(f.media);
      if (f.br) p.m.br.push(f.br);
      if (f.rec?.kind === 'caption' && f.rec.final) p.m.finals++;
      if (f.spot) p.m.spot = f.spot;
      if (f.role) p.m.roles++;
    });
  };
  await joinOne(people[0]);
  await until(() => people[0].m.id, 5000);
  await all(people.slice(1), joinOne);
  const full = await until(() => people.every((p) => p.m.id && p.m.roster.size === N), 30000);
  metric('everyone sees everyone (ms from first join)', now() - tMeet);
  ok(full >= 0, `all ${N} joined and every one of them sees all ${N}`);
  // Keepalive, as the browser does every 25 s; not needed inside this run.
  await sleep(1500);
  const idleBytes0 = people.map((p) => p.ms.rxBytes);
  await sleep(3000);
  const idleBytes = people.map((p, i) => p.ms.rxBytes - idleBytes0[i]);
  metric('meeting bytes per person over 3 s of nothing happening', dist(idleBytes));

  // Hands: everyone at once.
  const tHands = now();
  for (const p of people) p.ms.send((w) => encodeState(w, { participant: p.m.id, micMuted: true, camOff: true, sharing: false, handRaised: true, speaking: false, recording: false, role: 0 }));
  const hands = await until(() => people.every((p) => p.m.hands.size >= N - 1), 20000);
  ok(hands >= 0, `${N} hands up at once: everyone sees every other hand (${Math.round(now() - tHands)} ms)`);

  // One reaction each.
  for (const p of people) p.m.reactions = [];
  const tRx = now();
  for (const p of people) p.ms.send((w) => { w.header(Op.Reaction); w.str('👏'); });
  const rxAll = await until(() => people.every((p) => p.m.reactions.length >= N), 20000);
  ok(rxAll >= 0, `a reaction each: everyone receives all ${N} (${Math.round(now() - tRx)} ms)`);

  // Whiteboard: the host opens it, ten people draw twenty strokes each.
  host.ms.send((w) => encodeOpen(w));
  await sleep(800);
  const drawers = people.slice(0, Math.min(10, N));
  const tWb = now();
  for (let k = 0; k < 20; k++) {
    for (const d of drawers) {
      const pts = Array.from({ length: 24 }, (_, j) => [200 + d.i * 40 + j * 6, 300 + k * 30 + ((j * 7) % 13)]).flat();
      d.ms.send((w) => encodeStroke(w, W.stroke, k + 1, TOOL.pen, 1, 4, pts));
    }
    await sleep(20);
  }
  const strokesWant = drawers.length * 20;
  const wbAll = await until(() => people.every((p) => p.m.strokes >= strokesWant - (drawers.includes(p) ? 20 : 0)), 20000);
  ok(wbAll >= 0, `whiteboard: ${strokesWant} strokes from ${drawers.length} people reach everyone (${Math.round(now() - tWb)} ms)`);
  metric('strokes received (min over people)', people.reduce((m, p) => Math.min(m, p.m.strokes), Infinity));
  // Somebody who arrives now is given the whole board in one frame.
  const late = new Person(N, `stress-${tag}-late@example.test`, 'stress-' + randomBytes(9).toString('base64url'));
  await admin.req('POST', '/api/identity/users', { email: late.email, displayName: 'Late', password: late.password, role: 'member' });
  late.id = (await late.req('POST', '/api/identity/login', { email: late.email, password: late.password })).user.id;
  late.m = { id: '', roster: new Map(), hands: new Set(), reactions: [], strokes: 0, masks: 0, togOn: false, count: new Map(), br: [], finals: 0, spot: [], roles: 0 };
  await joinOne(late);
  const gotBoard = await until(() => late.m.board?.strokes?.length >= strokesWant, 10000);
  ok(gotBoard >= 0, `a late arrival is given the whole board: ${late.m.board?.strokes?.length ?? 0} of ${strokesWant} strokes, ${late.ms.rxBytes} B on arrival`);
  ok((late.m.board?.strokes ?? []).every((x) => x.points.length === 48), 'every stroke whole (24 points each)');
  late.ms.close();
  await until(() => people.every((p) => p.m.roster.size === N), 10000);

  // Together mode: the host turns it on; everyone sends an outline.
  host.ms.send((w) => encodeTogSet(w, true, 0));
  const togOn = await until(() => people.every((p) => p.m.togOn), 10000);
  ok(togOn >= 0, 'Together mode on for everyone');
  const bits = new Uint8Array(64 * 36);
  for (let y = 8; y < 36; y++) for (let x = 22; x < 42; x++) bits[y * 64 + x] = 1;
  const rle = encodeMask(bits);
  const tTog = now();
  for (const p of people) p.ms.send((w) => encodeOutline(w, rle));
  const togAll = await until(() => people.every((p) => p.m.masks >= N - 1), 20000);
  ok(togAll >= 0, `an outline each: everyone receives the other ${N - 1} (${Math.round(now() - tTog)} ms)`);

  // One person floods each kind of relayed frame: what reaches everybody
  // else is the allowance, not the flood (room/allowance.go).
  const flooder = people[N - 1];
  const victim = people[1];
  const floods = [
    ['reactions', Op.Reaction, (k, w) => { w.header(Op.Reaction); w.str('🎉'); }, 4],
    ['hand toggles', Op.StateDelta, (k, w) => encodeState(w, { participant: flooder.m.id, micMuted: true, camOff: true, sharing: false, handRaised: k % 2 === 0, speaking: false, recording: false, role: 0 }), 20],
    ['meeting chat', Op.Chat, (k, w) => { w.header(Op.Chat); w.blob(new TextEncoder().encode('spam ' + k)); }, 10],
    ['whiteboard previews', Op.Whiteboard, (k, w) => encodeStroke(w, W.live, 9000 + k, TOOL.pen, 1, 4, [10, 10, 20, 20]), 120],
  ];
  for (const [name, op, enc, burst] of floods) {
    await sleep(3000); // every allowance refilled
    const before = victim.m.count?.get(op) ?? 0;
    const b0 = victim.ms.rxBytes;
    for (let k = 0; k < 500; k++) flooder.ms.send((w) => enc(k, w));
    await sleep(2500);
    const relayed = (victim.m.count?.get(op) ?? 0) - before;
    metric(`flood of 500 ${name}: relayed to each other person / bytes`, { relayed, bytes: victim.ms.rxBytes - b0 });
    ok(relayed <= burst + 6, `500 ${name} from one person reach each of the other ${N - 1} as ${relayed} (allowance ${burst} at once)`);
  }
  ok(people.every((p) => p.ms.decodeErrors === 0), 'every meeting frame decoded');
  // Together and the board off again, for the gallery check.
  host.ms.send((w) => encodeTogSet(w, false, 0));
  host.ms.send((w) => { w.header(Op.Whiteboard); w.bits(W.close, 3); w.bits(0, 5); });
  await until(() => people.every((p) => !p.m.togOn), 10000);
  await end();


  // ---- media --------------------------------------------------------------
  // Audio, cameras and a screen share over the socket media path, from every
  // person at once, with synthetic payloads the size real encoders produce
  // (audio: 120 B every 20 ms at 48 kbit/s; a still camera at constant
  // quality: about 600 B a frame at 15 fps with an 8 KB key frame every 2 s,
  // ARCHITECTURE 6bl; a screen: 4 KB at 5 fps with a 60 KB key frame every
  // 5 s). Each payload starts with who sent it, its sequence number and when.
  if (MEDIA) {
    await begin('media');
    const KIND_AUDIO = 0, KIND_CAM = 1, KIND_SCREEN = 2;
    const stamp = (size, sender, kind, seq) => {
      const b = Buffer.alloc(Math.max(16, size));
      b.writeUInt16BE(sender, 0);
      b.writeUInt8(kind, 2);
      b.writeUInt32BE(seq, 3);
      b.writeDoubleBE(now(), 7);
      return new Uint8Array(b);
    };
    for (const p of people) {
      p.media = { got: new Map(), lat: { [KIND_AUDIO]: [], [KIND_CAM]: [], [KIND_SCREEN]: [] }, bytes0: p.ms.rxBytes };
      p.m.onMedia = (f) => {
        const b = Buffer.from(f.payload);
        if (b.length < 15) return;
        const sender = b.readUInt16BE(0), kind = b.readUInt8(2), seq = b.readUInt32BE(3), t = b.readDoubleBE(7);
        const key = `${sender}:${kind}`;
        const g = p.media.got.get(key) ?? { n: 0, last: -1, gaps: 0, dupes: 0 };
        if (seq <= g.last) g.dupes++;
        else if (g.last >= 0 && seq !== g.last + 1) g.gaps += seq - g.last - 1;
        g.last = Math.max(g.last, seq);
        g.n++;
        p.media.got.set(key, g);
        if ((seq & 3) === 0) p.media.lat[kind].push(now() - t);
      };
    }
    // Each person's gallery shows nine others, as a browser's does, and says
    // so (by participant id, as app.js reportViewport does): cameras go only
    // to those showing them. Spread so every camera is on somebody's screen.
    const SHOWN = Math.min(9, N - 1);
    for (const p of people) {
      p.shows = new Set(Array.from({ length: SHOWN }, (_, k) => people[(p.i + 1 + k) % N].i));
      p.ms.send((w) => encodeViewport(w, people.filter((q) => q !== p).map((q) => ({ track: q.m.id, width: 320, height: 180, visible: p.shows.has(q.i), pinned: false }))));
    }
    await sleep(800);
    const talkers = people.slice(0, 3);
    const presenter = people[1];
    const sent = new Map();
    const bump = (k) => sent.set(k, (sent.get(k) ?? 0) + 1);
    const timers = [];
    const tMedia = now();
    const cpu0 = await sample();
    // Audio: 50 a second from each talker.
    timers.push(setInterval(() => {
      for (const p of talkers) {
        const k = `${p.i}:${KIND_AUDIO}`;
        p.ms.send((w) => encodeMediaFrame(w, { kind: 0, key: false, ts: 0, payload: stamp(120, p.i, KIND_AUDIO, sent.get(k) ?? 0) }));
        bump(k);
      }
    }, 20));
    // Cameras: everyone, 15 a second, staggered so they do not all fire together.
    people.forEach((p, idx) => {
      setTimeout(() => timers.push(setInterval(() => {
        const k = `${p.i}:${KIND_CAM}`;
        const seq = sent.get(k) ?? 0;
        const key = seq % 30 === 0;
        p.ms.send((w) => encodeMediaFrame(w, { kind: 1, key, ts: Math.round(now()), payload: stamp(key ? 8000 : 600, p.i, KIND_CAM, seq), vp9: true }));
        bump(k);
      }, 66)), (idx * 66) / N);
    });
    // One screen: 5 a second.
    timers.push(setInterval(() => {
      const k = `${presenter.i}:${KIND_SCREEN}`;
      const seq = sent.get(k) ?? 0;
      const key = seq % 25 === 0;
      presenter.ms.send((w) => encodeMediaFrame(w, { kind: 1, key, ts: Math.round(now()), payload: stamp(key ? 60000 : 4000, presenter.i, KIND_SCREEN, seq), screen: true, vp9: true }));
      bump(k);
    }, 200));
    await sleep(MEDIA_SECONDS * 1000);
    for (const t of timers) clearInterval(t);
    await sleep(1500); // the last frames land
    const secs = (now() - tMedia) / 1000;
    const cpu1 = await sample();
    metric('server cpu during media (cores busy)', cpu0 && cpu1 ? (cpu1.cpuS - cpu0.cpuS) / secs : NaN);
    let lost = 0, dupes = 0, expected = 0, received = 0, unseen = 0;
    for (const p of people) {
      for (const [k, n] of sent) {
        const [sender, kind] = k.split(':').map(Number);
        if (sender === p.i) continue; // nobody is sent their own
        const g = p.media.got.get(k);
        if (kind === KIND_CAM && !p.shows.has(sender)) {
          unseen += g?.n ?? 0; // a camera this person is not showing
          continue;
        }
        expected += n;
        received += g?.n ?? 0;
        // The first frames after the viewport report can predate it; count
        // what is missing after the first second's worth.
        lost += Math.max(0, n - (g?.n ?? 0));
        dupes += g?.dupes ?? 0;
      }
    }
    ok(unseen === 0, `nobody was sent a camera they are not showing (${unseen} frames)`);
    metric('media frames sent / expected at receivers / received', { sent: [...sent.values()].reduce((a, b) => a + b, 0), expected, received });
    ok(lost === 0, `every audio, camera and screen frame reached every other person (${lost} missing of ${expected})`);
    ok(dupes === 0, `none twice (${dupes})`);
    for (const [kind, name] of [[KIND_AUDIO, 'audio'], [KIND_CAM, 'camera'], [KIND_SCREEN, 'screen']]) {
      metric(`${name} latency ms, sender to receiver`, dist(people.flatMap((p) => p.media.lat[kind])));
    }
    const rxRate = people.map((p) => ((p.ms.rxBytes - p.media.bytes0) * 8) / secs / 1e6);
    metric('received per person, Mbit/s', dist(rxRate));
    metric('server out, all people, Mbit/s', rxRate.reduce((a, b) => a + b, 0));
    ok(people.every((p) => !p.ms.closed), `nobody was dropped (${people.filter((p) => p.ms.closed).length} disconnected)`);

    // One person floods: 400 frames of 60 KB as fast as the socket takes them.
    const flooder2 = people[N - 2];
    const before = people.map((p) => p.ms.closed);
    for (const p of people) p.media.got.clear();
    const fb0 = people[0].ms.rxBytes;
    // As audio: audio goes to everyone, so this is the widest a flood can go.
    const memBefore = await sample();
    for (let k = 0; k < 400; k++) flooder2.ms.send((w) => encodeMediaFrame(w, { kind: 0, key: false, ts: 0, payload: stamp(60000, flooder2.i, KIND_AUDIO, k) }));
    await sleep(4000);
    const memAfter = await sample();
    metric('flood: server memory before / after (MB)', [memBefore?.mb, memAfter?.mb]);
    const knocked = people.filter((p, i) => !before[i] && p !== flooder2 && p.ms.closed).length;
    const relayedMB = (people[0].ms.rxBytes - fb0) / 1e6;
    metric('flood: 24 MB sent by one person; MB that reached each other person', relayedMB);
    metric('flood: people disconnected by it', knocked);
    ok(knocked === 0, `one person's media flood does not knock anybody else out of the meeting (${knocked} disconnected)`);
    ok(relayedMB < 8, `and what reaches each other person is the allowance (6 MB burst), not the flood: ${relayedMB.toFixed(1)} MB`);
    await end();
  }



  // ---- WebRTC: the path browsers use, with N real peer connections --------
  // engine/cmd/sfuload: each person joins over the meeting socket, negotiates
  // both peer connections, publishes a synthetic VP8 camera and an Opus
  // microphone over real ICE/DTLS/SRTP, shows nine tiles, and counts what
  // arrives per track.
  if (RTC) {
    await begin('webrtc');
    const loader = join(here, '.stress', platform() === 'win32' ? 'sfuload.exe' : 'sfuload');
    const b = spawnSync('go', ['build', '-o', loader, './cmd/sfuload'], { cwd: join(root, 'engine'), stdio: 'inherit' });
    ok(b.status === 0, 'the load client builds');
    const rtcRoom = `stress-rtc-${tag}`;
    const spec = join(here, '.stress', 'people.json');
    writeFileSync(spec, JSON.stringify({ base: BASE, room: rtcRoom, admin: { cookie: admin.cookie, ip: admin.ip, name: 'admin' }, people: people.map((p) => ({ cookie: p.cookie, ip: p.ip, name: `Person ${p.i}` })) }));
    const cpu0 = await sample();
    const t0r = now();
    const profiled = (async () => {
      await sleep(16000); // after joining and settling
      try {
        const prof = await fetch(`http://${PPROF}/debug/pprof/profile?seconds=10`);
        writeFileSync(join(here, '.stress', 'webrtc-cpu.prof'), Buffer.from(await prof.arrayBuffer()));
        const heap = await fetch(`http://${PPROF}/debug/pprof/heap`);
        writeFileSync(join(here, '.stress', 'webrtc-heap.prof'), Buffer.from(await heap.arrayBuffer()));
      } catch (e) {
        console.log('  (profile not taken: ' + e.message + ')');
      }
    })();
    // Run it alongside, so the server can be profiled while the meeting is
    // live: a CPU profile over the measured window, and the heap after it.
    const res = await new Promise((resolve) => {
      const ch = spawn(loader, ['-in', spec, '-secs', String(RTC_SECONDS), '-shown', String(Math.min(9, N - 1)), '-talkers', '3']);
      let stdout = '', stderr = '';
      ch.stdout.on('data', (d) => (stdout += d));
      ch.stderr.on('data', (d) => (stderr += d));
      ch.on('close', (status) => resolve({ stdout, stderr, status }));
    });
    const cpu1 = await sample();
    const line = (res.stdout || '').trim().split('\n').pop();
    let r = null;
    try {
      r = JSON.parse(line);
    } catch {
      console.log((res.stderr || '').slice(0, 2000));
    }
    await profiled;
    ok(!!r, 'the load client reported');
    if (r) {
      metric('webrtc report', r);
      metric('server cpu during it (cores busy)', cpu0 && cpu1 ? (cpu1.cpuS - cpu0.cpuS) / ((now() - t0r) / 1000) : NaN);
      ok(r.joined === N && r.errors === 0, `all ${N} joined and negotiated without an error (${r.joined} joined, ${r.errors} errors)`);
      // Media slots (engine sfu/slots.go): a receiver is sent the cameras it
      // has on screen and the voices that are talking, through a pool bounded
      // however large the meeting. Three people talk; a muted browser sends
      // no audio.
      const shownN = Math.min(9, N - 1);
      ok(r.videoSlotsPerPerson.every((n) => n >= shownN && n <= 49), `every receiver's video slots cover what it shows and stay within the 49 cap (${Math.min(...r.videoSlotsPerPerson)}-${Math.max(...r.videoSlotsPerPerson)} for ${N - 1} cameras)`);
      ok(r.audioSlotsPerPerson.every((n) => n <= 6), `audio slots stay within the cap of 6 (max ${Math.max(...r.audioSlotsPerPerson)})`);
      ok(r.audioStalled === 0, `every talker is heard by every other receiver (${r.audioStalled} missing)`);
      ok(r.shownCamerasStalled === 0, `no on-screen camera stalled (${r.shownCamerasStalled})`);
      ok(r.hiddenCameraKbpsAvg < r.shownCameraKbpsAvg / 4, `off-screen cameras are held back: ${r.hiddenCameraKbpsAvg.toFixed(1)} kbit/s each against ${r.shownCameraKbpsAvg.toFixed(1)} on screen`);
    }
    await end();
  }

  // ---- controls: every other meeting feature, with everyone in it --------
  await begin('controls');
  {
    const csrfHdr = (p) => ({ 'x-forwarded-for': p.ip, cookie: p.cookie, 'x-aether-csrf': p.jar.get('aether_csrf') ?? '' });
    const opCount = (p, op) => p.m.count.get(op) ?? 0;

    // Q&A: everyone asks at once; everyone likes the first question.
    const q0 = people.map((p) => opCount(p, Op.QnA));
    const tQ = now();
    const qs = await Promise.all(people.map((p) => p.req('POST', `/api/scheduling/rooms/${room}/qna`, { body: `Question from ${p.i}?`, anonymous: false }).catch((e) => ({ err: e.status }))));
    metric('Q&A: everyone asks at once (ms)', now() - tQ);
    ok(qs.every((q) => q && !q.err), `${N} questions accepted (${qs.filter((q) => q?.err).length} refused)`);
    const firstQ = qs.find((q) => q?.id)?.id;
    await all(people.slice(1), (p) => p.req('POST', `/api/scheduling/rooms/${room}/qna/${firstQ}/like`, {}).catch(() => null));
    const qPush = await until(() => people.every((p, i) => opCount(p, Op.QnA) - q0[i] >= N), 20000);
    ok(qPush >= 0, `every question pushed to everyone (${Math.round(qPush)} ms)`);
    const list = await host.req('GET', `/api/scheduling/rooms/${room}/qna`);
    const qList = Array.isArray(list) ? list : list.questions ?? list.items ?? [];
    const liked = qList.find((q) => q.id === firstQ);
    ok(qList.length >= N, `the list holds all ${N} (${qList.length})`);
    metric('Q&A: the liked question as listed', liked ? Object.fromEntries(Object.entries(liked).filter(([k]) => /like|vote|up/i.test(k))) : null);

    // Meeting notes: twenty people add an item at once.
    await host.req('POST', `/api/scheduling/rooms/${room}/notes`, {});
    const writers = people.slice(0, Math.min(20, N));
    await all(writers, (p) => p.req('POST', `/api/scheduling/rooms/${room}/notes/items`, { section: 'agenda', body: `Item from ${p.i}` }));
    const notes = await host.req('GET', `/api/scheduling/rooms/${room}/notes`);
    const items = JSON.stringify(notes).match(/Item from \d+/g) ?? [];
    ok(new Set(items).size === writers.length, `notes: ${writers.length} items added at once, all kept (${new Set(items).size})`);

    // Captions: transcription on; ten people speak at once.
    host.ms.send((w) => encodeRecSet(w, REC.transcription, true));
    await sleep(800);
    // A muted speaker's words reach nobody (captions are speech).
    for (const p of people) p.m.finals = 0;
    people[N - 1].ms.send((w) => encodeCaption(w, true, 1, 'said while muted'));
    await sleep(1000);
    ok(people.every((p) => p.m.finals === 0), `a muted speaker's caption reaches nobody (${people.reduce((a, p) => a + p.m.finals, 0)} delivered)`);
    // Ten speakers unmute and speak at once, at the browser's pace: the
    // whole line so far every 350 ms, then the final line.
    const speakers = people.slice(0, Math.min(10, N));
    for (const sp of speakers) sp.ms.send((w) => encodeState(w, { participant: sp.m.id, micMuted: false, camOff: true, sharing: false, handRaised: false, speaking: true, recording: false, role: 0 }));
    await sleep(500);
    const tCap = now();
    const line = (sp) => `Speaker ${sp.i} says something useful`;
    for (let k = 1; k <= 3; k++) {
      for (const sp of speakers) sp.ms.send((w) => encodeCaption(w, false, 1, line(sp).slice(0, k * 10)));
      await sleep(350);
    }
    for (const sp of speakers) sp.ms.send((w) => encodeCaption(w, true, 1, line(sp)));
    const caps = await until(() => people.every((p) => p.m.finals >= speakers.length - (speakers.includes(p) ? 1 : 0)), 15000);
    ok(caps >= 0, `captions: ${speakers.length} people speaking at once, every final line to everyone (${Math.round(now() - tCap)} ms from the first word)`);
    host.ms.send((w) => encodeRecSet(w, REC.transcription, false));

    // Host controls.
    const target = people[3];
    host.ms.send((w) => { w.header(Op.Spotlight); w.str(target.m.id); w.bool(true); w.bits(0, 7); });
    const spot = await until(() => people.every((p) => p.m.spot.includes(target.m.id)), 10000);
    ok(spot >= 0, `spotlight: everyone sees it (${Math.round(spot)} ms)`);
    host.ms.send((w) => { w.header(Op.Spotlight); w.str(''); w.bool(false); w.bits(0, 7); });
    await sleep(3000); // the state allowance refills after the earlier floods
    for (const p of people) p.ms.send((w) => encodeState(w, { participant: p.m.id, micMuted: false, camOff: true, sharing: false, handRaised: true, speaking: false, recording: false, role: 0 }));
    await until(() => people.every((p) => p.m.hands.size >= N - 1), 10000);
    const lowerSeen = people.map((p) => opCount(p, Op.LowerHands));
    host.ms.send((w) => w.header(Op.LowerHands));
    const lowered = await until(() => people.slice(1).every((p, i) => opCount(p, Op.LowerHands) > lowerSeen[i + 1]), 10000);
    ok(lowered >= 0, `lower all hands: every other person is told (${Math.round(lowered)} ms)`);
    const muteSeen = people.map((p) => opCount(p, Op.MuteAll));
    host.ms.send((w) => w.header(Op.MuteAll));
    const muted = await until(() => people.slice(1).every((p, i) => opCount(p, Op.MuteAll) > muteSeen[i + 1]), 10000);
    ok(muted >= 0, `mute all: every other person is muted (${Math.round(muted)} ms)`);
    for (const p of people) p.m.roles = 0;
    const promote = people.slice(5, 10);
    for (const p of promote) host.ms.send((w) => { w.header(Op.SetRole); w.str(p.m.id); w.bits(2, 2); w.bits(0, 6); });
    const roled = await until(() => people.every((p) => p.m.roles >= promote.length), 10000);
    ok(roled >= 0, `${promote.length} made presenters: everyone told of each (${Math.round(roled)} ms)`);

    // Breakout rooms: five, everyone but the host spread across them.
    for (const p of people) p.m.br = [];
    const others = people.slice(1);
    const rooms = Math.min(5, others.length);
    const members = Array.from({ length: rooms }, (_, k) => others.filter((_, j) => j % rooms === k).map((p) => p.m.id));
    host.ms.send((w) => encodeBrSet(w, { names: members.map((_, k) => `Room ${k + 1}`), members, limitSec: 0, autoMove: true, allowReturn: true }));
    await sleep(500);
    const tBr = now();
    host.ms.send((w) => encodeBrCmd(w, BR.open));
    const moved = await until(() => others.every((p) => p.m.br.some((x) => x?.kind === 'move')), 15000);
    ok(moved >= 0, `breakouts: all ${others.length} sent a move (${Math.round(moved)} ms)`);
    await all(others, async (p) => {
      const mv = p.m.br.find((x) => x?.kind === 'move');
      p.bm = { id: '', roster: new Map(), br: [] };
      p.bs = mv
        ? await ws(p, `/rtc?room=${encodeURIComponent(mv.room)}&name=Person%20${p.i}&bt=${encodeURIComponent(mv.ticket)}`, (h, r) => {
            const f = decodeMeetingFrame(h, r);
            if (f.welcome && !p.bm.id) p.bm.id = f.welcome.id;
            if (f.roster) {
              if (f.roster.present) p.bm.roster.set(f.roster.id, 1);
              else p.bm.roster.delete(f.roster.id);
            }
            if (f.br) p.bm.br.push(f.br);
          }).catch(() => null)
        : null;
    });
    const perRoom = Math.floor(others.length / rooms);
    const inRooms = await until(() => others.every((p) => p.bs && p.bm.roster.size >= perRoom), 20000);
    ok(inRooms >= 0, `everyone entered their breakout room on their ticket and sees its people (${Math.round(now() - tBr)} ms from open)`);
    host.ms.send((w) => encodeBrAnnounce(w, 'Two minutes left'));
    const heard = await until(() => others.every((p) => p.bm.br.some((x) => x?.kind === 'announce') || p.m.br.some((x) => x?.kind === 'announce')), 10000);
    ok(heard >= 0, `an announcement reaches every breakout room (${Math.round(heard)} ms)`);
    host.ms.send((w) => encodeBrCmd(w, BR.close));
    const back = await until(() => others.every((p) => p.bm.br.some((x) => x?.kind === 'return') || p.m.br.some((x) => x?.kind === 'return')), 15000);
    ok(back >= 0, `closing the rooms sends everyone back (${Math.round(back)} ms)`);
    for (const p of others) p.bs?.close();
    await sleep(1000);

    // A poll in the group: everyone votes at once; every socket converges.
    const poll = await host.req('POST', `/api/messaging/conversations/${g.id}/messages`, { body: 'Ship on Friday?', meta: JSON.stringify({ poll: { q: 'Ship on Friday?', options: ['Yes', 'No'], multi: false, anon: false } }) });
    await sleep(500);
    const votes = await Promise.all(people.slice(1).map((p) => p.req('POST', `/api/messaging/messages/${poll.id}/vote`, { options: [p.i % 2] }).then(() => 1, () => 0)));
    ok(votes.every(Boolean), `${N - 1} votes at once, all accepted (${votes.filter((v) => !v).length} refused)`);
    const tally = (p) => [...p.chat.msgs].reverse().find((m) => m.id === poll.id)?.poll?.total ?? 0;
    const conv = await until(() => people.every((p) => tally(p) === N - 1), 20000);
    ok(conv >= 0, `every socket converges on the tally of ${N - 1} (${Math.round(conv)} ms)`);

    // Files: fifty uploads at once, then fifty downloads of one of them.
    const uploaders = people.slice(0, Math.min(50, N));
    const blob = new Uint8Array(256 * 1024).map((_, i) => (i * 31) & 255);
    const tUp = now();
    const ups = await Promise.all(uploaders.map(async (p) => {
      const fd = new FormData();
      fd.append('body', `file from ${p.i}`);
      fd.append('file', new Blob([blob], { type: 'application/octet-stream' }), `data-${p.i}.bin`);
      const r = await fetch(`${BASE}/api/messaging/conversations/${g.id}/files`, { method: 'POST', headers: csrfHdr(p), body: fd });
      return r.status === 201 ? await r.json() : null;
    }));
    metric(`files: ${uploaders.length} uploads of 256 KB at once (ms)`, now() - tUp);
    ok(ups.every(Boolean), `every upload accepted (${ups.filter((u) => !u).length} refused)`);
    const fileId = ups.find(Boolean)?.attachments?.[0]?.id;
    const tDown = now();
    const downs = await Promise.all(uploaders.map(async (p) => {
      const r = await fetch(`${BASE}/api/messaging/files/${fileId}`, { headers: { 'x-forwarded-for': p.ip, cookie: p.cookie } });
      const b = new Uint8Array(await r.arrayBuffer());
      return r.status === 200 && b.length === blob.length && b[1000] === blob[1000];
    }));
    metric(`files: ${uploaders.length} downloads at once (ms)`, now() - tDown);
    ok(downs.every(Boolean), `every download whole and right (${downs.filter((d) => !d).length} wrong)`);

    // Search, everyone at once.
    const sMs = [];
    await all(people, async (p) => {
      const t = now();
      await p.req('GET', `/api/messaging/search?q=${encodeURIComponent('file from ' + ((p.i + 1) % N))}&limit=20`).catch(() => null);
      sMs.push(now() - t);
    });
    metric('search: everyone at once, ms', dist(sMs));
  }
  await end();

  // ---- the UI at N --------------------------------------------------------
  if (UI) {
    await begin('ui');
    const { launch } = await import('./meet.lib.mjs');
    const browser = await launch();
    const viewer = new Person(64000, `stress-${tag}-viewer@example.test`, 'stress-' + randomBytes(9).toString('base64url'));
    await admin.req('POST', '/api/identity/users', { email: viewer.email, displayName: 'Viewer', password: viewer.password, role: 'member' });
    const out = join(here, '.stress');
    mkdirSync(out, { recursive: true });
    for (const [w, h, name] of [[390, 844, 'phone'], [1280, 800, 'desktop']]) {
      const ctx = await browser.createBrowserContext();
      const page = await ctx.newPage();
      await page.setExtraHTTPHeaders({ 'x-forwarded-for': `10.250.0.${w === 390 ? 1 : 2}` });
      await page.setViewport({ width: w, height: h, isMobile: w < 500, hasTouch: w < 500 });
      await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
      await page.evaluate(async (e, pw) => (await import('/src/api.js')).login(e, pw), viewer.email, viewer.password);
      await page.goto(`${BASE}/?room=${room}`, { waitUntil: 'networkidle2' });
      await sleep(2500);
      await page.evaluate(() => document.getElementById('pj-join')?.click());
      const drawn = await page.waitForFunction(() => document.querySelectorAll('.strip-tile').length > 0, { timeout: 20000 }).then(() => true, () => false);
      await sleep(2500);
      const lay = () => page.evaluate(() => {
        const vw = innerWidth;
        // Off the screen and not inside something that scrolls to it.
        const scrolls = (e) => {
          for (let a = e.parentElement; a; a = a.parentElement) {
            const cs = getComputedStyle(a);
            if (/(auto|scroll)/.test(cs.overflowX + cs.overflowY) && (a.scrollWidth > a.clientWidth + 1 || a.scrollHeight > a.clientHeight + 1)) return true;
          }
          return false;
        };
        const offEls = [...document.querySelectorAll('button, .strip-tile, .person')].filter((e) => {
          const r = e.getBoundingClientRect();
          return r.width && getComputedStyle(e).visibility !== 'hidden' && (r.right > vw + 1 || r.left < -1) && !scrolls(e);
        });
        const off = offEls.length;
        const offWhat = offEls.slice(0, 4).map((e) => e.id || e.className || e.tagName);
        return { overflowX: document.documentElement.scrollWidth > vw + 1, off, offWhat, sameDevice: document.documentElement.dataset.sameDevice ?? '', tiles: document.querySelectorAll('.strip-tile').length, people: document.querySelectorAll('#view-people .person').length };
      });
      let L = await lay();
      ok(drawn && L.tiles > 0, `${name}: the gallery draws with ${N + 1} people in the meeting (${L.tiles} tiles on screen)`);
      ok(!L.overflowX && L.off === 0, `${name}: nothing leaves the screen (${L.off} off-screen ${JSON.stringify(L.offWhat)})`);
      ok(!L.sameDevice, `${name}: not told it shares a device with ${N} people from ${N} addresses (${L.sameDevice || 'not told'})`);
      await page.screenshot({ path: join(out, `gallery-${name}.png`) });
      await page.evaluate(() => document.getElementById('rail-people')?.click());
      await sleep(1500);
      L = await lay();
      ok(L.people === N + 1, `${name}: the People panel lists all ${N + 1} (${L.people})`);
      ok(!L.overflowX, `${name}: the People panel fits`);
      await page.screenshot({ path: join(out, `people-${name}.png`) });
      await page.evaluate(() => document.getElementById('rail-people')?.click());
      // Together mode at N, chosen by the host.
      host.ms.send((w) => encodeTogSet(w, true, 0));
      await sleep(2500);
      L = await lay();
      ok(!L.overflowX, `${name}: Together mode at ${N + 1} fits`);
      await page.screenshot({ path: join(out, `together-${name}.png`) });
      host.ms.send((w) => encodeTogSet(w, false, 0));
      await sleep(800);
      await ctx.close();
    }
    await browser.close();
    await end();
  }

  // ---- leave --------------------------------------------------------------
  await begin('leave');
  for (const p of people) p.ms?.close();
  const emptied = await until(() => people.every((p) => p.ms.closed), 10000);
  ok(emptied >= 0, 'every meeting socket closed');
  for (const p of people) p.cs?.close();
  await until(() => people.every((p) => p.cs.closed), 10000);
  // Leaks are judged on the server's own counts. OS handles cannot tell a
  // leak from Go keeping the threads it made during a spike (Go never gives
  // threads back); goroutines, connections, rooms and media routes can.
  const t0tel = report.phases.find((x) => x.name === 'sockets').tel0;
  let tel = await telemetry();
  const tSettle = now();
  // An empty room (and its media router) is reaped by a once-a-minute sweep
  // once it is two minutes old: allow for that.
  while (tel && (tel.connections > 0 || tel.rooms > 0 || tel.media > 0 || tel.goroutines > t0tel.goroutines + 25) && now() - tSettle < 200000) {
    await sleep(1000);
    tel = await telemetry();
  }
  metric('server after everyone left (telemetry)', tel);
  metric('settled after (ms)', now() - tSettle);
  metric('process after everyone left', await sample());
  ok(tel && tel.connections === 0, `no meeting connection left (${tel?.connections})`);
  ok(tel && tel.rooms === 0 && tel.media === 0, `no room or media route left (${tel?.rooms} rooms, ${tel?.media} routes)`);
  ok(tel && tel.goroutines <= t0tel.goroutines + 25, `goroutines back to their baseline (${t0tel.goroutines} before the sockets opened, ${tel?.goroutines} after)`);
  const warn = logLines.filter((l) => /level=(WARN|ERROR)/.test(l));
  metric('server warnings and errors in the log', warn.length);
  const backedUp = logLines.filter((l) => /backed up/.test(l)).length;
  ok(backedUp === 0, `no socket was dropped for falling behind (${backedUp})`);
  if (warn.length) console.log(warn.slice(0, 10).map((l) => '          ' + l.slice(0, 200)).join('\n'));
  await end();
} catch (e) {
  console.log('FAIL  threw: ' + (e?.stack ?? e));
  report.failures++;
} finally {
  report.finishedAt = new Date().toISOString();
  const out = join(here, '.stress');
  mkdirSync(out, { recursive: true });
  writeFileSync(join(out, 'report.json'), JSON.stringify(report, null, 2));
  server.kill();
  console.log(`\n${report.failures ? report.failures + ' FAILED' : 'all passed'}  (N=${N}; report in tools/e2e/.stress/report.json)`);
  if (!KEEP) setTimeout(() => process.exit(report.failures ? 1 : 0), 300);
}
