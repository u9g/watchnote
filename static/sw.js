// Minimal service worker: makes Watchnote installable so it can appear in the
// mobile share sheet. Everything goes to the network; nothing is cached.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
self.addEventListener("fetch", () => {});
