'use strict';
// This worker handles push only. It never intercepts requests, caches pages,
// or stores authenticated responses or conversation contents.
const basePath = '/tgw/webui/';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function notificationURL(value) {
  try {
    const url = new URL(value, self.location.origin);
    const id = url.searchParams.get('session_id');
    if (url.origin === self.location.origin && url.pathname === basePath && id && uuid.test(id)) return basePath + '?session_id=' + id.toLowerCase();
  } catch (_) { /* Invalid payloads open the session chooser. */ }
  return basePath;
}
self.addEventListener('install', event => event.waitUntil(self.skipWaiting()));
self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
self.addEventListener('push', event => {
  let data;
  try { data = event.data?.json(); } catch (_) { /* Every push must still display a notification. */ }
  const options = {
    body: 'A Codex turn has finished.',
    icon: basePath + 'static/webui-icon-192.png',
    data: { url: notificationURL(data?.url) }
  };
  if (typeof data?.tag === 'string' && uuid.test(data.tag)) options.tag = data.tag.toLowerCase();
  // No foreground suppression: iPhone requires a visible notification for
  // every delivered push. The page never emits duplicate local notifications.
  event.waitUntil(self.registration.showNotification('Turn finished', options));
});
async function focusConversation(url) {
  const pages = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  const existing = pages.find(client => {
    try { const current = new URL(client.url); return current.origin === self.location.origin && current.pathname === basePath; } catch (_) { return false; }
  });
  if (existing) {
    const channel = new MessageChannel();
    const handled = new Promise(resolve => {
      const timer = setTimeout(() => { channel.port1.close(); resolve(false); }, 750);
      channel.port1.onmessage = event => {
        if (event.data?.type !== 'notification-handled') return;
        clearTimeout(timer); channel.port1.close(); resolve(true);
      };
    });
    try {
      existing.postMessage({ type: 'codex-notification-open', url }, [channel.port2]);
      await existing.focus();
      if (await handled) return;
    } catch (_) { /* An old or closing page cannot handle this deep link. */ }
  }
  await self.clients.openWindow(url);
}
self.addEventListener('notificationclick', event => {
  event.notification.close();
  event.waitUntil(focusConversation(notificationURL(event.notification.data?.url)));
});
