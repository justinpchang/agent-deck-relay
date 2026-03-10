// Service worker for agent-deck relay PWA.
// Handles Web Push notifications and basic asset caching.

const CACHE = 'agent-deck-relay-v1';
const PRECACHE = ['/', '/manifest.json', '/icons/icon-192.png'];

// ── Install: precache shell ───────────────────────────────────────────────────
self.addEventListener('install', e => {
  e.waitUntil(
    caches.open(CACHE).then(c => c.addAll(PRECACHE)).then(() => self.skipWaiting())
  );
});

// ── Activate: clean old caches ────────────────────────────────────────────────
self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys().then(keys =>
      Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

// ── Fetch: network-first for API, cache-first for shell ──────────────────────
self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/') || url.pathname === '/health') return; // no caching
  e.respondWith(
    caches.match(e.request).then(cached => cached || fetch(e.request))
  );
});

// ── Push: show notification ───────────────────────────────────────────────────
self.addEventListener('push', e => {
  let data = { title: 'Agent Deck', body: 'A session needs your attention.', url: '/' };
  try { data = { ...data, ...e.data.json() }; } catch (_) {}

  e.waitUntil(
    self.registration.showNotification(data.title, {
      body: data.body,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-96.png',
      tag: `agent-deck-${data.sessionId || 'notify'}`,  // collapses duplicate alerts
      renotify: true,
      data: { url: data.url },
      // Action buttons (Android/macOS — iOS ignores these today)
      actions: [
        { action: 'open',   title: 'View' },
        { action: 'dismiss', title: 'Dismiss' },
      ],
    })
  );
});

// ── Notification click: open/focus the PWA ────────────────────────────────────
self.addEventListener('notificationclick', e => {
  e.notification.close();
  if (e.action === 'dismiss') return;

  const targetUrl = (e.notification.data?.url) || '/';

  e.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then(list => {
      // If PWA already open, focus it and navigate
      for (const client of list) {
        if (client.url.includes(self.registration.scope)) {
          client.postMessage({ type: 'navigate', url: targetUrl });
          return client.focus();
        }
      }
      // Otherwise open a new window
      return clients.openWindow(self.registration.scope + targetUrl.replace(/^\//, ''));
    })
  );
});