(function () {
  var STORAGE_KEY = "go-grip-width";
  var MODES = ["normal", "wide", "full"];

  function getPreference() {
    try {
      var stored = localStorage.getItem(STORAGE_KEY);
      if (stored && MODES.indexOf(stored) !== -1) return stored;
    } catch (e) {}
    return "normal";
  }

  function apply(mode) {
    document.documentElement.setAttribute("data-width", mode);
    var btn = document.getElementById("width-toggle");
    if (btn) {
      btn.title = "Width: " + mode.charAt(0).toUpperCase() + mode.slice(1);
    }
    try {
      localStorage.setItem(STORAGE_KEY, mode);
    } catch (e) {}
  }

  function toggle() {
    var current = getPreference();
    apply(MODES[(MODES.indexOf(current) + 1) % MODES.length]);
  }

  // On <html> before <body> exists, so the first paint is already at the stored width.
  document.documentElement.setAttribute("data-width", getPreference());

  document.addEventListener("DOMContentLoaded", function () {
    apply(getPreference());

    var btn = document.getElementById("width-toggle");
    if (btn) {
      btn.addEventListener("click", toggle);
    }
  });
})();
