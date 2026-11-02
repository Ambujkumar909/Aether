// The receive-side clock for audio carried over the socket: when to play
// each decoded chunk so that speech is continuous and as prompt as the
// network allows.
//
// The old rule was a fixed 150 ms lead and a 400 ms ceiling. Measured on
// loopback, the lead did not stay at 150: a burst at start (or any moment
// the audio clock paused) scheduled chunks back to back ahead of the clock,
// the lead climbed towards the ceiling and sat there, and a voice reached
// the ear half a second after it reached the socket. This clock is
// adaptive, and judges the buffer by its low-water mark, never by one
// chunk -- chunks over TCP arrive in small bursts (two or three at once is
// normal) and a rule that reacted to each arrival tore the audio apart
// (measured: 25 of 34 fifty-millisecond windows silent).
//
//   - the target lead starts at FLOOR (60 ms);
//   - an underrun (a chunk arriving after its slot) is the network saying
//     the lead was too small: the target grows by STEP, up to MAX, and the
//     clock re-anchors at the new target;
//   - every WINDOW chunks, the smallest lead seen in that window is compared
//     with the target: the excess above target+SLACK is worked off by
//     dropping every other chunk until it is gone -- 20 ms of speech at a
//     time, at twice real time, so a 400 ms burst is absorbed in under a
//     second with a brief roughness rather than a half-second delay for the
//     rest of the call;
//   - after DECAY_AFTER seconds with no underrun the target eases back
//     toward the floor, a little per chunk.
//
// Pure: time in, decision out. The Web Audio scheduling lives in wsmedia.js.
export const FLOOR = 0.06;
export const MAX = 0.4;
export const STEP = 0.02;
export const SLACK = 0.04;
export const WINDOW = 50; // chunks (~1 s of 20 ms chunks)
export const DECAY_AFTER = 10;
export const DECAY_PER_CHUNK = 0.0005;
// No chunk for this long is a pause in the stream (the sender is muted and
// sends nothing), not a late chunk: the clock anchors afresh without
// growing the target.
export const GAP = 0.25;

export function createJitterClock({ floor = FLOOR, max = MAX, window = WINDOW } = {}) {
  let target = floor;
  let next = 0; // when the next chunk should start, in the audio clock
  let lastUnderrun = -Infinity;
  let underruns = 0;
  let trimmed = 0;
  let minAhead = Infinity;
  let inWindow = 0;
  let toDrop = 0; // seconds of excess still to work off
  let parity = 0;
  let lastNow = -Infinity;

  // Decide for one chunk of `duration` seconds arriving at audio time `now`.
  // Returns { at } to schedule it at `at`, or { drop: true } to skip it.
  function schedule(now, duration) {
    const afterGap = now - lastNow > GAP;
    lastNow = now;
    if (next === 0 || next < now) {
      if (next !== 0 && !afterGap) {
        underruns++;
        lastUnderrun = now;
        target = Math.min(max, target + STEP);
      }
      next = now + target;
      minAhead = Infinity;
      inWindow = 0;
      toDrop = 0;
    }
    const ahead = next - now;
    minAhead = Math.min(minAhead, ahead);
    if (++inWindow >= window) {
      // A quarter of a chunk of excess is noise, not a backlog.
      const excess = minAhead - (target + SLACK);
      if (excess > 0.005) toDrop = Math.max(toDrop, excess);
      minAhead = Infinity;
      inWindow = 0;
    }
    if (toDrop > 0 && (parity++ & 1) === 0) {
      toDrop -= duration;
      trimmed++;
      return { drop: true, ahead, target };
    }
    if (now - lastUnderrun > DECAY_AFTER && target > floor) {
      target = Math.max(floor, target - DECAY_PER_CHUNK);
    }
    const at = next;
    next += duration;
    return { at, ahead, target };
  }

  // The clock stood still (the audio context was suspended): forget the
  // schedule so the next chunk anchors afresh instead of playing late.
  function reset() {
    next = 0;
    minAhead = Infinity;
    inWindow = 0;
    toDrop = 0;
  }

  return {
    schedule,
    reset,
    get target() {
      return target;
    },
    get underruns() {
      return underruns;
    },
    get trimmed() {
      return trimmed;
    },
  };
}
