// Bit-level measurement of the activity feed's pushes (CLAUDE.md rule 1).
// A sits on a raw chat socket against a PLAINTEXT server so tshark reads every
// WebSocket frame; B mentions A, reacts, takes the reaction back; A marks all
// read. Each event is followed by a quiet gap and printed with its time.
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -relay=false -db <scratch>.db -web ../web
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w act.pcapng -a duration:20 &
//   node tools/e2e/measure-activity.mjs
//   tshark -r act.pcapng -Y "websocket && tcp.srcport == 8444" -T fields -e frame.time_relative \
//     -e tcp.dstport -e frame.len -e tcp.len -e websocket.payload_length
import { connect } from 'node:net';
import { randomBytes } from 'node:crypto';
import { Reader, Op, newDictPair } from '../../web/src/wire.js';

const HOST = '127.0.0.1', PORT = 8444, BASE = `http://${HOST}:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;
const csrfOf = (cookie) => (cookie.split('; ').find((c) => c.startsWith('aether_csrf=')) ?? '').split('=')[1];

async function signup(label) {
  const email = `ma-${label}-${randomBytes(3).toString('hex')}@example.test`;
  const res = await fetch(BASE + '/api/identity/signup', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email, displayName: `Act ${label}`, password: 'measure-' + randomBytes(8).toString('base64url') }),
  });
  if (res.status !== 201) throw new Error(`signup ${label}: ${res.status}`);
  const j = await res.json();
  return { email, id: j.user.id, cookie: (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ') };
}
const call = async (who, method, path, body) => {
  const r = await fetch(BASE + path, { method, headers: { cookie: who.cookie, 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(who.cookie) }, body: body ? JSON.stringify(body) : undefined });
  if (r.status >= 300) throw new Error(`${method} ${path}: ${r.status}`);
  const t = await r.text();
  return t ? JSON.parse(t) : null;
};

// A's chat socket: reads server frames, counts the activity ones.
function chatSocket(who) {
  return new Promise((resolve, reject) => {
    const dicts = newDictPair();
    const seen = [];
    let buf = Buffer.alloc(0), up = false;
    const sock = connect({ host: HOST, port: PORT }, () => sock.write(
      `GET /ws?d=measure HTTP/1.1\r\nHost: ${HOST}:${PORT}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
      `Sec-WebSocket-Key: ${randomBytes(16).toString('base64')}\r\nSec-WebSocket-Version: 13\r\nOrigin: ${BASE}\r\nCookie: ${who.cookie}\r\n\r\n`));
    sock.on('error', () => {});
    sock.on('data', (d) => {
      buf = Buffer.concat([buf, d]);
      if (!up) {
        const end = buf.indexOf('\r\n\r\n');
        if (end < 0) return;
        if (!/^HTTP\/1\.1 101/.test(buf.toString('latin1', 0, end))) return reject(new Error('upgrade refused'));
        up = true;
        buf = buf.subarray(end + 4);
        resolve({ sock, seen, port: sock.localPort });
      }
      for (;;) {
        if (buf.length < 2) return;
        let len = buf[1] & 0x7f, off = 2;
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4; }
        if (buf.length < off + len) return;
        const payload = new Uint8Array(buf.subarray(off, off + len));
        buf = buf.subarray(off + len);
        const r = new Reader(payload, dicts.rx);
        const h = r.header();
        if (h) seen.push({ at: at(), op: h.op.toString(16), bytes: payload.length });
      }
    });
  });
}

const A = await signup('a');
const B = await signup('b');
const s = await chatSocket(A);
console.log(`A's socket is local port ${s.port}`);
await sleep(1500);

const g = await call(B, 'POST', '/api/messaging/groups', { title: 'Measure', members: [A.email] });
await sleep(1500);
const mine = await call(A, 'POST', `/api/messaging/conversations/${g.id}/messages`, { body: 'Draft is in the shared folder' });
await sleep(1500);

console.log(`${at()} B mentions A`);
await call(B, 'POST', `/api/messaging/conversations/${g.id}/messages`, { body: '@Act A could you take the Q3 section?', mentions: [A.id] });
await sleep(2000);
console.log(`${at()} B reacts to A's message`);
await call(B, 'POST', `/api/messaging/messages/${mine.id}/react`, { emoji: '👍', on: true });
await sleep(2000);
console.log(`${at()} B takes it back`);
await call(B, 'POST', `/api/messaging/messages/${mine.id}/react`, { emoji: '👍', on: false });
await sleep(2000);
console.log(`${at()} A marks all read (from another device)`);
await call(A, 'POST', '/api/messaging/activity/read', { all: true });
await sleep(2000);
console.log(`${at()} idle 4 s`);
const before = s.seen.length;
await sleep(4000);
console.log(`${at()} frames while idle: ${s.seen.length - before}`);
console.log(JSON.stringify(s.seen.filter((x) => x.op !== '0'), null, 0));
s.sock.destroy();
process.exit(0);
