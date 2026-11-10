// Push notifications, end to end against a fake push service
// (tools/e2e/fakepush.mjs on 127.0.0.1:9470):
//   - a browser registers the service worker and the console shows the
//     notifications switch;
//   - A subscribes over REST with keys this harness made (headless Chrome
//     has no push service of its own), then signs out of its page so A has
//     no socket;
//   - B sends A a direct message: the fake service receives one message,
//     with the headers a real one demands, and this harness decrypts it with
//     A's keys (RFC 8291) to the title, the words and where a tap goes;
//   - a group message without a mention sends nothing; an urgent one does;
//   - a subscription on a revoked session hears nothing; a gone endpoint is
//     forgotten after one try;
//   - rule 7: anonymous, no CSRF, bad keys, an https-less endpoint off the
//     loopback, the eleventh device, someone else's endpoint on unsubscribe.
//
//   node tools/e2e/push.e2e.mjs [https://localhost:8443]
import { webcrypto } from 'node:crypto';
import { BASE, accounts, launch, signedIn, until, ok, fail, failed, sleep } from './meet.lib.mjs';

const PUSH = process.env.FAKE_PUSH || 'http://127.0.0.1:9470';
const acc = await accounts(['A', 'B']);
const browser = await launch();
const log = [];
const subtle = webcrypto.subtle;
const b64u = (buf) => Buffer.from(buf).toString('base64url');

// REST as a signed-in person, from Node: cookie jar by hand.
async function restSession(acct) {
  const jar = {};
  const headers = () => ({ cookie: Object.entries(jar).map(([k, v]) => `${k}=${v}`).join('; '), 'content-type': 'application/json' });
  const keep = (res) => {
    for (const c of res.headers.getSetCookie?.() ?? []) {
      const [kv] = c.split(';');
      const [k, v] = kv.split('=');
      jar[k] = v;
    }
  };
  const call = async (method, path, body, csrf = true) => {
    const h = headers();
    if (csrf && jar.aether_csrf) h['X-Aether-CSRF'] = jar.aether_csrf;
    const res = await fetch(BASE + path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
    keep(res);
    return { status: res.status, body: await res.json().catch(() => null) };
  };
  const login = await call('POST', '/api/identity/login', { email: acct.email, password: acct.password }, false);
  if (login.status !== 200) throw new Error(`login ${acct.email}: ${login.status}`);
  return { call, jar };
}

// A browser-side subscription, made here: an ECDH P-256 key pair and a
// 16-byte auth secret, as PushManager would.
async function makeSubscriber(name) {
  const kp = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const pub = new Uint8Array(await subtle.exportKey('raw', kp.publicKey));
  const auth = webcrypto.getRandomValues(new Uint8Array(16));
  return { name, kp, pub, auth, endpoint: `${PUSH}/push/${name}`, json: { endpoint: `${PUSH}/push/${name}`, keys: { p256dh: b64u(pub), auth: b64u(auth) } } };
}

// RFC 8291 on the receiving side.
async function decrypt(sub, bodyB64) {
  const body = Buffer.from(bodyB64, 'base64url');
  const salt = body.subarray(0, 16);
  const rs = body.readUInt32BE(16);
  const idlen = body[20];
  if (rs !== 4096 || idlen !== 65) throw new Error(`header rs=${rs} idlen=${idlen}`);
  const asPub = body.subarray(21, 86);
  const cipher = body.subarray(86);
  const asKey = await subtle.importKey('raw', asPub, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
  const shared = await subtle.deriveBits({ name: 'ECDH', public: asKey }, sub.kp.privateKey, 256);
  const hkdf = async (ikm, saltBytes, info, len) => {
    const k = await subtle.importKey('raw', ikm, 'HKDF', false, ['deriveBits']);
    return new Uint8Array(await subtle.deriveBits({ name: 'HKDF', hash: 'SHA-256', salt: saltBytes, info }, k, len * 8));
  };
  const info = Buffer.concat([Buffer.from('WebPush: info\0'), Buffer.from(sub.pub), asPub]);
  const ikm = await hkdf(shared, sub.auth, info, 32);
  const cek = await hkdf(ikm, salt, Buffer.from('Content-Encoding: aes128gcm\0'), 16);
  const nonce = await hkdf(ikm, salt, Buffer.from('Content-Encoding: nonce\0'), 12);
  const key = await subtle.importKey('raw', cek, 'AES-GCM', false, ['decrypt']);
  const plain = Buffer.from(await subtle.decrypt({ name: 'AES-GCM', iv: nonce }, key, cipher));
  const end = plain.lastIndexOf(2);
  return JSON.parse(plain.subarray(0, end).toString('utf8'));
}

const inbox = async (name) => (await fetch(`${PUSH}/inbox/${name}`)).json();
const clearInbox = (name) => fetch(`${PUSH}/inbox/${name}`, { method: 'DELETE' });
const inboxHas = async (name, n) => {
  for (let i = 0; i < 60; i++) {
    const list = await inbox(name);
    if (list.length >= n) return list;
    await sleep(100);
  }
  return inbox(name);
};

try {
  // The browser side: the worker is registered and the console has the switch.
  const pageA = await signedIn(browser, acc.A, 'A', log);
  const reg = await until(pageA, async () => {
    const r = await navigator.serviceWorker.getRegistration('/');
    return !!r && r.scope.endsWith('/');
  }, null);
  ok(reg, 'the service worker is registered at the root scope');
  await pageA.evaluate(async () => (await import('/src/console.js')).openConsole?.('account'));
  const card = await until(pageA, () => !!document.getElementById('push-card') && /notifications/i.test(document.getElementById('push-card').textContent), null);
  ok(card, 'the console account page shows the notifications switch');
  const keyRes = await pageA.evaluate(() => fetch('/api/notify/push/key').then((r) => r.json()));
  ok(keyRes.enabled === true && /^[A-Za-z0-9_-]{87}$/.test(keyRes.key), `the server offers a 65-byte application key (${(keyRes.key || '').length} chars)`);
  await pageA.browserContext().close(); // A has no page, and so no socket

  // A subscribes two devices over REST; B is signed in over REST too.
  const A = await restSession(acc.A);
  const B = await restSession(acc.B);
  const laptop = await makeSubscriber('a-laptop-' + Math.random().toString(36).slice(2, 7));
  const phone = await makeSubscriber('a-phone-' + Math.random().toString(36).slice(2, 7));
  ok((await A.call('POST', '/api/notify/push/subscribe', laptop.json)).status === 204, 'A subscribes a laptop');
  ok((await A.call('POST', '/api/notify/push/subscribe', phone.json)).status === 204, 'and a phone');
  ok((await A.call('POST', '/api/notify/push/subscribe', laptop.json)).status === 204, 'subscribing the same endpoint again is fine');

  // B messages A directly.
  const me = await B.call('GET', '/api/identity/me');
  const conv = await B.call('POST', '/api/messaging/direct', { email: acc.A.email });
  ok(conv.status === 200 || conv.status === 201, `B opens a direct chat with A (${conv.status})`);
  const convID = conv.body?.id;
  const text = 'are you there? ' + Math.random().toString(36).slice(2, 6);
  const sent = await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: text });
  ok(sent.status === 201 || sent.status === 200, `B sends a message (${sent.status})`);
  const got = await inboxHas(laptop.name, 1);
  ok(got.length === 1, `the fake push service received one message for A's laptop (${got.length})`);
  ok((await inboxHas(phone.name, 1)).length === 1, 'and one for the phone');
  if (got.length) {
    const h = got[0].headers;
    ok(h.encoding === 'aes128gcm' && h.ttl === '600' && h.urgency === 'normal' && /^vapid t=[A-Za-z0-9_.-]+, k=[A-Za-z0-9_-]+$/.test(h.authorization), `with the headers a push service demands (${JSON.stringify(h).slice(0, 120)}...)`);
    try {
      const n = await decrypt(laptop, got[0].body);
      ok(n.title === (me.body?.displayName || 'E2E B') && n.body === text && n.url === `/?chat=${convID}` && n.tag === `conv-${convID}`, `decrypted with A's keys: ${JSON.stringify(n)}`);
      ok(got[0].bytes === Buffer.byteLength(JSON.stringify({ title: n.title, body: n.body, url: n.url, tag: n.tag })) + 1 + 16 + 86, `on the wire: ${got[0].bytes} bytes for a ${Buffer.byteLength(JSON.stringify(n))}-byte notice (header 86, tag 16, delimiter 1)`);
      console.log(`MEASURE push body ${got[0].bytes} bytes for a ${Buffer.byteLength(JSON.stringify(n))}-byte notice; authorization header ${got[0].headers.authorization.length} chars`);
    } catch (e) {
      ok(false, 'decrypting the message: ' + e.message);
    }
  }

  // A plain group message: nothing. An urgent one: a notice, urgently.
  await clearInbox(laptop.name);
  await clearInbox(phone.name);
  const group = await B.call('POST', '/api/messaging/groups', { title: 'Push E2E', members: [acc.A.email] });
  ok(group.status === 201 || group.status === 200, `B makes a group with A (${group.status})`);
  await B.call('POST', `/api/messaging/conversations/${group.body?.id}/messages`, { body: 'morning' });
  await sleep(700);
  ok((await inbox(laptop.name)).length === 0, 'a plain group message sends no notice');
  const urgent = await B.call('POST', `/api/messaging/conversations/${group.body?.id}/messages`, { body: 'server down', priority: 'urgent' });
  ok(urgent.status === 201 || urgent.status === 200, `an urgent group message is accepted (${urgent.status})`);
  const u = await inboxHas(laptop.name, 1);
  ok(u.length === 1 && u[0].headers.urgency === 'high' && u[0].headers.ttl === '60', `an urgent message is pushed with high urgency and a short life (${u[0]?.headers.urgency}, ${u[0]?.headers.ttl})`);

  // A revoked session's device hears nothing more.
  // Subscribe a third device on a second session, then revoke that session
  // (the newest one) from the first.
  const A2 = await restSession(acc.A);
  const tablet = await makeSubscriber('a-tablet-' + Math.random().toString(36).slice(2, 7));
  ok((await A2.call('POST', '/api/notify/push/subscribe', tablet.json)).status === 204, 'a tablet subscribes on a second session');
  const s2 = await A.call('GET', '/api/identity/sessions');
  const list = (s2.body?.sessions ?? s2.body ?? []).filter((s) => !s.revokedAt).sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt));
  const newest = list[0];
  const revoked = newest ? await A.call('DELETE', `/api/identity/sessions/${newest.id}`) : { status: 0 };
  ok(revoked.status === 200 || revoked.status === 204, `A revokes the tablet's session from the laptop (${revoked.status}; ${list.length} live sessions)`);
  await clearInbox(laptop.name);
  await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: 'after revoke' });
  await inboxHas(laptop.name, 1);
  ok((await inbox(tablet.name)).length === 0, 'the revoked session\'s device receives nothing');

  // A gone endpoint is forgotten.
  await fetch(`${PUSH}/gone/${phone.name}`);
  await clearInbox(phone.name); // what it heard before it was gone
  await clearInbox(laptop.name);
  await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: 'phone gone' });
  await inboxHas(laptop.name, 1);
  await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: 'phone still gone?' });
  await inboxHas(laptop.name, 2);
  await sleep(300);
  ok((await inbox(phone.name)).length === 0, 'a push service answering 410 gets no further posts');

  // Rule 7 at the edge.
  ok((await fetch(BASE + '/api/notify/push/subscribe', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(laptop.json) })).status === 401, 'anonymous: refused');
  ok((await A.call('POST', '/api/notify/push/subscribe', laptop.json, false)).status === 403, 'no CSRF header: refused');
  const bad = { ...laptop.json, keys: { p256dh: laptop.json.keys.p256dh.slice(0, 40), auth: laptop.json.keys.auth } };
  ok((await A.call('POST', '/api/notify/push/subscribe', bad)).status === 400, 'a short key: refused');
  ok((await A.call('POST', '/api/notify/push/subscribe', { ...laptop.json, endpoint: 'http://push.example.test/x' })).status === 400, 'plain http off the loopback: refused');
  let eleventh = 0;
  const extra = [];
  for (let i = 0; i < 9; i++) {
    const d = await makeSubscriber('a-many-' + i + '-' + Math.random().toString(36).slice(2, 5));
    extra.push(d);
    const r = await A.call('POST', '/api/notify/push/subscribe', d.json);
    if (r.status === 400 && r.body?.code === 'too_many') eleventh++;
  }
  ok(eleventh >= 1, `the eleventh device is refused by name (${eleventh} refused of 9 extra)`);
  // Leave the account as found, so the next run on the same database is
  // not already at the device limit.
  for (const d of [...extra, phone, tablet]) await A.call('POST', '/api/notify/push/unsubscribe', { endpoint: d.endpoint });
  ok((await B.call('POST', '/api/notify/push/unsubscribe', { endpoint: laptop.endpoint })).status === 204, 'B may ask to forget A\'s endpoint...');
  await clearInbox(laptop.name);
  await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: 'still here?' });
  ok((await inboxHas(laptop.name, 1)).length === 1, '...and nothing happens: A\'s laptop still hears');
  ok((await A.call('POST', '/api/notify/push/unsubscribe', { endpoint: laptop.endpoint })).status === 204, 'A forgets the laptop');
  await clearInbox(laptop.name);
  await B.call('POST', `/api/messaging/conversations/${convID}/messages`, { body: 'gone?' });
  await sleep(700);
  ok((await inbox(laptop.name)).length === 0, 'and it hears no more');
  ok(log.filter((l) => / PAGE ERROR: /.test(l)).length === 0, `no page errors (${log.filter((l) => / PAGE ERROR: /.test(l)).join(' | ')})`);
} catch (e) {
  fail(e);
} finally {
  await browser.close();
}
console.log(failed() ? `FAILED (${failed()})` : 'ALL PASSED');
process.exit(failed() ? 1 : 0);
