// Every state a person sees, read the way a person reads it.
//
// The layout harness measures boxes; this one reads text. Three people in a
// meeting walk it through the states that matter: the stage, the meeting chat
// long enough to scroll (read at its top and its bottom), every panel, the
// menus, a raised hand, a screen being presented (from the presenter's side
// and a viewer's), and the workspace screens in both themes. At phone, tablet
// and desktop width, in each state, it fails on:
//
//   - clipped text: a box that cuts its own words off without an ellipsis;
//   - contrast: text under 4.5:1 against what is really behind it (3:1 for
//     large text), composited through every translucent layer;
//   - covered keys: a meeting or navigation key something else sits on;
//   - page overflow.
//
// Each state is also saved as tools/e2e/.shots/visual-*.png, to be looked at.
//
//   node tools/e2e/visual.e2e.mjs https://localhost:9443
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { accounts, fail, failed, launch, ok, signedIn, sleep, enter, offOrigin } from './meet.lib.mjs';

const SHOTS = join(dirname(fileURLToPath(import.meta.url)), '.shots');
mkdirSync(SHOTS, { recursive: true });
const VIEWPORTS = [['desktop', 1280, 800], ['tablet', 820, 1180], ['phone', 390, 844]];

// Runs in the page. Returns what a reader would trip over.
function audit() {
  const parse = (c) => { const m = c.match(/[\d.]+/g); return m ? { r: +m[0], g: +m[1], b: +m[2], a: m[3] === undefined ? 1 : +m[3] } : null; };
  const lum = ({ r, g, b }) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
  const over = (top, under) => ({ r: top.r * top.a + under.r * (1 - top.a), g: top.g * top.a + under.g * (1 - top.a), b: top.b * top.a + under.b * (1 - top.a), a: 1 });
  const path = (el) => { const p = []; for (let e = el; e && e !== document.body && p.length < 3; e = e.parentElement) p.unshift(e.tagName.toLowerCase() + (e.id ? '#' + e.id : e.classList[0] ? '.' + e.classList[0] : '')); return p.join(' > '); };
  const hiddenForSight = (el) => {
    for (let e = el; e; e = e.parentElement) {
      const cs = getComputedStyle(e);
      if (cs.clipPath !== 'none' || cs.visibility === 'hidden' || +cs.opacity === 0) return true;
      const r = e.getBoundingClientRect();
      if (r.width <= 2 || r.height <= 2) return true;
    }
    return false;
  };
  // The colour behind el: translucent layers composited down to the first
  // opaque one. Null when a picture (video, canvas, image) is what is behind.
  const ground = (el) => {
    const layers = [];
    for (let e = el; e; e = e.parentElement) {
      const cs = getComputedStyle(e);
      if (cs.backgroundImage !== 'none' && !cs.backgroundImage.startsWith('linear-gradient')) return null;
      if (e !== el && e.querySelector(':scope > video, :scope > canvas, :scope > img')) {
        const bg = parse(cs.backgroundColor);
        if (!bg || bg.a < 1) return null;
      }
      const bg = parse(cs.backgroundColor);
      if (bg && bg.a > 0) { layers.push(bg); if (bg.a >= 1) break; }
    }
    let c = { r: 255, g: 255, b: 255, a: 1 };
    for (let i = layers.length - 1; i >= 0; i--) c = over(layers[i], c);
    return c;
  };
  const out = { checked: 0, skipped: 0, clipped: [], contrast: [], covered: [], overflow: document.documentElement.scrollWidth > innerWidth + 1 };
  for (const el of document.querySelectorAll('body *')) {
    if (!el.getClientRects().length) continue;
    if (!/\S/.test([...el.childNodes].filter((n) => n.nodeType === 3).map((n) => n.textContent).join(''))) continue;
    const r = el.getBoundingClientRect();
    if (r.bottom <= 0 || r.top >= innerHeight || r.right <= 0 || r.left >= innerWidth) continue;
    if (el.closest('svg, option, script, style, [aria-hidden="true"], .together')) continue;
    if (hiddenForSight(el)) continue;
    const cs = getComputedStyle(el);
    const text = el.textContent.trim().slice(0, 40);
    const scrolls = /auto|scroll/.test(cs.overflowX) || /auto|scroll/.test(cs.overflowY);
    if (!scrolls && /hidden|clip/.test(cs.overflowX) && cs.textOverflow !== 'ellipsis' && el.scrollWidth > el.clientWidth + 1 && cs.webkitLineClamp === 'none') out.clipped.push(`${path(el)} "${text}" (${el.scrollWidth} in ${el.clientWidth})`);
    // Words that run past the box that clips them: cut off at a panel edge,
    // or pushing a vertical list sideways. Sideways scrollers (a chip row,
    // a table, code) are meant to.
    {
      const range = document.createRange(); range.selectNodeContents(el);
      const tr = range.getBoundingClientRect();
      for (let e = el; e && e !== document.body; e = e.parentElement) {
        const ec = getComputedStyle(e);
        if (ec.overflowX === 'visible') continue;
        if (e === el) break; // its own clip is the check above
        // overflow-x: auto computes overflow-y to auto too, so tell a sideways
        // scroller by its shape: a row that does not wrap, or a box that does
        // not scroll down.
        const sideways = /auto|scroll/.test(ec.overflowX) && ((ec.display.includes('flex') && !ec.flexDirection.startsWith('column') && ec.flexWrap === 'nowrap') || e.scrollHeight <= e.clientHeight + 1);
        if (!sideways && !e.matches('pre, code, table, .tbl-wrap')) {
          const er = e.getBoundingClientRect();
          if (tr.right > er.right + 1 || tr.left < er.left - 1) out.clipped.push(`${path(el)} "${text}" runs ${Math.round(Math.max(tr.right - er.right, er.left - tr.left))} px past ${path(e)}`);
        }
        break;
      }
    }
    if (el.closest(':disabled, [aria-disabled="true"], .disabled')) continue;
    const fg = parse(cs.color); const bg = ground(el);
    if (!fg || !bg) { out.skipped++; continue; }
    out.checked++;
    const ink = over(fg, bg);
    const [hi, lo] = [lum(ink), lum(bg)].sort((x, y) => y - x);
    const ratio = (hi + 0.05) / (lo + 0.05);
    const px = parseFloat(cs.fontSize); const bold = +cs.fontWeight >= 700;
    const need = px >= 24 || (px >= 18.66 && bold) ? 3 : 4.5;
    if (ratio < need - 0.05) out.contrast.push(`${path(el)} "${text}" ${ratio.toFixed(2)}:1 (need ${need}) ${cs.color} on rgb(${Math.round(bg.r)},${Math.round(bg.g)},${Math.round(bg.b)})`);
  }
  // Icons are controls' faces: 3:1 against their ground (WCAG 1.4.11).
  out.icons = [];
  for (const svg of document.querySelectorAll('#meet-bar button svg, .home-rail .rail-btn svg, .mb-devices button svg')) {
    const k = svg.closest('button');
    if (!k.getClientRects().length || hiddenForSight(svg) || k.closest(':disabled')) continue;
    const fg = parse(getComputedStyle(svg).color); const bg = ground(k);
    if (!fg || !bg) continue;
    const ink = over(fg, bg);
    const [hi, lo] = [lum(ink), lum(bg)].sort((x, y) => y - x);
    const ratio = (hi + 0.05) / (lo + 0.05);
    if (ratio < 2.95) out.icons.push(`${path(k)} icon ${ratio.toFixed(2)}:1 (need 3)`);
  }
  // A tile's name plate never sits on its face.
  for (const tile of document.querySelectorAll('.strip-tile')) {
    if (!tile.getClientRects().length) continue;
    const av = tile.querySelector('.strip-ph:not([hidden]) .avatar'); const lb = tile.querySelector('.strip-label');
    if (!av || !lb || !av.getClientRects().length || hiddenForSight(av)) continue;
    const a = av.getBoundingClientRect(); const b = lb.getBoundingClientRect();
    const w = Math.min(a.right, b.right) - Math.max(a.left, b.left); const h = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top);
    if (w > 1 && h > 1) out.covered.push(`${path(tile)}: the name plate covers the avatar by ${Math.round(h)} px`);
  }
  const overlay = '.mb-pop, .react-pop, dialog, .modal, .toast, #toast, .sheet, .menu, .pop, [role="menu"], [role="dialog"]';
  for (const k of document.querySelectorAll('#meet-bar button, .home-rail .rail-btn')) {
    if (!k.getClientRects().length || hiddenForSight(k)) continue;
    const r = k.getBoundingClientRect();
    if (r.bottom <= 0 || r.top >= innerHeight) continue;
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    if (!at || k.contains(at) || at.contains(k) || at.closest(overlay)) continue;
    out.covered.push(`${path(k)} under ${path(at)}`);
  }
  return out;
}

const report = async (page, name) => {
  const a = await page.evaluate(audit);
  await page.screenshot({ path: join(SHOTS, `visual-${name}.png`) });
  const list = (xs) => xs.slice(0, 6).map((x) => '\n        ' + x).join('') + (xs.length > 6 ? `\n        ... ${xs.length - 6} more` : '');
  ok(a.clipped.length === 0, `${name}: no clipped text${list(a.clipped)}`);
  ok(a.checked > 0 && a.contrast.length === 0, `${name}: every text reads against its ground (${a.checked} read, ${a.skipped} over pictures)${list(a.contrast)}`);
  ok(a.icons.length === 0, `${name}: every key's icon reads against it${list(a.icons)}`);
  ok(a.covered.length === 0, `${name}: no key is covered${list(a.covered)}`);
  ok(!a.overflow, `${name}: no page overflow`);
};
const each = async (pages, name, fn) => {
  for (const [vp, w, h] of VIEWPORTS) {
    for (const p of pages) await p.setViewport({ width: w, height: h });
    await sleep(350);
    if (fn) await fn(vp);
    await report(pages[0], `${vp}-${name}`);
  }
};
const click = (page, id) => page.evaluate((id) => document.getElementById(id)?.click(), id);
const theme = (page, t) => page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), t);
// A presented slide: dark words on white, the way a deck looks.
const fakeScreen = (page) => page.evaluate(() => {
  navigator.mediaDevices.getDisplayMedia = async () => {
    const c = document.createElement('canvas'); c.width = 1280; c.height = 720;
    const g = c.getContext('2d'); let k = 0;
    setInterval(() => {
      g.fillStyle = '#fbfbf9'; g.fillRect(0, 0, 1280, 720);
      g.fillStyle = '#0d1b16'; g.font = 'bold 64px sans-serif'; g.fillText('Quarterly review', 90, 170);
      g.font = '34px sans-serif'; g.fillText('Revenue up 12%, churn down', 90, 260); g.fillText('Slide ' + (1 + (k++ % 9)), 90, 640);
    }, 120);
    return c.captureStream(8);
  };
});

const acct = await accounts(['A', 'B', 'C']);
const browser = await launch();
const log = [];
try {
  // ---- The workspace, both themes ---------------------------------------
  const A = await signedIn(browser, acct.A, 'A', log, { width: 1280, height: 800 });
  // Meetings on today's calendar, one in each light category fill, so the
  // calendar is read with its event blocks and not only empty hours.
  await A.evaluate(async () => {
    const api = await import('/src/api.js');
    const at = (h) => { const d = new Date(); d.setMinutes(0, 0, 0); d.setHours(Math.min(d.getHours() + h, 23)); return d.toISOString(); };
    for (const [title, category, h] of [['Visual review', 'internal', 0], ['Pricing call', 'business', 1], ['Design crit', 'design', 2]]) {
      try { await api.scheduling.create({ title, description: '', category, startsAt: at(h), endsAt: at(h + 1), invitees: [], coOrganizers: [] }); } catch {}
    }
  });
  // Made behind the page's back, so load it again to draw them.
  await A.reload({ waitUntil: 'networkidle2' });
  await sleep(1200);
  for (const t of ['light', 'dark']) {
    await theme(A, t);
    for (const dest of ['meet', 'calendar', 'activity', 'chat', 'teams']) {
      await A.evaluate((d) => document.querySelector(`.rail-btn[data-home="${d}"]`)?.click(), dest);
      await sleep(500);
      await each([A], `${t}-${dest}`);
    }
  }
  await theme(A, 'light');

  // ---- The meeting --------------------------------------------------------
  const B = await signedIn(browser, acct.B, 'B', log, { width: 1280, height: 800 });
  const C = await signedIn(browser, acct.C, 'C', log, { width: 1280, height: 800 });
  const room = 'visual-' + Math.random().toString(36).slice(2, 7);
  await enter(A, room);
  await enter(B, room);
  await enter(C, room);
  await sleep(3000);
  const peers = await A.evaluate(() => document.querySelectorAll('.strip-tile').length);
  ok(peers >= 3, `three people on A's stage (${peers} tiles)`);

  await each([A], 'stage');

  // A meeting chat long enough to scroll, with the words people really type:
  // a greeting, a long sentence, a link, a word with no place to break.
  const lines = [
    [A, 'Hey, good morning everyone!'], [B, 'Good morning! Testing the system like this.'], [C, 'Morning both, I can hear you clearly.'],
    [A, 'Great. Today we go through the quarterly numbers, then the launch plan, then questions.'],
    [B, 'Deck is at https://example.test/decks/quarterly-review-2026-final-v3.pdf for anyone who wants it'],
    [C, 'Supercalifragilisticexpialidociousandthensomemorewordswithoutanyspaceatall'],
  ];
  for (let i = 0; i < 24; i++) {
    const [page, text] = lines[i % lines.length];
    await page.evaluate((t) => { const inp = document.getElementById('chat-input'); inp.value = t; inp.dispatchEvent(new Event('input', { bubbles: true })); document.getElementById('chat-send').click(); }, i < lines.length ? text : `${text} (${i})`);
    await sleep(120);
  }
  await click(A, 'rail-chat');
  await sleep(800);
  const msgs = await A.evaluate(() => document.querySelectorAll('#chat-log .msg').length);
  ok(msgs >= 24, `A's meeting chat holds every message (${msgs})`);
  await each([A], 'chat-bottom', async () => A.evaluate(() => { const l = document.querySelector('#side .side-body'); l.scrollTop = l.scrollHeight; }));
  await each([A], 'chat-top', async () => A.evaluate(() => { document.querySelector('#side .side-body').scrollTop = 0; }));
  // The message box keeps room for what it asks: its whole placeholder fits.
  for (const [vp, w, h] of VIEWPORTS) {
    await A.setViewport({ width: w, height: h }); await sleep(250);
    const fit = await A.evaluate(() => {
      const i = document.getElementById('chat-input'); const cs = getComputedStyle(i);
      const g = document.createElement('canvas').getContext('2d'); g.font = `${cs.fontSize} ${cs.fontFamily}`;
      const need = g.measureText(i.placeholder).width + parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
      return { ok: i.clientWidth >= need - 1, need: Math.round(need), has: i.clientWidth, text: i.placeholder };
    });
    ok(fit.ok, `${vp}: the meeting chat box shows its whole prompt "${fit.text}" (${fit.has} px for ${fit.need})`);
  }
  const scrolls = await A.evaluate(() => { const l = document.querySelector('#side .side-body'); return l.scrollHeight > l.clientHeight && document.scrollingElement.scrollHeight <= innerHeight + 1; });
  ok(scrolls, 'the meeting chat scrolls inside its panel, not the page');

  for (const [key, name] of [['rail-people', 'people'], ['btn-qna', 'qna'], ['btn-notes', 'notes']]) {
    await click(A, key);
    await sleep(700);
    await each([A], name);
  }
  await A.evaluate(() => document.querySelector('.side-close, #side-close')?.click());
  await sleep(400);

  await click(B, 'btn-hand');
  await sleep(1200);
  await each([A], 'hand-raised');
  await click(B, 'btn-hand');

  for (const [key, pop] of [['btn-more', 'more'], ['btn-view', 'view']]) {
    await each([A], `menu-${pop}`, async () => {
      if (await A.evaluate((p) => document.getElementById(p + '-pop').hidden, pop)) await click(A, key);
      await sleep(300);
      ok(await A.evaluate((p) => !document.getElementById(p + '-pop').hidden, pop), `the ${pop} menu is open`);
    });
    await A.keyboard.press('Escape');
    await A.evaluate((p) => { const e = document.getElementById(p + '-pop'); if (e) e.hidden = true; }, pop);
  }

  // Cameras off for two of them, so the strip shows faces as avatars under
  // name plates, the way most meetings look.
  for (const p of [B, C]) await click(p, 'btn-cam');
  await sleep(1500);
  await fakeScreen(A);
  await click(A, 'btn-screen');
  await sleep(4000);
  const presenting = await B.evaluate(() => document.getElementById('stage')?.dataset.mode || document.getElementById('stage')?.dataset.layout);
  ok(!!presenting, `B's stage shows the presentation (${presenting})`);
  // The content and the people beside it never share pixels.
  const apart = (page, name) => page.evaluate(() => {
    const v = document.querySelector('#stage-video video')?.getBoundingClientRect();
    const tiles = [...document.querySelectorAll('#strip .strip-tile')].filter((t) => t.getClientRects().length).map((t) => t.getBoundingClientRect());
    if (!v || !v.width) return { ok: false, why: 'no content on the stage' };
    const hit = tiles.filter((t) => t.left < v.right - 1 && t.right > v.left + 1 && t.top < v.bottom - 1 && t.bottom > v.top + 1);
    return { ok: hit.length === 0, why: `${hit.length} of ${tiles.length} tiles over the content` };
  }).then((r) => ok(r.ok, `${name}: the presentation is not covered by faces (${r.why})`));
  await each([A], 'presenting', async (vp) => apart(A, `${vp}-presenting`));
  await each([B], 'viewing', async (vp) => apart(B, `${vp}-viewing`));
  await click(B, 'rail-chat');
  await sleep(600);
  await each([B], 'viewing-chat', async (vp) => apart(B, `${vp}-viewing-chat`));
  await click(A, 'btn-screen');
  await sleep(800);

  await A.setViewport({ width: 1280, height: 800 });
  ok(offOrigin(log).length === 0, `nothing left the server's host in any state (${offOrigin(log).slice(0, 3).join(' | ') || 'none'})`);
  ok(log.filter((l) => l.includes('PAGE ERROR')).length === 0, `no page errors (${log.filter((l) => l.includes('PAGE ERROR')).slice(0, 3).join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
