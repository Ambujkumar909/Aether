// Bit-level measurement of the missed-activity email (CLAUDE.md rule 1).
// The feature adds no frame to any socket and one field to the settings; its
// bytes are an SMTP conversation, once a period, to the mail server. This
// drives one against a PLAINTEXT server (so the REST side is readable) that
// mails through Mailpit over STARTTLS, as production does, with a period
// short enough to capture.
//
//   bash tools/e2e/mailpit.sh start
//   aether -addr 127.0.0.1:8444 -media-port 8802 -relay=false -db <scratch>.db -web ../web \
//     -smtp-addr 127.0.0.1:1025 -smtp-from noreply@aether.test -smtp-user aether -smtp-password e2e \
//     -smtp-ca ../tools/e2e/.mail/cert.pem -public-url http://127.0.0.1:8444 \
//     -signup-check-mail=false -mail-digest-minute 300ms
//   tshark -i \Device\NPF_Loopback -f "tcp port 1025 or tcp port 8444" -w digest.pcapng -a duration:25 &
//   node tools/e2e/measure-digest.mjs http://127.0.0.1:8444
//   tshark -r digest.pcapng -q -z conv,tcp
import { randomBytes } from 'node:crypto';
import { BASE, MAILPIT, mailedToken, sleep } from './meet.lib.mjs';

const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;
const csrfOf = (cookie) => (cookie.split('; ').find((c) => c.startsWith('aether_csrf=')) ?? '').split('=')[1];
const json = { 'content-type': 'application/json' };

async function person(label) {
  const email = `md-${label}-${randomBytes(3).toString('hex')}@example.test`;
  const password = 'measure-' + randomBytes(8).toString('base64url');
  let res = await fetch(BASE + '/api/identity/signup', { method: 'POST', headers: json, body: JSON.stringify({ email, displayName: `Digest ${label}`, password }) });
  if (res.status === 202) {
    const token = await mailedToken(email, 'verify');
    res = await fetch(BASE + '/api/identity/signup/verify', { method: 'POST', headers: json, body: JSON.stringify({ token, password }) });
  }
  if (res.status !== 201) throw new Error(`signup ${label}: ${res.status}`);
  return { email, cookie: (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ') };
}
const call = async (who, method, path, body) => {
  const res = await fetch(BASE + path, { method, headers: { ...json, cookie: who.cookie, connection: 'close', 'X-Aether-CSRF': csrfOf(who.cookie) }, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  if (res.status >= 300) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return text ? JSON.parse(text) : null;
};

const A = await person('a');
const B = await person('b');
const conv = (await call(A, 'POST', '/api/messaging/direct', { email: B.email })).id;
const prefs = (await call(A, 'GET', '/api/messaging/notify')).notify;
console.log(`${at()} A chooses every 10 minutes (three seconds here)`);
await call(A, 'PUT', '/api/messaging/notify', { ...prefs, email: '10m' });
console.log(`        the setting in the body: ${JSON.stringify({ email: '10m' }).length - 2} B`);
await sleep(4000);
console.log(`${at()} four seconds with nothing missed: the loop ticked thirteen times and sent nothing`);
console.log(`${at()} B writes to A twice; A has no socket open`);
await call(B, 'POST', `/api/messaging/conversations/${conv}/messages`, { body: 'are you there?' });
await call(B, 'POST', `/api/messaging/conversations/${conv}/messages`, { body: 'the build is red, can you look before standup?' });
let mail;
for (let i = 0; i < 60 && !mail; i++) {
  await sleep(250);
  const r = await (await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent('to:' + A.email + ' subject:"Unread messages in Aether"')}`)).json();
  if (r.messages?.length) mail = r.messages[0];
}
if (!mail) throw new Error('no email');
const full = await (await fetch(`${MAILPIT}/api/v1/message/${mail.ID}`)).json();
const raw = await (await fetch(`${MAILPIT}/api/v1/message/${mail.ID}/raw`)).text();
const text = full.Text.replaceAll('\r', '');
console.log(`${at()} the email arrived: ${raw.length} B as the mail server stored it (it adds headers), ${text.length} B of it the text a person reads`);
console.log(text.replace(/^/gm, '        | '));
await sleep(4000);
console.log(`${at()} four more seconds, still unread, nothing new: no second email`);
process.exit(0);
