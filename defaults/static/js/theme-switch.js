(function () {
  var STORAGE_KEY = "go-grip-theme";
  var MODES = ["light", "dark"];

  function browserPrefers() {
    if (window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) {
      return "dark";
    }
    return "light";
  }

  function getPreference() {
    try {
      var stored = localStorage.getItem(STORAGE_KEY);
      if (stored && MODES.indexOf(stored) !== -1) return stored;
    } catch (e) {}
    return browserPrefers();
  }

  function applyTheme(mode) {
    var lightCSS = document.getElementById("theme-light");
    var darkCSS = document.getElementById("theme-dark");
    var lightHL = document.getElementById("highlight-light");
    var darkHL = document.getElementById("highlight-dark");

    if (mode === "dark") {
      if (lightCSS) lightCSS.media = "not all";
      if (darkCSS) darkCSS.media = "all";
      if (lightHL) lightHL.media = "not all";
      if (darkHL) darkHL.media = "all";
      document.body.setAttribute("data-theme", "dark");
    } else {
      if (lightCSS) lightCSS.media = "all";
      if (darkCSS) darkCSS.media = "not all";
      if (lightHL) lightHL.media = "all";
      if (darkHL) darkHL.media = "not all";
      document.body.setAttribute("data-theme", "light");
    }

    updateIcon(mode);

    try {
      localStorage.setItem(STORAGE_KEY, mode);
    } catch (e) {}

    document.body.dispatchEvent(
      new CustomEvent("themechange", { detail: { mode: mode } })
    );
  }

  // SVG icons so all toggle buttons share the same visual size (text glyphs don't).
  var SUN_SVG =
    '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round">' +
    '<circle cx="8" cy="8" r="3.25" />' +
    '<path d="M8 1.5V3M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1.06 1.06M11.54 11.54l1.06 1.06M12.6 3.4l-1.06 1.06M4.46 11.54 3.4 12.6" />' +
    "</svg>";
  var MOON_SVG =
    '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor">' +
    '<path d="M6.5 2.2a6 6 0 1 0 7.3 7.3A5 5 0 0 1 6.5 2.2z" />' +
    "</svg>";

  function updateIcon(mode) {
    var btn = document.getElementById("theme-toggle");
    if (!btn) return;
    var icon = btn.querySelector(".theme-toggle-icon");
    if (!icon) return;

    if (mode === "dark") {
      icon.innerHTML = MOON_SVG;
      btn.title = "Theme: Dark";
    } else {
      icon.innerHTML = SUN_SVG;
      btn.title = "Theme: Light";
    }
  }

  function toggle() {
    var current = getPreference();
    applyTheme(current === "light" ? "dark" : "light");
  }

  document.addEventListener("DOMContentLoaded", function () {
    applyTheme(getPreference());

    var btn = document.getElementById("theme-toggle");
    if (btn) {
      btn.addEventListener("click", toggle);
    }
  });
})();
