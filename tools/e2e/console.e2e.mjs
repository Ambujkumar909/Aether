// The meeting console and the workspace bar, measured in a real browser.
//
// The redesign (styles.css "Aether · Console") rests on a few promises that a
// screenshot shows only at one width and a person notices at every width:
//
//   - the meeting bar is three zones on one line: the room at the left edge,
//     the console (Mic, Camera, Share, Leave) centred on the bar, the panel
//     keys at the right edge, every key on one centre line;
//   - keyboard focus walks the bar in the order it is drawn;
//   - a muted mic, a camera that is off and a screen being shared are states
//     you can see on the key itself, not only in its icon;
//   - every icon-only key has a name;
//   - nothing in the bar overlaps or leaves the viewport, from a narrow
//     laptop to a wide monitor;
//   - the workspace's destinations are a left rail of icon keys with
//     tooltips from a tablet up, and a named bottom bar on a phone.
//
//   node tools/e2e/console.e2e.mjs https://localhost:9443
import { accounts, fail, failed, launch, ok, signedIn, sleep, enter } from './meet.lib.mjs';

const acct = await accounts(['A', 'B']);
const browser = await launch();
const log = [];

const rect = (page, sel) => page.evaluate((s) => {
  const el = document.querySelector(s);
  if (!el || el.getClientRects().length === 0) return null;
  const r = el.getBoundingClientRect();
  return { l: r.left, r: r.right, t: r.top, b: r.bottom, w: r.width, h: r.height, cx: r.left + r.width / 2, cy: r.top + r.height / 2 };
}, sel);


// Every item a menu shows has words you can read: a row of nothing is a bug
// (the label-hiding rule for icon keys once blanked three of More's rows).
const blankItems = (page, key, pop) => page.evaluate(async (key, pop) => {
  document.getElementById(key).click();
  await new Promise((r) => setTimeout(r, 300));
  const rows = [...document.querySelectorAll(`#${pop} > button`)].filter((b) => b.getClientRects().length);
  // Text is readable when every box from the one holding it up to the row is
  // big enough and unclipped: a text node keeps its width inside a 1 px box,
  // so the text's own rectangle cannot tell.
  const readable = (b) => {
    const w = document.createTreeWalker(b, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      if (!n.textContent.trim()) continue;
      let ok = true;
      for (let el = n.parentElement; el && ok; el = el === b ? null : el.parentElement) {
        const cs = getComputedStyle(el); const r = el.getBoundingClientRect();
        ok = r.width > 8 && r.height > 6 && cs.clipPath === 'none' && cs.visibility !== 'hidden' && cs.opacity !== '0';
      }
      if (ok) return true;
    }
    return false;
  };
  const blank = rows.filter((b) => !readable(b)).map((b) => b.id || b.textContent.trim() || '?');
  document.getElementById(key).click();
  return { n: rows.length, blank };
}, key, pop);

try {
  // ---- The workspace rail ------------------------------------------------
  const home = await signedIn(browser, acct.A, 'A', log, { width: 1280, height: 800 });
  for (const [w, h, form] of [[1920, 1080, 'rail'], [1440, 900, 'rail'], [1280, 800, 'rail'], [900, 900, 'rail'], [820, 1180, 'rail'], [390, 844, 'bottom']]) {
    await home.setViewport({ width: w, height: h, isMobile: w < 721, hasTouch: w < 721 });
    await sleep(300);
    const nav = await home.evaluate(() => {
      const btns = [...document.querySelectorAll('.home-rail .rail-btn')];
      const rs = btns.map((b) => b.getBoundingClientRect());
      const labels = btns.map((b) => { const l = b.querySelector('.rail-label'); const r = l?.getBoundingClientRect(); return !!(r && r.width > 2 && r.height > 2); });
      const rail = document.querySelector('.home-rail').getBoundingClientRect();
      const active = document.querySelector('.home-rail .rail-btn.active');
      const bar = active ? getComputedStyle(active, '::after') : null;
      return {
        n: btns.length,
        oneRow: rs.every((r) => Math.abs(r.top - rs[0].top) < 1),
        oneColumn: rs.every((r) => Math.abs(r.left - rs[0].left) < 1) && rs.every((r, i) => i === 0 || r.top > rs[i - 1].top),
        named: labels.every(Boolean),
        anyNamed: labels.some(Boolean),
        titled: btns.every((b) => !!b.title),
        railTop: rail.top, railBottom: rail.bottom, railLeft: rail.left, railW: rail.width, railH: rail.height, vh: innerHeight,
        marker: bar ? { w: parseFloat(bar.width), h: parseFloat(bar.height) } : null,
        overflow: document.documentElement.scrollWidth > innerWidth,
      };
    });
    ok(nav.n === 5 && (form === 'rail' ? nav.oneColumn : nav.oneRow), `${w}px: the five destinations sit in one ${form === 'rail' ? 'column' : 'row'}`);
    ok(!nav.overflow, `${w}px: no horizontal overflow`);
    if (form === 'rail') ok(!nav.anyNamed && nav.titled && nav.railLeft < 1 && nav.railW < 100 && nav.railH > h * 0.8, `${w}px: a left rail of icons, each with a tooltip (${Math.round(nav.railW)} px wide)`);
    if (form === 'bottom') ok(nav.named && Math.abs(nav.railBottom - nav.vh) < 2, `${w}px: a bottom bar, each destination named`);
    ok(nav.marker && Math.min(nav.marker.w, nav.marker.h) >= 2 && Math.max(nav.marker.w, nav.marker.h) >= 16, `${w}px: the current destination is marked`);
  }
  await home.close();

  // ---- The sidebar folds away in the Windows app, and stays folded ----------
  // The app's own title bar carries the toggle (a browser page has no caption
  // to put it in). The shell's host functions are stubbed the way the layout
  // harness does, which is what makes the client draw that title bar.
  const app = await signedIn(browser, acct.A, 'A', log, { width: 1280, height: 800 });
  await app.evaluateOnNewDocument(() => {
    const noop = () => {};
    Object.assign(window, { aetherHostReady: () => true, aetherStartDrag: noop, aetherMinimise: noop, aetherToggleMaximise: noop, aetherCloseWindow: noop, aetherIsMaximised: async () => false, aetherShowWindow: noop, aetherNotify: noop, aetherBadge: noop, aetherRinging: noop, aetherPermissions: noop });
  });
  await app.evaluate(() => { try { localStorage.removeItem('aether.rail'); } catch {} });
  await app.reload({ waitUntil: 'networkidle2' });
  await sleep(1200);
  const rail = () => app.evaluate(() => {
    const b = document.getElementById('tb-collapse');
    const r = document.querySelector('.home-rail').getBoundingClientRect();
    const m = document.querySelector('.home-main').getBoundingClientRect();
    // Showing: drawn where a pointer can reach it, and reachable by Tab.
    const seen = [...document.querySelectorAll('.home-rail .rail-btn')].filter((x) => { const q = x.getBoundingClientRect(); const hit = document.elementFromPoint(q.left + q.width / 2, q.top + q.height / 2); return getComputedStyle(x).visibility !== 'hidden' && !!hit && x.contains(hit); }).length;
    return { has: !!b, title: b?.title, pressed: b?.getAttribute('aria-pressed'), collapsed: document.body.classList.contains('rail-collapsed'), w: Math.round(r.width), mainLeft: Math.round(m.left), seen };
  });
  const open1 = await rail();
  ok(open1.has && open1.title === 'Hide sidebar' && open1.pressed === 'false', `the app's title bar has the sidebar toggle ("${open1.title}", pressed ${open1.pressed})`);
  ok(!open1.collapsed && open1.w >= 60 && open1.seen === 5, `the sidebar starts open (${open1.w} px, ${open1.seen} destinations)`);
  await app.click('#tb-collapse');
  await sleep(500);
  const shut = await rail();
  ok(shut.collapsed && shut.w <= 1 && shut.seen === 0, `the toggle folds the sidebar away (${shut.w} px, ${shut.seen} destinations showing)`);
  ok(shut.mainLeft <= 1, `the page takes the sidebar's room (starts at ${shut.mainLeft} px)`);
  ok(shut.title === 'Show sidebar' && shut.pressed === 'true', `the toggle says what it will do next ("${shut.title}", pressed ${shut.pressed})`);
  ok(!(await app.evaluate(() => document.documentElement.scrollWidth > innerWidth)), 'folded: no horizontal overflow');
  await app.reload({ waitUntil: 'networkidle2' });
  await sleep(1200);
  ok((await rail()).collapsed, 'the sidebar stays folded after a restart');
  await app.click('#tb-collapse');
  await sleep(500);
  const back = await rail();
  ok(!back.collapsed && back.w >= 60 && back.seen === 5 && back.mainLeft >= 60, `the toggle brings it back (${back.w} px, page from ${back.mainLeft} px)`);
  await app.close();

  // ---- The meeting bar ----------------------------------------------------
  const room = 'console-' + Math.random().toString(36).slice(2, 7);
  const a = await signedIn(browser, acct.A, 'A', log, { width: 1440, height: 900 });
  const b = await signedIn(browser, acct.B, 'B', log, { width: 1440, height: 900 });
  await enter(a, room);
  await enter(b, room);
  await sleep(2500);

  for (const w of [1920, 1440, 1280, 1180, 1100, 1024, 900, 760]) {
    await a.setViewport({ width: w, height: 900 });
    await sleep(350);
    const bar = await rect(a, '#meet-bar');
    const left = await rect(a, '.mb-left');
    const dev = await rect(a, '.mb-devices');
    const pan = await rect(a, '.mb-panels');
    ok(bar && left && dev && pan, `${w}px: the bar has its three zones`);
    if (!(bar && left && dev && pan)) continue;
    // A laptop centres the console on the bar; a tablet, which has no room for
    // two equal side tracks, centres it between the room and the panels.
    const mid = w >= 1024 ? bar.cx : (left.r + pan.l) / 2;
    ok(Math.abs(dev.cx - mid) <= 2, `${w}px: the console is centred ${w >= 1024 ? 'on the bar' : 'between the zones'} (${(dev.cx - mid).toFixed(1)} px off)`);
    const labelled = await a.evaluate(() => { const r = document.querySelector('#btn-mic .ctl-label').getBoundingClientRect(); return r.width > 2 && r.height > 2; });
    ok(labelled === (w > 1180), `${w}px: the console keys ${w > 1180 ? 'carry their names' : 'are icons with names for assistive tech'}`);
    ok(left.r <= dev.l - 8 && dev.r <= pan.l - 8, `${w}px: the zones do not touch (${(dev.l - left.r).toFixed(0)} px and ${(pan.l - dev.r).toFixed(0)} px between)`);
    ok(left.l >= bar.l && pan.r <= bar.r && bar.r <= w + 0.5, `${w}px: nothing leaves the bar or the viewport`);
    const keys = await a.evaluate(() => [...document.querySelectorAll('#meet-bar .mb-devices .ctl.mb-btn, #meet-bar .mb-devices .mb-caret:not([hidden]), #btn-leave, #meet-bar .mb-panels > .ctl.mb-btn:not([hidden]), #meet-bar .mb-panels .mb-btn > button.ctl')]
      .filter((k) => k.getClientRects().length)
      .map((k) => { const r = k.getBoundingClientRect(); return { id: k.id || k.className, cy: r.top + r.height / 2, h: r.height, w: r.width }; }));
    const cys = keys.map((k) => k.cy);
    ok(Math.max(...cys) - Math.min(...cys) <= 1, `${w}px: every key on one centre line (spread ${(Math.max(...cys) - Math.min(...cys)).toFixed(1)} px)`);
    ok(keys.every((k) => k.h >= 40 && k.w >= 24), `${w}px: keys are at least 40 px tall and 24 wide (smallest ${Math.min(...keys.map((k) => k.h)).toFixed(0)} x ${Math.min(...keys.map((k) => k.w)).toFixed(0)})`);
    ok(!(await a.evaluate(() => document.documentElement.scrollWidth > innerWidth)), `${w}px: no horizontal overflow`);
  }

  // Every icon-only key is named; focus walks the bar left to right.
  await a.setViewport({ width: 1440, height: 900 });
  await sleep(300);
  const unnamed = await a.evaluate(() => [...document.querySelectorAll('#meet-bar button')].filter((x) => x.getClientRects().length && !(x.title || x.getAttribute('aria-label') || x.textContent.trim())).map((x) => x.id || x.className));
  ok(unnamed.length === 0, `every key in the bar has a name (${unnamed.join(', ') || 'none missing'})`);
  await a.evaluate(() => document.activeElement?.blur());
  await a.focus('#btn-cam');
  const order = [];
  for (let i = 0; i < 16; i++) {
    const f = await a.evaluate(() => { const e = document.activeElement; const r = e.getBoundingClientRect(); return { id: e.id, x: r.left, inBar: !!e.closest('#meet-bar') }; });
    if (!f.inBar) break;
    order.push(f);
    await a.keyboard.press('Tab');
  }
  ok(order.length >= 8 && order.every((f, i) => i === 0 || f.x > order[i - 1].x), `focus walks the bar left to right (${order.map((f) => f.id || '?').join(' > ')})`);
  const ring = await a.evaluate(() => { const e = document.getElementById('btn-mic'); e.focus(); return getComputedStyle(e).outlineStyle !== 'none' && parseFloat(getComputedStyle(e).outlineWidth) >= 2; });
  ok(ring, 'a focused key shows a ring');

  for (const [key, pop] of [['btn-more', 'more-pop'], ['btn-view', 'view-pop'], ['btn-cam-more', 'cam-pop'], ['btn-share-more', 'share-pop']]) {
    const m = await blankItems(a, key, pop);
    ok(m.n > 0 && m.blank.length === 0, `1440px: every row of ${pop} shows its words (${m.n} rows; blank: ${m.blank.join(', ') || 'none'})`);
  }

  // States on the key itself.
  const keyState = (id) => a.evaluate((id) => {
    const k = document.getElementById(id);
    const split = k.closest('.mb-split');
    return { off: k.classList.contains('off'), on: k.classList.contains('on'), color: getComputedStyle(k).color, ground: getComputedStyle(split).backgroundColor };
  }, id);
  const micBefore = await keyState('btn-mic');
  await a.evaluate(() => document.getElementById('btn-mic').click());
  await sleep(500);
  const micMuted = await keyState('btn-mic');
  ok(micBefore.off !== micMuted.off && micMuted.ground !== micBefore.ground, `muting the mic changes its key, not only its icon (${micBefore.ground} to ${micMuted.ground})`);
  await a.evaluate(() => document.getElementById('btn-mic').click());
  await sleep(500);
  ok((await keyState('btn-mic')).ground === micBefore.ground, 'unmuting restores the key');
  const cam = await keyState('btn-cam');
  ok(cam.off === (await a.evaluate(() => document.getElementById('cam-icon').getAttribute('href') === '#i-cam-off')), 'the camera key and its icon agree');

  await a.evaluate(() => {
    navigator.mediaDevices.getDisplayMedia = async () => {
      const cv = document.createElement('canvas'); cv.width = 640; cv.height = 360;
      const g = cv.getContext('2d'); setInterval(() => { g.fillStyle = '#3a6'; g.fillRect(0, 0, 640, 360); }, 100);
      return cv.captureStream(5);
    };
    document.getElementById('btn-screen').click();
  });
  await sleep(2500);
  const share = await keyState('btn-screen');
  ok(share.on, 'sharing marks the Share key');
  ok(share.ground !== micBefore.ground, `the Share key reads as on (${share.ground})`);
  await a.evaluate(() => document.getElementById('btn-screen').click());
  await sleep(1000);

  // A panel key, pressed, reads as pressed.
  await a.evaluate(() => document.getElementById('rail-chat').click());
  await sleep(500);
  const chat = await a.evaluate(() => { const k = document.getElementById('rail-chat'); return { pressed: k.getAttribute('aria-pressed') === 'true' || k.classList.contains('active'), bg: getComputedStyle(k).backgroundColor }; });
  ok(chat.pressed && chat.bg !== 'rgba(0, 0, 0, 0)', `an open panel's key reads as pressed (${chat.bg})`);

  // The room is dark whatever the workspace theme.
  for (const theme of ['light', 'dark']) {
    await a.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
    await sleep(200);
    const lum = await a.evaluate(() => {
      const c = getComputedStyle(document.getElementById('meet-bar')).backgroundColor.match(/\d+/g).map(Number);
      return (0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]) / 255;
    });
    ok(lum < 0.12, `the meeting bar is dark with the workspace in ${theme} (luminance ${lum.toFixed(3)})`);
  }

  // A phone: panels at the top, the console as the dock at the bottom.
  // Width only: switching puppeteer to isMobile reloads the page, out of the meeting.
  await a.setViewport({ width: 390, height: 844 });
  await sleep(500);
  const dock = await rect(a, '.mb-devices');
  const top = await rect(a, '.mb-panels');
  ok(dock && dock.b > 844 - 120 && top && top.t < 80, `390px: panels at the top (${top?.t.toFixed(0)}), console docked at the bottom (${dock?.b.toFixed(0)})`);
  ok(!(await a.evaluate(() => document.documentElement.scrollWidth > innerWidth)), '390px: no horizontal overflow');
  const small = await a.evaluate(() => [...document.querySelectorAll('#meet-bar button')].filter((x) => x.getClientRects().length).map((x) => x.getBoundingClientRect()).filter((r) => r.width < 44 || r.height < 44).length);
  ok(small === 0, `390px: every key is a 44 px target (${small} smaller)`);
  const pm = await blankItems(a, 'btn-more', 'more-pop');
  ok(pm.n > 0 && pm.blank.length === 0, `390px: every row of More shows its words (${pm.n} rows; blank: ${pm.blank.join(', ') || 'none'})`);

  ok(log.filter((l) => l.includes('PAGE ERROR')).length === 0, `no page errors (${log.filter((l) => l.includes('PAGE ERROR')).join(' | ') || 'none'})`);
} catch (e) {
  fail(e);
}
await browser.close();
const n = failed();
console.log(n ? `${n} FAILED` : 'all passed');
process.exit(n ? 1 : 0);
