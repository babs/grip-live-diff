(function () {
  var STORAGE_KEY = "grip-live-diff-annotate";
  var CONTEXT = 32;
  // pathname is already percent-encoded: decode first or "my doc.md" reaches the server
  // as "my%20doc.md".
  var ENDPOINT = "/__annotations?path=" + encodeURIComponent(decodeURIComponent(location.pathname));
  // Text that is not the document: diff deletions, diagrams and formulas (their rendered
  // text does not exist in the source), the code-block copy buttons.
  var SKIP = "del.gg-del, .mermaid, .math, mjx-container, script, style, button, [hidden]";

  var article, model, entries = [], doc = { annotations: [] }, pending = null, current = null, hovered = null;
  var bubble, tip, box, textarea, meta, hitFrame;

  var supported = "highlights" in CSS && typeof Highlight === "function";

  function wanted() {
    try {
      return localStorage.getItem(STORAGE_KEY) === "on";
    } catch (e) {
      return false;
    }
  }

  function remember(on) {
    try {
      localStorage.setItem(STORAGE_KEY, on ? "on" : "off");
    } catch (e) {}
  }

  // fold collapses whitespace runs to one space; map[i] is the raw index of folded char i.
  // Anchors are compared folded, so a re-wrapped paragraph still matches its quote.
  function fold(raw) {
    var out = "", map = [], space = false;
    for (var i = 0; i < raw.length; i++) {
      var c = raw[i];
      if (/\s/.test(c)) {
        if (!space && out.length) {
          out += " ";
          map.push(i);
        }
        space = true;
        continue;
      }
      space = false;
      out += c;
      map.push(i);
    }
    return { text: out, map: map };
  }

  // The text model: every text node of the article outside SKIP, with its raw offset.
  function buildModel() {
    var segs = [], raw = "", walker = document.createTreeWalker(article, NodeFilter.SHOW_TEXT);
    for (var node = walker.nextNode(); node; node = walker.nextNode()) {
      if (node.parentElement.closest(SKIP)) continue;
      segs.push({ node: node, start: raw.length });
      raw += node.data;
    }
    var folded = fold(raw);
    return { segs: segs, raw: raw, text: folded.text, map: folded.map };
  }

  // Raw offset of a DOM point, counting only modelled text.
  function offsetOf(container, offset) {
    var probe = document.createRange();
    probe.setStart(container, offset);
    probe.collapse(true);
    var total = 0;
    for (var i = 0; i < model.segs.length; i++) {
      var seg = model.segs[i];
      if (probe.comparePoint(seg.node, seg.node.length) <= 0) {
        total += seg.node.length;
        continue;
      }
      if (probe.comparePoint(seg.node, 0) >= 0) break;
      total += seg.node === container ? offset : 0;
      break;
    }
    return total;
  }

  // Folded [start, end) to live Ranges over the article, one per text node, so a skipped
  // node in between (a diff deletion) is left out of the mark.
  function rangesOf(start, end) {
    var rawStart = model.map[start], rawEnd = model.map[end - 1] + 1, out = [];
    for (var i = 0; i < model.segs.length; i++) {
      var seg = model.segs[i], segEnd = seg.start + seg.node.length;
      if (segEnd <= rawStart || seg.start >= rawEnd) continue;
      var range = document.createRange();
      range.setStart(seg.node, Math.max(rawStart, seg.start) - seg.start);
      range.setEnd(seg.node, Math.min(rawEnd, segEnd) - seg.start);
      if (!range.collapsed) out.push(range);
    }
    return out;
  }

  function rectOf(entry) {
    var first = entry.ranges[0].getBoundingClientRect();
    var last = entry.ranges[entry.ranges.length - 1].getBoundingClientRect();
    return { left: first.left, top: first.top, bottom: last.bottom };
  }

  function foldedOffset(raw) {
    var lo = 0, hi = model.map.length;
    while (lo < hi) {
      var mid = (lo + hi) >> 1;
      if (model.map[mid] < raw) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  // How many characters of prefix end at idx, plus how many of suffix start at idx + length.
  function context(text, idx, length, prefix, suffix) {
    var n = 0, m = 0;
    while (n < prefix.length && idx - 1 - n >= 0 && text[idx - 1 - n] === prefix[prefix.length - 1 - n]) n++;
    while (m < suffix.length && text[idx + length + m] === suffix[m]) m++;
    return n + m;
  }

  // Anchor a quote in the folded text: exact hits first (prefix/suffix pick among several),
  // then the Hypothesis fallback between prefix and suffix, then orphan.
  function anchor(a) {
    var text = model.text, exact = a.exact;
    var best = null, score = -1, idx = text.indexOf(exact);
    while (idx >= 0) {
      var s = context(text, idx, exact.length, a.prefix || "", a.suffix || "");
      if (s > score) {
        score = s;
        best = idx;
      }
      idx = text.indexOf(exact, idx + 1);
    }
    if (best !== null) return { start: best, end: best + exact.length, fuzzy: false };
    if (a.prefix && a.suffix) {
      var p = text.indexOf(a.prefix);
      if (p >= 0) {
        var from = p + a.prefix.length;
        var q = text.indexOf(a.suffix, from);
        if (q > from && q - from <= 3 * exact.length) return { start: from, end: q, fuzzy: true };
      }
    }
    return null;
  }

  function draw() {
    entries = [];
    var groups = { annot: [], "annot-fuzzy": [], "annot-done": [], "annot-active": [] };
    doc.annotations.forEach(function (a) {
      var entry = { a: a, ranges: [], start: null, fuzzy: false };
      if (a.exact) {
        var at = anchor(a);
        if (at) {
          entry.ranges = rangesOf(at.start, at.end);
          entry.start = at.start;
          entry.fuzzy = at.fuzzy;
        }
      }
      entries.push(entry);
      if (!entry.ranges.length) return;
      var group = a.status === "done" ? "annot-done" : entry.fuzzy ? "annot-fuzzy" : "annot";
      groups[group].push.apply(groups[group], entry.ranges);
      if ((current && current.a.id === a.id) || (hovered && hovered.a.id === a.id)) {
        groups["annot-active"].push.apply(groups["annot-active"], entry.ranges);
      }
    });
    Object.keys(groups).forEach(function (name) {
      CSS.highlights.set(name, new Highlight(...groups[name]));
    });
    // Entries are rebuilt on every draw: the open one is re-found by id or it would fall
    // out of the navigation order.
    if (current) current = byId(current.a.id);
    var nav = document.querySelector(".annotations-nav");
    if (nav) nav.hidden = !(doc.annotations.length || wanted());
    var counter = document.getElementById("annotation-counter");
    if (counter) {
      var at = current ? ordered().indexOf(current) + 1 : 0;
      counter.textContent = entries.length ? (at ? at + "/" : "") + entries.length : "";
    }
    drawList();
  }

  // The list at the foot of the document: the same entries in navigation order. Outside
  // the article, so its text never becomes part of the model an annotation anchors in.
  function drawList() {
    var list = document.querySelector(".annotations-list");
    if (!list) {
      list = document.createElement("section");
      list.className = "annotations-list";
      article.parentElement.append(list);
    }
    list.hidden = !entries.length;
    list.replaceChildren();
    if (!entries.length) return;
    var heading = document.createElement("h2");
    heading.textContent = "Annotations";
    list.append(heading);
    ordered().forEach(function (entry, i) {
      var a = entry.a, item = document.createElement("div");
      item.className = "annotations-item" + (a.status === "done" ? " annotations-item-done" : "") + (current === entry ? " annotations-item-active" : "");
      var quote = document.createElement("blockquote");
      quote.textContent = a.exact ? (a.exact.length > 120 ? a.exact.slice(0, 117) + "…" : a.exact) : "whole document";
      var note = !a.exact ? "" : !entry.ranges.length ? "text not found in the current version" : entry.fuzzy ? "re-anchored approximately" : "";
      var head = document.createElement("div");
      head.className = "annotations-item-head";
      head.append((i + 1) + ".", badgeOf(a));
      var thread = document.createElement("div");
      thread.className = "annot-thread";
      renderThread(thread, a.thread);
      item.append(head, quote, thread);
      if (note) {
        var small = document.createElement("small");
        small.textContent = note;
        item.append(small);
      }
      item.addEventListener("click", function () {
        current = entry;
        step(0);
      });
      // Hovering an item lights its mark in the text, without opening or scrolling.
      item.addEventListener("mouseenter", function () {
        hovered = entry;
        paint();
      });
      item.addEventListener("mouseleave", function () {
        hovered = null;
        paint();
      });
      list.append(item);
    });
  }

  // Only the active group changes on hover: a full draw would rebuild the list under the
  // pointer and fire mouseleave.
  function paint() {
    var ranges = [];
    entries.forEach(function (e) {
      if ((current && current.a.id === e.a.id) || (hovered && hovered.a.id === e.a.id)) ranges.push.apply(ranges, e.ranges);
    });
    CSS.highlights.set("annot-active", new Highlight(...ranges));
  }

  function byId(id) {
    for (var i = 0; i < entries.length; i++) if (entries[i].a.id === id) return entries[i];
    return null;
  }

  // Index of the reader message the textarea edits: the last one, when nothing follows it.
  // Once the agent has answered, the textarea composes a follow-up instead.
  function editable(a) {
    var last = a.thread[a.thread.length - 1];
    return last && last.by === "reader" ? a.thread.length - 1 : -1;
  }

  function withReader(thread, text) {
    var i = editable({ thread: thread }), out = thread.slice();
    if (i >= 0) out[i] = Object.assign({}, out[i], { text: text });
    else out.push({ by: "reader", text: text });
    return out;
  }

  // done is the agent's word; reopened is derived: a reader message after the agent's.
  function stateOf(a) {
    if (a.status === "done") return "done";
    var answered = false;
    for (var i = 0; i < a.thread.length; i++) {
      if (a.thread[i].by !== "reader") answered = true;
      else if (answered) return "reopened";
    }
    return "open";
  }

  function badgeOf(a) {
    var state = stateOf(a), badge = document.createElement("span");
    badge.className = "annot-badge annot-badge-" + state;
    badge.textContent = state;
    return badge;
  }

  function whenOf(m) {
    return m.at ? m.at.slice(0, 16).replace("T", " ") : "";
  }

  function whoOf(m) {
    return m.by === "reader" ? "you" : m.by;
  }

  function renderThread(container, messages) {
    container.replaceChildren();
    messages.forEach(function (m) {
      var p = document.createElement("p"), label = document.createElement("small"), text = document.createElement("span");
      p.className = "annot-msg annot-msg-" + (m.by === "reader" ? "reader" : "agent");
      label.textContent = whoOf(m) + (whenOf(m) ? " · " + whenOf(m) : "");
      text.textContent = m.text;
      p.append(label, text);
      container.append(p);
    });
  }

  // Navigation order: whole-document comments, then the marks in document order, then the
  // orphans whose text is gone.
  function ordered() {
    return entries.slice().sort(function (x, y) {
      var kx = x.a.exact ? (x.ranges.length ? 1 : 2) : 0;
      var ky = y.a.exact ? (y.ranges.length ? 1 : 2) : 0;
      if (kx !== ky) return kx - ky;
      return kx === 1 ? x.start - y.start : 0;
    });
  }

  function step(delta) {
    var list = ordered();
    if (!list.length) return;
    var i = current ? list.indexOf(current) : delta > 0 ? -1 : list.length;
    var entry = list[(i + delta + list.length) % list.length];
    if (!current && !delta) return;
    if (entry.ranges.length) {
      var rect = entry.ranges[0].getBoundingClientRect();
      if (rect.top < 80 || rect.bottom > window.innerHeight - 240) {
        window.scrollBy({ top: rect.top - window.innerHeight / 3, behavior: "instant" });
      }
    }
    openBox(entry, null);
  }

  function typing(target) {
    return target.closest && target.closest("input, textarea, select, [contenteditable]");
  }

  function load() {
    return fetch(ENDPOINT)
      .then(function (r) {
        if (!r.ok) throw new Error("annotations: " + r.status);
        return r.json();
      })
      .then(function (d) {
        doc = d;
        draw();
      });
  }

  function save(list) {
    return fetch(ENDPOINT, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ annotations: list }),
    })
      .then(function (r) {
        if (!r.ok) throw new Error("annotations: " + r.status);
        return r.json();
      })
      .then(function (d) {
        doc = d;
        current = null;
        draw();
      });
  }

  // The selection as a quote, or null when it is not usable (empty, outside the article,
  // or starting/ending in skipped text).
  function quoteOf(selection) {
    if (!selection || selection.isCollapsed || !selection.rangeCount) return null;
    var range = selection.getRangeAt(0);
    var start = range.startContainer, end = range.endContainer;
    if (!article.contains(start) || !article.contains(end)) return null;
    var el = function (n) {
      return n.nodeType === 1 ? n : n.parentElement;
    };
    if (el(start).closest(SKIP) || el(end).closest(SKIP)) return null;
    var from = foldedOffset(offsetOf(start, range.startOffset));
    var to = foldedOffset(offsetOf(end, range.endOffset));
    var text = model.text.slice(from, to);
    var lead = text.length - text.replace(/^\s+/, "").length;
    from += lead;
    text = text.trim();
    if (!text) return null;
    to = from + text.length;
    return {
      exact: text,
      prefix: model.text.slice(Math.max(0, from - CONTEXT), from),
      suffix: model.text.slice(to, to + CONTEXT),
      rect: range.getBoundingClientRect(),
    };
  }

  function showBubble() {
    var quote = quoteOf(document.getSelection());
    if (!quote || box.matches(":popover-open")) {
      bubble.hidden = true;
      return;
    }
    pending = quote;
    bubble.hidden = false;
    bubble.style.left = Math.min(quote.rect.right, window.innerWidth - bubble.offsetWidth - 8) + "px";
    bubble.style.top = quote.rect.bottom + 6 + "px";
  }

  // The part of the box that mirrors the entry: meta line and the messages above the
  // textarea (every one but the reader message being edited). Redrawn without touching
  // the textarea when the sidecar changes underneath.
  function fillBox(entry, quote) {
    var a = entry ? entry.a : null, notes = [];
    meta.replaceChildren();
    if (a) {
      if (a.thread[0] && whenOf(a.thread[0])) notes.push(whenOf(a.thread[0]));
      notes.push(a.file_hash.slice(7, 14));
      if (a.file_hash !== document.documentElement.getAttribute("data-file-hash")) notes.push("file changed since");
      if (a.exact && !entry.ranges.length) notes.push("text not found in the current version");
      if (entry.fuzzy) notes.push("re-anchored approximately");
      if (!a.exact) notes.push("whole document");
      meta.append(badgeOf(a), " ");
    } else {
      notes.push(quote ? "new annotation" : "new comment on the whole document");
    }
    meta.append(notes.join(" · "));
    var i = a ? editable(a) : -1;
    renderThread(box.querySelector(".annot-thread"), a ? (i < 0 ? a.thread : a.thread.slice(0, i)) : []);
    textarea.placeholder = a && i < 0 ? "Follow up" : "What should change here?";
  }

  function openBox(entry, quote) {
    current = entry || null;
    var a = entry ? entry.a : null, i = a ? editable(a) : -1;
    fillBox(entry, quote);
    textarea.value = i >= 0 ? a.thread[i].text : "";
    box.querySelector(".annot-delete").hidden = !a;
    bubble.hidden = true;
    var rect = entry && entry.ranges.length ? rectOf(entry) : quote ? quote.rect : null;
    box.style.left = rect ? Math.min(rect.left, window.innerWidth - 340) + "px" : "";
    box.style.top = rect ? Math.min(rect.bottom + 8, window.innerHeight - 200) + "px" : "";
    box.classList.toggle("annot-box-centered", !rect);
    box.showPopover();
    draw();
    textarea.focus();
  }

  function closeBox() {
    if (box.matches(":popover-open")) box.hidePopover();
    current = null;
    pending = null;
    draw();
  }

  function submit() {
    var text = textarea.value.trim();
    if (!text) return;
    var list = doc.annotations.slice();
    if (current) {
      list = list.map(function (a) {
        return a.id === current.a.id ? Object.assign({}, a, { thread: withReader(a.thread, text) }) : a;
      });
    } else {
      list.push({ exact: pending ? pending.exact : null, prefix: pending ? pending.prefix : null, suffix: pending ? pending.suffix : null, thread: [{ by: "reader", text: text }] });
    }
    box.hidePopover();
    pending = null;
    save(list).catch(failed);
  }

  function remove() {
    if (!current) return;
    var id = current.a.id;
    box.hidePopover();
    save(doc.annotations.filter(function (a) {
      return a.id !== id;
    })).catch(failed);
  }

  // The comment is still in the textarea: reopen rather than lose it behind an alert.
  function failed(err) {
    console.error(err);
    meta.textContent = "Save failed: " + err.message;
    if (!box.matches(":popover-open")) box.showPopover();
  }

  // Highlights are not elements: the entry under the pointer is found by hit-testing the
  // caret position against every anchored range.
  function entryAt(x, y) {
    var node, offset;
    if (document.caretPositionFromPoint) {
      var pos = document.caretPositionFromPoint(x, y);
      if (!pos) return null;
      node = pos.offsetNode;
      offset = pos.offset;
    } else if (document.caretRangeFromPoint) {
      var r = document.caretRangeFromPoint(x, y);
      if (!r) return null;
      node = r.startContainer;
      offset = r.startOffset;
    } else return null;
    for (var i = 0; i < entries.length; i++) {
      var e = entries[i];
      for (var j = 0; j < e.ranges.length; j++) {
        if (e.ranges[j].isPointInRange(node, offset)) return e;
      }
    }
    return null;
  }

  function hover(ev) {
    if (hitFrame) return;
    hitFrame = requestAnimationFrame(function () {
      hitFrame = 0;
      var e = entryAt(ev.clientX, ev.clientY);
      if (!e || box.matches(":popover-open")) {
        tip.hidden = true;
        return;
      }
      tip.textContent = e.a.thread.map(function (m) { return whoOf(m) + ": " + m.text; }).join("\n");
      tip.hidden = false;
      tip.style.left = Math.min(ev.clientX + 12, window.innerWidth - tip.offsetWidth - 8) + "px";
      tip.style.top = ev.clientY + 16 + "px";
    });
  }

  function mount() {
    bubble = document.createElement("button");
    bubble.className = "annot-bubble";
    bubble.textContent = "Annotate";
    bubble.hidden = true;
    // mousedown, not click: a click first collapses the selection the bubble is about.
    bubble.addEventListener("mousedown", function (ev) {
      ev.preventDefault();
      openBox(null, pending);
    });

    tip = document.createElement("div");
    tip.className = "annot-tip";
    tip.hidden = true;

    box = document.createElement("div");
    box.className = "annot-box";
    box.setAttribute("popover", "manual");
    box.innerHTML =
      '<div class="annot-meta"></div>' +
      '<div class="annot-thread"></div>' +
      '<textarea rows="4" placeholder="What should change here?"></textarea>' +
      '<div class="annot-actions">' +
      '<button type="button" class="annot-delete">Delete</button>' +
      '<span class="annot-spacer"></span>' +
      '<button type="button" class="annot-cancel">Cancel</button>' +
      '<button type="button" class="annot-save">Save</button>' +
      "</div>";
    textarea = box.querySelector("textarea");
    meta = box.querySelector(".annot-meta");
    box.querySelector(".annot-save").addEventListener("click", submit);
    box.querySelector(".annot-cancel").addEventListener("click", closeBox);
    box.querySelector(".annot-delete").addEventListener("click", remove);
    textarea.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape") closeBox();
      if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) submit();
    });

    document.body.append(bubble, tip, box);

    var docButton = document.getElementById("annotation-document");
    if (docButton) {
      docButton.addEventListener("click", function () {
        openBox(null, null);
      });
    }
    var prev = document.getElementById("annotation-prev"), next = document.getElementById("annotation-next");
    if (prev) prev.addEventListener("click", function () { step(-1); });
    if (next) next.addEventListener("click", function () { step(1); });

    document.addEventListener("keydown", function (ev) {
      if (ev.ctrlKey || ev.metaKey || ev.altKey || typing(ev.target)) return;
      if (ev.key === "n") step(1);
      else if (ev.key === "p") step(-1);
      else if (ev.key === "a" && wanted() && pending && !bubble.hidden) openBox(null, pending);
      else if (ev.key === "A" && wanted()) openBox(null, null);
      else if (ev.key === "Escape") closeBox();
      else return;
      ev.preventDefault();
    });

    document.addEventListener("selectionchange", function () {
      if (wanted()) showBubble();
    });
    article.addEventListener("pointermove", hover);
    article.addEventListener("pointerleave", function () {
      tip.hidden = true;
    });
    article.addEventListener("click", function (ev) {
      // A drag-select ending on a mark also fires click: that is a selection, not a click.
      if (!document.getSelection().isCollapsed) return;
      var e = entryAt(ev.clientX, ev.clientY);
      if (e) openBox(e, null);
    });
  }

  function reflect(button) {
    var on = wanted();
    button.setAttribute("aria-pressed", String(on));
    if (!on && bubble) bubble.hidden = true;
  }

  document.addEventListener("DOMContentLoaded", function () {
    var button = document.getElementById("annotate-toggle");
    article = document.querySelector(".container > div");
    if (!button || !article) return;
    if (!supported) {
      button.disabled = true;
      button.title = "Annotations need the CSS Custom Highlight API (Chrome 105+, Firefox 140+, Safari 17.2+)";
      return;
    }
    model = buildModel();
    mount();
    reflect(button);
    button.addEventListener("click", function () {
      remember(!wanted());
      reflect(button);
      draw();
    });
    load().catch(console.error);
  });
})();
