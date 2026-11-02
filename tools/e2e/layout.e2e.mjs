// Layout harness: the client at phone, tablet and desktop widths, in the
// installed Chrome, headless.
//
// Rule 8 in CLAUDE.md: every screen adapts to every device, strictly. This
// is the proof. It signs in as a throwaway account, walks the main screens
// at three viewports, and fails on any page-level horizontal overflow, any
// visible element that leaves the viewport without a clipping ancestor,
// and -- on touch viewports -- any tap target under 44x44 CSS px. It also
// writes a screenshot of every screen, because numbers do not see a button
// that is technically inside the viewport and unusable anyway.
//
//   node tools/e2e/layout.e2e.mjs [https://host:8443] [--only phone]
//
// Screenshots land in tools/e2e/.shots/<viewport>-<screen>.png.

import { existsSync, mkdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

const here = dirname(fileURLToPath(import.meta.url));
const BASE = process.argv.find((a) => a.startsWith('http')) || 'https://localhost:8443';
const onlyIdx = process.argv.indexOf('--only');
const ONLY = onlyIdx > 0 ? process.argv[onlyIdx + 1] : '';
// --shell: the Windows app's layout. The shell injects a handful of
// functions before the page loads and the client draws its own title bar
// when it sees them; stubbing the same names here renders that layout in
// Chrome, at the window's floor size and at the tablet breakpoint.
const SHELL = process.argv.includes('--shell');
const SHOTS = join(here, '.shots');
mkdirSync(SHOTS, { recursive: true });

const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
].find((p) => existsSync(p));
if (!CHROME) {
  console.error('no Chrome or Edge found to drive');
  process.exit(2);
}

// The messaging harness's throwaway account. Run it first if this is missing.
const host = new URL(BASE).host;
const accounts = JSON.parse(readFileSync(join(here, '.accounts.json'), 'utf8'));
const acct = accounts[host]?.A;
if (!acct) {
  console.error(`no stored account for ${host}; run messaging.e2e.mjs once first`);
  process.exit(2);
}

const VIEWPORTS = [
  { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true, deviceScaleFactor: 2 },
  { name: 'tablet', width: 820, height: 1180, isMobile: true, hasTouch: true, deviceScaleFactor: 2 },
  { name: 'desktop', width: 1280, height: 800, isMobile: false, hasTouch: false, deviceScaleFactor: 1 },
].filter((v) => !ONLY || v.name === ONLY);
if (SHELL) {
  VIEWPORTS.length = 0;
  VIEWPORTS.push(
    { name: 'shell-floor', width: 940, height: 620, isMobile: false, hasTouch: false, deviceScaleFactor: 1, shell: true },
    { name: 'shell-1024', width: 1024, height: 700, isMobile: false, hasTouch: false, deviceScaleFactor: 1, shell: true }
  );
}
const SHELL_STUBS = () => {
  const noop = () => {};
  Object.assign(window, {
    aetherHostReady: () => true, aetherStartDrag: noop, aetherMinimise: noop, aetherToggleMaximise: noop,
    aetherCloseWindow: noop, aetherIsMaximised: async () => false, aetherShowWindow: noop,
    aetherNotify: noop, aetherBadge: noop, aetherRinging: noop, aetherPermissions: noop,
  });
};

let failures = 0;
const fail = (msg) => {
  failures++;
  console.log(`  FAIL  ${msg}`);
};
const okay = (msg) => console.log(`  ok    ${msg}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Runs in the page. Returns what is wrong with the layout right now.
function auditInPage(touch) {
  const vw = innerWidth;
  // Visible means visible: the element and every ancestor drawn, not
  // faded out, not marked hidden, and the box has area. A signed-out panel
  // left in the DOM at opacity 0 behind the home screen is not a target.
  const hiddenCache = new Map();
  const hiddenBy = (el) => {
    if (hiddenCache.has(el)) return hiddenCache.get(el);
    let h = false;
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') h = true;
    else {
      const cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden' || Number(cs.opacity) === 0 || cs.pointerEvents === 'none' && el.matches('.auth, .home, .app, [role=dialog]')) h = true;
    }
    if (!h && el.parentElement && el.parentElement !== document.documentElement) h = hiddenBy(el.parentElement);
    hiddenCache.set(el, h);
    return h;
  };
  const isVisible = (el) => {
    if (hiddenBy(el)) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const clipsX = (el) => {
    const cs = getComputedStyle(el);
    return /(hidden|auto|scroll|clip)/.test(cs.overflowX) || /(hidden|auto|scroll|clip)/.test(cs.overflow);
  };
  const path = (el) => {
    const parts = [];
    for (let e = el, n = 0; e && e !== document.body && n < 4; e = e.parentElement, n++) {
      let p = e.tagName.toLowerCase();
      if (e.id) p += '#' + e.id;
      else if (e.className && typeof e.className === 'string') p += '.' + e.className.trim().split(/\s+/).slice(0, 2).join('.');
      parts.unshift(p);
    }
    return parts.join(' > ');
  };
  const out = { vw, pageScrollWidth: document.documentElement.scrollWidth, overflowing: [], smallTargets: [], counts: {} };
  const all = [...document.querySelectorAll('body *')];
  out.counts.elements = all.length;
  for (const el of all) {
    if (!isVisible(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.right > vw + 1 || r.left < -1) {
      // Inside something that clips horizontally? Then it is that thing's
      // business (a scrolling table, a marquee), not a page overflow.
      let clipped = false;
      for (let a = el.parentElement; a && a !== document.documentElement; a = a.parentElement) {
        if (clipsX(a)) {
          const ar = a.getBoundingClientRect();
          if (ar.right <= vw + 1 && ar.left >= -1) {
            clipped = true;
            break;
          }
        }
      }
      if (!clipped && out.overflowing.length < 25) {
        out.overflowing.push({ path: path(el), left: Math.round(r.left), right: Math.round(r.right), w: Math.round(r.width) });
      }
    }
  }
  if (touch) {
    const targets = [...document.querySelectorAll('button, a[href], input:not([type=hidden]), select, textarea, [role="button"], [role="tab"], [role="menuitem"]')];
    for (const el of targets) {
      if (!isVisible(el)) continue;
      const r = el.getBoundingClientRect();
      // Off-screen or inside a hidden panel does not count.
      if (r.bottom < 0 || r.top > innerHeight) continue;
      if (r.width < 44 || r.height < 44) {
        // A control with generous padding around it that is itself the hit
        // area (min-height via a parent) is fine; anything else is a miss.
        if (out.smallTargets.length < 40) out.smallTargets.push({ path: path(el), w: Math.round(r.width), h: Math.round(r.height) });
        out.counts.small = (out.counts.small || 0) + 1;
      }
    }
  }
  return out;
}

async function signIn(page) {
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  // Signed in means the sign-in panel is not drawn. (A first version tested
  // a [hidden] attribute the app does not use, took the auth page for the
  // home page, and audited the wrong screen seven times over.)
  const homeShown = () => getComputedStyle(document.getElementById('auth')).display === 'none' && getComputedStyle(document.getElementById('home')).display !== 'none';
  if (await page.evaluate(homeShown)) return;
  await page.evaluate(
    async (email, password) => {
      const m = await import('/src/api.js');
      await m.login(email, password);
    },
    acct.email,
    acct.password
  );
  await page.goto(BASE + '/', { waitUntil: 'networkidle2', timeout: 30000 });
  await page.waitForFunction(homeShown, { timeout: 15000 });
  await sleep(800);
}

const SCREENS = [
  { name: 'home', go: async () => {} },
  {
    name: 'chat-list',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('[data-home="chat"]')?.click());
      await sleep(900);
    },
  },
  {
    name: 'thread',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('.conv-row .conv')?.click());
      await sleep(1200);
    },
  },
  {
    name: 'thread-find',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('c-find-here')?.click());
      await sleep(300);
    },
  },
  {
    name: 'new-group',
    go: async (page) => {
      await page.evaluate(() => {
        document.getElementById('ts-close')?.click();
        document.getElementById('c-new')?.click();
      });
      await sleep(200);
      await page.evaluate(() => document.querySelector('[data-new="group"]')?.click());
      await sleep(500);
    },
  },
  {
    name: 'members',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('[data-cancel]')?.click());
      await sleep(200);
      await page.evaluate(() => (document.getElementById('c-people') || document.getElementById('c-add-people'))?.click());
      await sleep(700);
    },
  },
  {
    name: 'calendar',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('[data-cancel]')?.click());
      await sleep(200);
      await page.evaluate(() => document.querySelector('[data-home="calendar"]')?.click());
      await sleep(700);
    },
  },
  {
    // The schedule form (title, time, invitees, co-organizers): a dialog on
    // a desktop, a sheet on a phone.
    name: 'schedule',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('cal-new')?.click());
      await sleep(600);
    },
  },
  {
    name: 'schedule-closed',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('#dialog [data-cancel]')?.click());
      await sleep(300);
    },
  },
  // A meeting, start to finish: the pre-join lobby (fake camera and
  // microphone from Chrome's flags), the stage, its chat and people panels,
  // then leave so the room ends.
  {
    name: 'prejoin',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('[data-home="meet"]')?.click());
      await sleep(500);
      await page.evaluate(() => document.getElementById('next-start')?.click());
      await page.waitForFunction(() => getComputedStyle(document.getElementById('prejoin')).display !== 'none', { timeout: 15000 });
      await sleep(900);
    },
  },
  {
    name: 'stage',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('pj-join')?.click());
      await page.waitForFunction(() => getComputedStyle(document.getElementById('app')).display !== 'none', { timeout: 20000 });
      await sleep(1500);
    },
  },
  {
    name: 'stage-chat',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('rail-chat')?.click());
      await sleep(600);
    },
  },
  {
    name: 'stage-people',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('rail-people')?.click());
      await sleep(600);
    },
  },
  {
    // A person's "..." menu in the People panel (Pin for me, Spotlight ...).
    name: 'stage-person-menu',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('#view-people .person-more')?.click());
      await sleep(400);
    },
  },
  {
    // More -> Meeting options: a dialog on a desktop, a full sheet on a phone.
    name: 'meeting-options',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('tile-menu')?.remove());
      await page.evaluate(() => document.getElementById('more-options')?.click());
      await sleep(500);
    },
  },
  {
    // The View menu hanging from the bar (bottom dock on a phone is Leave's).
    name: 'view-menu',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('#dialog [data-no]')?.click());
      // Close the panel first: on a phone it is a sheet over the bar.
      await page.evaluate(() => document.getElementById('side-close')?.click());
      await sleep(200);
      await page.evaluate(() => document.getElementById('btn-view')?.click());
      await sleep(400);
    },
  },
  {
    // Leave's arrow, with "End meeting".
    name: 'leave-menu',
    go: async (page) => {
      await page.keyboard.press('Escape');
      await page.evaluate(() => document.getElementById('btn-leave-more')?.click());
      await sleep(400);
    },
  },
  {
    // Q&A: the composer, sorting, the host's tabs and settings.
    name: 'qna-panel',
    go: async (page) => {
      await page.keyboard.press('Escape');
      await page.evaluate(() => document.getElementById('btn-qna')?.click());
      await sleep(900);
    },
  },
  {
    // Notes, before the organizer adds them: "Add meeting notes".
    name: 'notes-empty',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('btn-notes')?.click());
      await sleep(900);
    },
  },
  {
    // Notes added: Agenda, Notes, Follow-up tasks, with a task.
    name: 'notes',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('nt-create')?.click());
      await sleep(900);
      await page.evaluate(() => {
        const el = document.querySelector('#view-notes [data-add="tasks"]');
        if (!el) return;
        el.value = 'Send the deck to finance';
        el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      });
      await sleep(900);
    },
  },
  {
    // Share > Whiteboard: the board on the stage with its tools.
    name: 'whiteboard',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('side-close')?.click());
      await page.evaluate(() => document.getElementById('btn-share-more')?.click());
      await sleep(150);
      await page.evaluate(() => document.getElementById('share-wb')?.click());
      await sleep(1000);
    },
  },
  {
    name: 'whiteboard-off',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('wb-close')?.click());
      await sleep(500);
    },
  },
  {
    // More, with "Record and transcribe" for the organizer.
    name: 'more-record',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('btn-more')?.click());
      await sleep(300);
    },
  },
  {
    // Start transcription: the banner, and the speaker asked about their speech.
    name: 'transcribing',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('more-tr')?.click());
      await sleep(1000);
    },
  },
  {
    // Show transcript: the panel, then transcription stopped from it.
    name: 'transcript',
    go: async (page) => {
      await page.evaluate(() => { document.getElementById('btn-more')?.click(); document.getElementById('more-transcript')?.click(); });
      await sleep(1000);
    },
  },
  {
    name: 'transcript-stopped',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('tr-toggle')?.click());
      await sleep(800);
      await page.evaluate(() => document.getElementById('side-close')?.click());
      await sleep(300);
    },
  },
  {
    // More > Call health.
    name: 'call-health',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('more-health')?.click());
      await sleep(1600);
    },
  },
  {
    // Rooms: "Create breakout rooms".
    name: 'rooms-create',
    go: async (page) => {
      await page.keyboard.press('Escape');
      await page.evaluate(() => document.getElementById('btn-rooms')?.click());
      await sleep(500);
    },
  },
  {
    // Rooms created: the list and the host's actions.
    name: 'rooms-list',
    go: async (page) => {
      await page.evaluate(() => {
        const c = document.getElementById('br-count');
        if (c) c.value = '3';
        document.getElementById('br-create')?.click();
      });
      await sleep(900);
    },
  },
  {
    // "Rooms settings": a dialog, a full sheet on a phone.
    name: 'rooms-settings',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('br-settings')?.click());
      await sleep(500);
    },
  },
  {
    name: 'rooms-closed-again',
    go: async (page) => {
      await page.evaluate(() => document.querySelector('#dialog [data-no]')?.click());
      await page.evaluate(() => document.getElementById('side-close')?.click());
      await sleep(300);
    },
  },
  {
    name: 'left',
    go: async (page) => {
      await page.evaluate(() => document.getElementById('btn-leave')?.click());
      await sleep(1500);
      // A "leave?" confirmation, if one is drawn, is answered.
      await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => /^(leave|end|yes)/i.test(b.textContent.trim()))?.click());
      await sleep(1200);
    },
  },
];

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: [
    '--ignore-certificate-errors', '--no-first-run', '--disable-gpu', '--autoplay-policy=no-user-gesture-required',
    '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
  ],
});

try {
  for (const vp of VIEWPORTS) {
    console.log(`\n== ${vp.name} ${vp.width}x${vp.height}${vp.hasTouch ? ' touch' : ''}`);
    const page = await browser.newPage();
    await page.setViewport(vp);
    if (vp.shell) await page.evaluateOnNewDocument(SHELL_STUBS);
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e.message || e)));
    await signIn(page);
    for (const screen of SCREENS) {
      await screen.go(page);
      const a = await page.evaluate(auditInPage, vp.hasTouch);
      const shot = join(SHOTS, `${vp.name}-${screen.name}.png`);
      await page.screenshot({ path: shot });
      const tag = `${vp.name}/${screen.name}`;
      if (a.pageScrollWidth > a.vw + 1) fail(`${tag}: page scrolls sideways (${a.pageScrollWidth} > ${a.vw})`);
      else okay(`${tag}: no page overflow`);
      if (a.overflowing.length) {
        fail(`${tag}: ${a.overflowing.length} element(s) leave the viewport`);
        for (const o of a.overflowing.slice(0, 6)) console.log(`          ${o.path}  [${o.left}..${o.right}] w=${o.w}`);
      } else okay(`${tag}: nothing leaves the viewport`);
      // Controls drawn on top of each other: the meeting bar's buttons and the
      // clock. Overflow and tap-size checks cannot see this, and it got past
      // them twice (the phone's clock under Chat and People).
      const overlaps = await page.evaluate(() => {
        const els = [...document.querySelectorAll('#meet-bar .mb-left > *, #meet-bar .mb-panels > *, #meet-bar .mb-devices > *')]
          .filter((e) => { const s = getComputedStyle(e); const r = e.getBoundingClientRect(); return s.display !== 'none' && s.visibility !== 'hidden' && r.width > 0 && r.height > 0; });
        const out = [];
        for (let i = 0; i < els.length; i++) for (let j = i + 1; j < els.length; j++) {
          const a = els[i].getBoundingClientRect(), b = els[j].getBoundingClientRect();
          const w = Math.min(a.right, b.right) - Math.max(a.left, b.left), h = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top);
          if (w > 2 && h > 2) out.push(`${els[i].id || els[i].className} / ${els[j].id || els[j].className}`);
        }
        return out;
      });
      if (overlaps.length) {
        fail(`${tag}: ${overlaps.length} control(s) in the meeting bar overlap`);
        for (const o of overlaps.slice(0, 6)) console.log(`          ${o}`);
      } else if (await page.evaluate(() => !!document.getElementById('meet-bar')?.offsetParent)) okay(`${tag}: nothing in the meeting bar overlaps`);
      if (vp.hasTouch) {
        if (a.counts.small) {
          fail(`${tag}: ${a.counts.small} tap target(s) under 44px`);
          for (const s of a.smallTargets.slice(0, 8)) console.log(`          ${s.path}  ${s.w}x${s.h}`);
        } else okay(`${tag}: every tap target is 44px or more`);
      }
    }
    if (errors.length) fail(`${vp.name}: page errors: ${[...new Set(errors)].slice(0, 3).join(' | ')}`);
    await page.close();
  }
} finally {
  await browser.close();
}

console.log(`\n${failures === 0 ? 'layout ok' : failures + ' layout failure(s)'}; screenshots in ${SHOTS}`);
process.exit(failures ? 1 : 0);
