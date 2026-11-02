// The voice leveler: an automatic gain control after the noise suppressor.
//
// A person sitting back from a laptop arrives 15-20 dB quieter than one
// leaning in, and nothing in the chain made up the difference (measured: a
// voice at -38 dBFS in was -38 dBFS out). This brings speech to a steady
// talking level and holds it there:
//
//   - it listens only to speech: a 10 ms frame counts when it stands 9 dB
//     above a tracked noise floor, so a fan, a hum or the suppressor's
//     residue is never "turned up to talking level";
//   - the gain moves slowly up (6 dB/s) and faster down (24 dB/s), and holds
//     through pauses, so there is no pumping between words;
//   - it is bounded: at most +24 dB, at least -12 dB;
//   - a peak limiter at -1 dBFS catches the first loud syllable before the
//     gain has come down, so nothing clips.
//
// No lookahead, so no added delay. Pure: samples in, samples out; the
// AudioWorklet wrapper is leveler.worklet.js.
export const RATE = 48000;
export const FRAME = 480; // 10 ms
export const TARGET_DB = -20;
export const MAX_GAIN_DB = 24;
export const MIN_GAIN_DB = -12;
export const SPEECH_OVER_FLOOR_DB = 9;
export const UP_DB_PER_S = 6;
export const DOWN_DB_PER_S = 24;
export const LIMIT = 0.891; // -1 dBFS
export const ACQUIRE_FRAMES = 20; // 200 ms of speech before the first level
// Speech more than this far below your tracked level is somebody else.
export const FOREGROUND_DB = 6;
// ...unless it is all there is for this long: then you moved, re-acquire.
export const REACQUIRE_FRAMES = 200; // 2 s

const dbToLin = (d) => Math.pow(10, d / 20);
const linToDb = (v) => 20 * Math.log10(v + 1e-12);

export function createLeveler() {
  let floorDb = null; // noise floor estimate; starts at the first frame heard
  let speechDb = null; // smoothed speech level; null until speech is heard
  let gainDb = 0; // applied gain
  let speechFrames = 0;
  let backgroundRun = 0; // consecutive speech frames judged background
  let backgroundSum = 0; // their mean level, for re-acquisition
  let frameSum = 0;
  let frameN = 0;
  let limitEnv = 1; // limiter gain, 1 = no reduction
  const step = FRAME / RATE; // seconds per frame
  const limitRelease = Math.exp(-1 / (0.05 * RATE)); // 50 ms

  function endFrame() {
    const lv = linToDb(Math.sqrt(frameSum / frameN));
    frameSum = 0;
    frameN = 0;
    // Floor: follows down at once, creeps up at 3 dB/s, so a burst of
    // speech does not drag it up but a noisier room does, within seconds.
    // It starts at the first frame rather than a guess: a guessed -60
    // took a -50 dBFS fan for speech and turned it up 2 dB (leveler.test).
    floorDb = floorDb == null || lv < floorDb ? lv : Math.min(lv, floorDb + 3 * step);
    let isSpeech = lv > floorDb + SPEECH_OVER_FLOOR_DB && lv > -70;
    // Follow the nearest talker, not the room. Once locked on, speech far
    // below the tracked level is background -- a crowd, the next desk --
    // and neither moves the level nor the gain. Without this, bursts of a
    // crowd between your words (RNNoise lets them through there) dragged
    // the tracked level 8 dB down and the gain up in every pause (voicebench:
    // your pauses rose 8-10 dB, your voice 4-6). If only quieter speech is
    // heard for 2 s, that is you, further away: re-acquire at that level.
    if (isSpeech && speechFrames >= ACQUIRE_FRAMES && lv < speechDb - FOREGROUND_DB) {
      backgroundSum += lv;
      if (++backgroundRun < REACQUIRE_FRAMES) {
        isSpeech = false;
      } else {
        speechDb = backgroundSum / backgroundRun;
        backgroundRun = 0;
        backgroundSum = 0;
        // Re-acquired: go to the new level at once, as at the start.
        gainDb = Math.max(MIN_GAIN_DB, Math.min(MAX_GAIN_DB, TARGET_DB - speechDb));
        return;
      }
    } else if (isSpeech) {
      backgroundRun = 0;
      backgroundSum = 0;
    }
    if (isSpeech) {
      speechFrames++;
      // The first 200 ms of speech are averaged plainly (a fair first
      // estimate), then smoothed with a ~200 ms time constant.
      speechDb = speechDb == null ? lv : speechDb + (speechFrames <= ACQUIRE_FRAMES ? 1 / speechFrames : 0.05) * (lv - speechDb);
    }
    if (speechDb == null) return; // nothing to level yet: hold 0 dB
    const want = Math.max(MIN_GAIN_DB, Math.min(MAX_GAIN_DB, TARGET_DB - speechDb));
    // Only move while someone is talking; hold through pauses.
    if (!isSpeech) return;
    // Acquire: after the first 200 ms of speech, go straight to the level
    // (the limiter covers the step). Ramping 18 dB at 6 dB/s left a quiet
    // talker's first three seconds as quiet as before (voicebench).
    if (speechFrames === ACQUIRE_FRAMES) {
      gainDb = want;
      return;
    }
    if (speechFrames < ACQUIRE_FRAMES) return;
    const d = want - gainDb;
    gainDb += d > 0 ? Math.min(d, UP_DB_PER_S * step) : Math.max(d, -DOWN_DB_PER_S * step);
  }

  // Process in place-safe: reads input, writes output (may be the same array).
  function process(input, output) {
    let g0 = dbToLin(gainDb);
    for (let i = 0; i < input.length; i++) {
      const x = input[i];
      frameSum += x * x;
      if (++frameN === FRAME) {
        endFrame();
        g0 = dbToLin(gainDb);
      }
      let y = x * g0;
      const a = Math.abs(y) * limitEnv;
      if (a > LIMIT) limitEnv = LIMIT / Math.abs(y);
      else limitEnv = 1 - (1 - limitEnv) * limitRelease;
      y *= limitEnv;
      output[i] = y;
    }
  }

  return {
    process,
    get gainDb() {
      return gainDb;
    },
    get speechDb() {
      return speechDb;
    },
  };
}
