// Bit-level measurement of notification settings (CLAUDE.md rule 1).
// They add no frame to any socket: a setting is read and written over REST
// and rides on the conversation list every device already fetches. This
// drives each of those against a PLAINTEXT server so tshark sees the bytes,
// with a chat socket open on the other account to show nothing is pushed.
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -relay=false -db <scratch>.db -web ../web
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w notify.pcapng -a duration:16 &
//   node tools/e2e/measure-notify.mjs
//   tshark -r notify.pcapng -Y http -T fields -e frame.time_relative -e tcp.srcport \
//     -e http.request.method -e http.request.uri -e http.response.code -e frame.len -e tcp.len
import { randomBytes } from 'node:crypto';
import { gzipSync } from 'node:zlib';

const BASE = 'http://127.0.0.1:8444';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;
const csrfOf = (cookie) => (cookie.split('; ').find((c) => c.startsWith('aether_csrf=')) ?? '').split('=')[1];

async function signup(label) {
  const email = `mn-${label}-${randomBytes(3).toString('hex')}@example.test`;
  const res = await fetch(BASE + '/api/identity/signup', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email, displayName: `Notify ${label}`, password: 'measure-' + randomBytes(8).toString('base64url') }),
  });
  if (res.status !== 201) throw new Error(`signup ${label}: ${res.status}`);
  return { email, cookie: (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ') };
}
// One request, alone on its connection, so its packets are its own.
const call = async (who, method, path, body) => {
  const res = await fetch(BASE + path, {
    method,
    headers: { cookie: who.cookie, connection: 'close', 'accept-encoding': 'gzip', 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(who.cookie) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (res.status >= 300) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return text;
};

const A = await signup('a');
const B = await signup('b');
await call(A, 'POST', '/api/messaging/direct', { email: B.email });
await sleep(1000);

console.log(`${at()} GET the conversation list (it now carries the settings)`);
const list = await call(A, 'GET', '/api/messaging/conversations');
const notify = JSON.stringify(JSON.parse(list).notify);
const without = JSON.stringify({ ...JSON.parse(list), notify: undefined });
console.log(`        body ${list.length} B, of which the settings ${notify.length + '"notify":,'.length} B; gzipped ${gzipSync(list).length} B against ${gzipSync(without).length} B without them`);
await sleep(1500);
console.log(`${at()} GET the settings alone`);
const got = await call(A, 'GET', '/api/messaging/notify');
console.log(`        body ${got.length} B`);
await sleep(1500);
console.log(`${at()} PUT the settings (direct quiet, previews off)`);
const put = JSON.stringify({ ...JSON.parse(got).notify, direct: 'quiet', preview: false });
await call(A, 'PUT', '/api/messaging/notify', JSON.parse(put));
console.log(`        request body ${put.length} B`);
await sleep(1500);
console.log(`${at()} B writes to A, who is not connected and has direct messages quiet: nothing is pushed anywhere`);
await call(B, 'POST', `/api/messaging/conversations/${JSON.parse(list).conversations[0].id}/messages`, { body: 'hello' });
await sleep(1500);
console.log(`${at()} done`);
process.exit(0);
