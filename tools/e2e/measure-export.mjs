// Bit-level measurement of the data export (CLAUDE.md rule 1): one GET, one
// streamed JSON body, for a person who has written a known number of
// messages. Against a PLAINTEXT server so tshark reads the exchange.
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -relay=false -db <scratch>.db -web ../web -signup-check-mail=false
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w export.pcapng -a duration:30 &
//   node tools/e2e/measure-export.mjs [messages, default 2000]
//   tshark -r export.pcapng -q -z conv,tcp
import { randomBytes } from 'node:crypto';
import { gzipSync } from 'node:zlib';

const BASE = 'http://127.0.0.1:8444';
const N = Number(process.argv[2] ?? 2000);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;
const csrfOf = (cookie) => (cookie.split('; ').find((c) => c.startsWith('aether_csrf=')) ?? '').split('=')[1];
const json = { 'content-type': 'application/json' };

async function person(label) {
  const email = `mx-${label}-${randomBytes(3).toString('hex')}@example.test`;
  const res = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: json, body: JSON.stringify({ email, displayName: `Export ${label}`, password: 'measure-' + randomBytes(8).toString('base64url') }) });
  if (res.status !== 201) throw new Error(`signup ${label}: ${res.status}`);
  return { email, cookie: (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ') };
}
const call = async (who, method, path, body, extra = {}) =>
  fetch(BASE + path, { method, headers: { ...json, cookie: who.cookie, 'X-Aether-CSRF': csrfOf(who.cookie), ...extra }, body: body ? JSON.stringify(body) : undefined });

const A = await person('a');
const B = await person('b');
const conv = (await (await call(A, 'POST', '/api/messaging/direct', { email: B.email })).json()).id;
// A typical line of chat: about sixty characters.
const line = (i) => `Message ${i}: the build is green again, I will merge after lunch.`;
console.log(`${at()} A writes ${N} messages (not captured as part of the figure; throttled to the API budget)`);
let bodies = 0;
for (let i = 0; i < N; i++) {
  const r = await call(A, 'POST', `/api/messaging/conversations/${conv}/messages`, { body: line(i) });
  if (r.status === 429) {
    await sleep(1100);
    i--;
    continue;
  }
  if (r.status >= 300) throw new Error(`send: ${r.status}`);
  await r.arrayBuffer();
  bodies += line(i).length;
}
await sleep(2000);
console.log(`${at()} quiet for two seconds, then the export, alone on its connection`);
// Twice (the budget allows two at once): as sent, then compressed the way a
// browser asks for it.
for (const enc of ['identity', 'gzip']) {
  const m0 = performance.now();
  const res = await call(A, 'GET', '/api/identity/export', undefined, { connection: 'close', 'accept-encoding': enc });
  const text = await res.text();
  const ms = performance.now() - m0;
  const doc = JSON.parse(text);
  console.log(`${at()} accept-encoding ${enc}: status ${res.status}, ${text.length} B of JSON in ${ms.toFixed(0)} ms; ${doc.messages.length} messages in it, their words ${bodies} B`);
  console.log(`        the messages section is ${JSON.stringify(doc.messages).length} B; gzip -9 of the whole file is ${gzipSync(text, { level: 9 }).length} B`);
  console.log(`        transfer-encoding: ${res.headers.get('transfer-encoding')}, content-encoding: ${res.headers.get('content-encoding')}`);
  await sleep(2000);
}
console.log(`${at()} done`);
process.exit(0);
