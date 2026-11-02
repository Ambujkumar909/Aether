// Bit-level measurement of meeting host-control frames (CLAUDE.md rule 1):
// spotlight on, stop all spotlights, end meeting. Three signed-in people on
// raw meeting sockets against a PLAINTEXT server, so tshark can read every
// WebSocket frame:
//
//   aether -addr 127.0.0.1:8444 -media-port 8802 -db <scratch>.db -web ../web
//   tshark -i \Device\NPF_Loopback -f "tcp port 8444" -w meet.pcapng -a duration:25 &
//   node tools/e2e/measure-meeting.mjs
//   tshark -r meet.pcapng -Y websocket -T fields -e frame.time_relative -e tcp.srcport \
//     -e frame.len -e tcp.len -e websocket.payload_length -e websocket.mask
//
// The script prints when each event happens, so the frames are attributable
// by time; each event is followed by a quiet gap.
import { connect } from 'node:net';
import { randomBytes } from 'node:crypto';
import { Writer, Reader, Op, newDictPair, splitBatch, encodeState, decodeState } from '../../web/src/wire.js';
import { decodeBreakout, encodeSet, encodeCmd, encodeAnnounce, K } from '../../web/src/breakout.js';
import { decodeWhiteboard, encodeOpen, encodeStroke, encodeSimple, encodeNote, W } from '../../web/src/whiteboard.js';
import { decodeRecording, encodeSet as encodeRecSet, encodeCaption, WHAT as REC } from '../../web/src/recording.js';
import { quantize, encodeMask, encodeSet as encodeTogSet, encodeOutline, decodeTogether } from '../../web/src/together.js';

const HOST = '127.0.0.1', PORT = 8444, BASE = `http://${HOST}:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = performance.now();
const at = () => `+${((performance.now() - t0) / 1000).toFixed(2)}s`;

async function signup(label) {
  const res = await fetch(BASE + '/api/identity/signup', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email: `mm-${label}-${randomBytes(3).toString('hex')}@example.test`, displayName: `Meet ${label}`, password: 'measure-' + randomBytes(8).toString('base64url') }),
  });
  if (res.status !== 201) throw new Error(`signup ${label}: ${res.status}`);
  return (res.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ');
}

// One person on the meeting socket. Reads server frames (to learn their own
// participant id from Welcome) and writes masked client frames, as a browser does.
function join(cookie, room, name, ticket = '') {
  return new Promise((resolve, reject) => {
    const dicts = newDictPair();
    const p = { id: '', ops: [], dicts, br: [], wb: [], rec: [], tog: [] };
    let buf = Buffer.alloc(0), up = false;
    const sock = connect({ host: HOST, port: PORT }, () => sock.write(
      `GET /rtc?room=${encodeURIComponent(room)}&name=${name}${ticket ? `&bt=${encodeURIComponent(ticket)}` : ''} HTTP/1.1\r\nHost: ${HOST}:${PORT}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
      `Sec-WebSocket-Key: ${randomBytes(16).toString('base64')}\r\nSec-WebSocket-Version: 13\r\nOrigin: ${BASE}\r\nCookie: ${cookie}\r\n\r\n`));
    p.sock = sock;
    sock.on('error', () => {});
    sock.on('data', (d) => {
      buf = Buffer.concat([buf, d]);
      if (!up) {
        const end = buf.indexOf('\r\n\r\n');
        if (end < 0) return;
        if (!/^HTTP\/1\.1 101/.test(buf.toString('latin1', 0, end))) return reject(new Error('upgrade refused'));
        up = true;
        buf = buf.subarray(end + 4);
      }
      for (;;) { // unmasked server frames
        if (buf.length < 2) return;
        let len = buf[1] & 0x7f, off = 2;
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4; }
        else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10; }
        if (buf.length < off + len) return;
        const payload = new Uint8Array(buf.subarray(off, off + len));
        buf = buf.subarray(off + len);
        if ((buf[0] & 0x0f) === 8) return;
        const each = (r) => {
          const h = r.header();
          if (!h) return;
          if (h.op === Op.Batch) return splitBatch(r, (f) => each(new Reader(f, dicts.rx)));
          p.ops.push(h.op);
          if (h.op === Op.Welcome && !p.id) { p.id = r.str(); resolve(p); }
          if (h.op === Op.Roster) { r.bool(); r.str(); r.str(); r.bits(2); r.bits(5); r.str(); } // teach the dictionary
          if (h.op === Op.Spotlight) { const n = Number(r.uvarint()); for (let i = 0; i < n; i++) r.str(); }
          if (h.op === Op.StateDelta) decodeState(r);
          if (h.op === Op.Breakout) p.br.push(decodeBreakout(r));
          if (h.op === Op.SetRole) r.str();
          if (h.op === Op.Whiteboard) p.wb.push(decodeWhiteboard(r));
          if (h.op === Op.Recording) p.rec.push(decodeRecording(r));
          if (h.op === Op.Together) p.tog.push(decodeTogether(r));
        };
        each(new Reader(payload, dicts.rx));
      }
    });
    p.send = (enc) => {
      const w = new Writer(dicts.tx);
      enc(w);
      const body = Buffer.from(w.bytes()), mask = randomBytes(4);
      const out = Buffer.alloc(body.length);
      for (let i = 0; i < body.length; i++) out[i] = body[i] ^ mask[i & 3];
      const head = body.length < 126 ? Buffer.from([0x82, 0x80 | body.length]) : Buffer.from([0x82, 0x80 | 126, body.length >> 8, body.length & 0xff]);
      sock.write(Buffer.concat([head, mask, out]));
    };
  });
}

const room = 'measure-' + randomBytes(3).toString('hex');
const [ca, cb, cc] = [await signup('A'), await signup('B'), await signup('C')];
const A = await join(ca, room, 'A'); // first in hosts
await sleep(300);
const B = await join(cb, room, 'B');
await sleep(300);
const C = await join(cc, room, 'C');
await sleep(2000); // let join-time traffic (SDP offers, roster) settle

const spot = (who, id, on) => who.send((w) => { w.header(Op.Spotlight); w.str(id); w.bool(on); w.bits(0, 7); });
console.log(`spotlight-on   ${at()}  (A spotlights B)`);
spot(A, B.id, true);
await sleep(1500);
console.log(`spotlight-same ${at()}  (A spotlights B again: should cost one client frame and nothing back)`);
spot(A, B.id, true);
await sleep(1500);
console.log(`stop-all       ${at()}`);
spot(A, '', false);
await sleep(1500);
console.log(`br-set         ${at()}  (A creates two rooms: B in Room 1, C in Room 2)`);
A.send((w) => encodeSet(w, { names: ['Room 1', 'Room 2'], members: [[B.id], [C.id]], limitSec: 0, autoMove: true, allowReturn: true }));
await sleep(1500);
console.log(`br-open        ${at()}  (A opens them: B and C are each sent a move)`);
A.send((w) => encodeCmd(w, K.open));
await sleep(1500);
const mv = B.br.find((x) => x?.kind === 'move');
console.log(`br-enter       ${at()}  (B enters Room 1 with its ticket on a second socket)`);
const B2 = await join(cb, mv.room, 'B', mv.ticket);
await sleep(1500);
console.log(`br-announce    ${at()}  (A: "Two minutes left")`);
A.send((w) => encodeAnnounce(w, 'Two minutes left'));
await sleep(1500);
console.log(`br-close       ${at()}  (A closes the rooms: B2 is sent back)`);
A.send((w) => encodeCmd(w, K.close));
await sleep(1500);
console.log(`  B2 saw: ${B2.br.map((x) => x?.kind).join(', ')}; A saw states: ${A.br.filter((x) => x?.kind === 'state').length}`);
B2.sock.destroy();
await sleep(800);
console.log(`block-mic      ${at()}  (A disables B's mic)`);
A.send((w) => { w.header(Op.Block); w.str(B.id); w.bool(true); w.bool(false); w.bits(0, 6); });
await sleep(1500);
console.log(`make-presenter ${at()}  (A makes B a presenter)`);
A.send((w) => { w.header(Op.SetRole); w.str(B.id); w.bits(2, 2); w.bits(0, 6); });
await sleep(1500);
console.log(`hand-up        ${at()}  (B raises a hand: a state delta, for comparison)`);
B.send((w) => encodeState(w, { participant: B.id, micMuted: true, camOff: true, sharing: false, handRaised: true, speaking: false, recording: false, role: 1 }));
await sleep(1500);
console.log(`lower-all      ${at()}  (A lowers all hands)`);
A.send((w) => w.header(Op.LowerHands));
await sleep(1500);
console.log(`remove         ${at()}  (A removes C)`);
A.send((w) => { w.header(Op.Remove); w.str(C.id); });
await sleep(1500);
console.log(`qna-ask        ${at()}  (B asks a 30-character question over HTTP; the room is pushed it)`);
const csrfOf = (cookie) => (cookie.split('; ').find((c) => c.startsWith('aether_csrf=')) ?? '').split('=')[1];
let qid = '';
{
  const r = await fetch(`${BASE}/api/scheduling/rooms/${room}/qna`, { method: 'POST', headers: { cookie: cb, 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(cb) }, body: JSON.stringify({ body: 'When does the new plan start?', anonymous: false }) });
  qid = (await r.json()).id;
  console.log(`  -> ${r.status}`);
}
await sleep(1500);
console.log(`qna-like       ${at()}  (A likes it)`);
{
  const r = await fetch(`${BASE}/api/scheduling/rooms/${room}/qna/${qid}/like`, { method: 'POST', headers: { cookie: ca, 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(ca) }, body: '{}' });
  console.log(`  -> ${r.status}`);
}
await sleep(1500);
console.log(`notes-create   ${at()}  (A adds meeting notes; the room is told)`);
await fetch(`${BASE}/api/scheduling/rooms/${room}/notes`, { method: 'POST', headers: { cookie: ca, 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(ca) }, body: '{}' });
await sleep(1500);
console.log(`notes-add      ${at()}  (B adds a 13-character agenda item; the room is pushed it)`);
await fetch(`${BASE}/api/scheduling/rooms/${room}/notes/items`, { method: 'POST', headers: { cookie: cb, 'content-type': 'application/json', 'X-Aether-CSRF': csrfOf(cb) }, body: JSON.stringify({ section: 'agenda', body: 'Budget review' }) });
await sleep(1500);
console.log(`attendance     ${at()}  (A downloads the attendance list over HTTP)`);
{
  const r = await fetch(`${BASE}/api/scheduling/rooms/${room}/attendance`, { headers: { cookie: ca } });
  const body = await r.text();
  console.log(`  -> ${r.status}, ${Buffer.byteLength(body)} B of CSV`);
}
await sleep(1500);
console.log(`wb-open        ${at()}  (A turns the whiteboard on)`);
A.send(encodeOpen);
await sleep(1500);
// B draws one 200-point stroke over 2 s, as a 100 Hz pointer would: the
// client sends what is new every 50 ms (repeating the last point so the
// preview has no gaps), then the whole stroke when the pen lifts.
const pts = [];
for (let i = 0, x = 600, y = 1100; i < 200; i++, x += 14) pts.push(x, Math.round(y + 120 * Math.sin(i / 12)));
console.log(`wb-live        ${at()}  (B draws for 2 s: one live frame per 50 ms)`);
let sent = 0;
for (let t = 1; t <= 40; t++) {
  await sleep(50);
  const have = Math.min(pts.length, t * 10);
  const from = Math.max(0, sent - 2);
  const chunk = pts.slice(from, have);
  B.send((w) => encodeStroke(w, W.live, 1, 0, 2, 3, chunk));
  sent = from + chunk.length;
}
console.log(`wb-stroke      ${at()}  (B lifts the pen: the 200-point stroke is committed)`);
B.send((w) => encodeStroke(w, W.stroke, 1, 0, 2, 3, pts));
await sleep(1500);
console.log(`wb-note        ${at()}  (A adds a 16-character sticky note)`);
A.send((w) => encodeNote(w, 0, 300, 200, 8, 'Decide by Friday'));
await sleep(1500);
const sid = A.wb.find((x) => x?.kind === 'stroke')?.id;
console.log(`wb-erase       ${at()}  (B erases the stroke, id ${sid})`);
B.send((w) => encodeSimple(w, W.erase, sid));
await sleep(1500);
console.log(`wb-clear       ${at()}  (A clears the board)`);
A.send((w) => encodeSimple(w, W.clear));
await sleep(1500);
console.log(`wb-close       ${at()}  (A turns it off)`);
A.send((w) => encodeSimple(w, W.close));
await sleep(1500);
console.log(`  whiteboard frames seen: A ${A.wb.length} (${A.wb.map((x) => x?.kind ?? 'BAD').join(',')}), B ${B.wb.length} (${B.wb.map((x) => x?.kind ?? 'BAD').join(',')}); B last ops ${B.ops.slice(-8).map((o) => o.toString(16))}`);
console.log(`rec-refused    ${at()}  (A asks to record; this deployment has recording off: A alone is told)`);
A.send((w) => encodeRecSet(w, REC.recording, true));
await sleep(1500);
console.log(`tr-start      ${at()}  (A starts transcription: everyone is told)`);
A.send((w) => encodeRecSet(w, REC.transcription, true));
await sleep(1500);
console.log(`cap-on        ${at()}  (B turns on live captions: everyone is told someone wants them)`);
B.send((w) => encodeRecSet(w, REC.captions, true));
await sleep(1500);
// A says a 46-character sentence over 3 s: the client sends the whole
// current text at most every 350 ms, then the final line.
const sentence = 'We will ship the new plan on the first of March';
console.log(`speech        ${at()}  (A speaks for 3 s: partials every 350 ms, then the final line)`);
for (let i = 1; i <= 8; i++) {
  A.send((w) => encodeCaption(w, false, 1, sentence.slice(0, Math.round((sentence.length * i) / 9))));
  await sleep(350);
}
A.send((w) => encodeCaption(w, true, 1, sentence));
await sleep(1500);
console.log(`  recording frames seen: A ${A.rec.map((x) => x?.kind).join(',')}; B ${B.rec.map((x) => x?.kind + (x?.final ? '(final)' : '')).join(',')}`);
// Together mode: A applies it; B sends the outline of a head and shoulders
// (256 x 256 confidences, as the segmenter gives them, quantized to 64 x 36).
console.log(`tog-on         ${at()}  (A applies Together mode, Café, for everyone)`);
A.send((w) => encodeTogSet(w, true, 1));
await sleep(1500);
const conf = new Float32Array(256 * 256);
for (let y = 0; y < 256; y++) for (let x = 0; x < 256; x++) conf[y * 256 + x] = (((x - 128) / 40) ** 2 + ((y - 80) / 50) ** 2 < 1) || (y > 140 && x > 50 && x < 206) ? 0.95 : 0.03;
const rle = encodeMask(quantize(conf, 256, 256));
console.log(`tog-outline    ${at()}  (B sends its outline: ${rle.length} B of run lengths)`);
B.send((w) => encodeOutline(w, rle));
await sleep(1500);
console.log(`tog-off        ${at()}  (A turns it off)`);
A.send((w) => encodeTogSet(w, false, 0));
await sleep(1500);
console.log(`  together frames seen: A ${A.tog.map((x) => x?.kind).join(',')}; B ${B.tog.map((x) => x?.kind).join(',')}`);
console.log(`end-meeting    ${at()}`);
A.send((w) => w.header(Op.EndMeeting));
await sleep(1500);
const n = (p, op) => p.ops.filter((o) => o === op).length;
for (const [p, l] of [[A, 'A'], [B, 'B'], [C, 'C']]) console.log(`${l} saw spotlight ${n(p, Op.Spotlight)}x, role ${n(p, Op.SetRole)}x, lower ${n(p, Op.LowerHands)}x, remove ${n(p, Op.Remove)}x, end ${n(p, Op.EndMeeting)}x`);
for (const p of [A, B, C]) p.sock.destroy();
