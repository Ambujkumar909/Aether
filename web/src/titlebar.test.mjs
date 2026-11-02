import test from 'node:test';
import assert from 'node:assert/strict';

import { WINDOW_BUTTONS, isDragTarget, inDesktopShell, isHistoryMouseButton, isHistoryKey } from './titlebar.js';

test('the mouse back and forward buttons are history; the others are not', () => {
  assert.equal(isHistoryMouseButton({ button: 3 }), true);
  assert.equal(isHistoryMouseButton({ button: 4 }), true);
  for (const b of [0, 1, 2]) assert.equal(isHistoryMouseButton({ button: b }), false);
  assert.equal(isHistoryMouseButton(null), false);
});

test('alt+arrows and the browser keys are history; plain arrows and ctrl+k are not', () => {
  assert.equal(isHistoryKey({ key: 'ArrowLeft', altKey: true }), true);
  assert.equal(isHistoryKey({ key: 'ArrowRight', altKey: true }), true);
  assert.equal(isHistoryKey({ key: 'BrowserBack' }), true);
  assert.equal(isHistoryKey({ key: 'ArrowLeft', altKey: false }), false);
  assert.equal(isHistoryKey({ key: 'k', ctrlKey: true }), false);
  assert.equal(isHistoryKey({ key: 'Backspace' }), false);
  assert.equal(isHistoryKey(null), false);
});

// The bar is only a title bar inside the desktop shell, and the test for that
// is a capability the shell provides -- the question is "can this window be
// dragged from script", and only the thing that can answer it should be asked.

test('a browser tab is not the desktop shell', () => {
  assert.equal(inDesktopShell(), false);
});

test('the shell is recognised by the binding it provides', () => {
  globalThis.window = { aetherStartDrag: () => {} };
  assert.equal(inDesktopShell(), true);
  globalThis.window = { aetherStartDrag: 'yes' };
  assert.equal(inDesktopShell(), false, 'a lookalike property is not a capability');
  delete globalThis.window;
});

// Windows' order. Reordering these is wrong in a way people feel unnamed.
test('the window buttons are minimise, maximise, close, in that order', () => {
  assert.deepEqual(WINDOW_BUTTONS, ['minimise', 'maximise', 'close']);
});

// Dragging is decided by the page, so a search field you cannot click because
// it moves the window is the failure this prevents.
function fakeEl({ inBar = true, interactive = false } = {}) {
  return {
    closest(sel) {
      if (sel === '[data-drag]') return inBar ? {} : null;
      if (sel.startsWith('button')) return interactive ? {} : null;
      return null;
    },
  };
}

test('the bar itself is a drag handle', () => {
  assert.equal(isDragTarget(fakeEl()), true);
});
test('nothing outside the bar is', () => {
  assert.equal(isDragTarget(fakeEl({ inBar: false })), false);
  assert.equal(isDragTarget(null), false);
  assert.equal(isDragTarget({}), false);
});
test('controls inside the bar are not drag handles', () => {
  assert.equal(isDragTarget(fakeEl({ interactive: true })), false);
});

// The search dropdown draws its rows as role="button" divs, and it lives
// inside the bar. A mousedown on one began a window drag and the click never
// arrived -- found in the app, where the same dropdown worked in a browser.
test('anything acting as a button is not a drag handle, whatever its tag', () => {
  const el = {
    closest(sel) {
      if (sel === '[data-drag]') return {};
      return sel.includes('[role="button"]') ? {} : null;
    },
  };
  assert.equal(isDragTarget(el), false);
});
