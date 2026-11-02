// When a meeting is running, and when it is over.
//
// The clock does not decide either one. A meeting booked for an hour that
// runs to ninety minutes is one meeting, not an hour of meeting followed by
// half an hour of trespass; a meeting everybody left after ten minutes is
// finished, and calling it "in progress" for the next fifty is a lie the
// calendar tells and the room can correct.
//
// So the server reports two facts this reads: `live`, which is whether
// anybody is in the room at this instant, and `status`, which it sets to
// "completed" when the last person leaves. The scheduled window is used only
// where neither says anything -- before anybody has arrived.
//
// Extracted from the interface so it can be tested without a browser: it is
// the one piece of the dashboard that is a decision rather than a drawing.

export const PHASE_LIVE = 'live';
export const PHASE_COMPLETED = 'completed';
export const PHASE_CANCELLED = 'cancelled';
export const PHASE_ENDED = 'ended';
export const PHASE_NOW = 'now';
export const PHASE_UPCOMING = 'upcoming';

/**
 * @param {{live?: boolean, status?: string, start: Date, end: Date}} m
 * @param {number} [now] epoch millis, for tests
 */
export function meetingPhase(m, now = Date.now()) {
  // Occupancy first: somebody being in the room outranks every other signal,
  // including a status row written a moment ago by a departure that was
  // followed by a return.
  if (m.live) return PHASE_LIVE;
  if (m.status === 'cancelled') return PHASE_CANCELLED;
  if (m.status === 'completed') return PHASE_COMPLETED;

  const start = m.start?.getTime?.() ?? 0;
  const end = m.end?.getTime?.() ?? 0;
  if (end && now >= end) return PHASE_ENDED;
  if (start && now >= start) return PHASE_NOW;
  return PHASE_UPCOMING;
}

// Whether a meeting still belongs in "Upcoming". Live meetings stay, however
// far past their end they run; completed and cancelled ones leave, however
// far inside their window they are.
export function isUpcoming(m, now = Date.now()) {
  const phase = meetingPhase(m, now);
  return phase === PHASE_LIVE || phase === PHASE_NOW || phase === PHASE_UPCOMING;
}

// The label beside a meeting. Phase decides the words; the countdown is only
// reached for one that has not started.
export function phaseLabel(m, now = Date.now()) {
  switch (meetingPhase(m, now)) {
    case PHASE_LIVE:
    case PHASE_NOW:
      return 'Happening now';
    case PHASE_COMPLETED:
      return 'Completed';
    case PHASE_CANCELLED:
      return 'Cancelled';
    case PHASE_ENDED:
      return 'Ended';
    default:
      return '';
  }
}

// "5 minutes left in your meeting." -- Teams' heads-up before a scheduled
// meeting's end time. A notice only: the meeting goes on (a meeting ends when
// the room empties, not on the clock). Returns how long to wait before
// showing it, 0 to show it now, or null when there is nothing to say: no
// scheduled end, the end already passed, or joined with less than a minute
// to go (the notice would only be noise).
export const END_NOTICE_MS = 5 * 60 * 1000;
export function endNoticeDelay(end, now = Date.now()) {
  const t = end instanceof Date ? end.getTime() : Number(end);
  if (!Number.isFinite(t) || t - now < 60 * 1000) return null;
  return Math.max(0, t - END_NOTICE_MS - now);
}
