// Finding a person, everywhere a person is found.
//
// One module because there were three pickers with three behaviours: one
// listed everybody the moment it opened, one showed a fixed handful and no
// search at all, and one was a bare text box. Typing a name should do the same
// thing in all of them, and results should appear as the name is typed rather
// than after a button.
//
// What is here is the part worth testing: ranking, highlighting, and the
// sequencing that keeps a slow answer to an old query from overwriting a fast
// answer to the current one. Drawing rows stays with each caller, because a
// meeting panel and a scheduling dialog want different rows.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

const norm = (s) => String(s ?? '').trim().toLowerCase();

// Where a query matched, best first. The order is the ranking, so a person
// whose name begins with what you typed comes before one who merely contains
// it -- which is the difference between a picker that feels like it read your
// mind and one you have to scroll.
export const MATCH_EXACT_EMAIL = 0;
export const MATCH_NAME_PREFIX = 1;
export const MATCH_WORD_PREFIX = 2;
export const MATCH_EMAIL_PREFIX = 3;
export const MATCH_CONTAINS = 4;
export const NO_MATCH = -1;

/**
 * How well a person matches a query. Lower is better; NO_MATCH means no.
 * An empty query matches everybody equally, so a picker can show your
 * contacts before a key is pressed without pretending they are search hits.
 */
export function matchScore(person, q) {
  const needle = norm(q);
  if (!needle) return MATCH_CONTAINS;

  const name = norm(person.name);
  const email = norm(person.email);

  if (email && email === needle) return MATCH_EXACT_EMAIL;
  if (name.startsWith(needle)) return MATCH_NAME_PREFIX;

  // Any word of the name: "kumar" should find "Ambuj Kumar". Split on spaces
  // and on the separators that appear in addresses used as names.
  for (const word of name.split(/[\s._-]+/)) {
    if (word && word.startsWith(needle)) return MATCH_WORD_PREFIX;
  }
  if (email.startsWith(needle)) return MATCH_EMAIL_PREFIX;
  if (name.includes(needle) || email.includes(needle)) return MATCH_CONTAINS;
  return NO_MATCH;
}

/**
 * Rank and trim. Ties break on name so the order does not shuffle between
 * keystrokes, which is the thing that makes a live list feel broken.
 */
export function rankPeople(people, q, limit = 20) {
  const scored = [];
  for (const p of people ?? []) {
    const score = matchScore(p, q);
    if (score === NO_MATCH) continue;
    scored.push({ p, score });
  }
  scored.sort((a, b) => {
    if (a.score !== b.score) return a.score - b.score;
    return norm(a.p.name).localeCompare(norm(b.p.name));
  });
  return scored.slice(0, limit).map((x) => x.p);
}

/** Merge server results into local ones without duplicating a person. */
export function mergePeople(...lists) {
  const seen = new Set();
  const out = [];
  for (const list of lists) {
    for (const p of list ?? []) {
      const key = p.userId || p.id || norm(p.email);
      if (!key || seen.has(key)) continue;
      seen.add(key);
      out.push(p);
    }
  }
  return out;
}

/**
 * The matched run of text, marked. Escapes first and inserts the marker after,
 * so a person who names themselves `<script>` is shown, not run.
 */
export function highlight(text, q) {
  const s = String(text ?? '');
  const needle = norm(q);
  if (!needle) return esc(s);
  const at = s.toLowerCase().indexOf(needle);
  if (at < 0) return esc(s);
  return (
    esc(s.slice(0, at)) +
    '<mark>' +
    esc(s.slice(at, at + needle.length)) +
    '</mark>' +
    esc(s.slice(at + needle.length))
  );
}

/**
 * A live search box: debounced, and immune to answers arriving out of order.
 *
 * The out-of-order case is the one that looks like a bug in the server. You
 * type "am", then "ambuj"; the first query is slower; its answer lands second
 * and the list fills with results for a query you have already replaced. Every
 * run carries a sequence number and anything stale is dropped.
 *
 * @param {(q: string) => Promise<any[]>} search
 * @param {(q: string, results: any[]) => void} onResults
 */
export function createSearch(search, onResults, { delay = 140 } = {}) {
  let timer = null;
  let latest = 0;
  let inFlight = 0;

  const run = async (q) => {
    const seq = ++latest;
    inFlight++;
    try {
      const results = await search(q);
      // Stale: a newer keystroke has already been issued.
      if (seq !== latest) return;
      onResults(q, results ?? []);
    } catch {
      if (seq !== latest) return;
      onResults(q, []);
    } finally {
      inFlight--;
    }
  };

  return {
    /** Called on every keystroke. */
    input(q, { immediate = false } = {}) {
      clearTimeout(timer);
      if (immediate) {
        run(q);
        return;
      }
      timer = setTimeout(() => run(q), delay);
    },
    /** Abandon anything pending, e.g. when the picker closes. */
    cancel() {
      clearTimeout(timer);
      latest++;
    },
    get pending() {
      return inFlight;
    },
  };
}
