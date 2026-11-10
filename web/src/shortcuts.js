// Keyboard shortcuts: one table of bindings, one matcher, one place that
// decides whether a key press is a shortcut at all (not while typing), and
// the sheet that lists them. The actions themselves live in app.js; this
// module only names them.
//
// Two kinds of binding. Bare letters (M, V, H...) work in a meeting when
// nothing is focused for typing: they are the fastest and the ones people
// reach for mid-call. Modifier chords (Ctrl+Shift+M...) work even while
// typing in the meeting chat, because muting must never wait for the
// cursor to leave a box. On a Mac the Ctrl in a chord is the Command key.

// A binding: what to press, where it applies, what it does, how it reads.
// `combo` is "Ctrl+Shift+M" or "M" or "?" -- modifiers then a key; "Mod" is
// Ctrl on Windows and Linux, Command on a Mac.
export const BINDINGS = [
  // In a meeting
  { combo: 'M', alt: 'Mod+Shift+M', scope: 'meeting', action: 'mic', label: 'Mute or unmute' },
  { combo: 'V', alt: 'Mod+Shift+O', scope: 'meeting', action: 'camera', label: 'Camera on or off' },
  { combo: 'S', alt: 'Mod+Shift+E', scope: 'meeting', action: 'share', label: 'Share your screen, or stop' },
  { combo: 'H', alt: 'Mod+Shift+K', scope: 'meeting', action: 'hand', label: 'Raise or lower your hand' },
  { combo: 'C', alt: 'Mod+Shift+C', scope: 'meeting', action: 'chat', label: 'Show or hide the chat' },
  { combo: 'P', alt: 'Mod+Shift+P', scope: 'meeting', action: 'people', label: 'Show or hide the people' },
  { combo: 'R', scope: 'meeting', action: 'react', label: 'Open reactions' },
  { combo: 'I', scope: 'meeting', action: 'pip', label: 'Picture in picture' },
  { combo: 'Mod+Space', scope: 'meeting', action: 'talk', hold: true, label: 'Hold to talk while muted' },
  { combo: 'Mod+Shift+H', scope: 'meeting', action: 'leave', label: 'Leave the meeting' },
  // At home
  { combo: 'Mod+1', scope: 'home', action: 'tab:meet', label: 'Meetings' },
  { combo: 'Mod+2', scope: 'home', action: 'tab:calendar', label: 'Calendar' },
  { combo: 'Mod+3', scope: 'home', action: 'tab:activity', label: 'Activity' },
  { combo: 'Mod+4', scope: 'home', action: 'tab:chat', label: 'Chat' },
  { combo: 'Mod+5', scope: 'home', action: 'tab:teams', label: 'Teams' },
  { combo: 'Mod+E', alt: '/', scope: 'home', action: 'search', label: 'Search people' },
  { combo: 'Mod+Shift+N', scope: 'home', action: 'new', label: 'Start a meeting now' },
  { combo: 'Mod+Shift+S', scope: 'home', action: 'schedule', label: 'Schedule a meeting' },
  // Anywhere
  { combo: '?', alt: 'Mod+.', scope: 'any', action: 'help', label: 'Show these shortcuts' },
];

const MAC = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform || '');

// parse turns "Mod+Shift+M" into its parts, once.
export function parse(combo) {
  const parts = combo.split('+');
  const key = parts.pop();
  const mods = new Set(parts.map((p) => p.toLowerCase()));
  return { key, mod: mods.has('mod'), shift: mods.has('shift'), alt: mods.has('alt') };
}

// matches says whether a key event is this combo. A chord's "Mod" is Ctrl,
// or Command on a Mac; a bare key must come with no modifier at all, so M
// alone mutes and Ctrl+M does not. Letters compare without case; the key
// for "?" and "/" is the character, so Shift+/ is "?".
export function matches(e, combo, mac = MAC) {
  const c = parse(combo);
  const mod = mac ? e.metaKey : e.ctrlKey;
  const otherMod = mac ? e.ctrlKey : e.metaKey;
  if (otherMod || !!e.altKey !== c.alt) return false;
  if (!!mod !== c.mod) return false;
  const key = e.key === ' ' ? 'Space' : e.key;
  if (c.key === 'Space') return key === 'Space' && !!e.shiftKey === c.shift;
  if (c.key.length === 1 && /[a-z0-9]/i.test(c.key)) {
    // A digit or a letter: the shift state must match the combo's, so
    // Shift+M (a capital) is not M.
    return key.toLowerCase() === c.key.toLowerCase() && !!e.shiftKey === c.shift;
  }
  // Punctuation ("?", "/", "."): the character says it all.
  return key === c.key;
}

// typing says whether the key press belongs to a text field: a bare
// letter there is a letter, not a shortcut. Chords still apply.
export function typing(target) {
  if (!target || !target.tagName) return false;
  const tag = target.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return !!target.isContentEditable;
}

// resolve finds the binding a key event means in a scope, or null. A bare
// binding is skipped while typing; a chord is not. Both the combo and its
// alternative count.
export function resolve(e, scope, mac = MAC) {
  const isTyping = typing(e.target);
  for (const b of BINDINGS) {
    if (b.scope !== 'any' && b.scope !== scope) continue;
    for (const combo of [b.combo, b.alt].filter(Boolean)) {
      if (!matches(e, combo, mac)) continue;
      if (isTyping && !parse(combo).mod) continue;
      return { binding: b, combo };
    }
  }
  return null;
}

// format writes a combo for people: "Ctrl + Shift + M", "⌘ ⇧ M" on a Mac.
export function format(combo, mac = MAC) {
  const c = parse(combo);
  const keys = [];
  if (c.mod) keys.push(mac ? '⌘' : 'Ctrl');
  if (c.shift) keys.push(mac ? '⇧' : 'Shift');
  if (c.alt) keys.push(mac ? '⌥' : 'Alt');
  keys.push(c.key === 'Space' ? 'Space' : c.key.length === 1 ? c.key.toUpperCase() : c.key);
  return keys.join(mac ? ' ' : ' + ');
}

// groups is the sheet's content: bindings by where they apply.
export function groups() {
  const titles = { meeting: 'In a meeting', home: 'At home', any: 'Anywhere' };
  const out = [];
  for (const scope of ['meeting', 'home', 'any']) {
    out.push({ title: titles[scope], items: BINDINGS.filter((b) => b.scope === scope) });
  }
  return out;
}
