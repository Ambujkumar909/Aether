// Message text, rendered.
//
// A small, predictable subset of Markdown -- the parts people type without
// thinking: **bold**, *italic*, ~~strike~~, `code`, fenced ```code``` blocks
// with line numbers, and bare URLs made clickable. Escaped first, always, so
// nothing a message contains can become markup; the formatting is applied to
// the escaped text and only ever emits tags of its own.

const esc = (s) =>
  String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

const URL_RE = /\bhttps?:\/\/[^\s<>"'`]+/g;

/** Trailing punctuation is prose, not URL. */
function trimURL(u) {
  return u.replace(/[.,;:!?)\]}]+$/, '');
}

/** Bare URLs in escaped text become links that open in a new tab. */
export function linkify(escaped) {
  return escaped.replace(URL_RE, (raw) => {
    const u = trimURL(raw);
    const tail = raw.slice(u.length);
    return `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>${tail}`;
  });
}

/** Inline marks on one escaped line. */
export function inline(escaped) {
  let s = escaped;
  // Code first, and its contents are left alone afterwards.
  const codes = [];
  s = s.replace(/`([^`\n]+)`/g, (_, c) => {
    codes.push(`<code>${c}</code>`);
    return `\u0000${codes.length - 1}\u0000`;
  });
  s = s.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*\w])\*(?=\S)([^*\n]*?\S)\*(?!\w)/g, '$1<em>$2</em>');
  s = s.replace(/~~(?=\S)([^~\n]*?\S)~~/g, '<s>$1</s>');
  s = linkify(s);
  s = s.replace(/\u0000(\d+)\u0000/g, (_, i) => codes[Number(i)]);
  return s;
}

/**
 * The whole body. Fenced code blocks are kept verbatim (escaped) with line
 * numbers; everything else gets inline marks; newlines survive.
 */
export function renderBody(text) {
  const src = String(text ?? '');
  const parts = src.split(/```/);
  // Even indexes are prose, odd are code (an unclosed fence is code to the end).
  return parts
    .map((part, i) => {
      if (i % 2 === 1) {
        let code = part.replace(/^\n/, '').replace(/\n$/, '');
        let lang = '';
        const m = /^([a-zA-Z0-9+#-]{1,20})\n/.exec(code);
        if (m) {
          lang = m[1];
          code = code.slice(m[0].length);
        }
        const lines = esc(code).split('\n');
        return `<pre class="code"${lang ? ` data-lang="${esc(lang)}"` : ''}><code>${lines
          .map((l, n) => `<span class="ln">${n + 1}</span>${l}`)
          .join('\n')}</code></pre>`;
      }
      return inline(esc(part)).replace(/\n/g, '<br>');
    })
    .join('');
}

/** Wrap the selection of an input in a mark, or insert the mark pair. */
export function wrapSelection(value, start, end, mark) {
  const before = value.slice(0, start);
  const sel = value.slice(start, end);
  const after = value.slice(end);
  const text = `${before}${mark}${sel}${mark}${after}`;
  return { text, start: start + mark.length, end: end + mark.length };
}

/** Whether the last message reads like a question worth a quick answer. */
export function looksLikeQuestion(text) {
  const t = String(text ?? '').trim();
  return t.endsWith('?') && t.length <= 200;
}

/** Quick replies for a question. Dumb on purpose: they are one tap, not advice. */
export function suggestedReplies(text) {
  const t = String(text ?? '').toLowerCase();
  if (!looksLikeQuestion(t)) return [];
  if (/\b(can|could|would|will) you\b/.test(t)) return ['Yes, on it', 'Not right now', 'Let me check'];
  if (/\b(when|what time)\b/.test(t)) return ['In a few minutes', 'Later today', 'Tomorrow'];
  if (/\b(are you|is it|did you|have you|do you)\b/.test(t)) return ['Yes', 'No', 'Let me check'];
  return ['Sounds good', 'Yes', 'Let me check'];
}

/** The text of a message with its marks removed, for previews and titles. */
export function plainText(text) {
  return String(text ?? '')
    .replace(/```[a-zA-Z0-9+#-]*\n?([\s\S]*?)```/g, '$1')
    .replace(/`([^`\n]+)`/g, '$1')
    .replace(/\*\*(\S[\s\S]*?\S)\*\*/g, '$1')
    .replace(/(^|[^*\w])\*(\S[^*\n]*?\S)\*(?!\w)/g, '$1$2')
    .replace(/~~(\S[^~\n]*?\S)~~/g, '$1')
    .replace(/\s+/g, ' ')
    .trim();
}
