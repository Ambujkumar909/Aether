// Aether's service worker: push notifications when no tab is open, and
// nothing else. No caching, no offline page, no fetch handler -- the
// application is one page served fresh, and a worker that intercepts
// requests is a second place for bugs to live.
//
// The payload is JSON the server encrypted for this browser alone
// (internal/modules/notify): a title, a line, a same-origin URL, a tag.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('push', (e) => {
  let n = {};
  try {
    n = e.data ? e.data.json() : {};
  } catch {
    n = { title: 'Aether', body: e.data ? e.data.text() : '' };
  }
  const title = String(n.title || 'Aether').slice(0, 120);
  const url = sameOrigin(n.url) ? n.url : '/';
  e.waitUntil(
    self.registration.showNotification(title, {
      body: String(n.body || '').slice(0, 300),
      tag: String(n.tag || 'aether'),
      renotify: !!n.tag,
      icon: '/brand/mark-256.png',
      badge: '/brand/mark-128.png',
      data: { url },
    })
  );
});

// A tap: bring an Aether window forward and tell it where to go, or open
// one at the URL. Focusing a window and sending it a message keeps what
// the person had open; navigating would reload it.
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = e.notification.data?.url || '/';
  e.waitUntil(
    (async () => {
      const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      const win = all.find((c) => 'focus' in c);
      if (win) {
        await win.focus();
        win.postMessage({ type: 'aether:open', url });
        return;
      }
      await self.clients.openWindow(url);
    })()
  );
});

function sameOrigin(u) {
  if (typeof u !== 'string' || !u) return false;
  try {
    return new URL(u, self.location.origin).origin === self.location.origin;
  } catch {
    return false;
  }
}
