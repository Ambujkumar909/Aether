// Aether's own sounds: synthesised, never sampled. A small set of cues in
// one voice -- a soft, rounded tone (a sine with a quiet triangle on top,
// a fast attack and a long tail) on a pentatonic motif -- so the ring, a
// message and a hand going up are plainly the same instrument saying
// different things. Every cue is a score: notes with a start, a pitch, a
// length and a loudness, rendered by Web Audio when it is played. No files,
// no network, nothing to download.
//
// What plays when is a policy (shouldPlay): do not disturb and quiet hours
// silence all but a call; the person's own level in the console silences
// more. The policy is a pure function so it is tested without a speaker.

// Pitches of the motif, in Hz. The major pentatonic on A, two octaves: it
// has no semitone clashes, so any two notes sound right together.
const A4 = 440;
const N = {
  A4,
  B4: A4 * 2 ** (2 / 12),
  Cs5: A4 * 2 ** (4 / 12),
  E5: A4 * 2 ** (7 / 12),
  Fs5: A4 * 2 ** (9 / 12),
  A5: A4 * 2,
  B5: A4 * 2 * 2 ** (2 / 12),
  Cs6: A4 * 2 * 2 ** (4 / 12),
  E6: A4 * 2 * 2 ** (7 / 12),
  A3: A4 / 2,
  E4: A4 * 2 ** (-5 / 12),
};

// A note: when it starts (s), its pitch (Hz), how long it rings (s), how
// loud (0..1).
const note = (at, hz, len, gain = 0.5) => ({ at, hz, len, gain });

// CUES are the whole set. Durations are the end of the last tail.
export const CUES = {
  // Someone is calling: three rising notes, then a breath, repeated by the
  // caller until answered (ringLoop below). Warm, not a bell.
  ring: { notes: [note(0, N.A4, 0.55, 0.45), note(0.22, N.Cs5, 0.55, 0.45), note(0.44, N.E5, 0.9, 0.5)], length: 1.4, repeat: 3.2 },
  // You are calling: one low, patient tone, on and off, like a line
  // ringing far away; unmistakably not the incoming one.
  ringback: { notes: [note(0, N.A3, 1.1, 0.3), note(0, N.E4, 1.1, 0.12)], length: 1.3, repeat: 3.0 },
  // A new message you were not looking at: two quick notes, up.
  message: { notes: [note(0, N.E5, 0.22, 0.35), note(0.1, N.A5, 0.45, 0.35)], length: 0.6 },
  // You were named, or it is urgent: three quick notes, up.
  mention: { notes: [note(0, N.E5, 0.2, 0.4), note(0.1, N.A5, 0.2, 0.4), note(0.2, N.Cs6, 0.5, 0.42)], length: 0.75 },
  // Somebody joined the meeting: a soft step up.
  join: { notes: [note(0, N.A4, 0.18, 0.22), note(0.09, N.E5, 0.4, 0.22)], length: 0.5 },
  // Somebody left: the same step, down.
  leave: { notes: [note(0, N.E5, 0.18, 0.2), note(0.09, N.A4, 0.4, 0.2)], length: 0.5 },
  // A hand went up: one clear note.
  hand: { notes: [note(0, N.Cs6, 0.5, 0.3)], length: 0.55 },
  // The call ended: two notes, down, slow.
  ended: { notes: [note(0, N.E5, 0.4, 0.3), note(0.3, N.A4, 0.8, 0.3)], length: 1.15 },
  // Declined, or nobody answered: a low pair.
  declined: { notes: [note(0, N.E4, 0.4, 0.3), note(0.3, N.A3, 0.8, 0.3)], length: 1.15 },
};

// Levels a person can choose: everything, only what is about them (calls,
// mentions, direct messages), or nothing but the ring.
export const LEVELS = ['all', 'important', 'calls'];
const LEVEL_KEY = 'aether.sounds';

export function soundLevel() {
  try {
    const v = localStorage.getItem(LEVEL_KEY);
    return LEVELS.includes(v) ? v : 'all';
  } catch {
    return 'all';
  }
}

export function setSoundLevel(level) {
  if (!LEVELS.includes(level)) return;
  try {
    localStorage.setItem(LEVEL_KEY, level);
  } catch {}
}

// shouldPlay decides. `kind` is a CUES key; `about` says whether the cue is
// about this person (a direct message, a mention, their own call); dnd and
// quiet come from presence (chat.js).
export function shouldPlay(kind, { level = 'all', about = false, dnd = false, quiet = false, urgent = false } = {}) {
  if (!CUES[kind]) return false;
  const call = kind === 'ring' || kind === 'ringback' || kind === 'ended' || kind === 'declined';
  // Do not disturb silences everything but your own calls' progress and an
  // urgent message; quiet hours let through what is about you.
  if (dnd && !(kind === 'ringback' || kind === 'ended' || kind === 'declined' || urgent)) return false;
  if (quiet && !about && !call && !urgent) return false;
  switch (level) {
    case 'calls':
      return call;
    case 'important':
      return call || about || urgent || kind === 'mention';
    default:
      return true;
  }
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// voice schedules one note into a context: a sine for the body, a quiet
// triangle an octave up for a little air, a fast attack and an exponential
// tail, through a shared gain so cues never clip.
function voice(ctx, out, n, t0) {
  const start = t0 + n.at;
  const g = ctx.createGain();
  g.gain.setValueAtTime(0.0001, start);
  g.gain.exponentialRampToValueAtTime(n.gain, start + 0.012);
  g.gain.exponentialRampToValueAtTime(0.0001, start + n.len);
  g.connect(out);
  const body = ctx.createOscillator();
  body.type = 'sine';
  body.frequency.value = n.hz;
  body.connect(g);
  const air = ctx.createGain();
  air.gain.value = 0.12;
  air.connect(g);
  const top = ctx.createOscillator();
  top.type = 'triangle';
  top.frequency.value = n.hz * 2;
  top.connect(air);
  body.start(start);
  top.start(start);
  body.stop(start + n.len + 0.02);
  top.stop(start + n.len + 0.02);
}

// schedule plays a cue into ctx at t0 (seconds on ctx's clock) and returns
// when it is over.
export function schedule(ctx, cue, t0, master = 0.8) {
  const out = ctx.createGain();
  out.gain.value = master;
  out.connect(ctx.destination);
  for (const n of cue.notes) voice(ctx, out, n, t0);
  return t0 + cue.length;
}

let shared = null;
function context() {
  if (!shared) {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return null;
    shared = new AC();
  }
  if (shared.state === 'suspended') shared.resume().catch(() => {});
  return shared;
}

// play sounds one cue once, now. Returns false when the browser has no
// audio or the cue is unknown.
export function play(kind) {
  const cue = CUES[kind];
  const ctx = cue && context();
  if (!ctx) return false;
  try {
    schedule(ctx, cue, ctx.currentTime + 0.01);
    return true;
  } catch {
    return false;
  }
}

// ringLoop repeats a cue with `repeat` seconds between starts until stop()
// is called: the incoming ring and the outgoing ringback.
export function ringLoop(kind) {
  const cue = CUES[kind];
  const ctx = cue && context();
  if (!ctx || !cue.repeat) return { stop() {} };
  let timer = 0;
  const tick = () => {
    try {
      schedule(ctx, cue, ctx.currentTime + 0.01);
    } catch {}
  };
  tick();
  timer = setInterval(tick, cue.repeat * 1000);
  return {
    stop() {
      clearInterval(timer);
      timer = 0;
    },
  };
}

// renderOffline renders a cue to samples with no speaker, for the harness:
// peak and length say the cue is audible and bounded.
export async function renderOffline(kind, rate = 44100) {
  const cue = CUES[kind];
  if (!cue) return null;
  const frames = Math.ceil(cue.length * rate);
  const ctx = new OfflineAudioContext(1, frames, rate);
  schedule(ctx, cue, 0);
  const buf = await ctx.startRendering();
  const data = buf.getChannelData(0);
  let peak = 0;
  let last = 0;
  for (let i = 0; i < data.length; i++) {
    const a = Math.abs(data[i]);
    if (a > peak) peak = a;
    if (a > 0.002) last = i;
  }
  return { peak, seconds: last / rate, length: cue.length };
}
