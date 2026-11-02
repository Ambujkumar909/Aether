// Live captions, recognised on the machine that is speaking.
//
// The recogniser is the browser's own, so this server never receives anyone's
// audio for transcription and pays no service by the minute; only the text
// reaches the meeting, a few dozen bytes where the audio is kilobytes a
// second. Where the browser cannot recognise on the device, its recogniser
// sends the audio to its maker's speech service (Chrome: Google's). That is a
// third party hearing the meeting, so on-device is used whenever the browser
// offers it (speechOnDevice), and a speaker is always asked first, told which
// it will be (app.js renderRecBanner).
//
// What goes on the wire is the full current sentence, every time. It is never
// a delta. A caption that arrived as "and then we" + " should ship" would be
// wrong forever if the middle frame were dropped, and recovering would cost
// more than resending -- which is the cross-frame-state rule this codebase
// keeps, applied to text.

// Interim results arrive many times a second. Sending each one would put more
// frames on the wire than the media path uses for a whole camera. This is the
// floor between interim sends; a final result is never delayed.
export const INTERIM_MS = 350;

/**
 * Whether this browser can recognise speech on the device itself, for this
 * language, right now. Where it cannot, the browser's recogniser sends the
 * audio to its maker's speech service (Chrome: Google's) -- which is why a
 * speaker is asked before their speech is turned into text.
 */
export async function speechOnDevice(lang = navigator.language || 'en-US') {
  const Impl = typeof window !== 'undefined' && (window.SpeechRecognition || window.webkitSpeechRecognition);
  try {
    return (await Impl?.available?.({ langs: [lang], processLocally: true })) === 'available';
  } catch {
    return false;
  }
}

/** Whether this browser can recognise speech at all. */
export function captionsSupported() {
  return typeof window !== 'undefined' &&
    !!(window.SpeechRecognition || window.webkitSpeechRecognition);
}

/**
 * Decide whether an interim result is worth sending yet.
 *
 * Two reasons to hold one back: it arrived too soon after the last, or it says
 * exactly what the last one said. Recognisers emit the same interim string
 * repeatedly while they think, and every one of those is a frame that changes
 * nothing.
 *
 * @param {{last: number, text: string}} state
 * @param {string} text
 * @param {boolean} isFinal
 * @param {number} now
 */
export function shouldSend(state, text, isFinal, now) {
  const t = String(text ?? '').trim();
  if (!t) return false;
  if (isFinal) return true;
  if (t === state.text) return false;
  return now - state.last >= INTERIM_MS;
}

/**
 * Captions age out. A sentence that stopped arriving is stale within a few
 * seconds, and a caption bar holding somebody's last words for the rest of the
 * meeting is worse than an empty one.
 */
export const HOLD_MS = 6000;

/**
 * The lines to show, newest last, stale ones dropped.
 *
 * At most two speakers are kept. A caption bar is two lines at the bottom of a
 * video; it is not a transcript, and trying to be one makes it unreadable.
 */
export function visibleLines(bySpeaker, now = Date.now(), limit = 2) {
  const live = [];
  for (const [id, line] of bySpeaker) {
    if (!line.text) continue;
    if (now - line.at > HOLD_MS) continue;
    live.push({ id, ...line });
  }
  live.sort((a, b) => a.at - b.at);
  return live.slice(-limit);
}

/**
 * A speech recogniser wired to a callback.
 *
 * Restarts itself. Every implementation of this API stops on its own -- after
 * a silence, after an error, after a while for no stated reason -- and a
 * caption track that quietly dies halfway through a meeting is the normal
 * outcome of not handling that.
 *
 * @param {(text: string, isFinal: boolean) => void} onText
 * @param {(err: string) => void} [onFatal]
 */
export function createRecogniser(onText, onFatal, opts = {}) {
  const Impl = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (!Impl) return null;

  let rec = null;
  let running = false;
  let stopping = false;
  const gate = { last: 0, text: '' };

  const build = () => {
    const r = new Impl();
    r.continuous = true;
    r.interimResults = true;
    r.lang = navigator.language || 'en-US';
    // On this device, when the browser can: the audio then goes nowhere.
    if (opts.local) r.processLocally = true;

    r.onresult = (e) => {
      // Only the newest result matters: earlier ones have already been sent,
      // and the recogniser keeps handing them back.
      const res = e.results[e.results.length - 1];
      if (!res) return;
      const text = res[0]?.transcript ?? '';
      const isFinal = !!res.isFinal;
      const now = Date.now();
      if (!shouldSend(gate, text, isFinal, now)) return;
      gate.last = now;
      gate.text = isFinal ? '' : text.trim();
      onText(text.trim(), isFinal);
    };

    r.onerror = (e) => {
      // "no-speech" and "aborted" are ordinary; the rest are worth saying out
      // loud once, because a denied microphone looks identical to a broken
      // feature from the outside.
      if (e.error === 'not-allowed' || e.error === 'service-not-allowed') {
        running = false;
        onFatal?.(e.error);
      }
    };

    r.onend = () => {
      if (!running || stopping) return;
      // It stopped on its own. Start it again, after a beat, so a permanent
      // failure does not become a tight loop.
      setTimeout(() => {
        if (running && !stopping) {
          try {
            r.start();
          } catch {
            // Already starting; the next onend will try again.
          }
        }
      }, 250);
    };
    return r;
  };

  return {
    start() {
      if (running) return;
      running = true;
      stopping = false;
      rec = build();
      try {
        rec.start();
      } catch {
        running = false;
      }
      return running;
    },
    stop() {
      running = false;
      stopping = true;
      try {
        rec?.stop();
      } catch {
        // Never started, or already stopped.
      }
      rec = null;
    },
    get running() {
      return running;
    },
  };
}
