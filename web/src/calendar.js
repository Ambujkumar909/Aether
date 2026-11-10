// Calendar arithmetic with no DOM in it: the week around a day, where
// events sit in a day column, what a repeat rule says in words, and the
// scheduling assistant's search for a time everybody is free. Kept apart
// from app.js so each piece is a function with a test.

const DAY = 86400000;

// weekOf returns the Monday 00:00 that starts the week holding d, local time.
export function weekOf(d) {
  const start = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  start.setDate(start.getDate() - ((start.getDay() + 6) % 7));
  return start;
}

export function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// hourSpan is the hours a set of meetings uses on a day, with an hour of air
// either side, and never narrower than the working day so an empty week
// still has somewhere to look.
export function hourSpan(meetings, floor = 8, ceil = 18) {
  let first = floor;
  let last = ceil;
  for (const m of meetings) {
    first = Math.min(first, m.start.getHours() - 1);
    last = Math.max(last, m.end.getHours() + (m.end.getMinutes() > 0 ? 1 : 0) + 1);
  }
  return [Math.max(0, first), Math.min(24, last)];
}

// layoutColumns places overlapping events side by side: each gets a column
// and the count of columns in its cluster, so 10:00-11:00 and 10:30-11:30
// each take half the width and a lone event takes all of it.
export function layoutColumns(events) {
  const sorted = [...events].sort((a, b) => a.start - b.start || a.end - b.end);
  const out = [];
  let cluster = [];
  let clusterEnd = 0;
  const flush = () => {
    const cols = Math.max(1, ...cluster.map((e) => e.col + 1));
    for (const e of cluster) out.push({ ...e, cols });
    cluster = [];
  };
  for (const m of sorted) {
    if (cluster.length && m.start >= clusterEnd) flush();
    // The first column whose last event has ended.
    const ends = [];
    for (const e of cluster) ends[e.col] = Math.max(ends[e.col] ?? 0, e.m.end.getTime());
    let col = 0;
    while ((ends[col] ?? 0) > m.start.getTime()) col++;
    cluster.push({ m, col });
    clusterEnd = Math.max(clusterEnd, m.end.getTime());
  }
  if (cluster.length) flush();
  return out;
}

const DAY_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

// recurrenceText says a rule in words: "Every 2 weeks on Mon and Thu, until 12 Dec".
export function recurrenceText(r) {
  if (!r || !r.freq) return '';
  const n = r.interval && r.interval > 1 ? r.interval : 1;
  const unit = { daily: 'day', weekly: 'week', monthly: 'month' }[r.freq] ?? r.freq;
  let s = n === 1 ? `Every ${unit}` : `Every ${n} ${unit}s`;
  if (r.freq === 'weekly' && r.days?.length) {
    const names = [...r.days].sort().map((d) => DAY_NAMES[d]);
    s += ' on ' + (names.length > 1 ? names.slice(0, -1).join(', ') + ' and ' + names.at(-1) : names[0]);
  }
  if (r.until) {
    const u = new Date(r.until);
    if (!isNaN(u)) s += ', until ' + u.toLocaleDateString([], { day: 'numeric', month: 'short' });
  }
  return s;
}

// busyBlocks maps busy spans onto a day's visible hours as percentages, for
// the assistant's bars; spans outside the hours are clipped or dropped.
export function busyBlocks(spans, dayStart, firstHour, lastHour) {
  const from = dayStart.getTime() + firstHour * 3600000;
  const to = dayStart.getTime() + lastHour * 3600000;
  const width = to - from;
  const out = [];
  for (const b of spans) {
    const s = Math.max(from, new Date(b.from).getTime());
    const e = Math.min(to, new Date(b.to).getTime());
    if (e <= s) continue;
    out.push({ left: ((s - from) / width) * 100, width: ((e - s) / width) * 100 });
  }
  return out;
}

// freeSlot finds the first start, from `after`, on a step boundary inside
// working hours on a weekday, where no one's busy span overlaps a meeting of
// `minutes`. Looks ahead `days` days; null when there is none.
export function freeSlot(busyByEmail, after, minutes, { days = 5, hours = [8, 18], step = 30 } = {}) {
  const spans = Object.values(busyByEmail)
    .flat()
    .map((b) => [new Date(b.from).getTime(), new Date(b.to).getTime()]);
  const dur = minutes * 60000;
  const stepMs = step * 60000;
  let t = Math.ceil(after.getTime() / stepMs) * stepMs;
  const limit = after.getTime() + days * DAY;
  while (t < limit) {
    const d = new Date(t);
    const h = d.getHours() + d.getMinutes() / 60;
    const weekday = d.getDay() !== 0 && d.getDay() !== 6;
    if (!weekday || h < hours[0] || h + minutes / 60 > hours[1]) {
      // Jump to the next working-hours start.
      const next = new Date(d.getFullYear(), d.getMonth(), d.getDate() + (h + minutes / 60 > hours[1] || !weekday ? 1 : 0), hours[0], 0);
      t = Math.max(next.getTime(), t + stepMs);
      continue;
    }
    if (!spans.some(([s, e]) => s < t + dur && e > t)) return d;
    t += stepMs;
  }
  return null;
}
