// Emoji in every composer.
//
// No library and no image sprites: the platform already has the glyphs, and a
// picker that ships a megabyte of PNGs to draw characters the font can render
// is the kind of thing this codebase measures and then deletes. What is here
// is a curated list, a keyword search over it, and the insertion arithmetic --
// which is the part that is actually easy to get wrong, so it is the part with
// tests.

const RECENT_KEY = 'aether.emoji.recent';
const RECENT_MAX = 24;

// Curated rather than exhaustive. A picker with every emoji in Unicode is a
// scrolling exercise; these are the ones people actually send, grouped the way
// they are looked for. Keywords are what the search matches on.
export const GROUPS = [
  {
    name: 'Smileys',
    icon: '\u{1F600}',
    items: [
      ['\u{1F600}', 'grin smile happy'],
      ['\u{1F602}', 'joy laugh cry funny lol'],
      ['\u{1F923}', 'rofl laugh rolling funny'],
      ['\u{1F60A}', 'blush smile happy'],
      ['\u{1F609}', 'wink'],
      ['\u{1F60D}', 'love heart eyes'],
      ['\u{1F618}', 'kiss love'],
      ['\u{1F642}', 'slight smile'],
      ['\u{1F60E}', 'cool sunglasses'],
      ['\u{1F914}', 'thinking hmm think'],
      ['\u{1F610}', 'neutral straight'],
      ['\u{1F644}', 'eyeroll roll eyes'],
      ['\u{1F62C}', 'grimace awkward'],
      ['\u{1F605}', 'sweat smile phew'],
      ['\u{1F62D}', 'sob cry sad'],
      ['\u{1F621}', 'angry rage mad'],
      ['\u{1F631}', 'scream shock fear'],
      ['\u{1F634}', 'sleep tired zzz'],
      ['\u{1F92F}', 'mind blown shocked'],
      ['\u{1F971}', 'yawn tired bored'],
      ['\u{1F60F}', 'smirk'],
      ['\u{1F615}', 'confused unsure'],
      ['\u{1F97A}', 'pleading please puppy'],
      ['\u{1F929}', 'star struck amazed wow'],
    ],
  },
  {
    name: 'Gestures',
    icon: '\u{1F44D}',
    items: [
      ['\u{1F44D}', 'thumbs up yes ok good approve'],
      ['\u{1F44E}', 'thumbs down no bad'],
      ['\u{1F44F}', 'clap applause well done'],
      ['\u{1F64C}', 'raise hands celebrate praise'],
      ['\u{1F64F}', 'pray thanks please thank you'],
      ['\u{1F44B}', 'wave hello hi bye'],
      ['\u{1F91D}', 'handshake deal agree'],
      ['\u{270C}\u{FE0F}', 'peace victory'],
      ['\u{1F44C}', 'ok perfect'],
      ['\u{1F918}', 'rock horns'],
      ['\u{1F4AA}', 'muscle strong'],
      ['\u{261D}\u{FE0F}', 'point up one'],
      ['\u{1F91A}', 'raised hand hi'],
      ['\u{270A}', 'fist'],
    ],
  },
  {
    name: 'Hearts',
    icon: '\u{2764}\u{FE0F}',
    items: [
      ['\u{2764}\u{FE0F}', 'red heart love'],
      ['\u{1F9E1}', 'orange heart'],
      ['\u{1F49B}', 'yellow heart'],
      ['\u{1F49A}', 'green heart'],
      ['\u{1F499}', 'blue heart'],
      ['\u{1F49C}', 'purple heart'],
      ['\u{1F5A4}', 'black heart'],
      ['\u{1F494}', 'broken heart'],
      ['\u{1F495}', 'two hearts love'],
      ['\u{2728}', 'sparkles shiny nice'],
    ],
  },
  {
    name: 'Work',
    icon: '\u{1F4BB}',
    items: [
      ['\u{1F4BB}', 'laptop computer work'],
      ['\u{1F4C5}', 'calendar date meeting schedule'],
      ['\u{23F0}', 'alarm clock time late'],
      ['\u{1F4DE}', 'phone call'],
      ['\u{1F4F9}', 'video camera call meeting'],
      ['\u{1F3A4}', 'mic microphone audio'],
      ['\u{1F5A5}\u{FE0F}', 'screen display share'],
      ['\u{1F4CE}', 'attach paperclip file'],
      ['\u{1F4DD}', 'note memo write'],
      ['\u{1F4CA}', 'chart graph data'],
      ['\u{2705}', 'check done tick complete yes'],
      ['\u{274C}', 'cross no wrong fail'],
      ['\u{26A0}\u{FE0F}', 'warning careful'],
      ['\u{1F525}', 'fire hot urgent'],
      ['\u{1F680}', 'rocket ship launch ship it'],
      ['\u{1F41B}', 'bug defect issue'],
      ['\u{1F4A1}', 'idea bulb light'],
      ['\u{1F389}', 'party tada celebrate ship'],
      ['\u{1F4CC}', 'pin important'],
      ['\u{1F517}', 'link url'],
    ],
  },
  {
    name: 'Things',
    icon: '\u{2615}',
    items: [
      ['\u{2615}', 'coffee tea break'],
      ['\u{1F355}', 'pizza food lunch'],
      ['\u{1F382}', 'cake birthday'],
      ['\u{1F37B}', 'beers cheers drink'],
      ['\u{1F34E}', 'apple fruit'],
      ['\u{1F31F}', 'star good great'],
      ['\u{1F308}', 'rainbow'],
      ['\u{2600}\u{FE0F}', 'sun sunny weather'],
      ['\u{1F327}\u{FE0F}', 'rain weather'],
      ['\u{1F3E0}', 'home house wfh'],
      ['\u{2708}\u{FE0F}', 'plane travel flight'],
      ['\u{1F697}', 'car drive'],
      ['\u{1F436}', 'dog pet'],
      ['\u{1F431}', 'cat pet'],
      ['\u{1F440}', 'eyes look watching'],
      ['\u{1F480}', 'skull dead dying'],
      ['\u{1F3AF}', 'target bullseye goal'],
      ['\u{1F9E0}', 'brain smart idea'],
    ],
  },
];

/** Every entry, flattened, for search. */
export const ALL = GROUPS.flatMap((g) =>
  g.items.map(([ch, words]) => ({ ch, words, group: g.name }))
);

/**
 * Keyword search. Word-prefix matching, so "th" finds "thumbs up" and
 * "thinking" but "umb" finds nothing -- people type the start of a word.
 */
export function searchEmoji(q, limit = 40) {
  const needle = String(q ?? '').trim().toLowerCase();
  if (!needle) return [];
  const exact = [];
  const partial = [];
  for (const e of ALL) {
    const words = e.words.split(/\s+/);
    if (words.includes(needle)) exact.push(e);
    else if (words.some((w) => w.startsWith(needle))) partial.push(e);
  }
  return [...exact, ...partial].slice(0, limit);
}

/**
 * Insert at the caret and report where the caret goes next.
 *
 * The arithmetic nobody gets right first time: a selection is replaced, not
 * pushed aside, and the caret lands after what was inserted rather than back
 * at the start of the box.
 */
export function insertAt(text, start, end, insert) {
  const s = String(text ?? '');
  const a = Math.max(0, Math.min(s.length, start ?? s.length));
  const b = Math.max(a, Math.min(s.length, end ?? a));
  const out = s.slice(0, a) + insert + s.slice(b);
  return { text: out, caret: a + insert.length };
}

/** Most recent first, no duplicates, bounded. */
export function pushRecent(list, ch, max = RECENT_MAX) {
  const out = [ch, ...(list ?? []).filter((x) => x !== ch)];
  return out.slice(0, max);
}

export function loadRecent() {
  try {
    const raw = JSON.parse(localStorage.getItem(RECENT_KEY) ?? '[]');
    return Array.isArray(raw) ? raw.filter((x) => typeof x === 'string').slice(0, RECENT_MAX) : [];
  } catch {
    // A private window, or storage the browser refuses. The picker works
    // without a history; it just does not remember one.
    return [];
  }
}

export function saveRecent(list) {
  try {
    localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, RECENT_MAX)));
  } catch {
    // Not worth interrupting anybody over.
  }
}

// A message that is nothing but emoji is shown large, the way every messaging
// app does it -- a single thumbs-up reads as a gesture, not as a word.
const EMOJI_ONLY =
  /^(?:[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}\u{2190}-\u{21FF}\u{2B00}-\u{2BFF}\u{1F1E6}-\u{1F1FF}\u{FE0F}\u{200D}\u{20E3}\u{0023}\u{002A}\u{0030}-\u{0039}]|\s)+$/u;

export function isOnlyEmoji(text) {
  const s = String(text ?? '').trim();
  if (!s) return false;
  // Digits and # and * are only emoji as part of a keycap sequence, so a bare
  // "2024" must not count.
  if (/^[\s\d#*]+$/u.test(s)) return false;
  if (!EMOJI_ONLY.test(s)) return false;
  // Long strings of emoji are a message, not a gesture.
  return [...s.replace(/\s/g, '')].length <= 12;
}

/**
 * Attach a picker to a composer.
 *
 * @param {HTMLElement} button   the toggle
 * @param {HTMLElement} panel    an empty container the picker draws into
 * @param {HTMLInputElement} input  the composer it types into
 */
const cell = (ch) =>
  `<button type="button" class="emoji-cell" data-e="${ch}" title="${ch}">${ch}</button>`;

/**
 * Paint a picker into a panel: search field, recent, the groups. Shared by
 * the composer picker (stays open, types into the input) and the reaction
 * picker (floats by the message, closes on the choice), so the two never
 * drift apart.
 */
function paintPanel(panel, { query, recent, onQuery, onPick }) {
  const found = query ? searchEmoji(query) : null;
  const body = found
    ? found.length
      ? `<div class="emoji-grid">${found.map((e) => cell(e.ch)).join('')}</div>`
      : `<div class="emoji-none">Nothing matches “${query.replace(/[<>&]/g, '')}”.</div>`
    : (recent.length
        ? `<div class="emoji-group-name">Recent</div>
           <div class="emoji-grid">${recent.map(cell).join('')}</div>`
        : '') +
      GROUPS.map(
        (g) => `<div class="emoji-group-name">${g.name}</div>
          <div class="emoji-grid">${g.items.map(([ch]) => cell(ch)).join('')}</div>`
      ).join('');

  panel.innerHTML = `
    <input class="input emoji-search" placeholder="Search emoji" autocomplete="off" spellcheck="false" />
    <div class="emoji-body">${body}</div>`;

  const search = panel.querySelector('.emoji-search');
  search.value = query;
  search.oninput = (e) => onQuery(e.target.value);
  panel.querySelectorAll('[data-e]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      onPick(b.dataset.e);
    };
  });
}

// One floating picker at a time. Opening another closes the first, and every
// way out -- a choice, a click anywhere else, Escape -- runs the same teardown,
// so there is no path that leaves a panel behind.
let floating = null;

export function closeEmojiMenu() {
  if (!floating) return;
  const f = floating;
  floating = null;
  f.teardown();
}

/**
 * A picker beside an element, for reactions. Resolves with the emoji, or
 * null when dismissed.
 */
export function pickEmoji(anchor) {
  closeEmojiMenu();
  return new Promise((resolve) => {
    const panel = document.createElement('div');
    panel.className = 'emoji-panel emoji-float';
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'Pick a reaction');
    document.body.appendChild(panel);

    let query = '';
    let recent = loadRecent();
    let settled = false;
    const finish = (ch) => {
      if (settled) return;
      settled = true;
      closeEmojiMenu();
      resolve(ch);
    };
    const draw = () => {
      paintPanel(panel, {
        query,
        recent,
        onQuery: (q) => {
          query = q;
          draw();
          panel.querySelector('.emoji-search')?.focus();
        },
        onPick: (ch) => {
          recent = pushRecent(recent, ch);
          saveRecent(recent);
          finish(ch);
        },
      });
    };
    draw();

    // Below the anchor, left-aligned to it, kept inside the viewport; above
    // it when there is no room below.
    const place = () => {
      const r = anchor.getBoundingClientRect();
      const w = Math.min(320, window.innerWidth - 16);
      const h = panel.offsetHeight || 320;
      let left = Math.min(Math.max(8, r.left), window.innerWidth - w - 8);
      let top = r.bottom + 6;
      if (top + h > window.innerHeight - 8) top = Math.max(8, r.top - h - 6);
      panel.style.left = `${left}px`;
      panel.style.top = `${top}px`;
      panel.style.width = `${w}px`;
    };
    place();
    panel.querySelector('.emoji-search')?.focus();

    const onDoc = (e) => {
      const path = typeof e.composedPath === 'function' ? e.composedPath() : [];
      if (path.includes(panel) || path.includes(anchor)) return;
      finish(null);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') finish(null);
    };
    // Registered after the click that opened the picker has finished
    // bubbling, or that same click would close it.
    const arm = setTimeout(() => document.addEventListener('click', onDoc), 0);
    document.addEventListener('keydown', onKey);
    window.addEventListener('resize', place);

    floating = {
      teardown() {
        clearTimeout(arm);
        document.removeEventListener('click', onDoc);
        document.removeEventListener('keydown', onKey);
        window.removeEventListener('resize', place);
        panel.remove();
        if (!settled) {
          settled = true;
          resolve(null);
        }
      },
    };
  });
}

export function attachPicker(button, panel, input) {
  if (!button || !panel || !input) return null;

  let recent = loadRecent();
  let query = '';
  let open = false;

  const draw = () => {
    paintPanel(panel, {
      query,
      recent,
      onQuery: (q) => {
        query = q;
        draw();
        panel.querySelector('.emoji-search')?.focus();
      },
      onPick: (ch) => {
        const { text, caret } = insertAt(
          input.value,
          input.selectionStart,
          input.selectionEnd,
          ch
        );
        input.value = text;
        input.focus();
        try {
          input.setSelectionRange(caret, caret);
        } catch {
          // Some input types refuse a selection range; the text is in.
        }
        recent = pushRecent(recent, ch);
        saveRecent(recent);
        // Stay open: people send more than one.
        if (!query) draw();
      },
    });
  };

  const show = (yes) => {
    open = yes;
    panel.hidden = !yes;
    button.setAttribute('aria-expanded', String(yes));
    if (yes) {
      query = '';
      recent = loadRecent();
      draw();
      panel.querySelector('.emoji-search')?.focus();
    }
  };

  button.onclick = (e) => {
    e.preventDefault();
    e.stopPropagation();
    show(!open);
  };

  // Clicking away closes it; so does Escape, which returns you to the message
  // you were writing rather than leaving the panel over it.
  const onDocClick = (e) => {
    if (!open) return;
    // composedPath, not contains: choosing an emoji redraws the grid, so the
    // clicked cell has already left the panel by the time this runs, and
    // contains() would call an inside click an outside one -- which is
    // exactly how the panel closed after every single emoji.
    const path = typeof e.composedPath === 'function' ? e.composedPath() : [];
    if (path.includes(panel) || path.includes(button)) return;
    if (panel.contains(e.target) || button.contains(e.target)) return;
    show(false);
  };
  const onKey = (e) => {
    if (e.key === 'Escape' && open) {
      show(false);
      input.focus();
    }
  };
  document.addEventListener('click', onDocClick);
  document.addEventListener('keydown', onKey);

  show(false);
  return {
    close: () => show(false),
    destroy() {
      document.removeEventListener('click', onDocClick);
      document.removeEventListener('keydown', onKey);
    },
  };
}
