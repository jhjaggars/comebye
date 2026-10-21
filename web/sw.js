// Service worker: makes the app installable and receives Web Push. It has
// no fetch handler and caches nothing; everything is served fresh.
//
// Deploys: app.js notices a new server version and reloads the page
// itself. A changed sw.js is picked up by the browser's own update check
// (on navigation, including that reload); skipWaiting + clients.claim
// just swap it in, and the page does not listen for controllerchange, so
// the two never trigger each other or fight over a reload.
"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (ev) => ev.waitUntil(self.clients.claim()));

// sameOriginPath keeps notification links inside the app.
function sameOriginPath(u) {
  try {
    const url = new URL(u || "/", self.location.origin);
    if (url.origin !== self.location.origin) return "/";
    return url.pathname + url.search + url.hash;
  } catch {
    return "/";
  }
}

// Every push must show a notification (iOS revokes subscriptions that do
// not), so this never returns early; the server skips pushes for an agent
// that is on screen.
self.addEventListener("push", (ev) => {
  let d = {};
  if (ev.data) {
    try { d = ev.data.json(); } catch { d = { body: ev.data.text() }; }
  }
  const opts = {
    body: d.body || "",
    icon: "icon-192.png",
    badge: "icon-192.png",
    data: { url: sameOriginPath(d.url) },
  };
  if (d.tag) {
    opts.tag = d.tag;           // a newer notification for an agent replaces the older one
    opts.renotify = !!d.renotify; // ...and buzzes again for "needs input"
  }
  ev.waitUntil(self.registration.showNotification(d.title || "herdr", opts));
});

self.addEventListener("notificationclick", (ev) => {
  ev.notification.close();
  const path = sameOriginPath(ev.notification.data && ev.notification.data.url);
  ev.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const win = wins.find((c) => new URL(c.url).origin === self.location.origin);
    if (win) {
      // Route in place: the app listens for this and updates its hash, so
      // its state (and SSE connection) survives.
      try { await win.focus(); } catch {}
      win.postMessage({ type: "navigate", url: path });
      return;
    }
    await self.clients.openWindow(path);
  })());
});

// The push service rotated the subscription: re-subscribe with the same
// server key and tell the server, replacing the old endpoint.
self.addEventListener("pushsubscriptionchange", (ev) => {
  ev.waitUntil((async () => {
    const old = ev.oldSubscription;
    let sub = ev.newSubscription;
    if (!sub) {
      const key = old && old.options && old.options.applicationServerKey;
      if (!key) return;
      sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
    }
    await fetch("/api/push/subscribe", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...sub.toJSON(), replaces: old ? old.endpoint : "" }),
    });
  })());
});
