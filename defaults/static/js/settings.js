(function () {
  var root = document.documentElement;
  // Rendered by the server from the settings file: the first paint is already right.
  var current = {
    width: root.getAttribute("data-width") || "normal",
    theme: root.getAttribute("data-theme-setting") || "system",
    hold_reload: root.getAttribute("data-hold-reload") === "true",
  };
  var prefersDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)");

  function resolvedTheme() {
    if (current.theme !== "system") return current.theme;
    return prefersDark && prefersDark.matches ? "dark" : "light";
  }

  // Same media values as the server renders, so a change here matches the next load.
  function applyTheme() {
    var mode = resolvedTheme();
    ["light", "dark"].forEach(function (m) {
      var media =
        current.theme === "system" ? "(prefers-color-scheme: " + m + ")" : m === mode ? "all" : "not all";
      ["theme-", "highlight-"].forEach(function (prefix) {
        var el = document.getElementById(prefix + m);
        if (el) el.media = media;
      });
    });
    document.body.setAttribute("data-theme", mode);
    document.body.dispatchEvent(new CustomEvent("themechange", { detail: { mode: mode } }));
  }

  function save() {
    fetch("/__settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(current),
    })
      .then(function (r) {
        if (!r.ok) console.warn("settings not saved: HTTP " + r.status);
      })
      .catch(function (e) {
        console.warn("settings not saved:", e);
      });
  }

  document.addEventListener("DOMContentLoaded", function () {
    applyTheme();
    if (prefersDark) {
      prefersDark.addEventListener("change", function () {
        if (current.theme === "system") applyTheme();
      });
    }

    var menu = document.getElementById("settings-menu");
    if (!menu) return;
    // annotate.js cancels every Escape on the document, which also stops the popover's own close.
    document.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape" && menu.matches(":popover-open")) menu.hidePopover();
    }, true);
    menu.querySelectorAll("input[type=radio]").forEach(function (input) {
      var key = input.name.replace("settings-", "");
      input.checked = current[key] === input.value;
      input.addEventListener("change", function () {
        current[key] = input.value;
        if (key === "width") root.setAttribute("data-width", input.value);
        else applyTheme();
        save();
      });
    });
  });
})();
