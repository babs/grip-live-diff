(function () {
  var STORAGE_KEY = "grip-live-diff-minimap";
  var DEBOUNCE_MS = 150;
  var MIN_MARK_PX = 3;

  var panel, map, marks, viewport, hit, article, scale, articleTop, timer, observers;

  function wanted() {
    try {
      return localStorage.getItem(STORAGE_KEY) !== "off";
    } catch (e) {
      return true;
    }
  }

  function remember(on) {
    try {
      localStorage.setItem(STORAGE_KEY, on ? "on" : "off");
    } catch (e) {}
  }

  function docHeight() {
    return document.documentElement.scrollHeight;
  }

  function maxScroll() {
    return Math.max(0, docHeight() - window.innerHeight);
  }

  // The map is taller than the window on long documents; it slides with the scroll so
  // the current position is always on screen (VS Code behaviour).
  function mapOffset() {
    var overflow = article.offsetHeight * scale - panel.clientHeight;
    if (overflow <= 0 || maxScroll() === 0) return 0;
    return (window.scrollY / maxScroll()) * overflow;
  }

  function sync() {
    if (!panel) return;
    var offset = mapOffset();
    map.style.transform = "translateY(" + -offset + "px)";
    viewport.style.top = (window.scrollY - articleTop) * scale - offset + "px";
    viewport.style.height = window.innerHeight * scale + "px";
  }

  function scrollToPanelY(y) {
    var docY = (y + mapOffset()) / scale + articleTop;
    window.scrollTo(0, docY - window.innerHeight / 2);
  }

  function attachPointer() {
    var grab = null;
    hit.addEventListener("pointerdown", function (e) {
      var top = viewport.offsetTop;
      var inViewport = e.offsetY >= top && e.offsetY <= top + viewport.offsetHeight;
      if (!inViewport) scrollToPanelY(e.offsetY);
      grab = e.offsetY - viewport.offsetTop;
      hit.setPointerCapture(e.pointerId);
    });
    hit.addEventListener("pointermove", function (e) {
      if (grab === null) return;
      var top = e.offsetY - grab;
      var overflow = article.offsetHeight * scale - panel.clientHeight;
      if (overflow > 0) {
        // Slider mode: the viewport box moves over the panel height, not the map height.
        var track = panel.clientHeight - viewport.offsetHeight;
        window.scrollTo(0, (top / track) * maxScroll());
      } else {
        window.scrollTo(0, top / scale + articleTop);
      }
    });
    var release = function () {
      grab = null;
    };
    hit.addEventListener("pointerup", release);
    hit.addEventListener("pointercancel", release);
  }

  function drawMarks() {
    marks.innerHTML = "";
    var origin = article.getBoundingClientRect().top;
    var nodes = article.querySelectorAll("ins.gg-ins, del.gg-del");
    for (var i = 0; i < nodes.length; i++) {
      var rect = nodes[i].getBoundingClientRect();
      var mark = document.createElement("div");
      mark.className = "minimap-mark " + (nodes[i].tagName === "INS" ? "minimap-ins" : "minimap-del");
      mark.style.top = (rect.top - origin) * scale + "px";
      mark.style.height = Math.max(rect.height * scale, MIN_MARK_PX) + "px";
      marks.appendChild(mark);
    }
  }

  // ponytail: full DOM clone per rebuild; paint the map on a canvas if long documents drag.
  function build() {
    // Zero under the narrow-window media query: nothing to scale against.
    if (!panel || !panel.clientWidth) return;
    var width = article.getBoundingClientRect().width;
    scale = panel.clientWidth / width;
    articleTop = article.getBoundingClientRect().top + window.scrollY;

    var clone = article.cloneNode(true);
    clone.className = "minimap-clone";
    clone.style.width = width + "px";
    clone.style.transform = "scale(" + scale + ")";
    // Duplicate ids would hijack anchor links and getElementById lookups; SVG ids stay,
    // mermaid scopes its inline stylesheet on them.
    var ids = clone.querySelectorAll("[id]");
    for (var i = 0; i < ids.length; i++) {
      if (!ids[i].closest("svg")) ids[i].removeAttribute("id");
    }
    map.replaceChildren(clone, marks);
    drawMarks();
    sync();
  }

  function scheduleBuild() {
    clearTimeout(timer);
    timer = setTimeout(build, DEBOUNCE_MS);
  }

  function mount() {
    if (panel) return;
    article = document.querySelector(".container > div");
    if (!article) return;

    panel = document.createElement("div");
    panel.className = "minimap";
    panel.inert = true;
    map = document.createElement("div");
    map.className = "minimap-map";
    marks = document.createElement("div");
    marks.className = "minimap-marks";
    viewport = document.createElement("div");
    viewport.className = "minimap-viewport";
    panel.append(map, viewport);
    // Separate hit layer: an inert subtree receives no pointer events at all.
    hit = document.createElement("div");
    hit.className = "minimap-hit";
    document.body.append(panel, hit);
    document.documentElement.setAttribute("data-minimap-shown", "");

    attachPointer();
    window.addEventListener("scroll", sync, { passive: true });
    window.addEventListener("resize", scheduleBuild);
    // Width-mode switches change the size without touching the DOM; mermaid re-rendering
    // on a theme switch touches the DOM without changing the size. Both are needed.
    observers = [new ResizeObserver(scheduleBuild), new MutationObserver(scheduleBuild)];
    observers[0].observe(article);
    observers[1].observe(article, { childList: true, subtree: true });
    build();
  }

  function unmount() {
    if (!panel) return;
    clearTimeout(timer);
    observers.forEach(function (o) {
      o.disconnect();
    });
    window.removeEventListener("scroll", sync);
    window.removeEventListener("resize", scheduleBuild);
    panel.remove();
    hit.remove();
    document.documentElement.removeAttribute("data-minimap-shown");
    panel = hit = null;
  }

  function reflect(button) {
    button.setAttribute("aria-pressed", String(wanted()));
  }

  // Not gated on MathJax.startup.promise: MathJax 4 leaves it pending after the initial
  // typeset. A late typeset or font load moves the article height and rebuilds the map.
  window.addEventListener("load", function () {
    if (wanted()) mount();
    var button = document.getElementById("minimap-toggle");
    if (!button) return;
    reflect(button);
    button.addEventListener("click", function () {
      var on = !wanted();
      remember(on);
      reflect(button);
      if (on) mount();
      else unmount();
    });
  });
})();
