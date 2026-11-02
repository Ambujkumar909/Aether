// Bit-level measurement of the new push frames (CLAUDE.md rule 1).
//
// Against a PLAINTEXT server on 127.0.0.1:8444 (so tshark can see the
// WebSocket frames), two fresh accounts: B holds a push socket open while A
// sets a status, posts a poll, B votes, and A sends a plain message. tshark
// captures the loopback adapter in parallel; the frame lengths it reports are
// the measurement, and this script prints the JSON sizes of the same events
// as the honest baseline.
//
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w capture.pcapng -a duration:20 &
//   node tools/e2e/measure.mjs
//   tshark -r capture.pcapng -Y "websocket && tcp.srcport==8444" -T fields -e frame.time_relative -e frame.len -e ip.hdr_len -e tcp.hdr_len -e websocket.payload.length

import { connect } from 'node:net';
import { randomBytes } from 'node:crypto';
import { Writer, Op, newDictPair } from '../../web/src/wire.js';

const BASE = 'http://127.0.0.1:8444';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

class Client {
  constructor() {
    this.cookies = new Map();
  }
  cookieHeader() {
    return [...this.cookies].map(([k, v]) => `${k}=${v}`).join('; ');
  }
  async req(method, path, body) {
    const h = {};
    if (this.cookies.size) h.cookie = this.cookieHeader();
    if (body !== undefined) h['content-type'] = 'application/json';
    const csrf = this.cookies.get('aether_csrf');
    if (csrf && method !== 'GET') h['X-Aether-CSRF'] = csrf;
    const res = await fetch(BASE + path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [pair] = sc.split(';');
      const i = pair.indexOf('=');
      this.cookies.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
    }
    const text = await res.text();
    return { status: res.status, text, json: text ? JSON.parse(text) : null };
  }
}

async function signup(label) {
  const c = new Client();
  const r = await c.req('POST', '/api/identity/signup', {
    email: `m-${label}-${randomBytes(3).toString('hex')}@example.test`,
    displayName: `Measure ${label}`,
    password: 'measure-' + randomBytes(8).toString('base64url'),
  });
  if (r.status !== 201) throw new Error(`signup ${label}: ${r.status} ${r.text}`);
  c.user = r.json.user;
  c.email = r.json.user.email;
  return c;
}

function openPush(client) {
  return new Promise((resolve, reject) => {
    const key = randomBytes(16).toString('base64');
    const sock = connect({ host: '127.0.0.1', port: 8444 }, () => {
      sock.write(
        `GET /ws HTTP/1.1\r\nHost: 127.0.0.1:8444\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
          `Sec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\nOrigin: ${BASE}\r\nCookie: ${client.cookieHeader()}\r\n\r\n`
      );
    });
    let up = false;
    sock.on('data', (d) => {
      if (!up) {
        up = true;
        if (!/^HTTP\/1\.1 101/.test(d.toString())) reject(new Error('upgrade refused: ' + d.toString().split('\r\n')[0]));
        else resolve(sock);
      }
    });
    sock.on('error', reject);
  });
}

const A = await signup('A');
const B = await signup('B');
const conv = (await A.req('POST', '/api/messaging/direct', { email: B.email })).json;
const push = await openPush(B);
await sleep(600);

const sizes = {};
const mark = async (name, fn) => {
  console.log(`event ${name} at +${(performance.now() / 1000).toFixed(2)}s`);
  const r = await fn();
  sizes[name] = r?.text ? Buffer.byteLength(r.text) : 0;
  await sleep(1200); // a quiet gap so the frames are attributable in the capture
  return r;
};

await mark('plain', () => A.req('POST', `/api/messaging/conversations/${conv.id}/messages`, { body: 'hello there' }));
await mark('status', () => A.req('PUT', '/api/messaging/status', { status: 'dnd', note: 'Heads down till 3' }));
const poll = await mark('poll', () =>
  A.req('POST', `/api/messaging/conversations/${conv.id}/messages`, {
    body: 'Lunch?', meta: JSON.stringify({ poll: { q: 'Lunch?', options: ['Pizza', 'Sushi', 'Salad'] } }),
  })
);
await mark('vote', () => B.req('POST', `/api/messaging/messages/${poll.json.id}/vote`, { options: [1] }));
// A connects while B is online: the connect-time presence snapshot, one
// frame per online peer, is the only server->client frame on A's new socket.
const pushA = await mark('presence', async () => openPush(A));
// Typing: A says "typing" then "stopped" on its socket (client->server, masked
// like a browser's), and B is pushed each once. Four frames in the capture.
const txA = newDictPair().tx;
const masked = (payload) => {
  const mask = randomBytes(4);
  const out = Buffer.alloc(payload.length);
  for (let i = 0; i < payload.length; i++) out[i] = payload[i] ^ mask[i & 3];
  return Buffer.concat([Buffer.from([0x82, 0x80 | payload.length]), mask, out]);
};
const typingFrame = (on) => {
  const w = new Writer(txA);
  w.header(Op.Typing);
  w.str(conv.id);
  w.bool(on);
  w.bits(0, 7);
  return masked(Buffer.from(w.bytes()));
};
await mark('typing-on', async () => {
  pushA.write(typingFrame(true));
  return null;
});
await mark('typing-off', async () => {
  pushA.write(typingFrame(false));
  return null;
});
await sleep(500);
push.destroy();
console.log('JSON baseline bytes (the REST body carrying the same event):', JSON.stringify(sizes));
