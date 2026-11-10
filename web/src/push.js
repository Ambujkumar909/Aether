// Push: being told while Aether is closed.
//
// The service worker (sw.js) shows the notice; this registers it and keeps
// the browser's subscription on the server for this sign-in (engine
// modules/notify). Permission is never asked for here out of the blue: it
// is asked where the person can see why (a ring, a chat), and once granted
// this subscribes. The switch in the console turns it off for this browser,
// which also tells the server. The desktop shell has its own tray and
// ringing and never calls this.

import { push as pushApi } from './api.js';

const PUSH_OFF = 'aether.pushOff';

export function pushSupported() {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

function pushOff() {
  try {
    return localStorage.getItem(PUSH_OFF) === '1';
  } catch {
    return false;
  }
}

async function registration() {
  if (!pushSupported()) return null;
  try {
    return await navigator.serviceWorker.register('/sw.js', { scope: '/' });
  } catch (e) {
    console.warn('push: service worker', e);
    return null;
  }
}

// subscribePush asks the browser for a subscription (the push service's
// endpoint and this browser's keys) and hands it to the server. Idempotent:
// an existing subscription is sent again, which refreshes the row.
export async function subscribePush() {
  const reg = await registration();
  if (!reg || Notification.permission !== 'granted') return false;
  try {
    const { enabled, key } = await pushApi.key();
    if (!enabled || !key) return false;
    const sub =
      (await reg.pushManager.getSubscription()) ||
      (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(key) }));
    await pushApi.subscribe(sub.toJSON());
    return true;
  } catch (e) {
    console.warn('push: subscribe', e);
    return false;
  }
}

export async function unsubscribePush() {
  const reg = await registration();
  const sub = reg && (await reg.pushManager.getSubscription());
  if (!sub) return;
  try {
    await pushApi.unsubscribe(sub.endpoint);
  } catch {}
  try {
    await sub.unsubscribe();
  } catch {}
}

// setPushOff is the console switch: off unsubscribes and remembers; on
// forgets the choice and subscribes (asking permission if it was never
// asked, which is a moment the person chose).
export async function setPushOff(off) {
  try {
    localStorage.setItem(PUSH_OFF, off ? '1' : '0');
  } catch {}
  if (off) {
    await unsubscribePush();
    return 'off';
  }
  if (Notification.permission === 'default') {
    try {
      await Notification.requestPermission();
    } catch {}
  }
  return (await subscribePush()) ? 'on' : await pushNoticeState();
}

// pushNoticeState is what the console shows: unsupported, denied (by the
// browser), off (by the person), on, or ask (never decided).
export async function pushNoticeState() {
  if (!pushSupported()) return 'unsupported';
  if (Notification.permission === 'denied') return 'denied';
  if (pushOff()) return 'off';
  try {
    const reg = await navigator.serviceWorker.getRegistration('/');
    const sub = reg && (await reg.pushManager.getSubscription());
    if (sub) return 'on';
  } catch {}
  return Notification.permission === 'granted' ? 'off' : 'ask';
}

// b64ToBytes decodes the server's base64url key for PushManager.
export function b64ToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const bin = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

// initPush registers the worker, subscribes when allowed, and routes what a
// tapped notice names through `route(url)` -- only a same-origin URL.
export function initPush(route) {
  if (!pushSupported()) return;
  navigator.serviceWorker.addEventListener('message', (e) => {
    const d = e.data;
    if (!d || d.type !== 'aether:open' || typeof d.url !== 'string') return;
    let u;
    try {
      u = new URL(d.url, location.origin);
    } catch {
      return;
    }
    if (u.origin !== location.origin) return;
    route(u);
  });
  registration().then(() => {
    if (!pushOff() && Notification.permission === 'granted') subscribePush();
  });
  // Permission granted later, for a ring or a chat: subscribe then.
  if (navigator.permissions?.query) {
    navigator.permissions
      .query({ name: 'notifications' })
      .then((st) => {
        st.onchange = () => {
          if (st.state === 'granted' && !pushOff()) subscribePush();
        };
      })
      .catch(() => {});
  }
}
