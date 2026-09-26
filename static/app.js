// Progressive enhancement only: every form works without this file.
(() => {
  const csrf = document.body.dataset.csrf;

  // Record the browser's timezone once, so quiet hours and digests use local time.
  if (document.body.dataset.tz === "" && csrf) {
    const body = new FormData();
    body.set("csrf", csrf);
    body.set("tz", Intl.DateTimeFormat().resolvedOptions().timeZone || "");
    fetch("/settings/tz", { method: "POST", body });
  }

  document.querySelectorAll("[data-detect-tz]").forEach((btn) => {
    btn.addEventListener("click", () => {
      btn.closest("form").querySelector('[name="tz"]').value = Intl.DateTimeFormat().resolvedOptions().timeZone;
    });
  });

  // "Everything" / "Status only" chips set the category switches; they're highlighted when they match.
  document.querySelectorAll("[data-presets]").forEach((group) => {
    const form = group.closest("form");
    const boxes = [...form.querySelectorAll('input[name="cat"]')];
    const chips = [...group.querySelectorAll("[data-preset]")];
    const sync = () => {
      const on = boxes.filter((b) => b.checked).map((b) => b.value).join(",");
      chips.forEach((c) => c.classList.toggle("on", c.dataset.preset === on));
    };
    chips.forEach((c) =>
      c.addEventListener("click", () => {
        const want = c.dataset.preset.split(",");
        boxes.forEach((b) => (b.checked = want.includes(b.value)));
        sync();
      }),
    );
    boxes.forEach((b) => b.addEventListener("change", sync));
    sync();
  });

  document.querySelectorAll("form[data-confirm]").forEach((f) =>
    f.addEventListener("submit", (e) => { if (!confirm(f.dataset.confirm)) e.preventDefault(); }),
  );
  // Filter boxes over long lists, like a token's repos in Settings.
  document.querySelectorAll("[data-filter]").forEach((input) => {
    const items = [...document.querySelector(input.dataset.filter).children];
    input.hidden = false;
    input.addEventListener("input", () => {
      const q = input.value.trim().toLowerCase();
      items.forEach((li) => (li.hidden = !li.dataset.name.includes(q)));
    });
  });
  document.querySelectorAll("[data-select]").forEach((i) => i.addEventListener("focus", () => i.select()));

  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");
})();
