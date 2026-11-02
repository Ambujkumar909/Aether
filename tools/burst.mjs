// burst — drive realistic control-plane load so coalescing is exercised.
//
// The interop harness sends one frame and waits for its effect, which is
// correct for a correctness test and useless for a bandwidth one: nothing
// ever arrives close enough together to coalesce. Real clients do the
// opposite. A person joining fires roster, state and track frames inside a
// millisecond; somebody scrolling a gallery fires a viewport update per
// animation frame.
//
// This produces that shape on purpose, so bitcheck measures the protocol as
// it actually behaves rather than at its worst case.

import WebSocket from 'ws';
import { Writer, newDictPair, encodeBatch, encodeState, encodeSpeakers, Role } from './wire-shim.mjs';

const base = process.argv[2] ?? 'ws://localhost:8899';
const http = base.replace(/^ws/, 'http');
const room = 'burst-' + Math.floor(Date.now() / 1000);
const EMAIL = process.env.AETHER_EMAIL ?? 'admin@aether.local';
const PASSWORD = process.env.AETHER_PASSWORD ?? 'bootstrap-passphrase-01';

// The client-side coalescing window, matching COALESCE_MS in app.js.
const COALESCE_MS = 15;

async function signIn() {
  const res = await fetch(`${http}/api/identity/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: EMAIL, password: PASSWORD }),
  });
  if (!res.ok) throw new Error(`sign-in failed (${res.status})`);
  return (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ');
}

async function main() {
  const cookie = await signIn();
  const ws = new WebSocket(`${base}/rtc?room=${room}&name=Burst`, { headers: { Cookie: cookie } });
  ws.binaryType = 'arraybuffer';

  const dicts = newDictPair();
  let sent = 0;
  let messages = 0;

  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = rej;
  });

  // Wait for the welcome so our own id is interned and deltas are 3 bytes,
  // which is the steady state the design is actually about.
  await new Promise((r) => setTimeout(r, 400));

  const outbox = [];
  let timer = null;

  const flush = () => {
    timer = null;
    if (!outbox.length) return;
    const frames = outbox.splice(0);
    ws.send(encodeBatch(frames));
    messages++;
  };

  const queue = (encode) => {
    const w = new Writer(dicts.tx);
    encode(w);
    outbox.push(w.bytes());
    sent++;
    if (!timer) timer = setTimeout(flush, COALESCE_MS);
  };

  // Four bursts of the traffic a real client produces: a state flurry when
  // somebody toggles things, and a speaker-energy stream while they talk.
  for (let round = 0; round < 4; round++) {
    for (let i = 0; i < 12; i++) {
      queue((w) =>
        encodeState(w, {
          participant: 'burst-self',
          micMuted: i % 2 === 0,
          camOff: i % 3 === 0,
          handRaised: i % 5 === 0,
          role: Role.Member,
        })
      );
      queue((w) => encodeSpeakers(w, [{ participant: 'burst-self', energy: (i % 10) / 10 }]));
    }
    await new Promise((r) => setTimeout(r, 120));
  }

  flush();
  await new Promise((r) => setTimeout(r, 300));
  ws.close();

  console.log(
    JSON.stringify({ logicalFrames: sent, wsMessages: messages, ratio: +(sent / messages).toFixed(2) })
  );
}

main().catch((e) => {
  console.error('burst failed:', e.message);
  process.exit(1);
});
