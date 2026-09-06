(function () {
  var STORAGE_KEY = "grip-live-diff-ref";
  var REFS = ["open", "last", "head"];

  function stored() {
    try {
      var ref = localStorage.getItem(STORAGE_KEY);
      if (REFS.indexOf(ref) !== -1) return ref;
    } catch (e) {}
    return "open";
  }

  function typing(target) {
    return target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable);
  }

  document.addEventListener("DOMContentLoaded", function () {
    var toggle = document.querySelector(".diff-toggle");
    var status = document.querySelector(".diff-status");
    var mode = status ? status.getAttribute("data-diff-mode") : "";

    // The toggle is on/off: off, it comes back to the reference last used, not always "open".
    if (mode) {
      try {
        localStorage.setItem(STORAGE_KEY, mode);
      } catch (e) {}
    } else if (toggle) {
      toggle.href = location.pathname + "?diff=" + stored();
    }

    document.addEventListener("keydown", function (e) {
      if (e.ctrlKey || e.metaKey || e.altKey || typing(e.target)) return;
      if (e.key === "d" && toggle) {
        location.href = toggle.href;
        return;
      }
      // Works with the diff off too; an unavailable HEAD is the server's to fall back from.
      var i = "123".indexOf(e.key);
      if (i === -1 || REFS[i] === mode) return;
      if (REFS[i] === "head" && document.querySelector(".diff-ref-disabled")) return;
      location.href = location.pathname + "?diff=" + REFS[i];
    });
  });
})();
