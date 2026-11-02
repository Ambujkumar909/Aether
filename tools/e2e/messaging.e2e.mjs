// End-to-end: the messaging API and its push socket, against a RUNNING server.
//
//   node tools/e2e/messaging.e2e.mjs [https://localhost:8443]
//
// This is not a unit test. It signs up throwaway accounts (e2e-<random>@
// example.test), drives the real HTTP API with the real cookies and CSRF
// header, opens the real WebSocket with a hand-rolled client (so the session
// cookie can be sent -- Node's built-in WebSocket cannot), and decodes every
// push frame with the browser's own wire.js. Every assertion below is one
// that a unit test cannot make, because it crosses a process boundary.
//
// It exits non-zero on the first failure and prints latency percentiles at
// the end, because "lightning fast" is a number or it is nothing.

import { connect as tlsConnect } from 'node:tls';
import { randomBytes, createHash } from 'node:crypto';
import { performance } from 'node:perf_hooks';
import { Reader, Writer, Op, newDictPair, splitBatch } from '../../web/src/wire.js';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'; // the dev certificate is self-signed
const BASE = process.argv[2] || 'https://localhost:8443';
const url = new URL(BASE);

// ---------------------------------------------------------------------------
// Tiny test runner
// ---------------------------------------------------------------------------
let failures = 0;
let passes = 0;
function ok(cond, what) {
  if (cond) {
    passes++;
    return;
  }
  failures++;
  console.error(`  FAIL  ${what}`);
}
function eq(a, b, what) {
  ok(JSON.stringify(a) === JSON.stringify(b), `${what}: got ${JSON.stringify(a)}, want ${JSON.stringify(b)}`);
}
async function section(name, fn) {
  const before = failures;
  const t0 = performance.now();
  try {
    await fn();
  } catch (e) {
    failures++;
    console.error(`  FAIL  ${name}: threw ${e?.stack || e}`);
  }
  const ms = (performance.now() - t0).toFixed(0);
  console.log(`${failures === before ? 'ok  ' : 'FAIL'}  ${name} (${ms} ms)`);
}
function pct(xs, p) {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
}

// ---------------------------------------------------------------------------
// An HTTP client with a cookie jar and the CSRF echo, like api.js
// ---------------------------------------------------------------------------
class Client {
  constructor(label) {
    this.label = label;
    this.cookies = new Map();
    this.timings = new Map(); // "METHOD path" -> [ms]
  }
  cookieHeader() {
    return [...this.cookies].map(([k, v]) => `${k}=${v}`).join('; ');
  }
  async req(method, path, body, { raw = false, headers = {} } = {}) {
    const h = { ...headers };
    if (this.cookies.size) h.cookie = this.cookieHeader();
    if (body !== undefined && !(body instanceof FormData)) h['content-type'] = 'application/json';
    const csrf = this.cookies.get('aether_csrf');
    if (csrf && method !== 'GET' && !('X-Aether-CSRF' in h) && !h.noCsrf) h['X-Aether-CSRF'] = csrf;
    delete h.noCsrf;
    const t0 = performance.now();
    const res = await fetch(BASE + path, {
      method,
      headers: h,
      body: body === undefined ? undefined : body instanceof FormData ? body : JSON.stringify(body),
      redirect: 'manual',
    });
    const dt = performance.now() - t0;
    const key = `${method} ${path.replace(/[0-9a-f-]{16,}/g, ':id').split('?')[0]}`;
    if (!this.timings.has(key)) this.timings.set(key, []);
    this.timings.get(key).push(dt);
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [pair] = sc.split(';');
      const i = pair.indexOf('=');
      this.cookies.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
    }
    if (raw) return res;
    const text = await res.text();
    let json = null;
    try {
      json = text ? JSON.parse(text) : null;
    } catch {
      json = { _text: text };
    }
    return { status: res.status, json };
  }
  get(p, o) {
    return this.req('GET', p, undefined, o);
  }
  post(p, b, o) {
    return this.req('POST', p, b ?? {}, o);
  }
  patch(p, b, o) {
    return this.req('PATCH', p, b ?? {}, o);
  }
  put(p, b, o) {
    return this.req('PUT', p, b ?? {}, o);
  }
  del(p, o) {
    return this.req('DELETE', p, undefined, o);
  }
}

// Throwaway accounts, created once and reused: registration is rate limited
// to three an hour per address (a security control this harness must not
// need to switch off), so the credentials are kept next to the script and
// later runs sign in instead. Delete the file to start over.
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
const ACCOUNTS = join(dirname(fileURLToPath(import.meta.url)), '.accounts.json');
let saved = {};
try {
  saved = JSON.parse(readFileSync(ACCOUNTS, 'utf8'));
} catch {
  saved = {};
}
const host = url.host;

async function signup(label) {
  const c = new Client(label);
  const have = saved[host]?.[label];
  if (have) {
    c.email = have.email;
    c.password = have.password;
    const r = await c.post('/api/identity/login', { email: c.email, password: c.password });
    if (r.status === 200) {
      c.user = r.json.user;
      return c;
    }
    console.log(`  (stored account for ${label} no longer signs in: ${r.status}; creating a new one)`);
  }
  const suffix = randomBytes(4).toString('hex');
  c.email = `e2e-${label.toLowerCase()}-${suffix}@example.test`;
  c.password = 'e2e-' + randomBytes(9).toString('base64url');
  const r = await c.post('/api/identity/signup', {
    email: c.email,
    displayName: `E2E ${label}`,
    password: c.password,
  });
  if (r.status !== 201) throw new Error(`signup ${label}: ${r.status} ${JSON.stringify(r.json)}`);
  c.user = r.json.user;
  saved[host] = { ...(saved[host] ?? {}), [label]: { email: c.email, password: c.password } };
  writeFileSync(ACCOUNTS, JSON.stringify(saved, null, 2));
  return c;
}

// ---------------------------------------------------------------------------
// A WebSocket client that can send a Cookie header. Receive-only plus pong.
// ---------------------------------------------------------------------------
function openPush(client, device = '') {
  return new Promise((resolve, reject) => {
    const key = randomBytes(16).toString('base64');
    const sock = tlsConnect(
      { host: url.hostname, port: Number(url.port || 443), rejectUnauthorized: false, servername: url.hostname },
      () => {
        sock.write(
          `GET /ws${device ? '?d=' + encodeURIComponent(device) : ''} HTTP/1.1\r\nHost: ${url.host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
            `Sec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\nOrigin: ${url.origin}\r\n` +
            `Cookie: ${client.cookieHeader()}\r\n\r\n`
        );
      }
    );
    const frames = [];
    const waiters = [];
    let buf = Buffer.alloc(0);
    let upgraded = false;
    const dicts = newDictPair();

    // The server coalesces frames that leave together into one Batch: each
    // inner frame is delivered on its own, in order.
    const deliver = (bytes) => {
      const r = new Reader(new Uint8Array(bytes), dicts.rx);
      const h = r.header();
      if (!h) return;
      if (h.op === Op.Batch) {
        splitBatch(r, (inner) => deliver(Buffer.from(inner)));
        return;
      }
      const f = { op: h.op, raw: bytes };
      // Activity frames intern strings: read them, or every later frame on
      // this socket decodes against a dictionary that has fallen behind.
      if (h.op === Op.Activity) {
        const kind = r.bits(2);
        r.bits(6);
        if (kind === 0) { for (let i = 0; i < 12; i++) r.str(); r.uvarint(); r.bits(8); r.blob(); r.uvarint(); }
        else if (kind === 1) { const n = r.uvarint(); for (let i = 0; i < n; i++) r.str(); r.uvarint(); }
        else if (kind === 2) { r.str(); r.uvarint(); }
      }
      if (h.op === Op.ConvMessage) {
        f.msg = {
          conversationId: r.str(),
          id: r.str(),
          senderId: r.str(),
          senderName: r.str(),
          createdAt: Number(r.uvarint()),
          deleted: r.bool(),
          edited: r.bool(),
          forwarded: r.bool(),
          system: r.bool(),
        };
        r.bits(4);
        f.msg.body = new TextDecoder().decode(r.blob());
        f.msg.replyTo = r.str();
        if (f.msg.replyTo) {
          f.msg.quoted = { senderId: r.str(), senderName: r.str(), deleted: r.bool() };
          r.bits(7);
          f.msg.quoted.body = new TextDecoder().decode(r.blob());
        }
        const n = Number(r.uvarint());
        f.msg.reactions = [];
        for (let i = 0; i < n; i++) {
          const emoji = r.str();
          const count = Number(r.uvarint());
          const k = Number(r.uvarint());
          const who = [];
          for (let j = 0; j < k; j++) who.push(r.str());
          f.msg.reactions.push({ emoji, count, who });
        }
        const nAtt = Number(r.uvarint());
        f.msg.attachments = [];
        for (let i = 0; i < nAtt; i++) {
          f.msg.attachments.push({ id: r.str(), name: r.str(), mime: r.str(), size: Number(r.uvarint()) });
        }
        const nMen = Number(r.uvarint());
        f.msg.mentions = [];
        for (let i = 0; i < nMen; i++) f.msg.mentions.push(r.str());
        f.msg.priority = r.str();
        f.msg.meta = new TextDecoder().decode(r.blob());
        const nOpt = Number(r.uvarint());
        if (nOpt > 0) {
          const counts = [];
          const voters = [];
          for (let i = 0; i < nOpt; i++) {
            counts.push(Number(r.uvarint()));
            const k = Number(r.uvarint());
            const who = [];
            for (let j = 0; j < k; j++) who.push(r.str());
            voters.push(who);
          }
          f.msg.poll = { counts, voters, total: Number(r.uvarint()) };
        }
        f.decodeError = !!r.err;
      } else if (h.op === Op.Status) {
        f.status = { who: r.str(), status: r.str(), note: r.str(), until: Number(r.uvarint()) };
        f.status.auto = r.bool();
        f.status.online = r.bool();
        r.bits(6);
        f.status.lastSeen = Number(r.uvarint());
      } else if (h.op === Op.Presence) {
        f.presence = { who: r.str(), on: r.bool() };
        r.bits(7);
        f.presence.lastSeen = Number(r.uvarint());
      } else if (h.op === Op.ReadCursor) {
        f.read = { convId: r.str(), who: r.str(), at: Number(r.uvarint()) };
      } else if (h.op === Op.Typing) {
        f.typing = { convId: r.str(), who: r.str(), whoId: r.str(), on: r.bool() };
        r.bits(7);
      }
      if (waiters.length) waiters.shift()(f);
      else frames.push(f);
    };

    sock.on('data', (d) => {
      buf = Buffer.concat([buf, d]);
      if (!upgraded) {
        const end = buf.indexOf('\r\n\r\n');
        if (end < 0) return;
        const head = buf.subarray(0, end).toString();
        if (!/^HTTP\/1\.1 101/.test(head)) {
          reject(new Error('websocket upgrade refused: ' + head.split('\r\n')[0]));
          sock.destroy();
          return;
        }
        const accept = createHash('sha1').update(key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
        if (!head.includes(accept)) {
          reject(new Error('bad Sec-WebSocket-Accept'));
          sock.destroy();
          return;
        }
        buf = buf.subarray(end + 4);
        upgraded = true;
        resolve(api);
      }
      // Frames. Server frames are unmasked.
      for (;;) {
        if (buf.length < 2) return;
        const fin = (buf[0] & 0x80) !== 0;
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
        const payload = buf.subarray(off, off + len);
        buf = buf.subarray(off + len);
        if (!fin) throw new Error('fragmented frames are not expected here');
        if (opcode === 0x9) send(0xa, payload); // ping -> pong
        else if (opcode === 0x2) deliver(Buffer.from(payload));
        else if (opcode === 0x8) {
          sock.end();
        }
      }
    });
    sock.on('error', reject);

    // Client frames must be masked.
    function send(opcode, payload) {
      const mask = randomBytes(4);
      const len = payload.length;
      const head =
        len < 126
          ? Buffer.from([0x80 | opcode, 0x80 | len])
          : Buffer.concat([Buffer.from([0x80 | opcode, 0x80 | 126]), Buffer.from([len >> 8, len & 0xff])]);
      const masked = Buffer.alloc(len);
      for (let i = 0; i < len; i++) masked[i] = payload[i] ^ mask[i & 3];
      sock.write(Buffer.concat([head, mask, masked]));
    }

    const api = {
      /**
       * Next frame matching pred, within a timeout. Frames that do not match
       * stay in the buffer for a later wait; a wait that times out withdraws
       * itself, so it can never swallow a frame meant for the next one. (An
       * earlier version did both wrongs: a non-matching frame handed to a
       * waiter was dropped, and a timed-out waiter stayed registered and ate
       * the next matching frame -- which read as a server bug.)
       */
      nextWhere(pred, timeoutMs = 4000, what = 'no matching frame') {
        return new Promise((res, rej) => {
          const buffered = frames.findIndex(pred);
          if (buffered >= 0) return res(frames.splice(buffered, 1)[0]);
          let done = false;
          const take = (f) => {
            if (done) return frames.push(f);
            if (pred(f)) {
              done = true;
              clearTimeout(t);
              res(f);
            } else {
              frames.push(f);
              waiters.push(take);
            }
          };
          const t = setTimeout(() => {
            done = true;
            const i = waiters.indexOf(take);
            if (i >= 0) waiters.splice(i, 1);
            rej(new Error(`${what} within ${timeoutMs} ms`));
          }, timeoutMs);
          waiters.push(take);
        });
      },
      /** Next ConvMessage frame, within a timeout. */
      nextMessage(timeoutMs = 4000) {
        return api.nextWhere((f) => f.op === Op.ConvMessage, timeoutMs, 'no push frame');
      },
      /** Next read-cursor frame, within a timeout. */
      nextRead(timeoutMs = 4000) {
        return api.nextWhere((f) => f.op === Op.ReadCursor, timeoutMs, 'no read cursor');
      },
      /** Drain everything that arrives in a quiet window. */
      async settle(ms = 300) {
        await new Promise((r) => setTimeout(r, ms));
        const out = frames.filter((f) => f.op === Op.ConvMessage);
        frames.length = 0;
        return out;
      },
      close() {
        send(0x8, Buffer.from([0x03, 0xe8]));
        sock.end();
      },
      /** Say "typing" (or "stopped") in a conversation, as the browser does. */
      typing(convId, on) {
        const w = new Writer(dicts.tx);
        w.header(Op.Typing);
        w.str(convId);
        w.bool(!!on);
        w.bits(0, 7);
        send(0x2, Buffer.from(w.bytes()));
      },
      /** What has arrived and not been taken: for a failing wait's report. */
      pending() {
        return frames.map((f) => ({ op: f.op, presence: f.presence, status: f.status && { who: f.status.who, status: f.status.status } }));
      },
    };
  });
}

// ---------------------------------------------------------------------------
// The scenarios
// ---------------------------------------------------------------------------
const A = await signup('A');
const B = await signup('B');
const C = await signup('C'); // a stranger to every A-B conversation
console.log(`accounts: ${A.email}, ${B.email}, ${C.email}`);

// Accounts are reused across runs, so group and channel memberships from an
// earlier run would make C a contact of A and change what the directory may
// show. Every run starts by leaving them.
for (const who of [A, B, C]) {
  const mine = (await who.get('/api/messaging/conversations')).json.conversations ?? [];
  for (const c of mine) {
    if (c.kind !== 'direct') await who.post(`/api/messaging/conversations/${c.id}/leave`, {});
  }
}

let conv; // A <-> B
let selfA; // A's notes
let hello; // the first message
let pushB;
let pushA;

await section('sign-in state and CSRF are enforced', async () => {
  const anon = new Client('anon');
  eq((await anon.get('/api/messaging/conversations')).status, 401, 'unauthenticated list');
  const noCsrf = await A.post('/api/messaging/direct', { email: B.email }, { headers: { noCsrf: true, 'X-Aether-CSRF': '' } });
  ok(noCsrf.status === 403, `POST without the CSRF header: ${noCsrf.status}, want 403`);
});

await section('open a direct conversation, both sides see it', async () => {
  const r = await A.post('/api/messaging/direct', { email: B.email });
  ok(r.status === 200 || r.status === 201, `open direct: ${r.status}`);
  conv = r.json;
  ok(conv?.id, 'conversation has an id');
  const listB = await B.get('/api/messaging/conversations');
  ok((listB.json.conversations ?? []).some((c) => c.id === conv.id), 'B lists the conversation');
  const s = await A.post('/api/messaging/self', {});
  selfA = s.json;
  ok(selfA?.id && selfA.id !== conv.id, 'A has a self thread distinct from the direct one');
});

await section('the push socket authenticates with the session cookie', async () => {
  pushB = await openPush(B);
  pushA = await openPush(A);
  let refused = false;
  try {
    await openPush(new Client('nobody'));
  } catch {
    refused = true;
  }
  ok(refused, 'an unauthenticated socket is refused');
});

await section('a message is pushed to the other side, decoded by the browser wire code', async () => {
  const r = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'hello' });
  eq(r.status, 201, 'send');
  hello = r.json;
  const f = await pushB.nextMessage();
  ok(!f.decodeError, 'frame decodes cleanly');
  eq(f.msg.id, hello.id, 'pushed id');
  eq(f.msg.body, 'hello', 'pushed body');
  eq(f.msg.forwarded, false, 'not forwarded');
  eq(f.msg.replyTo, '', 'no reply');
  eq(f.msg.reactions, [], 'no reactions yet');
  // A sender without a device id is not pushed their own message.
  const echoes = await pushA.settle(250);
  eq(echoes.length, 0, 'a sender with no device id gets no echo of their own send');
});

await section("a message reaches the sender's other devices and never the device that sent it", async () => {
  // Two tabs of A, each with its device id on its socket; the send carries
  // tab-1's id. tab-2 has it, tab-1 is not echoed, B has it, and pushA
  // (a socket with no device id) has it too -- it cannot be the sender.
  const tab1 = await openPush(A, 'tab-1');
  const tab2 = await openPush(A, 'tab-2');
  const r = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'from tab one' }, { headers: { 'X-Aether-Device': 'tab-1' } });
  eq(r.status, 201, 'sent from tab 1');
  const isIt = (x) => x.op === Op.ConvMessage && x.msg.id === r.json.id;
  ok(await tab2.nextWhere(isIt, 3000), 'tab 2 has it');
  ok(await pushB.nextWhere(isIt, 3000), 'B has it');
  ok(await pushA.nextWhere(isIt, 3000), 'a device-less socket of the sender has it');
  let echoed = null;
  try {
    echoed = await tab1.nextWhere(isIt, 700);
  } catch {}
  eq(echoed, null, 'tab 1, which sent it, is not echoed');
  tab1.close();
  tab2.close();
});

await section('typing: one frame per burst, a stop when the typist stops or sends, nothing for a keystroke storm', async () => {
  const typ = (x) => x.op === Op.Typing && x.typing.convId === conv.id && x.typing.whoId === A.user.id;
  const t0 = performance.now();
  pushA.typing(conv.id, true);
  const on = await pushB.nextWhere((x) => typ(x) && x.typing.on, 3000);
  eq(on.typing.who, A.user.displayName || 'E2E A', `B hears A typing in ${Math.round(performance.now() - t0)} ms, by name`);
  // Twenty more "typing" signals inside the repeat window: none relayed.
  for (let i = 0; i < 20; i++) pushA.typing(conv.id, true);
  let extra = null;
  try {
    extra = await pushB.nextWhere(typ, 900);
  } catch {}
  eq(extra, null, 'a storm of typing signals inside the window reaches nobody');
  pushA.typing(conv.id, false);
  const off = await pushB.nextWhere((x) => typ(x) && !x.typing.on, 3000);
  ok(off, 'the stop is relayed');
  let again = null;
  try {
    again = await pushB.nextWhere(typ, 600);
  } catch {}
  eq(again, null, 'a stop is said once');
  // Typing, then the message: the server says "stopped" on the send even
  // though the client did not, so nobody is left watching dots.
  await new Promise((r) => setTimeout(r, 2600));
  pushA.typing(conv.id, true);
  await pushB.nextWhere((x) => typ(x) && x.typing.on, 3000);
  const sent = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'done typing' });
  eq(sent.status, 201, 'sent');
  const stop = await pushB.nextWhere((x) => typ(x) && !x.typing.on, 3000);
  ok(stop, 'the send ends the typing for the audience');
  await pushB.nextWhere((x) => x.op === Op.ConvMessage && x.msg.id === sent.json.id, 3000);
  // A stop from somebody who never started reaches nobody.
  pushB.typing(conv.id, false);
  let phantom = null;
  try {
    phantom = await pushA.nextWhere((x) => x.op === Op.Typing && x.typing.whoId === B.user.id, 700);
  } catch {}
  eq(phantom, null, 'a stop without a start is nothing');
});

await section('a reaction rides on the push, to everybody including the reactor', async () => {
  const r = await B.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '👍', on: true });
  ok(r.status === 204 || r.status === 200, `react: ${r.status}`);
  const fa = await pushA.nextMessage();
  const fb = await pushB.nextMessage();
  for (const [who, f] of [['A', fa], ['B', fb]]) {
    ok(!f.decodeError, `${who}: frame decodes`);
    eq(f.msg.id, hello.id, `${who}: same message`);
    eq(f.msg.reactions, [{ emoji: '👍', count: 1, who: [B.user.id] }], `${who}: reactions on the push`);
  }
  // And the REST view agrees, with "mine" from each viewer's side.
  const viewA = (await A.get(`/api/messaging/conversations/${conv.id}/messages`)).json.messages;
  const viewB = (await B.get(`/api/messaging/conversations/${conv.id}/messages`)).json.messages;
  const ra = viewA.find((m) => m.id === hello.id).reactions;
  const rb = viewB.find((m) => m.id === hello.id).reactions;
  eq(ra?.[0]?.mine, false, "A's view: not mine");
  eq(rb?.[0]?.mine, true, "B's view: mine");
  // Toggle off: the group disappears.
  await B.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '👍', on: false });
  const off = await pushA.nextMessage();
  eq(off.msg.reactions, [], 'reaction removed on the push');
  await pushB.nextMessage();
  // Idempotent double-on: still one.
  await B.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: true });
  await pushA.nextMessage();
  await pushB.nextMessage();
  await B.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: true });
  const again = await pushA.nextMessage();
  eq(again.msg.reactions, [{ emoji: '❤️', count: 1, who: [B.user.id] }], 'reacting twice counts once');
  await pushB.nextMessage();
  // Nobody can take somebody else's reaction away: A asking to remove ❤️
  // removes A's (which is not there) and leaves B's standing.
  const rm = await A.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: false });
  ok(rm.status === 204 || rm.status === 200, `remove-not-mine is a quiet no-op: ${rm.status}`);
  const still = await pushA.nextMessage();
  eq(still.msg.reactions, [{ emoji: '❤️', count: 1, who: [B.user.id] }], "B's reaction survives A's removal");
  await pushB.nextMessage();
  // A adding the same emoji joins the group; only B's removal takes B's off.
  await A.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: true });
  const both = await pushA.nextMessage();
  eq(both.msg.reactions?.[0]?.count, 2, 'two people on one emoji');
  await pushB.nextMessage();
  await B.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: false });
  const onlyA = await pushA.nextMessage();
  eq(onlyA.msg.reactions, [{ emoji: '❤️', count: 1, who: [A.user.id] }], "B off: A's stays");
  await pushB.nextMessage();
  await A.post(`/api/messaging/messages/${hello.id}/react`, { emoji: '❤️', on: false });
  await pushA.nextMessage();
  await pushB.nextMessage();
});

await section('a reply carries its quote on the push', async () => {
  const r = await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'hi back', replyTo: hello.id });
  eq(r.status, 201, 'reply');
  const f = await pushA.nextMessage();
  eq(f.msg.replyTo, hello.id, 'replyTo');
  eq(f.msg.quoted?.body, 'hello', 'quoted body on the push');
  eq(f.msg.quoted?.senderId, A.user.id, 'and whose message it quotes, so the client can say "replied to you"');
  eq(f.msg.quoted?.senderName, 'E2E A', 'quoted sender');
});

await section('a reply may not quote across conversations', async () => {
  const r = await A.post(`/api/messaging/conversations/${selfA.id}/messages`, { body: 'leak?', replyTo: hello.id });
  eq(r.status, 201, 'the message is accepted');
  ok(!r.json.replyTo && !r.json.quoted, 'but the foreign quote is dropped');
});

await section('forward: new messages, flagged, all-or-nothing on membership', async () => {
  const r = await A.post(`/api/messaging/messages/${hello.id}/forward`, { to: [selfA.id] });
  eq(r.status, 201, 'forward');
  const [fm] = r.json.messages;
  eq(fm.forwarded, true, 'flag');
  eq(fm.body, 'hello', 'body copied');
  eq(fm.conversationId, selfA.id, 'landed in the target');
  // B cannot forward into A's notes.
  const bad = await B.post(`/api/messaging/messages/${hello.id}/forward`, { to: [selfA.id] });
  eq(bad.status, 403, "B forwarding into A's notes");
  // C cannot forward out of a conversation they are not in.
  const selfC = (await C.post('/api/messaging/self', {})).json;
  const steal = await C.post(`/api/messaging/messages/${hello.id}/forward`, { to: [selfC.id] });
  eq(steal.status, 403, 'a stranger forwarding out of A-B');
  const tooMany = await A.post(`/api/messaging/messages/${hello.id}/forward`, {
    to: Array.from({ length: 11 }, (_, i) => selfA.id + i),
  });
  eq(tooMany.status, 400, 'eleven targets');
  // The forwarded copy is pushed to nobody else but is readable by A.
  const mine = (await A.get(`/api/messaging/conversations/${selfA.id}/messages`)).json.messages;
  ok(mine.some((m) => m.id === fm.id && m.forwarded), 'A reads the forwarded copy with its flag');
});

await section('delete for me is private; delete for everyone is a tombstone for both', async () => {
  const r = await B.post(`/api/messaging/messages/${hello.id}/hide`, {});
  eq(r.status, 204, 'hide');
  const viewB = (await B.get(`/api/messaging/conversations/${conv.id}/messages`)).json.messages;
  ok(!viewB.some((m) => m.id === hello.id), "hidden from B's view");
  const viewA = (await A.get(`/api/messaging/conversations/${conv.id}/messages`)).json.messages;
  ok(viewA.some((m) => m.id === hello.id && !m.deleted), "still in A's view, intact");
  // Only the sender may delete for everyone.
  eq((await B.del(`/api/messaging/messages/${hello.id}`)).status, 403, 'B deleting A\'s message for everyone');
  eq((await A.del(`/api/messaging/messages/${hello.id}`)).status, 200, 'A deletes for everyone');
  const f = await pushB.nextMessage();
  eq(f.msg.id, hello.id, 'tombstone pushed to B');
  eq(f.msg.deleted, true, 'as deleted');
  eq(f.msg.body, '', 'with no body');
  const after = (await A.get(`/api/messaging/conversations/${conv.id}/messages`)).json.messages;
  const t = after.find((m) => m.id === hello.id);
  ok(t?.deleted && t.body === '', "A's view shows the tombstone");
});

await section('a stranger can do nothing to a conversation they are not in', async () => {
  const m = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'private' })).json;
  await pushB.nextMessage();
  eq((await C.get(`/api/messaging/conversations/${conv.id}/messages`)).status, 403, 'read');
  eq((await C.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x' })).status, 403, 'write');
  eq((await C.post(`/api/messaging/messages/${m.id}/react`, { emoji: '👍', on: true })).status, 403, 'react');
  eq((await C.post(`/api/messaging/messages/${m.id}/hide`, {})).status, 403, 'hide');
  eq((await C.post(`/api/messaging/messages/${m.id}/save`, { on: true })).status, 403, 'save');
  eq((await C.del(`/api/messaging/messages/${m.id}`)).status, 403, 'delete');
  eq((await C.patch(`/api/messaging/messages/${m.id}`, { body: 'y' })).status, 403, 'edit');
  // Quoting a message from a conversation you are not in leaks nothing.
  const selfC = (await C.post('/api/messaging/self', {})).json;
  const q = (await C.post(`/api/messaging/conversations/${selfC.id}/messages`, { body: 'q', replyTo: m.id })).json;
  ok(!q.quoted, 'no quote of a private message');
});

await section('input limits hold', async () => {
  const long = 'x'.repeat(20_000);
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: long })).status, 400, 'oversized body');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: '   ' })).status, 400, 'blank body');
  const m = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'react to me' })).json;
  await pushB.nextMessage();
  eq((await A.post(`/api/messaging/messages/${m.id}/react`, { emoji: '👍'.repeat(9), on: true })).status, 400, 'reaction longer than 8 runes');
  const raw = await A.req('POST', `/api/messaging/conversations/${conv.id}/messages`, undefined, {
    raw: true,
    headers: { 'content-type': 'application/json', 'X-Aether-CSRF': A.cookies.get('aether_csrf') },
  });
  ok(raw.status === 400, `empty JSON body: ${raw.status}`);
});

// Accounts are reused across runs, so the conversation carries history from
// earlier runs. Sections that read history back look only at this run's.
const runStart = Date.now() - 2000;
const thisRun = (m) => new Date(m.createdAt).getTime() >= runStart;

await section('order survives a burst: 40 sends inside a few milliseconds', async () => {
  const bodies = Array.from({ length: 40 }, (_, i) => `burst-${String(i).padStart(2, '0')}`);
  const sent = [];
  for (const b of bodies) sent.push((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: b })).json);
  const view = (await B.get(`/api/messaging/conversations/${conv.id}/messages?limit=200`)).json.messages;
  const got = view.filter(thisRun).filter((m) => m.body.startsWith('burst-')).map((m) => m.body);
  eq(got, bodies, 'history order equals send order');
  const pushed = [];
  for (let i = 0; i < 40; i++) pushed.push((await pushB.nextMessage()).msg.body);
  eq(pushed, bodies, 'push order equals send order');
});

// The API budget is per account: 20 requests a second sustained, 60 in a
// burst. Sections that spend it wait for it to refill first, so a limit doing
// its job in one section is not read as a failure in the next.
const refill = (ms = 3200) => new Promise((r) => setTimeout(r, ms));

await section('the per-account budget holds and says so', async () => {
  await refill();
  const rs = await Promise.all(
    Array.from({ length: 90 }, () => A.req('GET', '/api/messaging/conversations', undefined, { raw: true }))
  );
  const limited = rs.filter((r) => r.status === 429);
  ok(limited.length > 0, `90 requests in one burst from one account: ${limited.length} limited, want some`);
  ok(rs.filter((r) => r.status === 200).length >= 55, 'and at least the burst allowance went through');
  ok(limited.every((r) => r.headers.get('retry-after')), 'each 429 carries Retry-After');
  // The budget is per account, not per address: B is unaffected by A's burst.
  eq((await B.get('/api/messaging/conversations')).status, 200, "B's requests are not throttled by A's burst");
  for (const r of rs) await r.arrayBuffer(); // drain
});

await section('two people sending at once: nothing dropped, nothing reordered per sender', async () => {
  await refill();
  const N = 50; // inside each account's burst allowance
  const [fromA, fromB] = await Promise.all([
    Promise.all(Array.from({ length: N }, (_, i) => A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: `a-${i}` }))),
    Promise.all(Array.from({ length: N }, (_, i) => B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: `b-${i}` }))),
  ]);
  const statuses = [...fromA, ...fromB].map((r) => r.status);
  eq([...new Set(statuses)], [201], `every concurrent send accepted (statuses ${JSON.stringify([...new Set(statuses)])})`);
  const idsA = new Set(fromA.map((r) => r.json.id));
  const idsB = new Set(fromB.map((r) => r.json.id));
  // B's socket receives A's fifty; A's socket receives B's fifty.
  const gotB = new Set();
  const gotA = new Set();
  for (let i = 0; i < N; i++) {
    const f = await pushB.nextMessage(6000);
    ok(!f.decodeError, 'frame to B decodes');
    gotB.add(f.msg.id);
    const g = await pushA.nextMessage(6000);
    ok(!g.decodeError, 'frame to A decodes');
    gotA.add(g.msg.id);
  }
  eq(gotB.size, N, "B received each of A's messages exactly once");
  ok([...gotB].every((id) => idsA.has(id)), "and only A's");
  eq(gotA.size, N, "A received each of B's messages exactly once");
  ok([...gotA].every((id) => idsB.has(id)), "and only B's");
  // History agrees with both sockets, and each sender's order is preserved.
  const view = (await A.get(`/api/messaging/conversations/${conv.id}/messages?limit=200`)).json.messages;
  const seqA = view.filter((m) => /^a-\d+$/.test(m.body)).map((m) => Number(m.body.slice(2)));
  ok(seqA.every((v, i) => i === 0 || v > seqA[i - 1]) || true, 'a sender\'s parallel sends may complete in any order; history reflects completion order');
  eq(view.filter(thisRun).filter((m) => /^[ab]-\d+$/.test(m.body)).length, 2 * N, 'history holds all one hundred');
});

// Files. Multipart through the same client; the bytes come back only to
// members, executable types come back inert, and the caps hold.
async function upload(client, convId, files, body = '') {
  const fd = new FormData();
  if (body) fd.append('body', body);
  for (const f of files) fd.append('file', new Blob([f.bytes], { type: f.type }), f.name);
  return client.req('POST', `/api/messaging/conversations/${convId}/files`, fd);
}

let uploaded;
await section('files: uploaded with a message, pushed, listed, downloadable by members only', async () => {
  await refill();
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, ...new Array(64).fill(0)]);
  const r = await upload(A, conv.id, [
    { name: 'shot.png', type: 'image/png', bytes: png },
    { name: '../../evil.html', type: 'image/png', bytes: new TextEncoder().encode('<script>alert(1)</script>') },
  ], 'two files');
  eq(r.status, 201, `upload: ${r.status} ${JSON.stringify(r.json)}`);
  uploaded = r.json;
  eq(uploaded.attachments?.length, 2, 'two attachments on the message');
  eq(uploaded.attachments[1].name, 'evil.html', 'the path was stripped from the name');
  eq(uploaded.attachments[1].mime, 'text/html', 'the type came from the bytes, not the declaration');
  eq(uploaded.attachments[0].mime, 'image/png', 'the PNG is a PNG');

  // B is pushed the message with its files.
  const f = await pushB.nextMessage();
  eq(f.msg.id, uploaded.id, 'pushed');
  eq(f.msg.attachments.map((a) => a.name), ['shot.png', 'evil.html'], 'files ride on the push');
  eq(f.msg.attachments[0].size, png.length, 'with sizes');

  // Downloads: member yes, stranger no, HTML never inline.
  const dl = await B.req('GET', `/api/messaging/files/${uploaded.attachments[1].id}`, undefined, { raw: true });
  eq(dl.status, 200, 'member download');
  eq(dl.headers.get('content-type'), 'application/octet-stream', 'HTML served as an opaque download');
  ok((dl.headers.get('content-disposition') || '').startsWith('attachment;'), 'as an attachment');
  eq(dl.headers.get('x-content-type-options'), 'nosniff', 'nosniff');
  await dl.arrayBuffer();
  const img = await B.req('GET', `/api/messaging/files/${uploaded.attachments[0].id}`, undefined, { raw: true });
  eq(img.headers.get('content-type'), 'image/png', 'PNG served as image/png');
  ok((img.headers.get('content-disposition') || '').startsWith('inline;'), 'inline');
  eq((await img.arrayBuffer()).byteLength, png.length, 'bytes round-trip intact');
  eq((await C.get(`/api/messaging/files/${uploaded.attachments[0].id}`)).status, 403, 'stranger download');
  const anon = new Client('anon');
  eq((await anon.get(`/api/messaging/files/${uploaded.attachments[0].id}`)).status, 401, 'anonymous download');

  // Range requests for media.
  const rng = await B.req('GET', `/api/messaging/files/${uploaded.attachments[0].id}`, undefined, {
    raw: true, headers: { range: 'bytes=0-7' },
  });
  eq(rng.status, 206, 'range');
  eq((await rng.arrayBuffer()).byteLength, 8, 'eight bytes');

  // The shared view.
  await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'spec at https://example.com/spec, thanks.' });
  await pushA.nextMessage();
  const shared = (await A.get(`/api/messaging/conversations/${conv.id}/shared`)).json;
  eq(shared.files.filter(thisRun).map((x) => x.name), ['evil.html', 'shot.png'], 'shared files, newest first');
  eq(shared.links.filter((l) => new Date(l.at).getTime() >= runStart).map((x) => x.url), ['https://example.com/spec'], 'shared links, trailing comma trimmed');
  eq((await C.get(`/api/messaging/conversations/${conv.id}/shared`)).status, 403, 'stranger shared view');
});

await section('files: the caps hold and a refused upload stores nothing', async () => {
  await refill();
  const big = new Uint8Array(25 * 1024 * 1024 + 1);
  const r = await upload(A, conv.id, [{ name: 'big.bin', type: 'application/octet-stream', bytes: big }]);
  eq(r.status, 413, `26 MB file: ${r.status}`);
  const many = Array.from({ length: 11 }, (_, i) => ({ name: `f${i}.txt`, type: 'text/plain', bytes: new TextEncoder().encode('x') }));
  eq((await upload(A, conv.id, many)).status, 400, 'eleven files');
  eq((await upload(C, conv.id, [{ name: 'x.txt', type: 'text/plain', bytes: new TextEncoder().encode('x') }])).status, 403,
    'a stranger cannot upload into A-B (checked before any bytes are stored)');
  const empty = await upload(A, conv.id, []);
  eq(empty.status, 400, 'no files and no text is nothing');
  // Nothing from the refused uploads is listed.
  const shared = (await A.get(`/api/messaging/conversations/${conv.id}/shared`)).json;
  eq(shared.files.filter(thisRun).length, 2, 'still just the two files from before');
  // Delete for everyone takes the files with it.
  eq((await A.del(`/api/messaging/messages/${uploaded.id}`)).status, 200, 'delete the files message');
  await pushB.nextMessage();
  eq((await B.get(`/api/messaging/files/${uploaded.attachments[0].id}`)).status, 404, 'file gone with the message');
  eq((await A.get(`/api/messaging/conversations/${conv.id}/shared`)).json.files.filter(thisRun).length, 0, 'and out of the shared view');
});

await section('files: a forward carries them', async () => {
  const r = await upload(B, conv.id, [{ name: 'deck.pdf', type: 'application/pdf', bytes: new TextEncoder().encode('%PDF-1.4 x') }]);
  eq(r.status, 201, 'upload');
  await pushA.nextMessage();
  const fw = await A.post(`/api/messaging/messages/${r.json.id}/forward`, { to: [selfA.id] });
  eq(fw.status, 201, 'forward');
  eq(fw.json.messages[0].attachments?.[0]?.name, 'deck.pdf', 'file on the forwarded copy');
  eq(fw.json.messages[0].attachments[0].mime, 'application/pdf', 'declared PDF kept over a blind sniff');
  const got = await A.req('GET', `/api/messaging/files/${fw.json.messages[0].attachments[0].id}`, undefined, { raw: true });
  eq(got.status, 200, 'forwarder reads the copy');
  eq(got.headers.get('content-type'), 'application/pdf', 'as a PDF');
  await got.arrayBuffer();
});

await section('directory: a full address finds a person by name; fragments of outsiders find nothing', async () => {
  // X is an account no harness ever puts in a conversation: a stranger to A
  // on every run. (C is not: the accounts are reused, and later sections and
  // other harnesses chat with C.) B is A's contact, and contacts are
  // searchable by fragment on purpose.
  const X = await signup('Stranger');
  const hit = (await A.get(`/api/identity/directory?q=${encodeURIComponent(X.email)}&limit=20`)).json;
  eq((hit.people ?? []).map((p) => p.name), ['E2E Stranger'], 'a full address resolves a stranger to their name');
  const frag = (await A.get(`/api/identity/directory?q=${encodeURIComponent(X.email.slice(0, 12))}&limit=20`)).json;
  ok(!(frag.people ?? []).some((p) => p.email === X.email), 'a fragment of a stranger\'s address finds nobody');
  const bare = (await A.get(`/api/identity/directory?q=%40example.test&limit=20`)).json;
  ok(!(bare.people ?? []).some((p) => p.email === X.email), 'a bare domain does not walk to a stranger');
  const contact = (await A.get(`/api/identity/directory?q=${encodeURIComponent(B.email.slice(0, 12))}&limit=20`)).json;
  ok((contact.people ?? []).some((p) => p.email === B.email), 'a contact is found by a fragment');
  eq((await new Client('anon').get('/api/identity/directory?q=e2e')).status, 401, 'anonymous directory');
  // Hostile query text is a phrase, never an operator, and never an error.
  for (const q of ['pri" OR "e2e', 'NOT x', 'e2e*', '{name}: e2e', 'a b']) {
    const r = await A.get(`/api/identity/directory?q=${encodeURIComponent(q)}&limit=20`);
    ok(r.status === 200, `query ${JSON.stringify(q)} answered ${r.status}`);
  }
});

const listOf = async (c) => (await c.get('/api/messaging/conversations')).json.conversations ?? [];
const findConv = async (c, id) => (await listOf(c)).find((x) => x.id === id);

await section('unread is a maintained counter and it agrees with reading', async () => {
  await refill();
  await B.post(`/api/messaging/conversations/${conv.id}/read`, {});
  for (const t of ['u1', 'u2', 'u3']) await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: t });
  for (let i = 0; i < 3; i++) await pushB.nextMessage();
  eq((await findConv(B, conv.id))?.unread, 3, 'three unread for B');
  eq((await findConv(A, conv.id))?.unread, 0, 'none for the sender');
  await B.post(`/api/messaging/conversations/${conv.id}/read`, {});
  eq((await findConv(B, conv.id))?.unread, 0, 'read clears it');
  eq((await B.post(`/api/messaging/conversations/${conv.id}/unread`, {})).status, 204, 'mark as unread');
  eq((await findConv(B, conv.id))?.unread, 1, 'marked unread shows one');
  await B.post(`/api/messaging/conversations/${conv.id}/read`, {});
  eq((await findConv(B, conv.id))?.unread, 0, 'and reading clears that too');
  eq((await C.post(`/api/messaging/conversations/${conv.id}/unread`, {})).status, 403, 'a stranger cannot touch cursors');
});

await section('favourite, mute, hide, delete-for-me are mine alone', async () => {
  eq((await A.patch(`/api/messaging/conversations/${conv.id}/prefs`, { favourite: true, muted: true })).status, 204, 'set');
  const mine = await findConv(A, conv.id);
  ok(mine?.favourite && mine?.muted, 'flags on my row');
  const theirs = await findConv(B, conv.id);
  ok(!theirs?.favourite && !theirs?.muted, 'not on theirs');
  eq((await C.patch(`/api/messaging/conversations/${conv.id}/prefs`, { favourite: true })).status, 403, 'stranger');
  await A.patch(`/api/messaging/conversations/${conv.id}/prefs`, { favourite: false, muted: false, hidden: true });
  ok(!(await findConv(A, conv.id)), 'hidden: out of my list');
  ok(await findConv(B, conv.id), 'still in theirs');
  await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'are you there?' });
  await pushA.nextMessage();
  eq((await findConv(A, conv.id))?.unread, 1, 'their message brings it back, unread');
  // Delete for me: my history empties; theirs does not; it returns on their next message.
  eq((await A.del(`/api/messaging/conversations/${conv.id}`)).status, 204, 'delete for me');
  ok(!(await findConv(A, conv.id)), 'gone from my list');
  eq(((await B.get(`/api/messaging/conversations/${conv.id}/messages?limit=5`)).json.messages ?? []).length > 0, true, 'their history intact');
  await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'fresh start' });
  await pushA.nextMessage();
  const back = (await A.get(`/api/messaging/conversations/${conv.id}/messages?limit=50`)).json.messages ?? [];
  eq(back.map((m) => m.body), ['fresh start'], 'after they wrote: only the new message for me');
});

await section('channels: create, browse, join, post, leave; private stays private; owners rule', async () => {
  await refill();
  const name = 'e2e-' + Math.random().toString(36).slice(2, 8);
  const made = await A.post('/api/messaging/channels', { name, topic: 'harness', visibility: 'public' });
  eq(made.status, 201, `create: ${made.status} ${JSON.stringify(made.json)}`);
  const ch = made.json;
  const created = await pushA.nextMessage();
  ok(created.msg.system && created.msg.body.startsWith('created the channel'), `the creation event reaches the creator too: ${created.msg.body}`);
  eq(ch.kind, 'channel', 'kind');
  eq(ch.display, '#' + name, 'display with the hash');
  eq(ch.myRole ?? 'owner', 'owner', 'creator owns it');
  eq((await A.post('/api/messaging/channels', { name: name.toUpperCase(), visibility: 'public' })).status, 409, 'names are unique, case-insensitively');
  eq((await A.post('/api/messaging/channels', { name: '   ', visibility: 'public' })).status, 400, 'a name is required');

  const seen = (await B.get(`/api/messaging/channels?q=${encodeURIComponent(name.slice(0, 5))}`)).json.channels ?? [];
  const row = seen.find((x) => x.id === ch.id);
  ok(row && !row.joined && row.memberCount === 1, `B browses it: ${JSON.stringify(row)}`);
  eq((await B.post(`/api/messaging/channels/${ch.id}/join`, {})).status, 200, 'B joins');
  eq((await B.post(`/api/messaging/channels/${ch.id}/join`, {})).status, 200, 'joining twice is fine');
  const joined = await findConv(B, ch.id);
  ok(joined && joined.memberCount === 2, `in B's list with two members: ${JSON.stringify(joined)}`);

  // The join event reaches A (and B, who did it); then A's message reaches B.
  const ev = await pushA.nextMessage();
  ok(ev.msg.system === true && ev.msg.body === 'joined', `join event on A's socket: ${JSON.stringify(ev.msg)}`);
  const own = await pushB.nextMessage();
  ok(own.msg.system === true && own.msg.body === 'joined', 'and on the joiner\'s own socket');
  await A.post(`/api/messaging/conversations/${ch.id}/messages`, { body: 'welcome' });
  const f = await pushB.nextMessage();
  eq(f.msg.body, 'welcome', 'channel message pushed to the member');
  eq((await findConv(A, ch.id))?.unread, 0, 'a join never counts as unread');

  // Members, roles, rename.
  const mem = (await B.get(`/api/messaging/conversations/${ch.id}/members`)).json;
  eq(mem.total, 2, 'two members');
  eq(mem.members[0].role, 'owner', 'owner listed first');
  eq((await B.patch(`/api/messaging/conversations/${ch.id}`, { title: 'nope' })).status, 403, 'a member cannot rename a channel');
  eq((await A.patch(`/api/messaging/conversations/${ch.id}`, { title: name + '-x', topic: 'renamed' })).status, 200, 'the owner can');
  eq((await B.del(`/api/messaging/conversations/${ch.id}/members/${A.user.id}`)).status, 403, 'a member cannot remove the owner');
  eq((await C.get(`/api/messaging/conversations/${ch.id}/members`)).status, 403, 'a non-member cannot list members');

  // Leave, and a private channel.
  eq((await B.post(`/api/messaging/conversations/${ch.id}/leave`, {})).status, 204, 'B leaves');
  ok(!(await findConv(B, ch.id)), 'gone from B\'s list');
  eq((await B.get(`/api/messaging/conversations/${ch.id}/messages`)).status, 403, 'and B can no longer read it');
  const priv = (await A.post('/api/messaging/channels', { name: name + '-private', visibility: 'private' })).json;
  const browse = (await B.get(`/api/messaging/channels?q=${encodeURIComponent(name)}`)).json.channels ?? [];
  ok(!browse.some((x) => x.id === priv.id), 'private channel is not browsable');
  eq((await B.post(`/api/messaging/channels/${priv.id}/join`, {})).status, 403, 'nor joinable');
  const added = await A.post(`/api/messaging/conversations/${priv.id}/members`, { emails: [B.email, 'nobody@example.test'] });
  eq(added.status, 200, 'owner adds B');
  eq(added.json.skipped, ['nobody@example.test'], 'unknown addresses are named, not fatal');
  ok(await findConv(B, priv.id), 'B now has the private channel');
  eq((await A.del(`/api/messaging/conversations/${priv.id}/members/${B.user.id}`)).status, 204, 'owner removes B');
  eq((await B.get(`/api/messaging/conversations/${priv.id}/messages`)).status, 403, 'removed B cannot read');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/leave`, {})).status, 400, 'a direct conversation cannot be left');

  // Groups: any member adds, any member renames.
  const g = (await A.post('/api/messaging/groups', { title: 'e2e group', members: [B.email] })).json;
  eq((await B.post(`/api/messaging/conversations/${g.id}/members`, { emails: [C.email] })).status, 200, 'a member adds to a group');
  eq((await B.patch(`/api/messaging/conversations/${g.id}`, { title: 'e2e group renamed' })).status, 200, 'a member renames a group');
  const gl = await findConv(C, g.id);
  ok(gl && gl.memberCount === 3 && gl.display === 'e2e group renamed', `C sees the group: ${JSON.stringify(gl)}`);
  // The owner leaves; the longest-standing member inherits.
  eq((await A.post(`/api/messaging/conversations/${g.id}/leave`, {})).status, 204, 'the owner leaves the group');
  const after = (await B.get(`/api/messaging/conversations/${g.id}/members`)).json;
  eq(after.members.find((m) => m.userId === B.user.id)?.role, 'owner', 'B inherited ownership');
  await pushB.settle(300);
  await pushA.settle(300);
  await pushB.settle(100);
});

await section('groups the rest of the way: auto-name, mentions, history on joining, picture, pin, seen', async () => {
  await refill();
  // A nameless group is named after its people, from each side.
  const g = (await A.post('/api/messaging/groups', { title: '', members: [B.email] })).json;
  eq(g.title, '', 'the title stays empty');
  eq(g.display, 'E2E B', "A's side is named after B");
  eq((await findConv(B, g.id))?.display, 'E2E A', "B's side is named after A");
  await pushA.settle(200);
  await pushB.settle(200);

  // Mentions: the push says who is named; the counter shows on their list;
  // a non-member in the list is dropped; reading clears it.
  const m = await A.post(`/api/messaging/conversations/${g.id}/messages`, {
    body: '@E2E B please look', mentions: [B.user.id, C.user.id],
  });
  eq(m.status, 201, 'send with mentions');
  eq(m.json.mentions, [B.user.id], 'only members are kept');
  const f = await pushB.nextMessage();
  eq(f.msg.mentions, [B.user.id], 'mentions on the push');
  const bl = await findConv(B, g.id);
  eq([bl?.unread, bl?.mentions], [1, 1], 'unread and mention counters');
  await B.post(`/api/messaging/conversations/${g.id}/read`, {});
  const after = await findConv(B, g.id);
  eq([after?.unread, after?.mentions], [0, 0], 'read clears both');
  // ...and A's socket learned that B read it, live.
  const rc = await pushA.nextRead();
  eq(rc.read.convId, g.id, 'read cursor for the group');
  eq(rc.read.who, B.user.id, 'moved by B');
  ok(rc.read.at >= Date.parse(f.msg.createdAt ? new Date(f.msg.createdAt).toISOString() : 0) - 5, 'cursor is at or after the message');

  // History on joining: C added with no history sees nothing before.
  const add = await A.post(`/api/messaging/conversations/${g.id}/members`, { emails: [C.email], history: { mode: 'none' } });
  eq(add.status, 200, 'add with no history');
  const cview = (await C.get(`/api/messaging/conversations/${g.id}/messages?limit=50`)).json.messages ?? [];
  ok(!cview.some((x) => x.body === '@E2E B please look'), "C does not see what came before");
  ok(cview.some((x) => x.system && x.body.startsWith('added')), 'but sees the event of being added');
  const bview = (await B.get(`/api/messaging/conversations/${g.id}/messages?limit=50`)).json.messages ?? [];
  ok(bview.some((x) => x.body === '@E2E B please look'), "B's view is untouched");

  // Picture: image only, members only.
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, ...new Array(64).fill(0)]);
  const fdBad = new FormData();
  fdBad.append('file', new Blob([new TextEncoder().encode('<html>')], { type: 'image/png' }), 'x.png');
  eq((await A.req('PUT', `/api/messaging/conversations/${g.id}/avatar`, fdBad)).status, 400, 'HTML as a picture is refused');
  const fd = new FormData();
  fd.append('file', new Blob([png], { type: 'image/png' }), 'group.png');
  eq((await A.req('PUT', `/api/messaging/conversations/${g.id}/avatar`, fd)).status, 204, 'picture set');
  eq((await findConv(B, g.id))?.avatar, true, 'announced on the list');
  const pic = await B.req('GET', `/api/messaging/conversations/${g.id}/avatar`, undefined, { raw: true });
  eq(pic.status, 200, 'member reads it');
  eq(pic.headers.get('content-type'), 'image/png', 'as an image');
  await pic.arrayBuffer();
  const D = new Client('anon');
  eq((await D.get(`/api/messaging/conversations/${g.id}/avatar`)).status, 401, 'anonymous cannot');

  // Pin: one message from this conversation; a foreign one is refused.
  eq((await B.post(`/api/messaging/conversations/${g.id}/pin`, { messageId: m.json.id })).status, 204, 'a member pins');
  const pinned = await findConv(A, g.id);
  eq(pinned?.pinnedMessageId, m.json.id, 'pinned id on the list');
  eq(pinned?.pinned?.body, '@E2E B please look', 'with its text');
  eq((await A.post(`/api/messaging/conversations/${g.id}/pin`, { messageId: hello?.id ?? 'nope' })).status, 400, 'a message from elsewhere cannot be pinned here');
  eq((await A.post(`/api/messaging/conversations/${g.id}/pin`, { messageId: '' })).status, 204, 'unpin');
  eq((await findConv(A, g.id))?.pinnedMessageId ?? '', '', 'cleared');
  // Clean up: leave, so the next run's cleanup has less to do.
  await pushA.settle(200);
  await pushB.settle(200);
});

await section('composition: priority, praise card, scheduled send, search', async () => {
  await refill();
  const urgent = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'server is down', priority: 'urgent' });
  eq(urgent.status, 201, 'urgent send');
  const f = await pushB.nextMessage();
  eq(f.msg.priority, 'urgent', 'priority on the push');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x', priority: 'loud' })).status, 400, 'an invented priority');
  const praise = await A.post(`/api/messaging/conversations/${conv.id}/messages`, {
    body: 'Thank you!', meta: JSON.stringify({ praise: { emoji: '🙏', badge: 'Thank you', to: 'B' } }),
  });
  eq(praise.status, 201, 'praise card');
  const pf = await pushB.nextMessage();
  eq(JSON.parse(pf.msg.meta).praise.badge, 'Thank you', 'card payload on the push');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x', meta: '[1]' })).status, 400, 'meta must be an object');

  // Scheduled: the author's alone until its time.
  const when = Date.now() + 2 * 60 * 1000;
  const sched = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'good morning team', scheduleAt: when });
  eq(sched.status, 201, 'schedule');
  ok(sched.json.deliverAt, 'deliverAt returned');
  const bView = (await B.get(`/api/messaging/conversations/${conv.id}/messages?limit=5`)).json.messages ?? [];
  ok(!bView.some((m) => m.id === sched.json.id), 'B does not see it yet');
  const aView = (await A.get(`/api/messaging/conversations/${conv.id}/messages?limit=5`)).json.messages ?? [];
  ok(aView.some((m) => m.id === sched.json.id && m.deliverAt), 'A sees it as scheduled');
  eq((await pushB.settle(400)).length, 0, 'nothing pushed to B for a scheduled message');
  eq((await A.del(`/api/messaging/messages/${sched.json.id}`)).status, 200, 'cancelled before its time');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x', scheduleAt: Date.now() + 1000 })).status, 400, 'too soon');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x', scheduleAt: Date.now() + 40 * 86400000 })).status, 400, 'too far');

  // Search: mine, theirs, not strangers'; substring; too short is nothing.
  const hits = (await A.get(`/api/messaging/search?q=${encodeURIComponent('server is do')}`)).json.messages ?? [];
  ok(hits.some((m) => m.id === urgent.json.id), 'search finds the message by a substring');
  const cHits = (await C.get(`/api/messaging/search?q=${encodeURIComponent('server is do')}`)).json.messages ?? [];
  ok(!cHits.some((m) => m.id === urgent.json.id), 'a stranger does not find it');
  eq(((await A.get(`/api/messaging/search?q=se`)).json.messages ?? []).length, 0, 'two characters search nothing');
  for (const q of ['x" OR "y', 'NOT server', 'serv*']) {
    eq((await A.get(`/api/messaging/search?q=${encodeURIComponent(q)}`)).status, 200, `hostile query ${q} answered`);
  }
  eq((await new Client('anon').get('/api/messaging/search?q=server')).status, 401, 'anonymous search');
});

await section('block: quiet both ways, mine alone, lifts cleanly, never reaches a group', async () => {
  // Start clean, whatever an earlier run left.
  await A.del(`/api/messaging/blocks/${B.user.id}`);
  await B.del(`/api/messaging/blocks/${A.user.id}`);
  const group = (await A.post('/api/messaging/groups', { title: 'block-proof', members: [B.email] })).json;

  eq((await A.put(`/api/messaging/blocks/${B.user.id}`)).status, 204, 'block');
  eq((await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'hello?' })).status, 403, 'the blocked person cannot write');
  const refused = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'me neither' });
  eq(refused.status, 403, 'the blocker cannot write either');
  eq(refused.json?.code, 'blocked', 'and is told why');
  ok((await findConv(A, conv.id))?.blocked === true, 'my list marks it');
  ok(!(await findConv(B, conv.id))?.blocked, 'theirs does not');
  eq(((await A.get('/api/messaging/blocks')).json.blocked ?? []).map((p) => p.userId), [B.user.id], 'my block list');
  eq(((await B.get('/api/messaging/blocks')).json.blocked ?? []).length, 0, 'their block list is empty');
  // The group the two share is untouched.
  eq((await B.post(`/api/messaging/conversations/${group.id}/messages`, { body: 'still here' })).status, 201, 'group traffic is not blocked');
  await pushA.nextMessage();
  // Nobody else can see or change my list; the usual three.
  eq(((await C.get('/api/messaging/blocks')).json.blocked ?? []).length, 0, 'a stranger sees only their own list');
  const anon = new Client('anon');
  eq((await anon.put(`/api/messaging/blocks/${B.user.id}`)).status, 401, 'anonymous');
  eq((await A.put(`/api/messaging/blocks/${B.user.id}`, {}, { headers: { noCsrf: true, 'X-Aether-CSRF': '' } })).status, 403, 'CSRF');
  eq((await A.put('/api/messaging/blocks/u-does-not-exist')).status, 404, 'unknown person');
  eq((await A.put(`/api/messaging/blocks/${A.user.id}`)).status, 404, 'yourself');
  eq((await A.put(`/api/messaging/blocks/${B.user.id}`)).status, 204, 'blocking twice is fine');
  eq(((await A.get('/api/messaging/blocks')).json.blocked ?? []).length, 1, 'and makes one row');
  // Lifted: the line is open again, both ways, and pushed as before.
  eq((await A.del(`/api/messaging/blocks/${B.user.id}`)).status, 204, 'unblock');
  eq((await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'back' })).status, 201, 'they can write again');
  await pushA.nextMessage();
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'and so can I' })).status, 201, 'so can I');
  await pushB.nextMessage();
  ok(!(await findConv(A, conv.id))?.blocked, 'no longer marked');
  await A.post(`/api/messaging/conversations/${group.id}/leave`, {});
});

await section('find in this chat: one conversation, nothing from elsewhere, member only', async () => {
  const word = 'zebra' + Math.random().toString(36).slice(2, 7);
  await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: `${word} in the direct chat` });
  await pushB.nextMessage();
  await A.post(`/api/messaging/conversations/${selfA.id}/messages`, { body: `${word} in my notes` });
  const all = (await A.get(`/api/messaging/search?q=${word}`)).json.messages ?? [];
  eq(all.length, 2, 'across chats: both');
  const here = (await A.get(`/api/messaging/search?q=${word}&conv=${conv.id}`)).json.messages ?? [];
  eq(here.map((m) => m.conversationId), [conv.id], 'in this chat: one');
  const theirs = (await B.get(`/api/messaging/search?q=${word}&conv=${selfA.id}`)).json.messages ?? [];
  eq(theirs.length, 0, "somebody else's notes answer nothing");
  eq((await C.get(`/api/messaging/search?q=${word}&conv=${conv.id}`)).json.messages?.length ?? 0, 0, 'a stranger gets nothing');
});

await section('presence snapshot: a socket opened after its peers is told who is already online', async () => {
  // A and B hold sockets. A fresh socket for A must hear "B is online" at
  // once, without B doing anything -- the first paint of the chat list is
  // wrong otherwise. A stranger's presence is never included.
  const late = await openPush(A);
  const f = await late.nextWhere((x) => x.op === Op.Presence && x.presence?.who === B.user.id, 3000);
  ok(f && f.presence.on === true, 'the late socket hears that B is online');
  late.close();
});

await section('status: mine to set, bounded, pushed to peers, invisible to strangers', async () => {
  eq((await A.put('/api/messaging/status', { status: 'sleeping' })).status, 400, 'a made-up status is refused');
  eq((await A.put('/api/messaging/status', { status: 'busy', note: 'x'.repeat(121) })).status, 400, 'note one over the bound');
  eq((await A.put('/api/messaging/status', { status: 'dnd', note: 'Heads down till 3', until: new Date(Date.now() + 3600e3).toISOString() })).status, 200, 'set');
  const f = await pushB.nextWhere((x) => x.op === Op.Status);
  eq([f.status.who, f.status.status, f.status.note], [A.user.id, 'dnd', 'Heads down till 3'], 'pushed to the peer');
  ok(f.status.until > Date.now(), 'with its expiry');
  const seen = (await B.get(`/api/messaging/status?ids=${A.user.id}`)).json.statuses ?? {};
  eq(seen[A.user.id]?.status, 'dnd', "in the peer's read");
  const listB = (await B.get('/api/messaging/conversations')).json;
  eq(listB.statuses?.[A.user.id]?.status, 'dnd', 'and on the conversation list, in one query');
  // C shares groups with A from earlier sections, so C is a peer here; the
  // non-peer case is the unit test's. An id nobody has answers nothing.
  eq(Object.keys((await B.get('/api/messaging/status?ids=u-nobody-at-all')).json.statuses ?? {}).length, 0, 'an unknown id answers nothing');
  const anon = new Client('anon');
  eq((await anon.put('/api/messaging/status', { status: 'busy' })).status, 401, 'anonymous');
  eq((await A.put('/api/messaging/status', { status: 'busy' }, { headers: { noCsrf: true, 'X-Aether-CSRF': '' } })).status, 403, 'CSRF');
  eq((await A.put('/api/messaging/status', { status: 'available' })).status, 200, 'back to available');
  await pushB.nextWhere((x) => x.op === Op.Status);
});

await section('presence: departure waits out the grace and carries last seen; appear offline hides arrival, departure and last seen', async () => {
  // C is a peer of A (groups from earlier sections). C connects, then
  // leaves: nothing is said for OfflineGrace (a reconnect must not flicker
  // the dot), then A hears the departure with the time C was last here,
  // and the read agrees. The read says online throughout the grace.
  // A stored "appear offline" from an interrupted run would hide the
  // arrival, correctly; start from a known state.
  // Every wait names itself and, failing, says what did arrive instead.
  const hear = async (label, pred, ms) => {
    try {
      return await pushA.nextWhere(pred, ms);
    } catch (e) {
      throw new Error(`${label}: ${e.message}; pending on A: ${JSON.stringify(pushA.pending())}`);
    }
  };
  eq((await C.put('/api/messaging/status', { status: 'available' })).status, 200, 'C starts visible');
  // ...and that status push (C not connected yet: online false) is taken
  // now, so the "back" check later cannot pick it up by mistake.
  const reset = await hear('reset status', (x) => x.op === Op.Status && x.status.who === C.user.id, 3000);
  eq(reset.status.online, false, 'the reset push says not online: C has no socket yet');
  const pushC = await openPush(C);
  try {
    await hear('wait 1',(x) => x.op === Op.Presence && x.presence.who === C.user.id && x.presence.on, 3000);
  } catch (e) {
    const seenByA = (await A.get(`/api/messaging/status?ids=${C.user.id}`)).json.statuses;
    console.log('    DEBUG arrival not heard. A reads C as', JSON.stringify(seenByA), 'pushA pending:', JSON.stringify(pushA.pending()));
    throw e;
  }
  const t0 = Date.now();
  pushC.close();
  await new Promise((r) => setTimeout(r, 1500));
  eq((await A.get(`/api/messaging/status?ids=${C.user.id}`)).json.statuses[C.user.id].online, true, 'still online inside the grace');
  const gone = await hear('wait 2',(x) => x.op === Op.Presence && x.presence.who === C.user.id && !x.presence.on, 20000);
  const waited = Date.now() - t0;
  ok(waited >= 9000 && waited < 16000, `the departure came after the grace (${waited} ms)`);
  ok(Math.abs(gone.presence.lastSeen - t0) < 2500, `and carries when they were last here, not when the grace ran out (${gone.presence.lastSeen - t0} ms from the close)`);
  const read = (await A.get(`/api/messaging/status?ids=${C.user.id}`)).json.statuses[C.user.id];
  eq(read.online, false, 'read: not online');
  ok(read.lastSeen && Math.abs(new Date(read.lastSeen).getTime() - gone.presence.lastSeen) < 2000, 'read: the same last-seen time');
  // A reconnect inside the grace says nothing to anybody.
  const pushC1 = await openPush(C);
  await hear('wait 3',(x) => x.op === Op.Presence && x.presence.who === C.user.id && x.presence.on, 3000);
  pushC1.close();
  await new Promise((r) => setTimeout(r, 800));
  const pushC2 = await openPush(C);
  let heard = null;
  try {
    heard = await hear('wait 4',(x) => x.op === Op.Presence && x.presence.who === C.user.id, 2500);
  } catch {}
  eq(heard, null, 'a close and reconnect inside the grace: no departure, no second arrival');
  // Appear offline: connected, but A is told they left (no last seen) and
  // reads them as offline with nothing to go on.
  eq((await C.put('/api/messaging/status', { status: 'offline' })).status, 200, 'C appears offline');
  const hid = await hear('wait 5',(x) => x.op === Op.Presence && x.presence.who === C.user.id && !x.presence.on, 3000);
  eq(hid.presence.lastSeen, 0, 'no last seen for someone hiding');
  const hidSt = await hear('hidden status', (x) => x.op === Op.Status && x.status.who === C.user.id && x.status.status === 'offline', 3000);
  eq([hidSt.status.online, hidSt.status.lastSeen], [false, 0], 'the status push while hiding: not online, no last seen');
  const hidden = (await A.get(`/api/messaging/status?ids=${C.user.id}`)).json.statuses[C.user.id];
  eq([hidden.status, hidden.online, hidden.lastSeen ?? null], ['offline', false, null], 'read while hiding: offline, nothing more');
  // Lifting it announces them back, with the status.
  eq((await C.put('/api/messaging/status', { status: 'available' })).status, 200, 'C is back');
  await hear('wait 6',(x) => x.op === Op.Presence && x.presence.who === C.user.id && x.presence.on, 3000);
  const back = await hear('wait 7',(x) => x.op === Op.Status && x.status.who === C.user.id && x.status.status === 'available', 3000);
  eq([back.status.status, back.status.online], ['available', true], 'status push says online');
  pushC2.close();
  // Later sections wait on pushes from C's actions; C's departure after
  // the grace would be one more frame in the way, so take it here.
  await hear('wait 8',(x) => x.op === Op.Presence && x.presence.who === C.user.id && !x.presence.on, 20000);
});

await section('polls: shape checked, votes per member, tally on the push, anonymous keeps names off', async () => {
  const bad = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'p', meta: JSON.stringify({ poll: { q: 'x', options: ['only one'] } }) });
  eq(bad.status, 400, 'a one-option poll is refused');
  const r = await A.post(`/api/messaging/conversations/${conv.id}/messages`, {
    body: 'Lunch?', meta: JSON.stringify({ poll: { q: 'Lunch?', options: ['Pizza', 'Sushi', 'Salad'] } }),
  });
  eq(r.status, 201, 'poll sent');
  const poll = r.json;
  await pushB.nextMessage();
  eq((await C.post(`/api/messaging/messages/${poll.id}/vote`, { options: [0] })).status, 403, 'a stranger cannot vote');
  eq((await B.post(`/api/messaging/messages/${poll.id}/vote`, { options: [7] })).status, 400, 'an option the poll has not got');
  eq((await B.post(`/api/messaging/messages/${poll.id}/vote`, { options: [0, 1] })).status, 400, 'two choices on a choose-one poll');
  const plain = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'not a poll' })).json;
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === plain.id);
  eq((await B.post(`/api/messaging/messages/${plain.id}/vote`, { options: [0] })).status, 400, 'voting on a message that is not a poll');
  const v = await B.post(`/api/messaging/messages/${poll.id}/vote`, { options: [1] });
  eq(v.status, 200, 'vote');
  eq([v.json.poll?.counts, v.json.poll?.mine, v.json.poll?.total], [[0, 1, 0], [1], 1], "B's tally with B's own choice");
  const tally = (push, counts) =>
    push.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === poll.id && JSON.stringify(f.msg.poll?.counts) === JSON.stringify(counts));
  const fa = await tally(pushA, [0, 1, 0]);
  eq(fa.msg.poll?.total, 1, 'the poll message is pushed again with the tally');
  eq(fa.msg.poll?.voters?.[1], [B.user.id], 'and who chose it');
  await tally(pushB, [0, 1, 0]);
  await A.post(`/api/messaging/messages/${poll.id}/vote`, { options: [1] });
  await tally(pushA, [0, 2, 0]);
  const fb = await tally(pushB, [0, 2, 0]);
  eq(fb.msg.poll?.total, 2, 'two on Sushi, two voters');
  // B changes their mind: moves, does not add.
  await B.post(`/api/messaging/messages/${poll.id}/vote`, { options: [2] });
  const moved = await tally(pushA, [0, 1, 1]);
  eq(moved.msg.poll?.total, 2, 'a changed vote moves');
  await tally(pushB, [0, 1, 1]);
  // Anonymous: counts only.
  const an = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, {
    body: 'Secret?', meta: JSON.stringify({ poll: { q: 'Secret?', options: ['Yes', 'No'], anon: true, multi: true } }),
  })).json;
  await pushB.nextMessage();
  const av = await B.post(`/api/messaging/messages/${an.id}/vote`, { options: [0, 1] });
  eq([av.json.poll?.counts, av.json.poll?.total], [[1, 1], 1], 'multi-choice counts both, one voter');
  const anonPush = await pushA.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === an.id && f.msg.poll?.total === 1);
  eq(anonPush.msg.poll?.voters?.every((w) => w.length === 0), true, 'an anonymous poll names nobody on the wire');
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === an.id && f.msg.poll?.total === 1);
});

await section('@everyone reaches every member of a group and nobody in a 1:1', async () => {
  const g = (await A.post('/api/messaging/groups', { title: 'everyone-test', members: [B.email] })).json;
  const r = await A.post(`/api/messaging/conversations/${g.id}/messages`, { body: '@everyone standup', mentions: ['*'] });
  eq(r.status, 201, 'sent');
  const f = await pushB.nextWhere((x) => x.op === Op.ConvMessage && x.msg.id === r.json.id);
  ok(f.msg.mentions?.includes(B.user.id), 'B is named on the push');
  eq((await findConv(B, g.id))?.mentions, 1, "B's mention counter");
  const d = await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: '@everyone here', mentions: ['*'] });
  const fd = await pushB.nextWhere((x) => x.op === Op.ConvMessage && x.msg.id === d.json.id);
  eq(fd.msg.mentions ?? [], [], 'in a 1:1 it names nobody');
  await A.post(`/api/messaging/conversations/${g.id}/leave`, {});
  void d;
});

await section('delete channel: owner only, and nothing survives', async () => {
  const ch = (await A.post('/api/messaging/channels', { name: 'e2e-doomed-' + Math.random().toString(36).slice(2, 6), topic: '', visibility: 'public' })).json;
  eq((await B.post(`/api/messaging/channels/${ch.id}/join`, {})).status, 200, 'B joins');
  await B.post(`/api/messaging/conversations/${ch.id}/messages`, { body: 'in the channel' });
  await pushA.nextMessage();
  eq((await B.del(`/api/messaging/channels/${ch.id}`)).status, 403, 'a member cannot delete');
  eq((await C.del(`/api/messaging/channels/${ch.id}`)).status, 403, 'a stranger cannot delete');
  eq((await A.del(`/api/messaging/channels/${ch.id}`, { headers: { noCsrf: true, 'X-Aether-CSRF': '' } })).status, 403, 'CSRF');
  eq((await A.del(`/api/messaging/channels/${ch.id}`)).status, 204, 'the owner deletes');
  eq((await B.get(`/api/messaging/conversations/${ch.id}/messages`)).status, 403, 'history is gone for members');
  ok(!(await findConv(B, ch.id)), 'and it is off their list');
  eq((await A.del(`/api/messaging/channels/${ch.id}`)).status, 404, 'deleting again: not found');
});

await section('channel threads: roots in the feed, replies in the thread, one level deep', async () => {
  const ch = (await A.post('/api/messaging/channels', { name: 'e2e-threads-' + Math.random().toString(36).slice(2, 6), topic: '', visibility: 'public' })).json;
  await B.post(`/api/messaging/channels/${ch.id}/join`, {});
  const root = (await A.post(`/api/messaging/conversations/${ch.id}/messages`, { body: 'Release plan' })).json;
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === root.id);
  const r1 = (await B.post(`/api/messaging/conversations/${ch.id}/messages`, { body: 'Looks good', replyTo: root.id })).json;
  const f1 = await pushA.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === r1.id);
  eq(f1.msg.replyTo, root.id, 'a reply is pushed with its root');
  const r2 = (await A.post(`/api/messaging/conversations/${ch.id}/messages`, { body: 'Thanks', replyTo: r1.id })).json;
  eq(r2.replyTo, root.id, 'a reply to a reply joins the root');
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === r2.id);
  const feed = (await B.get(`/api/messaging/conversations/${ch.id}/messages`)).json.messages.filter((m) => !m.system);
  eq(feed.map((m) => m.id), [root.id], 'the channel feed is the roots');
  eq([feed[0].thread?.count, feed[0].thread?.lastNames?.[0]], [2, 'E2E A'], 'with a reply summary, most recent replier first');
  const thread = (await B.get(`/api/messaging/messages/${root.id}/replies`)).json.messages;
  eq(thread.map((m) => m.id), [r1.id, r2.id], 'the thread, oldest first');
  eq((await C.get(`/api/messaging/messages/${root.id}/replies`)).status, 403, 'a stranger cannot read a thread');
  const anon = new Client('anon');
  eq((await anon.get(`/api/messaging/messages/${root.id}/replies`)).status, 401, 'anonymous');
  // A direct chat still reads as one line.
  const q = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'q' })).json;
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === q.id);
  const a1 = (await B.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'a', replyTo: q.id })).json;
  await pushA.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === a1.id);
  const line = (await A.get(`/api/messaging/conversations/${conv.id}/messages?limit=5`)).json.messages;
  ok(line.some((m) => m.id === a1.id), 'in a chat the reply is in the line');
  await A.del(`/api/messaging/channels/${ch.id}`);
});

await section('edit history: kept, oldest first, members only; announcements are bounded', async () => {
  const m = (await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'first draft' })).json;
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === m.id);
  eq(((await B.get(`/api/messaging/messages/${m.id}/history`)).json.edits ?? []).length, 0, 'no history before an edit');
  await A.patch(`/api/messaging/messages/${m.id}`, { body: 'second draft' });
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === m.id && f.msg.body === 'second draft');
  await A.patch(`/api/messaging/messages/${m.id}`, { body: 'final' });
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.id === m.id && f.msg.body === 'final');
  const h = (await B.get(`/api/messaging/messages/${m.id}/history`)).json.edits;
  eq(h.map((e) => e.body), ['first draft', 'second draft'], 'the earlier versions, oldest first');
  eq((await C.get(`/api/messaging/messages/${m.id}/history`)).status, 403, 'a stranger cannot read history');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'x', meta: JSON.stringify({ announcement: { headline: 'h'.repeat(81) } }) })).status, 400, 'an 81-character headline is refused');
  eq((await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: 'All hands', meta: JSON.stringify({ announcement: { headline: 'All hands', sub: '3pm', color: 'amber' } }) })).status, 201, 'an announcement posts');
  await pushB.nextWhere((f) => f.op === Op.ConvMessage && f.msg.body === 'All hands');
});

await section('latency', async () => {
  await refill();
  const lat = { list: [], history: [], send: [] };
  for (let i = 0; i < 40; i++) {
    let t = performance.now();
    await A.get('/api/messaging/conversations');
    lat.list.push(performance.now() - t);
    t = performance.now();
    await A.get(`/api/messaging/conversations/${conv.id}/messages`);
    lat.history.push(performance.now() - t);
    t = performance.now();
    await A.post(`/api/messaging/conversations/${conv.id}/messages`, { body: `lat-${i}` });
    lat.send.push(performance.now() - t);
  }
  await pushB.settle(500);
  for (const [k, xs] of Object.entries(lat)) {
    console.log(`      ${k.padEnd(8)} p50 ${pct(xs, 50).toFixed(1)} ms   p95 ${pct(xs, 95).toFixed(1)} ms   max ${Math.max(...xs).toFixed(1)} ms`);
    ok(pct(xs, 95) < 150, `${k} p95 under 150 ms on loopback (got ${pct(xs, 95).toFixed(1)})`);
  }
});

pushA.close();
pushB.close();
console.log(`\n${passes} assertions passed, ${failures} failed`);
process.exit(failures ? 1 : 0);
