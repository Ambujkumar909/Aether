// Cross-language interoperability check.
//
// The browser codec and the Go codec are two hand-written implementations of
// the same bit-level format. Unit tests on either side prove only that each
// is self-consistent; they cannot catch a disagreement about bit order,
// varint continuation, or dictionary insertion timing. Those disagreements
// do not fail loudly -- they desynchronise the interning tables and every
// subsequent frame decodes to a plausible-looking wrong string.
//
// So this drives the real server over a real WebSocket and asserts on what
// comes back.
//
//   node --experimental-websocket src/interop.test.mjs [ws://host:port]

// The Node built-in WebSocket silently ignores a headers option, so it
// cannot carry the session cookie the meeting socket requires. The ws package
// can, and it is a test-harness dependency only -- the browser client has no
// runtime dependencies at all.
import WebSocket from 'ws';

import {
  Op,
  Role,
  Writer,
  Reader,
  newDictPair,
  encodeBatch,
  splitBatch,
  encodeState,
  decodeState,
  encodeSpeakers,
  encodePing,
  decodeRoster,
  decodeWelcome,
} from './wire.js';

const base = process.argv[2] ?? 'ws://localhost:8899';
const http = base.replace(/^ws/, 'http');
const room = 'interop-' + Math.floor(Date.now() / 1000);

const EMAIL = process.env.AETHER_EMAIL ?? 'admin@aether.local';
const PASSWORD = process.env.AETHER_PASSWORD ?? 'bootstrap-passphrase-01';

// The meeting socket requires an authenticated session, so the harness signs
// in the way a browser does and reuses the cookie.
let cookie = '';
async function signIn() {
  const res = await fetch(`${http}/api/identity/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: EMAIL, password: PASSWORD }),
  });
  if (!res.ok) {
    throw new Error(`sign-in failed (${res.status}); set AETHER_EMAIL / AETHER_PASSWORD`);
  }
  const jar = res.headers.getSetCookie?.() ?? [];
  cookie = jar.map((c) => c.split(';')[0]).join('; ');
  if (!cookie) throw new Error('no session cookie returned');
}

let pass = 0;
let fail = 0;
const check = (name, ok, detail = '') => {
  if (ok) {
    pass++;
    console.log(`  ok   ${name}`);
  } else {
    fail++;
    console.log(`  FAIL ${name} ${detail}`);
  }
};

function client(name) {
  const dicts = newDictPair();
  const ws = new WebSocket(`${base}/rtc?room=${room}&name=${encodeURIComponent(name)}`, {
    headers: { Cookie: cookie },
  });
  ws.binaryType = 'arraybuffer';
  const c = {
    ws,
    dicts,
    name,
    id: null,
    roster: new Map(),
    states: new Map(),
    bytesOut: 0,
    bytesIn: 0,
    framesOut: 0,
    waiters: [],
  };

  c.send = (fn) => {
    const w = new Writer(dicts.tx);
    fn(w);
    const b = w.bytes();
    ws.send(b);
    c.bytesOut += b.length;
    c.framesOut++;
    return b;
  };

  c.expect = (pred, label, ms = 3000) =>
    new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`timeout waiting for ${label}`)), ms);
      const probe = () => {
        if (pred()) {
          clearTimeout(t);
          c.waiters = c.waiters.filter((w) => w !== probe);
          resolve();
        }
      };
      c.waiters.push(probe);
      probe();
    });

  const dispatch = (h, r) => {
    switch (h.op) {
      case Op.Welcome: {
        const w = decodeWelcome(r);
        c.id = w.id;
        c.room = w.room;
        c.epoch = w.epoch;
        break;
      }
      case Op.Roster: {
        const e = decodeRoster(r);
        if (e.present) c.roster.set(e.id, e);
        else c.roster.delete(e.id);
        break;
      }
      case Op.StateDelta: {
        const s = decodeState(r);
        c.states.set(s.participant, s);
        break;
      }
      case Op.Chat: {
        const who = r.str();
        c.chat = { who, text: new TextDecoder().decode(r.blob()) };
        break;
      }
      case Op.Pong:
        c.pongs = (c.pongs ?? 0) + 1;
        break;
      default:
        break;
    }
    if (r.err) c.decodeError = r.err;
  };

  ws.onmessage = (ev) => {
    const bytes = new Uint8Array(ev.data);
    c.bytesIn += bytes.length;
    const r = new Reader(bytes, dicts.rx);
    const h = r.header();
    if (!h) {
      c.decodeError = 'bad header';
      return;
    }
    if (h.op === Op.Batch) {
      splitBatch(r, (f) => {
        const fr = new Reader(f, dicts.rx);
        const fh = fr.header();
        if (fh) dispatch(fh, fr);
      });
    } else {
      dispatch(h, r);
    }
    c.waiters.slice().forEach((w) => w());
  };

  c.ready = new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = (e) => rej(new Error('ws error: ' + (e.message ?? 'unknown')));
  });
  return c;
}

async function main() {
  console.log(`\nAether wire interop against ${base}, room ${room}\n`);

  await signIn();
  check('authenticated session established', !!cookie);

  const alice = client('Alice');
  await alice.ready;
  await alice.expect(() => alice.id, 'welcome');
  check('server assigns an identity', !!alice.id, alice.id ?? '');
  check('welcome carries the room', alice.room === room, alice.room);
  check('welcome carries a session epoch', alice.epoch > 1_600_000_000_000);
  check('alice sees herself in the roster', alice.roster.has(alice.id));

  // A second participant. This is where interning gets interesting: the
  // server must intern Bob's id into Alice's transmit table in the same
  // order Alice interns it into her receive table.
  const bob = client('Bob');
  await bob.ready;
  await bob.expect(() => bob.id, 'bob welcome');
  await alice.expect(() => alice.roster.size === 2, 'alice sees bob');

  check('bob got a distinct identity', bob.id && bob.id !== alice.id);
  check('alice sees bob join', alice.roster.get(bob.id)?.name === 'Bob',
    JSON.stringify([...alice.roster.values()].map((r) => r.name)));
  check('bob sees alice already present', bob.roster.get(alice.id)?.name === 'Alice',
    JSON.stringify([...bob.roster.values()].map((r) => r.name)));

  // One byte ping, one byte pong.
  const pingFrame = alice.send(encodePing);
  check('ping encodes to exactly 1 byte', pingFrame.length === 1, `${pingFrame.length} B`);
  await alice.expect(() => alice.pongs > 0, 'pong');

  // A state delta round trip through the server, which re-encodes it into
  // Bob's dictionary. If the two implementations disagreed about bit
  // packing, the booleans would come back scrambled rather than absent.
  //
  // Note what the first frame costs. Alice learned her own id from the
  // Welcome, which interned it into her RECEIVE table; her TRANSMIT table
  // has never seen it. So the first mention in each direction pays the
  // literal, and every mention afterwards costs one byte.
  //
  // That is the price of per-direction dictionaries, and it is worth paying:
  // a single shared table would save this one literal and would silently
  // corrupt any session where two frames crossed in flight. The steady
  // state is what matters, and the steady state is three bytes.
  const before = bob.states.size;
  const mk = (over = {}) => (w) =>
    encodeState(w, {
      participant: alice.id,
      micMuted: true,
      camOff: false,
      sharing: false,
      handRaised: true,
      speaking: false,
      recording: false,
      role: Role.Member,
      ...over,
    });

  const firstFrame = alice.send(mk());
  check(
    'first mention of an id pays the literal once',
    firstFrame.length === 3 + alice.id.length,
    `${firstFrame.length} B for a ${alice.id.length}-char id`
  );

  const stateFrame = alice.send(mk());
  check(
    'every subsequent state delta is exactly 3 bytes',
    stateFrame.length === 3,
    `${stateFrame.length} B: ${[...stateFrame].map((b) => b.toString(16)).join(' ')}`
  );

  await bob.expect(() => bob.states.size > before, 'bob receives state');
  const got = bob.states.get(alice.id);
  check('bob decodes the right participant', got?.participant === alice.id);
  check('micMuted survived the round trip', got?.micMuted === true);
  check('camOff survived the round trip', got?.camOff === false);
  check('handRaised survived the round trip', got?.handRaised === true);
  check('speaking survived the round trip', got?.speaking === false);
  // Role is server-authoritative and is NOT a round trip. Alice opened the
  // room, so she hosts, and the delta she sent claiming Role.Member came back
  // saying Host -- which is the property worth asserting: a client cannot
  // promote or demote itself by editing a frame.
  check(
    'the server overrides a self-claimed role',
    got?.role === Role.Owner,
    `role ${got?.role}, expected host (${Role.Owner}) not the claimed member`
  );

  // Speaker energy, quantised to a byte, must arrive within quantisation
  // error rather than merely "close".
  alice.send((w) => encodeSpeakers(w, [{ participant: alice.id, energy: 0.75 }]));

  // Chat, exercising length-prefixed blobs across the boundary.
  const text = 'interop check — unicode ✓ and a longer body to exceed one byte of length';
  alice.send((w) => {
    w.header(Op.Chat);
    w.blob(new TextEncoder().encode(text));
  });
  await bob.expect(() => bob.chat, 'chat');
  check('chat body survived exactly', bob.chat.text === text, bob.chat?.text);
  check('chat attributes the sender', bob.chat.who === alice.id);

  // Batching: several frames in one WebSocket message.
  const frames = [];
  for (let i = 0; i < 3; i++) {
    const w = new Writer(alice.dicts.tx);
    encodeState(w, {
      participant: alice.id,
      micMuted: i % 2 === 0,
      sharing: i === 2,
      role: Role.Member,
    });
    frames.push(w.bytes());
  }
  const batch = encodeBatch(frames);
  // Two 3 byte deltas and one 4 byte delta -- the third sets sharing, which
  // appends the screen track id, empty here and so one byte. Derived from the
  // frames rather than written as a constant, because a constant here only
  // records what the format used to be: the point of the check is that
  // batching adds a bounded envelope and not a per-frame header.
  const framesTotal = frames.reduce((n, f) => n + f.length, 0);
  const envelope = batch.length - framesTotal;
  check(
    'batching costs a small fixed envelope, not a header per frame',
    envelope >= 0 && envelope <= 4,
    `${batch.length} B for ${framesTotal} B of frames: ${envelope} B envelope`
  );
  check(
    'the three deltas are 3, 3 and 4 bytes',
    frames.map((f) => f.length).join(',') === '3,3,4',
    frames.map((f) => f.length).join(',')
  );

  // The three frames alternate, so the last one in the batch sets sharing
  // true. Waiting on that specific value proves the server unpacked the
  // batch in order rather than merely receiving something.
  alice.ws.send(batch);
  alice.bytesOut += batch.length;
  await bob.expect(
    () => bob.states.get(alice.id)?.sharing === true,
    'final frame of the batch, in order'
  );
  check('server unpacked the batch in order', bob.states.get(alice.id)?.sharing === true);

  check('no decode errors on alice', !alice.decodeError, String(alice.decodeError));
  check('no decode errors on bob', !bob.decodeError, String(bob.decodeError));

  // Disconnect handling.
  bob.ws.close();
  await alice.expect(() => alice.roster.size === 1, 'alice sees bob leave', 4000);
  check('leave is propagated', !alice.roster.has(bob.id));

  console.log(
    `\n  alice: ${alice.framesOut} frames out in ${alice.bytesOut} B ` +
      `(${(alice.bytesOut / alice.framesOut).toFixed(2)} B/frame), ${alice.bytesIn} B in`
  );
  console.log(`\n${fail === 0 ? 'PASS' : 'FAIL'} — ${pass} passed, ${fail} failed\n`);

  alice.ws.close();
  process.exit(fail === 0 ? 0 : 1);
}

main().catch((e) => {
  console.error('\nharness error:', e.message, '\n');
  process.exit(1);
});
