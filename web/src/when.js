// Time, written for a person reading a thread.
//
// Kept apart from chat.js so it can be tested without a DOM: chat.js pulls in
// the API layer and the socket, and neither exists under node.

/**
 * The header line of a run: "Today · 8:41 AM", "Thu · 8:41 AM", "11 Sep · 8:41 AM".
 *
 * Day and time are separate on purpose -- the day is what you scan for, the
 * time is what you check -- and today is named rather than implied, so a
 * thread left open overnight does not read as if everything happened this
 * morning.
 */
export function whenFull(iso, now = new Date()) {
  const d = new Date(iso);
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate());
  const days = Math.round((day(now) - day(d)) / 86400000);
  let label;
  if (days === 0) label = 'Today';
  else if (days === 1) label = 'Yesterday';
  else if (days > 1 && days < 7) label = d.toLocaleDateString([], { weekday: 'short' });
  else label = d.toLocaleDateString([], { day: 'numeric', month: 'short' });
  return `${label} · ${d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}`;
}
