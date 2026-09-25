// ==UserScript==
// @name         Watchnote: Watch with note
// @namespace    __BASE_URL__
// @version      1.0.0
// @description  Adds a "Watch with note" button to GitHub issues and pull requests.
// @match        https://github.com/*
// @connect      __HOST__
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM.xmlHttpRequest
// @grant        GM.getValue
// @grant        GM.setValue
// @downloadURL  __BASE_URL__/watchnote.user.js
// @updateURL    __BASE_URL__/watchnote.user.js
// ==/UserScript==

(() => {
  "use strict";
  const BASE = "__BASE_URL__";
  const ITEM_RE = /^\/[^/]+\/[^/]+\/(issues|pull)\/\d+/;

  // Tampermonkey/Violentmonkey expose GM_*, Greasemonkey 4 exposes GM.*.
  const gm = {
    get: (k) => (typeof GM_getValue === "function" ? Promise.resolve(GM_getValue(k, "")) : GM.getValue(k, "")),
    set: (k, v) => (typeof GM_setValue === "function" ? Promise.resolve(GM_setValue(k, v)) : GM.setValue(k, v)),
    xhr: (o) => (typeof GM_xmlhttpRequest === "function" ? GM_xmlhttpRequest(o) : GM.xmlHttpRequest(o)),
  };

  async function token(forceAsk) {
    let t = forceAsk ? "" : await gm.get("token");
    if (!t) {
      t = (prompt(`Paste your Watchnote token (Settings → GitHub button at ${BASE}/settings):`) || "").trim();
      if (t) await gm.set("token", t);
    }
    return t;
  }

  function api(method, path, body, retried) {
    return token(false).then(
      (t) =>
        new Promise((resolve, reject) => {
          if (!t) return reject(new Error("No Watchnote token"));
          gm.xhr({
            method,
            url: BASE + path,
            headers: { Authorization: "Bearer " + t, "Content-Type": "application/json" },
            data: body ? JSON.stringify(body) : undefined,
            onerror: () => reject(new Error("Couldn't reach Watchnote")),
            onload: async (r) => {
              let json = {};
              try { json = JSON.parse(r.responseText); } catch (_) {}
              if (r.status === 401 && !retried) {
                await token(true);
                return api(method, path, body, true).then(resolve, reject);
              }
              if (r.status >= 400) return reject(new Error(json.error || "Watchnote error " + r.status));
              resolve(json);
            },
          });
        }),
    );
  }

  function el(tag, props, ...children) {
    const e = document.createElement(tag);
    Object.assign(e, props || {});
    if (props && props.style) e.style.cssText = props.style;
    for (const c of children) e.append(c);
    return e;
  }

  const BTN = "display:inline-flex;align-items:center;gap:6px;padding:5px 12px;border-radius:6px;font:600 13px -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;cursor:pointer;border:1px solid #2f6fed;";
  let root = null;
  let currentPath = "";

  function itemURL() {
    const m = location.pathname.match(ITEM_RE);
    return m ? location.origin + m[0] : null;
  }

  function mount() {
    const url = itemURL();
    if (!url) return unmount();
    if (root && document.body.contains(root) && currentPath === url) return;
    unmount();
    currentPath = url;
    root = el("div", { id: "watchnote-root", style: "position:fixed;right:20px;bottom:20px;z-index:2147483000;display:flex;flex-direction:column;align-items:flex-end;gap:8px" });
    const button = el("button", { type: "button", textContent: "Watchnote…", style: BTN + "background:#fff;color:#2f6fed;box-shadow:0 4px 14px rgba(0,0,0,.15)" });
    root.append(button);
    document.body.append(root);

    api("GET", "/api/watch?url=" + encodeURIComponent(url))
      .then((w) => render(button, url, w))
      .catch(() => render(button, url, { watching: false }));
  }

  function unmount() {
    if (root) root.remove();
    root = null;
    currentPath = "";
  }

  function render(button, url, w) {
    if (w.watching) {
      button.textContent = "✓ Watching";
      button.title = w.note || "";
      button.style.cssText = BTN + "background:#e8efff;color:#2f6fed;box-shadow:0 4px 14px rgba(0,0,0,.15)";
      button.onclick = () => window.open(w.url, "_blank");
      return;
    }
    button.textContent = "🔖 Watch with note";
    button.style.cssText = BTN + "background:#2f6fed;color:#fff;box-shadow:0 4px 14px rgba(47,111,237,.4)";
    button.onclick = () => openForm(button, url);
  }

  function openForm(button, url) {
    if (root.querySelector("form")) return;
    const note = el("textarea", { rows: 4, placeholder: "Why are you watching this?", style: "width:100%;box-sizing:border-box;border:1px solid #2f6fed;border-radius:8px;padding:8px;font:14px -apple-system,sans-serif;resize:vertical" });
    const preset = (value, label, checked) =>
      el("label", { style: "display:inline-flex;gap:4px;align-items:center;margin-right:12px;font-size:13px" },
        el("input", { type: "radio", name: "wn-preset", value, checked }), label);
    const error = el("div", { style: "color:#cf222e;font-size:13px;display:none" });
    const save = el("button", { type: "submit", textContent: "Save", style: BTN + "background:#2f6fed;color:#fff" });
    const cancel = el("button", { type: "button", textContent: "Cancel", style: BTN + "background:#fff;color:#2f6fed;border-color:transparent" });
    const form = el("form", { style: "width:320px;background:#fff;color:#1d1d1f;border:1px solid #d0d7de;border-radius:12px;padding:14px;box-shadow:0 8px 24px rgba(0,0,0,.18);display:flex;flex-direction:column;gap:10px;font:14px -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif" },
      el("b", { textContent: "Why are you watching this?" }),
      note,
      el("div", {}, preset("everything", "Everything", true), preset("status", "Status only", false)),
      error,
      el("div", { style: "display:flex;gap:8px" }, save, cancel));
    cancel.onclick = () => form.remove();
    form.onsubmit = (e) => {
      e.preventDefault();
      if (!note.value.trim()) { note.focus(); return; }
      save.disabled = true;
      save.textContent = "Saving…";
      api("POST", "/api/watch", { url, note: note.value, preset: form.querySelector('input[name="wn-preset"]:checked').value })
        .then((w) => { form.remove(); render(button, url, w); })
        .catch((err) => {
          error.textContent = err.message;
          error.style.display = "block";
          save.disabled = false;
          save.textContent = "Save";
        });
    };
    root.insertBefore(form, button);
    note.focus();
  }

  // GitHub navigates without full page loads, so re-check the URL periodically.
  mount();
  setInterval(mount, 1000);
})();
