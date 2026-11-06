// Aether binary control protocol -- browser implementation.
//
// This file is the exact mirror of engine/internal/wire. Divergence between
// the two is not a bug that degrades gracefully; it desynchronises the
// interning dictionaries and every subsequent frame decodes to garbage. Any
// change here requires the identical change there, and the size assertions
// in wire_test.go are the contract both sides are held to.

export const VERSION = 0b01;

const FLAG_REQ_ID = 1 << 5;
const FLAG_EXT = 1 << 4;
const MASK_OP = 0x0f;
const MASK_VER = 0xc0;

export const Op = {
  Ping: 0,
  Pong: 1,
  Batch: 2,
  StateDelta: 3,
  Speakers: 4,
  LayerReq: 5,
  LayerGrant: 6,
  BweReport: 7,
  Subscribe: 8,
  Unsubscribe: 9,
  KeyframeReq: 10,
  Sdp: 11,
  Ice: 12,
  Roster: 13,
  MediaFrame: 14, // one-byte header for the most frequent frame; see opcode.go
  Error: 15,
  // Extended, two control bytes.
  Hello: 0x100,
  Welcome: 0x101,
  Recording: 0x102,
  ScreenShare: 0x103,
  Breakout: 0x104,
  QualityCap: 0x105,
  StatsDump: 0x106,
  Goodbye: 0x107,
  Viewport: 0x108,
  ConvMessage: 0x109,
  Typing: 0x10a,
  ConvRead: 0x10b,
  Presence: 0x10c,

  // Remote control.
  ControlState: 0x10d,
  ControlInput: 0x10e,
  ControlFrame: 0x10f,

  // The waiting room. A knocker receives LobbyStatus and nothing else: until
  // a host admits them they are not in the meeting, so no roster, no chat and
  // no media frame is addressed to them.
  LobbyStatus: 0x110,
  LobbyList: 0x111,
  LobbyDecide: 0x112,
  MeetingPolicy: 0x113,

  // Per-meeting host controls. Enforced on the server; the client's copy
  // only greys out buttons.
  Permissions: 0x114,
  MuteAll: 0x115,
  Chat: 0x116,
  Ring: 0x117,
  RingAnswer: 0x118,

  // Live captions, recognised on the speaker's machine and sent as text.
  Caption: 0x119,
  // A reaction: one emoji, thrown at the room and gone.
  Reaction: 0x11a,
  ReadCursor: 0x11b,
  Status: 0x11c,
  Idle: 0x11d,
  SameDevice: 0x11e,
  // Host puts people on everybody's stage; the whole list, only on change.
  Spotlight: 0x11f,
  // Host ends the meeting for everyone; sent to each just before the close.
  EndMeeting: 0x120,
  // Roles (attendee / presenter / co-organizer), "Remove from meeting",
  // "Lower all hands". Server side done; client handlers not yet written
  // (docs/RESUME.md).
  SetRole: 0x121,
  Remove: 0x122,
  LowerHands: 0x123,
  // "Disable mic" / "Disable camera" for one attendee.
  Block: 0x124,
  // A Q&A change, as JSON, whole.
  QnA: 0x125,
  // A meeting-notes change, as JSON, whole.
  Notes: 0x126,
  // The whiteboard: open, strokes, live previews, erase, notes, clear, close.
  Whiteboard: 0x127,
  Together: 0x128,
  Activity: 0x129,
  Slots: 0x12a,
};

// Which of the two peer connections a signalling frame concerns.
export const Dir = { Publisher: 0, Subscriber: 1 };

export const Role = { Guest: 0, Member: 1, Moderator: 2, Owner: 3 };

// The meeting-scoped authority: admitting people, locking the meeting,
// changing who has to wait. Deliberately the same value as Owner and
// deliberately separate in meaning from the account roles above -- being an
// administrator of the deployment does not make you host of a meeting you
// happened to walk into.
export const RoleHost = Role.Owner;

// Who has to wait. Anything unrecognised is treated as the strictest, which
// is the only safe direction for a parsing mistake to fail in.
export const Admission = { Open: 'open', Trusted: 'trusted', Knock: 'knock' };

// Must match wire.Preload in Go, in order.
export const PRELOAD = [
  'opus', 'av1', 'vp9', 'vp8', 'h264',
  'cam', 'mic', 'screen', 'screen-audio',
  'f', 'h', 'q',
  'active', 'muted', 'hidden', 'paused',
];

const MAX_DICT_ENTRIES = 4096;
const MAX_INTERNED_STRING = 512;

// Dict is the append-only interning table. One per direction -- see the Go
// implementation for why sharing a single table across both directions
// corrupts the session.
export class Dict {
  constructor() {
    this.byStr = new Map();
    this.byHandle = [];
    this.sealed = false;
  }
  handle(s) {
    const h = this.byStr.get(s);
    return h === undefined ? null : h;
  }
  lookup(h) {
    return h < this.byHandle.length ? this.byHandle[h] : null;
  }
  intern(s) {
    if (s.length > MAX_INTERNED_STRING) return;
    if (this.sealed || this.byStr.has(s)) return;
    if (this.byHandle.length >= MAX_DICT_ENTRIES) {
      this.sealed = true;
      return;
    }
    this.byStr.set(s, this.byHandle.length);
    this.byHandle.push(s);
  }
  get size() {
    return this.byHandle.length;
  }
}

export function newDictPair() {
  const tx = new Dict();
  const rx = new Dict();
  for (const s of PRELOAD) {
    tx.intern(s);
    rx.intern(s);
  }
  return { tx, rx };
}

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export class Writer {
  constructor(dict = null) {
    this.buf = [];
    this.acc = 0;
    this.nbit = 0;
    this.dict = dict;
  }
  reset() {
    this.buf = [];
    this.acc = 0;
    this.nbit = 0;
  }
  align() {
    if (this.nbit > 0) {
      this.buf.push((this.acc << (8 - this.nbit)) & 0xff);
      this.acc = 0;
      this.nbit = 0;
    }
  }
  bytes() {
    this.align();
    return new Uint8Array(this.buf);
  }
  bits(v, n) {
    while (n > 0) {
      let take = 8 - this.nbit;
      if (take > n) take = n;
      const chunk = (v >>> (n - take)) & ((1 << take) - 1);
      this.acc = ((this.acc << take) | chunk) & 0xff;
      this.nbit += take;
      n -= take;
      if (this.nbit === 8) {
        this.buf.push(this.acc);
        this.acc = 0;
        this.nbit = 0;
      }
    }
  }
  bool(b) {
    this.bits(b ? 1 : 0, 1);
  }
  u8(v) {
    this.align();
    this.buf.push(v & 0xff);
  }
  uvarint(v) {
    this.align();
    // Values above 2^31 are rare here; Number is exact to 2^53 which covers
    // every field this protocol carries, including millisecond epochs.
    while (v >= 0x80) {
      this.buf.push((v & 0x7f) | 0x80);
      v = Math.floor(v / 128);
    }
    this.buf.push(v & 0x7f);
  }
  svarint(v) {
    this.uvarint(v >= 0 ? v * 2 : -v * 2 - 1);
  }
  // Q8 quantises a 0..1 ratio into one byte.
  q8(v) {
    if (v <= 0) return this.u8(0);
    if (v >= 1) return this.u8(255);
    this.u8(Math.round(v * 255));
  }
  blob(bytes) {
    this.uvarint(bytes.length);
    this.align();
    for (const b of bytes) this.buf.push(b);
  }
  // raw writes bytes with no length: the last field of a frame, whose end is
  // the frame's end (the WebSocket message, or the batch's per-frame length).
  raw(bytes) {
    this.align();
    for (const b of bytes) this.buf.push(b);
  }
  str(s) {
    if (!this.dict) {
      const raw = encoder.encode(s);
      this.uvarint(raw.length * 2 + 1);
      this.align();
      for (const b of raw) this.buf.push(b);
      return;
    }
    const h = this.dict.handle(s);
    if (h !== null) {
      this.uvarint(h * 2);
      return;
    }
    const raw = encoder.encode(s);
    this.uvarint(raw.length * 2 + 1);
    this.align();
    for (const b of raw) this.buf.push(b);
    this.dict.intern(s);
  }
  header(op, reqID = 0, hasID = false) {
    let ctrl = (VERSION << 6) | (op & MASK_OP);
    if (op > 0x0f) ctrl = (VERSION << 6) | FLAG_EXT | ((op >> 8) & MASK_OP);
    if (hasID) ctrl |= FLAG_REQ_ID;
    this.u8(ctrl);
    if (hasID) this.uvarint(reqID);
    if (op > 0x0f) this.u8(op & 0xff);
  }
}

export class Reader {
  constructor(bytes, dict = null) {
    this.buf = bytes;
    this.pos = 0;
    this.acc = 0;
    this.nbit = 0;
    this.dict = dict;
    this.err = null;
  }
  fail(e) {
    if (!this.err) this.err = e;
  }
  get remaining() {
    return Math.max(0, this.buf.length - this.pos);
  }
  align() {
    this.acc = 0;
    this.nbit = 0;
  }
  bits(n) {
    let out = 0;
    while (n > 0) {
      if (this.nbit === 0) {
        if (this.pos >= this.buf.length) {
          this.fail('truncated');
          return 0;
        }
        this.acc = this.buf[this.pos++];
        this.nbit = 8;
      }
      let take = this.nbit;
      if (take > n) take = n;
      const shift = this.nbit - take;
      const chunk = (this.acc >>> shift) & ((1 << take) - 1);
      out = (out << take) | chunk;
      this.nbit -= take;
      n -= take;
    }
    return out;
  }
  bool() {
    return this.bits(1) === 1;
  }
  u8() {
    this.align();
    if (this.pos >= this.buf.length) {
      this.fail('truncated');
      return 0;
    }
    return this.buf[this.pos++];
  }
  uvarint() {
    this.align();
    let v = 0;
    let mul = 1;
    for (let i = 0; i < 10; i++) {
      if (this.pos >= this.buf.length) {
        this.fail('truncated');
        return 0;
      }
      const b = this.buf[this.pos++];
      v += (b & 0x7f) * mul;
      if (b < 0x80) return v;
      mul *= 128;
    }
    this.fail('bad varint');
    return 0;
  }
  svarint() {
    const v = this.uvarint();
    return v % 2 === 0 ? v / 2 : -(v + 1) / 2;
  }
  q8() {
    return this.u8() / 255;
  }
  blob() {
    const n = this.uvarint();
    this.align();
    if (this.pos + n > this.buf.length) {
      this.fail('truncated');
      return new Uint8Array(0);
    }
    const b = this.buf.subarray(this.pos, this.pos + n);
    this.pos += n;
    return b;
  }
  // tail returns everything left in the frame. See Writer.raw.
  tail() {
    this.align();
    const b = this.buf.subarray(this.pos);
    this.pos = this.buf.length;
    return b;
  }
  str() {
    const tag = this.uvarint();
    if (this.err) return '';
    if (tag % 2 === 0) {
      const h = tag / 2;
      if (!this.dict) {
        this.fail('bad handle');
        return '';
      }
      const s = this.dict.lookup(h);
      if (s === null) {
        this.fail('bad handle');
        return '';
      }
      return s;
    }
    const n = (tag - 1) / 2;
    if (n > MAX_INTERNED_STRING) {
      this.fail('string too long');
      return '';
    }
    this.align();
    if (this.pos + n > this.buf.length) {
      this.fail('truncated');
      return '';
    }
    const s = decoder.decode(this.buf.subarray(this.pos, this.pos + n));
    this.pos += n;
    if (this.dict) this.dict.intern(s);
    return s;
  }
  header() {
    const ctrl = this.u8();
    if (this.err) return null;
    if ((ctrl & MASK_VER) !== VERSION << 6) {
      this.fail('bad version');
      return null;
    }
    const h = { op: 0, reqID: 0, hasID: false };
    if (ctrl & FLAG_REQ_ID) {
      h.reqID = this.uvarint();
      h.hasID = true;
    }
    if (ctrl & FLAG_EXT) {
      h.op = ((ctrl & MASK_OP) << 8) | this.u8();
    } else {
      h.op = ctrl & MASK_OP;
    }
    return h;
  }
}

// encodeBatch packs several encoded frames into one Batch frame. A single
// frame is passed through unwrapped, so the one-byte fast path survives.
export function encodeBatch(frames) {
  if (frames.length === 1) return frames[0];
  const w = new Writer(null);
  w.header(Op.Batch);
  for (const f of frames) {
    w.uvarint(f.length);
    w.align();
    for (const b of f) w.buf.push(b);
  }
  return w.bytes();
}

// splitBatch walks a Batch body, yielding each contained frame.
export function splitBatch(r, fn) {
  while (r.remaining > 0) {
    const n = r.uvarint();
    if (r.err) return;
    r.align();
    if (n <= 0 || r.pos + n > r.buf.length) {
      r.fail('truncated');
      return;
    }
    fn(r.buf.subarray(r.pos, r.pos + n));
    r.pos += n;
  }
}

// --------------------------------------------------------------------------
// Message helpers
// --------------------------------------------------------------------------

// A participant state delta: three bytes once the id has been interned.
export function encodeState(w, s) {
  w.header(Op.StateDelta);
  w.str(s.participant);
  w.bool(!!s.micMuted);
  w.bool(!!s.camOff);
  w.bool(!!s.sharing);
  w.bool(!!s.handRaised);
  w.bool(!!s.speaking);
  w.bool(!!s.recording);
  w.bits(s.role ?? Role.Member, 2);
  // Present only while sharing, so a participant who is not presenting still
  // encodes to exactly three bytes. See the Go side for why the receiver
  // cannot work this out for itself: browser msid identifiers are UUIDs, so
  // there is nothing in a screen track that says it is a screen.
  if (s.sharing) w.str(s.screenTrack ?? '');
}

export function decodeState(r) {
  const s = {
    participant: r.str(),
    micMuted: r.bool(),
    camOff: r.bool(),
    sharing: r.bool(),
    handRaised: r.bool(),
    speaking: r.bool(),
    recording: r.bool(),
    role: r.bits(2),
  };
  s.screenTrack = s.sharing ? r.str() : '';
  return s;
}

export function encodeSpeakers(w, list) {
  w.header(Op.Speakers);
  w.uvarint(list.length);
  for (const s of list) {
    w.str(s.participant);
    w.q8(s.energy);
  }
}

export function decodeSpeakers(r) {
  const n = r.uvarint();
  if (r.err || n > 256) return [];
  const out = [];
  for (let i = 0; i < n; i++) {
    out.push({ participant: r.str(), energy: r.q8() });
  }
  return out;
}

export function encodeLayerReq(w, req) {
  w.header(Op.LayerReq);
  w.str(req.track);
  w.bits(req.spatial ?? 0, 2);
  w.bits(req.temporal ?? 0, 2);
  w.bits(req.reason ?? 0, 3);
  w.bool(!!req.paused);
}

export function encodeBwe(w, b) {
  w.header(Op.BweReport);
  w.uvarint(b.estimateKbps | 0);
  w.q8(b.lossFraction ?? 0);
  w.uvarint(b.jitterMs | 0);
  w.uvarint(b.rttMs | 0);
}

export function encodePing(w) {
  w.header(Op.Ping);
}

export function decodeRoster(r) {
  const present = r.bool();
  if (!present) return { present: false, id: r.str() };
  return {
    present: true,
    id: r.str(),
    name: r.str(),
    role: r.bits(2),
    // The organizer: the one person whose role nobody can change and who
    // cannot be removed (engine room/roles.go).
    organizer: r.bits(1) === 1,
    _reserved: r.bits(4),
    // The account behind the participant, empty for a guest. This is what
    // lets the meeting chat address a person instead of a tile.
    userId: r.str(),
  };
}

export function decodeWelcome(r) {
  return { id: r.str(), room: r.str(), epoch: r.uvarint() };
}

// --------------------------------------------------------------------------
// Media plane signalling
// --------------------------------------------------------------------------

export function encodeSDP(w, { dir, isOffer, body }) {
  w.header(Op.Sdp);
  w.bits(dir, 1);
  w.bool(isOffer);
  w.bits(0, 6);
  w.blob(new TextEncoder().encode(body));
}

export function decodeSDP(r) {
  const dir = r.bits(1);
  const isOffer = r.bool();
  r.bits(6);
  return { dir, isOffer, body: new TextDecoder().decode(r.blob()) };
}

// What each of this receiver's WebRTC media slots carries (engine sfu/slots.go):
// [{ slot, publisher, track }], publisher '' for an idle slot. The whole map
// every time, so a missed update is corrected by the next.
export function decodeSlots(r) {
  const n = r.uvarint();
  if (r.err || n > 256) return [];
  const out = [];
  for (let i = 0; i < n; i++) {
    out.push({ slot: r.str(), publisher: r.str(), track: r.str() });
  }
  return out;
}

export function encodeICE(w, { dir, candidate }) {
  w.header(Op.Ice);
  w.bits(dir, 1);
  w.bits(0, 7);
  w.blob(new TextEncoder().encode(candidate));
}

export function decodeICE(r) {
  const dir = r.bits(1);
  r.bits(7);
  return { dir, candidate: new TextDecoder().decode(r.blob()) };
}

// Media over the signalling socket. One frame per encoded chunk.
//
// Client to server carries no publisher: the server knows who sent it. Server
// to client prefixes the publisher id, interned like every other string, so
// after the first frame it costs one byte. kind: 0 audio, 1 video. key: this
// chunk is decodable on its own (a video keyframe; always true for audio).
// screen: this video frame is a shared screen, not a camera. Rides in the
// low bit of the spare field; the server forwards the field verbatim and uses
// this bit to apply the "share" control rather than "camera".
//
// Two fields are NOT here, on purpose, and each was measured before it went:
//   - Audio has no timestamp. Chunks are contiguous 20 ms of one stream over
//     an ordered transport; the receiver counts them, and the decoder needs
//     only a monotonic value. On the wire the capture time was a 3-4 byte
//     varint on every 20 ms frame, in both directions. Video keeps its
//     capture time: the decoder paces on it.
//   - The payload has no length. It is the last field, and the frame is
//     already delimited -- by the WebSocket message when it travels alone,
//     by the batch container's per-frame length when it does not. That was
//     one more byte per frame saying what the frame's end already said.
// A 120 B Opus frame (48 kbit/s x 20 ms) is 122 B client to server and 123 B
// server to client (one-byte header), against 127 before; tshark figures in
// ARCHITECTURE.md.
// Flags: bit 0 of the six spare bits marks a screen, bit 1 says the video is
// VP9 (else VP8). The server relays them untouched.
export function encodeMediaFrame(w, { kind, key, ts, payload, screen = false, vp9 = false }) {
  w.header(Op.MediaFrame);
  w.bits(kind & 1, 1);
  w.bits(key ? 1 : 0, 1);
  w.bits((screen ? 1 : 0) | (vp9 ? 2 : 0), 6);
  if (kind === 1) w.uvarint(ts);
  w.raw(payload);
}

export function decodeMediaFrame(r) {
  const publisher = r.str();
  const kind = r.bits(1);
  const key = r.bits(1) === 1;
  const spare = r.bits(6);
  const ts = kind === 1 ? r.uvarint() : 0;
  const payload = r.tail();
  return { publisher, kind, key, ts, payload, screen: (spare & 1) === 1, vp9: (spare & 2) === 2 };
}

// The whole gallery in one frame. A scroll moves every tile at once, and
// sending one frame per tile would pay twenty control headers to say a single
// thing.
export function encodeViewport(w, tiles) {
  w.header(Op.Viewport);
  w.uvarint(tiles.length);
  for (const t of tiles) {
    w.str(t.track);
    w.uvarint(t.width | 0);
    w.uvarint(t.height | 0);
    w.bool(!!t.visible);
    w.bool(!!t.pinned);
    w.bits(0, 6);
  }
}

export function encodeKeyframeReq(w, track) {
  w.header(Op.KeyframeReq);
  w.str(track);
}
