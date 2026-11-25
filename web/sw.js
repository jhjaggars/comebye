// Service worker: makes the app installable, receives Web Push, and keeps the
// app shell (the page, its scripts and icons) so a cold open with no network
// still loads the app; the lists the app saved on the device (IndexedDB) then
// show under the Offline strip.
//
// The shell is network first: online, every load is the hub's current copy,
// so a new release shows at once; the cache answers only when the network
// fails (or the hub's proxy says the hub is down: 502, 503, 504). Only the
// files in SHELL are ever handled. Everything else (/api/*, the event stream,
// uploads, Pages, /stt/, sign-in and any redirect) is not intercepted at all,
// so the browser fetches it as if there were no worker, and nothing
// authenticated or per user is ever cached.
//
// Versions: the hub writes its build version into VERSION when it serves this
// file, so every release is a byte-changed sw.js with its own cache. Deploys:
// app.js notices a new server version and reloads the page itself; the
// browser's own update check on that navigation finds the new sw.js, which
// fills its cache, skips waiting and claims the page; activate deletes the old
// caches. There is no prompt: network first already showed the new release,
// and the page does not listen for controllerchange, so nothing reloads twice.
"use strict";

const VERSION = "__COMEBYE_VERSION__";
const PREFIX = "comebye-shell-";
const CACHE = PREFIX + VERSION;
const SCOPE = new URL("./", self.location).pathname;
// SHELL: what index.html loads at start. The lazy, large files (mermaid,
// highlight.js, the speech-to-text runtime) are left out: offline they are
// simply not there, as now.
const SHELL = ["./", "themes.js", "vendor/marked.umd.js", "vendor/purify.min.js", "app.js", "unlockcrypto.js",
  "keys.js", "sidepanel.js", "stt.js", "manifest.webmanifest", "favicon.svg", "icon-192.png", "icon-512.png"];
const SHELL_PATHS = new Set(SHELL.map((p) => new URL(p, self.location).pathname));

// shellKey is the cache key for a request URL, or "" when it is not shell.
// index.html and the scope root are one entry; a query string is ignored.
function shellKey(url) {
  if (url.origin !== self.location.origin) return "";
  const p = url.pathname === SCOPE + "index.html" ? SCOPE : url.pathname;
  return SHELL_PATHS.has(p) ? new URL(p, self.location.origin).href : "";
}

// keepable: a plain 200 from this origin, never a redirect or an error page.
const keepable = (res) => res && res.status === 200 && res.type === "basic" && !res.redirected;

self.addEventListener("install", (ev) => ev.waitUntil((async () => {
  const cache = await caches.open(CACHE);
  // Each file on its own: one missing file must not leave the rest uncached.
  await Promise.all(SHELL.map(async (p) => {
    const key = new URL(p, self.location).href;
    try {
      const res = await fetch(new Request(key, { cache: "no-cache", credentials: "same-origin" }));
      if (keepable(res)) await cache.put(key, res);
    } catch {}
  }));
  await self.skipWaiting();
})()));

self.addEventListener("activate", (ev) => ev.waitUntil((async () => {
  const names = await caches.keys();
  await Promise.all(names.filter((n) => n.startsWith(PREFIX) && n !== CACHE).map((n) => caches.delete(n)));
  await self.clients.claim();
})()));

self.addEventListener("fetch", (ev) => {
  const req = ev.request;
  if (req.method !== "GET" || req.headers.has("range")) return;
  const key = shellKey(new URL(req.url));
  if (!key) return; // not shell: the browser fetches it, untouched
  if (req.mode === "navigate" && key !== new URL(SCOPE, self.location.origin).href) return;
  ev.respondWith((async () => {
    let res;
    try {
      res = await fetch(req);
    } catch (e) {
      const hit = await caches.match(key);
      if (hit) return hit;
      throw e;
    }
    if (keepable(res)) {
      const copy = res.clone();
      ev.waitUntil(caches.open(CACHE).then((c) => c.put(key, copy)).catch(() => {}));
      return res;
    }
    if (res.status === 502 || res.status === 503 || res.status === 504) {
      const hit = await caches.match(key);
      if (hit) return hit;
    }
    return res;
  })());
});

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
  ev.waitUntil(self.registration.showNotification(d.title || "Comebye", opts));
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
