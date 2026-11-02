// The application's own title bar.
//
// In the desktop app the system caption is stripped -- only the caption, the
// resize borders and the window's own maximise and minimise behaviour stay
// with Windows -- and this bar is drawn in its place. So the search field and
// the account sit in the top strip beside the minimise, maximise and close
// buttons, coloured like the product: one band, not two.
//
// In a browser tab there is no caption to replace, so the buttons and the drag
// handle are not drawn. The test is a capability the shell provides rather
// than a user-agent string, because the question is "can this window be
// dragged from script" and only the thing that can answer it should be asked.

/** Whether the page is running inside the desktop shell. */
export function inDesktopShell() {
  return typeof window !== 'undefined' && typeof window.aetherStartDrag === 'function';
}

/**
 * Windows' order. An application that reorders these is wrong in a way people
 * feel without being able to name.
 */
export const WINDOW_BUTTONS = ['minimise', 'maximise', 'close'];

/**
 * Whether a mousedown here should drag the window.
 *
 * Anything interactive is excluded. This is the whole reason dragging is
 * handed to Windows from the page rather than hit-tested in the shell: the
 * page knows what is interactive, the shell does not, so the page decides.
 */
export function isDragTarget(el) {
  if (!el || typeof el.closest !== 'function') return false;
  if (!el.closest('[data-drag]')) return false;
  return !el.closest('button, a, input, select, textarea, [role="button"], [data-no-drag]');
}

/**
 * Whether a mouse event is the back or forward button. Chromium turns the
 * unhandled mouseup into history navigation; the shell never sees mouse
 * buttons, so the page has to consume it.
 */
export function isHistoryMouseButton(e) {
  return e != null && (e.button === 3 || e.button === 4);
}

/**
 * Whether a key event is one a browser would turn into history navigation.
 * The shell swallows these before the page sees them; this is the page's
 * own copy of the rule, for a browser tab that is not the shell and for
 * the moment between the view being created and the shell's hook landing.
 */
export function isHistoryKey(e) {
  if (!e) return false;
  if (e.key === 'BrowserBack' || e.key === 'BrowserForward') return true;
  return Boolean(e.altKey) && (e.key === 'ArrowLeft' || e.key === 'ArrowRight');
}

let shellLocked = false;

/**
 * Stop the page behaving like a browser tab inside the desktop shell: the
 * mouse's back and forward buttons, Alt+arrows, and a dragged image or link
 * lifting a ghost off the page. Once per document.
 */
export function lockShellNavigation(doc = typeof document !== 'undefined' ? document : null) {
  if (shellLocked || !doc || !inDesktopShell()) return;
  shellLocked = true;
  const swallowMouse = (e) => {
    if (isHistoryMouseButton(e)) e.preventDefault();
  };
  for (const type of ['mousedown', 'mouseup', 'auxclick', 'pointerup']) {
    doc.addEventListener(type, swallowMouse, { capture: true });
  }
  doc.addEventListener(
    'keydown',
    (e) => {
      if (isHistoryKey(e)) e.preventDefault();
    },
    { capture: true }
  );
  // Only the file picker's and the composer's own drags mean anything here.
  doc.addEventListener(
    'dragstart',
    (e) => {
      const t = e.target;
      if (t && typeof t.closest === 'function' && t.closest('img, a, svg') && !t.closest('[draggable="true"]')) {
        e.preventDefault();
      }
    },
    { capture: true }
  );
}

/**
 * Attach dragging, double-click-to-maximise, the window buttons and search.
 *
 * Returns a teardown, because the bar is re-rendered when the session changes
 * and a listener bound to a replaced element is a leak with a long fuse.
 */
export function attachTitleBar(root, { onSearch } = {}) {
  if (!root) return () => {};
  const offs = [];

  const find = root.querySelector('[data-appbar-search]');
  if (find && onSearch) {
    const onInput = () => onSearch(find.value.trim());
    const onKey = (e) => {
      if (e.key !== 'Escape') return;
      find.value = '';
      onSearch('');
      find.blur();
    };
    find.addEventListener('input', onInput);
    find.addEventListener('keydown', onKey);
    offs.push(() => {
      find.removeEventListener('input', onInput);
      find.removeEventListener('keydown', onKey);
    });
  }

  if (!inDesktopShell()) return () => offs.forEach((f) => f());
  lockShellNavigation();

  const onMouseDown = (e) => {
    // Left button only. A right-drag on a real caption opens the system menu;
    // starting a move instead is the small wrongness that reads as fake.
    if (e.button !== 0 || !isDragTarget(e.target)) return;
    window.aetherStartDrag();
  };
  const onDblClick = (e) => {
    if (!isDragTarget(e.target)) return;
    window.aetherToggleMaximise();
    refreshMaximiseState(root);
  };
  root.addEventListener('mousedown', onMouseDown);
  root.addEventListener('dblclick', onDblClick);
  offs.push(() => {
    root.removeEventListener('mousedown', onMouseDown);
    root.removeEventListener('dblclick', onDblClick);
  });

  root.querySelectorAll('[data-win]').forEach((b) => {
    b.onclick = () => {
      const what = b.dataset.win;
      if (what === 'minimise') window.aetherMinimise();
      else if (what === 'maximise') {
        window.aetherToggleMaximise();
        refreshMaximiseState(root);
      } else if (what === 'close') window.aetherCloseWindow();
    };
  });

  refreshMaximiseState(root);
  const onResize = () => refreshMaximiseState(root);
  window.addEventListener('resize', onResize);
  offs.push(() => window.removeEventListener('resize', onResize));

  return () => offs.forEach((f) => f());
}

// The maximise button shows which way it will go. Asked of the shell rather
// than tracked here: the window can be maximised by dragging it to the top
// edge or by a shortcut, neither of which passes through this file.
async function refreshMaximiseState(root) {
  const btn = root.querySelector('[data-win="maximise"]');
  if (!btn || typeof window.aetherIsMaximised !== 'function') return;
  try {
    const max = await window.aetherIsMaximised();
    btn.classList.toggle('is-max', !!max);
    btn.title = max ? 'Restore' : 'Maximise';
  } catch {
    // The shell is gone; leave the button as it is.
  }
}

/** The window buttons. Drawn only in the app; a browser tab has its own. */
export function windowButtonsHTML() {
  if (!inDesktopShell()) return '';
  return `<div class="win-buttons" data-no-drag>
    <button class="win-btn" data-win="minimise" title="Minimise" aria-label="Minimise">
      <svg width="10" height="10" viewBox="0 0 10 10"><path d="M0 5h10" stroke="currentColor" stroke-width="1"/></svg>
    </button>
    <button class="win-btn" data-win="maximise" title="Maximise" aria-label="Maximise">
      <svg class="ico-max" width="10" height="10" viewBox="0 0 10 10">
        <rect x="0.5" y="0.5" width="9" height="9" fill="none" stroke="currentColor" stroke-width="1"/>
      </svg>
      <svg class="ico-restore" width="10" height="10" viewBox="0 0 10 10">
        <rect x="0.5" y="2.5" width="7" height="7" fill="none" stroke="currentColor" stroke-width="1"/>
        <path d="M2.5 2.5V0.5h7v7h-2" fill="none" stroke="currentColor" stroke-width="1"/>
      </svg>
    </button>
    <button class="win-btn danger" data-win="close" title="Close" aria-label="Close">
      <svg width="10" height="10" viewBox="0 0 10 10">
        <path d="M0 0l10 10M10 0L0 10" stroke="currentColor" stroke-width="1"/>
      </svg>
    </button>
  </div>`;
}
