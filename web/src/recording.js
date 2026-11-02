// Recording, transcription and live captions: the wire half (OpRecording
// 0x102, engine/internal/signal/recording.go is the other) and the parts of
// the local recorder that can be tested without a browser.
//
// Teams' words, from its package: "Start recording", "Stop recording",
// "Start transcription", "Stop transcription", "This meeting is being
// recorded.", "{{initiatorName}} owns the recording. It'll be saved to their
// computer".

import { Op } from './wire.js';

export const WHAT = { recording: 0, transcription: 1, captions: 2 };
export const MAX_CAPTION_BYTES = 500;

export function encodeSet(w, what, on) {
  w.header(Op.Recording);
  w.bits(0, 3);
  w.bits(what, 2);
  w.bool(on);
  w.bits(0, 2);
}

// The text is cut at a character boundary to the bound: a frame past it is
// dropped whole by the server, and a sentence that long is better shown cut
// than not at all.
export function clampUtf8(text, max = MAX_CAPTION_BYTES) {
  const b = new TextEncoder().encode(text);
  if (b.length <= max) return b;
  let n = max;
  while (n > 0 && (b[n] & 0xc0) === 0x80) n--; // not inside a character
  return b.slice(0, n);
}

export function encodeCaption(w, final, utt, text) {
  w.header(Op.Recording);
  w.bits(1, 3);
  w.bool(final);
  w.bits(0, 4);
  w.uvarint(utt);
  w.blob(clampUtf8(text));
}

export function decodeRecording(r) {
  const kind = r.bits(3);
  if (kind === 0) {
    const s = { kind: 'state', recording: r.bool(), transcribing: r.bool(), captions: r.bool(), recBy: '', recSince: 0, trBy: '', trSince: 0 };
    r.bits(2);
    if (s.recording) {
      s.recBy = r.str();
      s.recSince = Number(r.uvarint());
    }
    if (s.transcribing) {
      s.trBy = r.str();
      s.trSince = Number(r.uvarint());
    }
    return r.err ? null : s;
  }
  if (kind === 1) {
    const final = r.bool();
    r.bits(4);
    const speaker = r.str();
    const utt = Number(r.uvarint());
    const text = new TextDecoder().decode(r.blob());
    return r.err ? null : { kind: 'caption', final, speaker, utt, text };
  }
  if (kind === 2) {
    const what = r.bits(2), reason = r.bits(1);
    r.bits(2);
    return r.err ? null : { kind: 'refused', what, reason: reason === 0 ? 'deployment' : 'role' };
  }
  return null;
}

// The banner, in Teams' words.
export function banner(s) {
  if (s.recording && s.transcribing) return 'This meeting is being recorded and transcribed.';
  if (s.recording) return 'This meeting is being recorded.';
  if (s.transcribing) return 'This meeting is being transcribed.';
  return '';
}

// "00:12" under an hour, "1:02:03" after.
export function elapsed(ms) {
  const t = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(t / 3600), m = Math.floor(t / 60) % 60, s = String(t % 60).padStart(2, '0');
  return h ? `${h}:${String(m).padStart(2, '0')}:${s}` : `${String(m).padStart(2, '0')}:${s}`;
}

// The best container the browser can write, VP9 first: at the same quality
// it is the smaller file, and nothing here caps the bitrate (rule 3).
export const MIME_PREFERENCE = ['video/webm;codecs=vp9,opus', 'video/webm;codecs=vp8,opus', 'video/webm', 'video/mp4'];
export function pickMime(isSupported) {
  return MIME_PREFERENCE.find((m) => isSupported(m)) ?? '';
}

// Without a file to stream into, the recording is held in memory, and memory
// is bounded: past the cap the recording stops and is saved, rather than
// taking the tab down with it.
export const MEMORY_CAP = 1 << 30; // 1 GiB, a little under an hour at 2.5 Mbit/s
export class ChunkSink {
  constructor(cap = MEMORY_CAP) {
    this.cap = cap;
    this.chunks = [];
    this.bytes = 0;
  }
  // false once the next chunk would pass the cap; that chunk is not kept.
  push(blob) {
    if (this.bytes + blob.size > this.cap) return false;
    this.chunks.push(blob);
    this.bytes += blob.size;
    return true;
  }
}

// "Meeting recording 2026-11-02 1405.webm"
export function fileName(date, mime) {
  const p = (n) => String(n).padStart(2, '0');
  const ext = mime.startsWith('video/mp4') ? 'mp4' : 'webm';
  return `Meeting recording ${date.getFullYear()}-${p(date.getMonth() + 1)}-${p(date.getDate())} ${p(date.getHours())}${p(date.getMinutes())}.${ext}`;
}
