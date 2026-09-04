(function () {
  var STORAGE_KEY = "grip-live-diff-scroll";

  // "Mark as read" is a POST + redirect: a fresh navigation, so the browser does not
  // restore the scroll position the way it does on a reload.
  function save() {
    try {
      sessionStorage.setItem(STORAGE_KEY, location.pathname + " " + window.scrollY);
    } catch (e) {}
  }

  function restore() {
    var stored;
    try {
      stored = sessionStorage.getItem(STORAGE_KEY);
      sessionStorage.removeItem(STORAGE_KEY);
    } catch (e) {}
    if (!stored) return;
    var sep = stored.lastIndexOf(" ");
    if (stored.slice(0, sep) !== location.pathname) return;
    window.scrollTo(0, Number(stored.slice(sep + 1)));
  }

  document.addEventListener("DOMContentLoaded", function () {
    restore();
    var form = document.querySelector("form.mark-read-form");
    if (form) form.addEventListener("submit", save);
  });
})();
