// Picture-in-picture: the meeting follows you when you leave the tab.
//
// Two mechanisms, because the platform gives us two and neither is sufficient
// alone.
//
//  1. Document Picture-in-Picture (Chrome 116+) opens a real, styleable
//     window. We put a live grid and working controls in it, so the meeting
//     stays usable rather than merely visible. This is what Google Meet uses.
//     It requires transient user activation, so it hangs off a button.
//
//  2. The `autopictureinpicture` attribute lets Chrome enter element PiP by
//     itself when the tab is hidden -- but only for an installed PWA, which
//     is why this repository ships a web app manifest. Without installation
//     the browser refuses, and refuses correctly: a page that could summon a
//     floating window on its own whenever you looked away would be a
//     nuisance vector, not a feature.
//
// So: install the app and switching tabs pops the meeting out automatically;
// otherwise press the button. We never pretend the automatic path worked
// when the browser declined it.

let pipWindow = null;
let refreshTimer = null;

export function pipSupported() {
  return 'documentPictureInPicture' in window || document.pictureInPictureEnabled;
}

export function pipActive() {
  return !!pipWindow || !!document.pictureInPictureElement;
}

/**
 * @param {object} opts
 *   getTiles()   -> [{id, name, stream, muted, speaking, self}]
 *   onToggleMic(), onToggleCam(), onLeave()
 *   getState()   -> {mic, cam}
 *   onClose()
 */
export async function openPiP(opts) {
  if (pipWindow) {
    pipWindow.focus();
    return true;
  }
  if ('documentPictureInPicture' in window) {
    try {
      return await openDocumentPiP(opts);
    } catch (err) {
      // Falls through to element PiP. A refused document PiP is usually a
      // missing user gesture, which the simpler API sometimes tolerates.
      console.warn('document PiP unavailable:', err.message);
    }
  }
  return openElementPiP(opts);
}

export async function closePiP() {
  if (pipWindow) {
    pipWindow.close();
    pipWindow = null;
  }
  if (document.pictureInPictureElement) {
    try {
      await document.exitPictureInPicture();
    } catch {
      // Already gone.
    }
  }
  clearInterval(refreshTimer);
  refreshTimer = null;
}

// ---------------------------------------------------------------------------
// Document PiP
// ---------------------------------------------------------------------------

async function openDocumentPiP(opts) {
  const win = await window.documentPictureInPicture.requestWindow({
    width: 420,
    height: 300,
    // Keeps the window out of the tab-switching order, matching how a
    // floating meeting is expected to behave.
    disallowReturnToOpener: false,
  });
  pipWindow = win;

  // The PiP window is a separate document with no styles of its own. Copying
  // the sheet rather than re-authoring one keeps the two surfaces from
  // drifting apart the first time somebody edits a colour.
  copyStyles(win);
  win.document.documentElement.setAttribute(
    'data-theme',
    document.documentElement.getAttribute('data-theme') ?? ''
  );

  const root = win.document.createElement('div');
  root.className = 'pip-root';
  root.innerHTML = `
    <div class="pip-grid" id="pip-grid"></div>
    <div class="pip-bar">
      <button class="pip-btn" id="pip-mic" title="Mute"></button>
      <button class="pip-btn" id="pip-cam" title="Camera"></button>
      <button class="pip-btn pip-back" id="pip-back" title="Back to tab">Back</button>
    </div>`;
  win.document.body.append(root);

  const grid = win.document.getElementById('pip-grid');
  const micBtn = win.document.getElementById('pip-mic');
  const camBtn = win.document.getElementById('pip-cam');

  micBtn.onclick = () => {
    opts.onToggleMic?.();
    paint();
  };
  camBtn.onclick = () => {
    opts.onToggleCam?.();
    paint();
  };
  win.document.getElementById('pip-back').onclick = () => {
    win.close();
    window.focus();
  };

  // Streams are attached to fresh video elements rather than by moving the
  // originals. A MediaStream can drive any number of elements, and moving a
  // node between documents fights with the main grid the moment it re-renders.
  const attached = new Map();

  function paint() {
    const tiles = opts.getTiles?.() ?? [];
    const seen = new Set();

    for (const t of tiles) {
      seen.add(t.id);
      let cell = attached.get(t.id);
      if (!cell) {
        cell = win.document.createElement('div');
        cell.className = 'pip-tile';
        cell.innerHTML = `<video autoplay playsinline muted></video>
          <span class="pip-name"></span>`;
        grid.append(cell);
        attached.set(t.id, cell);
      }
      const v = cell.querySelector('video');
      if (t.stream && v.srcObject !== t.stream) v.srcObject = t.stream;
      cell.classList.toggle('speaking', !!t.speaking);
      cell.classList.toggle('mirror', !!t.mirror);
      cell.querySelector('.pip-name').textContent = t.name;
    }
    for (const [id, cell] of attached) {
      if (!seen.has(id)) {
        cell.remove();
        attached.delete(id);
      }
    }
    grid.dataset.count = String(Math.min(tiles.length, 4));

    const st = opts.getState?.() ?? {};
    micBtn.textContent = st.mic ? 'Mute' : 'Unmute';
    micBtn.classList.toggle('off', !st.mic);
    camBtn.textContent = st.cam ? 'Camera' : 'Camera off';
    camBtn.classList.toggle('off', !st.cam);
  }

  paint();
  refreshTimer = setInterval(paint, 500);

  win.addEventListener('pagehide', () => {
    clearInterval(refreshTimer);
    refreshTimer = null;
    pipWindow = null;
    opts.onClose?.();
  });

  return true;
}

function copyStyles(win) {
  for (const sheet of document.styleSheets) {
    try {
      const css = [...sheet.cssRules].map((r) => r.cssText).join('\n');
      const style = win.document.createElement('style');
      style.textContent = css;
      win.document.head.append(style);
    } catch {
      // A cross-origin sheet cannot be read. Link it instead.
      if (sheet.href) {
        const link = win.document.createElement('link');
        link.rel = 'stylesheet';
        link.href = sheet.href;
        win.document.head.append(link);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Element PiP fallback
// ---------------------------------------------------------------------------

async function openElementPiP(opts) {
  const tiles = opts.getTiles?.() ?? [];
  // Prefer somebody else: a floating window of your own face is not what
  // anyone wants to keep an eye on.
  const target = tiles.find((t) => !t.self && t.stream) ?? tiles.find((t) => t.stream);
  if (!target) return false;

  const v = document.querySelector(`.tile[data-id="${CSS.escape(target.id)}"] video`);
  if (!v || !document.pictureInPictureEnabled || v.disablePictureInPicture) return false;

  try {
    await v.requestPictureInPicture();
    v.addEventListener('leavepictureinpicture', () => opts.onClose?.(), { once: true });
    return true;
  } catch (err) {
    console.warn('element PiP refused:', err.message);
    return false;
  }
}

// ---------------------------------------------------------------------------
// Automatic pop-out
// ---------------------------------------------------------------------------

/**
 * Marks a video as the one Chrome should float when the tab is hidden.
 * Only takes effect for an installed PWA; harmless otherwise.
 */
export function markAutoPiP(video) {
  if (!video) return;
  try {
    video.autoPictureInPicture = true;
    video.setAttribute('autopictureinpicture', '');
  } catch {
    // Unsupported. The manual button still works.
  }
}

/**
 * Reports whether automatic pop-out can actually happen, so the interface can
 * tell the truth rather than promising a behaviour the browser will refuse.
 */
export function autoPiPAvailable() {
  const installed =
    window.matchMedia?.('(display-mode: standalone)').matches ||
    window.navigator.standalone === true;
  return installed && 'documentPictureInPicture' in window;
}
