// Grouping messages into runs.
//
// A run is a stretch of messages from one person that reads as one turn of
// the conversation: the header (who, day, time) is shown once at the top and
// the rest sit directly beneath. Getting the boundaries wrong is what makes a
// thread read like a log file -- or, the other way, like one endless speech.
//
// The rule is about gaps, not clock minutes. Somebody who starts typing at
// 10:50:55 and sends at 10:51:05 has not started a new thought, and a rule
// that keyed on the minute would say they had. So:
//
//   1. Same sender, and neither message deleted ("Message deleted" inside a
//      run reads as if the person said it; it needs its own attribution).
//   2. The gap since the PREVIOUS message is under GAP_MS. Five minutes:
//      a reply half an hour later is a new thought and deserves its own time.
//   3. The run has not been going for longer than SPAN_MS since its FIRST
//      message. Without this a slow, steady stream -- one line every four
//      minutes for an hour -- never shows a time again, and the reader has no
//      idea when anything was said. Twenty minutes is long enough for a real
//      exchange and short enough to keep the timeline honest.
//
// Kept apart from chat.js so it can be tested without a DOM.

export const GAP_MS = 5 * 60 * 1000;
export const SPAN_MS = 20 * 60 * 1000;

/**
 * Whether `m` continues the run that `prev` belongs to.
 *
 * @param {object|null} prev   the message immediately before, or null
 * @param {object|null} m      the message being placed
 * @param {object|null} first  the first message of prev's run (defaults to prev)
 */
export function sameRun(prev, m, first = prev) {
  if (!prev || !m) return false;
  if (prev.senderId !== m.senderId) return false;
  if (prev.deleted || m.deleted) return false;
  const t = new Date(m.createdAt).getTime();
  if (t - new Date(prev.createdAt).getTime() >= GAP_MS) return false;
  const start = first ?? prev;
  return t - new Date(start.createdAt).getTime() < SPAN_MS;
}

/**
 * Walk messages in order and say, for each, whether it is grouped under the
 * one before. The caller supplies `breakBefore(m)` for things that cut a run
 * regardless of timing -- the "New" divider is one.
 *
 * @returns {boolean[]} grouped[i] is true when messages[i] continues a run
 */
export function groupRuns(messages, breakBefore = () => false) {
  const out = [];
  let prev = null;
  let first = null;
  for (const m of messages) {
    if (breakBefore(m)) {
      prev = null;
      first = null;
    }
    const grouped = sameRun(prev, m, first);
    out.push(grouped);
    if (!grouped) first = m;
    prev = m;
  }
  return out;
}
