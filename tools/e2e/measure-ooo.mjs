// Bit-level measurement of out of office on the status push (CLAUDE.md rule 1).
// B sits on a raw chat socket against a PLAINTEXT server so tshark reads every
// WebSocket frame; A, who shares a direct chat with B, sets a status, turns
// out of office on, changes status while away, sits idle, and turns it off.
// Each event is followed by a quiet gap and printed with its time, beside
// the size the same push would be as JSON.
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -relay=false -db <scratch>.db -web ../web
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w ooo.pcapng -a duration:24 &
//   node tools/e2e/measure-ooo.mjs
//   tshark -r ooo.pcapng -Y "websocket && tcp.srcport == 8444" -T fields -e frame.time_relative \
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
  const email = `mo-${label}-${randomBytes(3).toString('hex')}@example.test`;
  const res = await fetch(BASE + '/api/identity/signup', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email, displayName: `Ooo ${label}`, password: 'measure-' + randomBytes(8).toString('base64url') }),
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

// B's chat socket: reads server frames and keeps the size of each.
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
await call(A, 'POST', '/api/messaging/direct', { email: B.email });
const s = await chatSocket(B);
console.log(`B's socket is local port ${s.port}`);
await sleep(1500);

const MESSAGE = 'Back on Monday. Asha is covering the release.';
const until = new Date(Date.now() + 3 * 24 * 3600e3);
const json = (o) => Buffer.byteLength(JSON.stringify(o));
// The status as the REST read returns it: what a JSON push would carry.
const status = (st, ooo) => ({
  userId: A.id,
  status: st,
  updatedAt: new Date().toISOString(),
  online: true,
  ...(ooo ? { ooo: { message: MESSAGE, until: until.toISOString() } } : {}),
});

console.log(`${at()} A sets busy, no out of office: JSON would be ${json(status('busy'))} B`);
await call(A, 'PUT', '/api/messaging/status', { status: 'busy' });
await sleep(2000);
console.log(`${at()} A turns out of office on (${Buffer.byteLength(MESSAGE)} B message, with an end): JSON would be ${json(status('busy', true))} B`);
await call(A, 'PUT', '/api/messaging/ooo', { message: MESSAGE, until: until.toISOString() });
await sleep(2000);
console.log(`${at()} A sets available while away: JSON would be ${json(status('available', true))} B`);
await call(A, 'PUT', '/api/messaging/status', { status: 'available' });
await sleep(2000);
console.log(`${at()} idle 6 s with it on`);
const before = s.seen.length;
await sleep(6000);
console.log(`${at()} frames while idle: ${s.seen.length - before}`);
console.log(`${at()} A turns it off: JSON would be ${json(status('available'))} B`);
await call(A, 'DELETE', '/api/messaging/ooo');
await sleep(2000);
console.log(JSON.stringify(s.seen.filter((x) => x.op !== '0'), null, 0));
s.sock.destroy();
process.exit(0);
