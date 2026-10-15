// Service worker: makes netpr installable and receives Android share-sheet files.
//
// A shared file arrives as a POST to /share. Handling it here (rather than on
// the server) means an expired Authelia session can't swallow the upload: the
// files wait in Cache Storage while the page loads, logs in if needed, and
// then uploads them itself.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== "POST" || url.pathname !== "/share") return;
  event.respondWith(stashShare(event.request));
});

async function stashShare(request) {
  const form = await request.formData();
  const cache = await caches.open("netpr-share");
  for (const key of await cache.keys()) await cache.delete(key);
  let i = 0;
  for (const file of form.getAll("files")) {
    if (typeof file === "string") continue;
    await cache.put(
      `/__share/${i++}`,
      new Response(file, {
        headers: {
          "Content-Type": file.type || "application/octet-stream",
          "X-Filename": encodeURIComponent(file.name || "shared"),
        },
      }),
    );
  }
  return Response.redirect("/?shared=1", 303);
}
