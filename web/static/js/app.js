import * as pdfjs from "/static/vendor/pdfjs/pdf.min.mjs";

pdfjs.GlobalWorkerOptions.workerSrc = "/static/vendor/pdfjs/pdf.worker.min.mjs";

// The server retargets error responses to #result; swap them instead of
// dropping them, which is htmx's default for 4xx/5xx.
htmx.config.responseHandling = [
  { code: "204", swap: false },
  { code: "[23]..", swap: true },
  { code: "[45]..", swap: true, error: true },
];

const THUMB_PAGES = 3;
const PREVIEW_MAX_PAGES = 60;

// ---- thumbnails & preview ----

async function renderPage(pdf, n, height) {
  const page = await pdf.getPage(n);
  const base = page.getViewport({ scale: 1 });
  const scale = (height * (window.devicePixelRatio || 1)) / base.height;
  const vp = page.getViewport({ scale });
  const canvas = document.createElement("canvas");
  canvas.width = Math.floor(vp.width);
  canvas.height = Math.floor(vp.height);
  await page.render({ canvasContext: canvas.getContext("2d"), viewport: vp }).promise;
  return canvas;
}

async function renderThumbs(button) {
  if (button.dataset.rendered) return;
  button.dataset.rendered = "1";
  try {
    const pdf = await pdfjs.getDocument({ url: button.dataset.preview }).promise;
    const n = Math.min(pdf.numPages, THUMB_PAGES);
    for (let i = 1; i <= n; i++) button.append(await renderPage(pdf, i, 62));
    if (pdf.numPages > n) {
      const more = document.createElement("span");
      more.className = "more";
      more.textContent = `+${pdf.numPages - n}`;
      button.append(more);
    }
    pdf.destroy();
  } catch (err) {
    button.textContent = "No preview";
    console.warn("thumbnail", err);
  }
}

let previewTask = 0;

async function openPreview(button) {
  const dialog = document.getElementById("preview");
  const pages = document.getElementById("preview-pages");
  const task = ++previewTask;
  document.getElementById("preview-title").textContent = button.dataset.title || "Preview";
  pages.replaceChildren();
  dialog.showModal();
  try {
    const pdf = await pdfjs.getDocument({ url: button.dataset.preview }).promise;
    const height = Math.min(1100, Math.max(400, pages.clientWidth * 1.3));
    const n = Math.min(pdf.numPages, PREVIEW_MAX_PAGES);
    for (let i = 1; i <= n; i++) {
      if (task !== previewTask || !dialog.open) break;
      pages.append(await renderPage(pdf, i, height));
    }
    if (pdf.numPages > n && task === previewTask) {
      const note = document.createElement("p");
      note.textContent = `Showing the first ${n} of ${pdf.numPages} pages.`;
      pages.append(note);
    }
    pdf.destroy();
  } catch (err) {
    const note = document.createElement("p");
    note.textContent = "Preview unavailable.";
    pages.append(note);
    console.warn("preview", err);
  }
}

// ---- options & estimate ----

function updateOptions() {
  const form = document.getElementById("print-form");
  if (!form) return;
  const cards = [...form.querySelectorAll(".card[data-id]")];
  const range = form.elements.range;
  const single = cards.length === 1;
  range.disabled = !single;
  range.placeholder = single ? "All" : "All (one file only)";
  if (!single) range.value = "";

  const copies = Math.max(1, parseInt(form.elements.copies.value, 10) || 1);
  const duplex = form.elements.duplex.checked && !form.elements.duplex.disabled;
  let pages = 0;
  for (const card of cards) {
    const total = parseInt(card.dataset.pages, 10) || 0;
    pages += single ? countRange(range.value, total) : total;
  }
  const sheets = (duplex ? Math.ceil(pages / 2) : pages) * copies;
  const estimate = document.getElementById("estimate");
  estimate.textContent = cards.length
    ? `${cards.length} file${cards.length === 1 ? "" : "s"} · ≈${sheets} sheet${sheets === 1 ? "" : "s"}`
    : "";
}

// Mirrors the server's parser loosely; the server has the final say.
function countRange(spec, total) {
  if (!spec.trim()) return total;
  const pages = new Set();
  for (const part of spec.split(",")) {
    const [a, b] = part.split("-").map((s) => s.trim());
    const from = parseInt(a, 10);
    if (!from) continue;
    const to = b === undefined ? from : b === "" ? total : parseInt(b, 10);
    for (let p = from; p <= Math.min(to, total); p++) pages.add(p);
  }
  return pages.size;
}

// ---- drag & drop ----

function setupDropzone() {
  const zone = document.getElementById("dropzone");
  const input = document.getElementById("file-input");
  if (!zone || !input) return;
  let depth = 0;
  document.addEventListener("dragenter", (e) => {
    if (!e.dataTransfer?.types.includes("Files")) return;
    depth++;
    zone.classList.add("is-over");
  });
  document.addEventListener("dragleave", () => {
    if (--depth <= 0) { depth = 0; zone.classList.remove("is-over"); }
  });
  document.addEventListener("dragover", (e) => {
    if (e.dataTransfer?.types.includes("Files")) e.preventDefault();
  });
  document.addEventListener("drop", (e) => {
    if (!e.dataTransfer?.files.length) return;
    e.preventDefault();
    depth = 0;
    zone.classList.remove("is-over");
    input.files = e.dataTransfer.files;
    input.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

// ---- share target ----

// The service worker stashes shared files in Cache Storage and redirects here
// with ?shared=1; upload them through the normal authenticated path.
async function consumeShared() {
  const url = new URL(location.href);
  if (!url.searchParams.has("shared") || !("caches" in window)) return;
  history.replaceState(null, "", "/");
  const cache = await caches.open("netpr-share");
  const keys = await cache.keys();
  if (!keys.length) return;
  const form = new FormData();
  for (const req of keys) {
    const res = await cache.match(req);
    const name = decodeURIComponent(res.headers.get("X-Filename") || "shared");
    form.append("files", await res.blob(), name);
    await cache.delete(req);
  }
  const target = document.getElementById("staged");
  const progress = document.getElementById("upload-progress");
  progress?.classList.add("htmx-request");
  try {
    const token = JSON.parse(document.body.getAttribute("hx-headers") || "{}")["X-CSRF-Token"];
    const res = await fetch("/upload", {
      method: "POST",
      body: form,
      headers: { "HX-Request": "true", "X-CSRF-Token": token || "" },
    });
    const html = await res.text();
    if (res.headers.get("HX-Retarget") === "#result") {
      document.getElementById("result").innerHTML = html;
    } else {
      target.insertAdjacentHTML("beforeend", html);
      htmx.process(target);
      init(target);
    }
  } finally {
    progress?.classList.remove("htmx-request");
  }
}

// ---- toasts ----

// The server sends a job-finished event (HX-Trigger) when a job it was
// polling reaches a final state.
function toast({ title, name, ok }) {
  const box = document.getElementById("toasts");
  if (!box) return;
  const el = document.createElement("div");
  el.className = `toast${ok ? "" : " toast--error"}`;
  el.setAttribute("role", ok ? "status" : "alert");
  const t = document.createElement("strong");
  t.textContent = title;
  const n = document.createElement("span");
  n.textContent = name;
  el.append(t, n);
  el.addEventListener("click", () => el.remove());
  box.append(el);
  setTimeout(() => {
    el.classList.add("is-leaving");
    setTimeout(() => el.remove(), 300);
  }, ok ? 6000 : 12000);
}

document.body.addEventListener("job-finished", (e) => toast(e.detail));

// ---- wiring ----

function init(root) {
  root.querySelectorAll?.(".thumbs[data-preview]").forEach(renderThumbs);
  updateOptions();
}

document.addEventListener("click", (e) => {
  const t = e.target.closest("[data-preview], [data-dismiss], [data-clear-result], [data-close-preview]");
  if (!t) return;
  if (t.matches("[data-preview]")) openPreview(t);
  else if (t.matches("[data-dismiss]")) t.closest("li")?.remove();
  else if (t.matches("[data-clear-result]")) document.getElementById("result").replaceChildren();
  else if (t.matches("[data-close-preview]")) document.getElementById("preview").close();
});

document.getElementById("preview")?.addEventListener("click", (e) => {
  if (e.target.id === "preview") e.target.close(); // backdrop click
});

document.addEventListener("input", (e) => {
  if (e.target.closest("#print-form")) updateOptions();
});

document.body.addEventListener("htmx:xhr:progress", (e) => {
  if (!e.target.closest?.("#upload-form") || !e.detail.lengthComputable) return;
  const pct = Math.round((e.detail.loaded / e.detail.total) * 100);
  document.getElementById("upload-progress").style.setProperty("--progress", `${pct}%`);
});

document.body.addEventListener("htmx:afterRequest", (e) => {
  if (e.target.id === "upload-form") {
    e.target.reset(); // allow re-selecting the same file
    document.getElementById("upload-progress").style.removeProperty("--progress");
  }
});

// Runs for the initial page and every piece of content htmx swaps in.
htmx.onLoad((el) => init(el));
// Removing a card doesn't load anything new, so recount explicitly.
document.body.addEventListener("htmx:afterSwap", updateOptions);

setupDropzone();
consumeShared();

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js").catch((err) => console.warn("service worker", err));
}
