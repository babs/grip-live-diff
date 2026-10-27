(function () {
  var STORAGE_KEY = "grip-live-diff-annotate";
  var CONTEXT = 32;
  // pathname is already percent-encoded: decode first or "my doc.md" reaches the server
  // as "my%20doc.md".
  var ENDPOINT = "/__annotations?path=" + encodeURIComponent(decodeURIComponent(location.pathname));
  var READY = ENDPOINT.replace("/__annotations?", "/__annotations/ready?");
  // Text that is not the document: diff deletions, diagrams and formulas (their rendered
  // text does not exist in the source), the code-block copy buttons.
  var SKIP = "del.gg-del, .mermaid, .math, mjx-container, script, style, button, [hidden]";

  var article, model, entries = [], doc = { annotations: [] }, pending = null, current = null, hovered = null;
  var bubble, tip, tipThread, box, textarea, meta, hitFrame;
  // A reload offered while the reader is mid-selection (pointer down), has the bubble to
  // click, a box open, or (hold_reload on) a batch sent is held, then run by release() once
  // none of that is left.
  var pointerDown = false, held = false, saving = false, wordSelect = false;
  // A dead agent changes nothing on disk: while one works, its liveness is polled.
  var pollTimer = 0, polling = false, lastTrigger = null;
  // Requests are numbered: an answer older than the one last applied is dropped, or a GET
  // sent before a cancel could bring "working" back.
  var asked = 0, applied = 0;
  // A failed save stays in the box until the next save or the box closes: a refresh redraws
  // the meta line.
  var saveError = "";

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
    var had = current;
    if (current) current = byId(current.a.id);
    // Every doc swap lands here: an entry deleted under its open box closes it, as refresh() does.
    if (had && !current && box.matches(":popover-open")) return closeBox();
    var nav = document.querySelector(".annotations-nav");
    if (nav) nav.hidden = !(doc.annotations.length || wanted() || manual());
    var counter = document.getElementById("annotation-counter");
    if (counter) {
      var at = current ? ordered().indexOf(current) + 1 : 0;
      counter.textContent = entries.length ? (at ? at + "/" : "") + entries.length : "";
    }
    drawList();
    drawTrigger();
  }

  function manual() {
    return doc.trigger === "manual";
  }

  // The batch waits on the agent. A ready file in auto is stale: neither shown nor held for.
  function queued() {
    var state = doc.ready && doc.ready.state;
    return manual() && (state === "sent" || state === "working");
  }

  function drawTrigger() {
    var mode = document.getElementById("annotation-mode");
    if (!mode) return;
    var go = document.getElementById("annotation-go"), cancel = document.getElementById("annotation-cancel");
    var ready = doc.ready || { state: "none" }, focused = document.activeElement;
    // Same rule as the server's: the reader spoke last and the entry is not done.
    var open = doc.annotations.some(function (a) {
      return a.status !== "done" && editable(a) >= 0;
    });
    mode.hidden = false;
    mode.textContent = manual() ? "manual" : "auto";
    mode.setAttribute("aria-pressed", String(manual()));
    mode.title = manual() ? "Manual: the agent waits for Go. Click to switch to auto" : "Auto: the agent acts on every save. Click to switch to manual";
    go.hidden = !manual();
    // Gone too: with nothing open, ✕ is the only way to clear a dead agent's batch.
    cancel.hidden = !manual() || ready.state === "none";
    go.setAttribute("data-state", ready.state);
    go.textContent = { sent: "Sent", working: "Agent working · pid " + ready.pid, gone: "Agent gone · pid " + ready.pid }[ready.state] || "Go";
    go.disabled = queued() || !open;
    go.title = queued() ? "The agent has the batch; ✕ cancels it" : !open ? "Nothing to send: no entry waits for the agent" : ready.state === "gone" ? "The agent exited: send the batch again" : "Send every open entry to the agent";
    var status = document.getElementById("annotation-trigger-state"), label = manual() ? go.textContent : "";
    if (status.textContent !== label) status.textContent = label;
    // Live only once the first label is in: a page load must not announce it.
    if (!status.hasAttribute("aria-live")) status.setAttribute("aria-live", "polite");
    // The button that had the focus may just have been disabled or hidden: hand it on.
    if ((focused === go || focused === cancel) && (focused.hidden || focused.disabled)) {
      var other = focused === go ? cancel : go;
      (other.hidden || other.disabled ? mode : other).focus();
    }
    // An error is about the state it was raised in; a new state makes it stale.
    var key = manual() + ready.state + open;
    if (lastTrigger !== null && key !== lastTrigger) triggerError("");
    lastTrigger = key;
    if (manual() && ready.state === "working") {
      if (!pollTimer) pollTimer = setInterval(poll, 5000);
    } else if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = 0;
    }
  }

  function fresh(n) {
    if (n < applied) return false;
    applied = n;
    return true;
  }

  function get() {
    return fetch(ENDPOINT).then(function (r) {
      if (!r.ok) throw new Error("annotations: " + r.status);
      return r.json();
    });
  }

  // Liveness check: only the toolbar is redrawn, so a selection in the list and an open box
  // are left alone. A changed sidecar is redrawn whole; the box closes only if its entry is gone.
  function poll() {
    if (polling) return;
    polling = true;
    var n = ++asked;
    get()
      .then(function (d) {
        if (!fresh(n)) return;
        if (d.trigger !== doc.trigger || JSON.stringify(d.annotations) !== JSON.stringify(doc.annotations)) {
          doc = d;
          draw();
        } else {
          doc.ready = d.ready;
          drawTrigger();
        }
        release();
      })
      .catch(console.error)
      .finally(function () {
        polling = false;
      });
  }

  // A failed toolbar action says so beside the buttons; the next success clears it.
  function triggerError(text) {
    document.getElementById("annotation-trigger-error").textContent = text;
  }

  function setTrigger(next) {
    var n = ++asked;
    fetch(ENDPOINT, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ trigger: next }) })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (d) {
        triggerError("");
        if (!fresh(n)) return;
        doc = d;
        draw();
        release();
      })
      .catch(function (err) {
        triggerError("Mode not saved: " + err.message);
      });
  }

  // POST sends the batch, DELETE cancels it. A 409 means the page is behind the sidecar.
  function sendReady(method) {
    var n = ++asked;
    fetch(READY, { method: method })
      .then(function (r) {
        // Refetched first: the redraw would clear a message set before it.
        if (r.status === 409) {
          return r.text().then(function (text) {
            return refresh().then(function () {
              triggerError("Not sent: " + text.trim());
            });
          });
        }
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json().then(function (st) {
          triggerError("");
          if (!fresh(n)) return;
          doc.ready = st;
          draw();
          release();
        });
      })
      .catch(function (err) {
        triggerError((method === "POST" ? "Not sent: " : "Not cancelled: ") + err.message);
      });
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
        var small = document.createElement("small"), link = !entry.ranges.length && revisionLink(a);
        small.textContent = note;
        if (link) small.append(" · ", link);
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

  // A link to the diff against the version the entry was written on, when the server kept
  // a copy of it; the lost passage shows there struck through.
  function revisionLink(a) {
    var kept = doc.revisions && doc.revisions[a.file_hash];
    if (!kept) return null;
    var link = document.createElement("a");
    link.className = "annot-revision";
    link.href = location.pathname + "?diff=" + kept.slice(kept.lastIndexOf("/") + 1).replace(/\.md$/, "");
    link.textContent = "in the annotated version";
    link.addEventListener("click", function (ev) {
      ev.stopPropagation();
    });
    return link;
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
    var n = ++asked;
    return get().then(function (d) {
      if (!fresh(n)) return;
      doc = d;
      draw();
      // The ready file may have gone: a reload held for it runs now.
      release();
    });
  }

  // The sidecar changed underneath (the agent wrote, or this page saved): refetch and
  // redraw. An open box keeps what is being typed and follows its entry; it closes only
  // when the entry is gone.
  function refresh() {
    var id = current ? current.a.id : null;
    return load().then(function () {
      if (!id || !box.matches(":popover-open")) return;
      if (current) fillBox(current, null);
      else closeBox();
    });
  }

  function save(list) {
    var n = ++asked;
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
        current = null;
        if (fresh(n)) doc = d;
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
      start: from,
      end: to,
    };
  }

  function showBubble() {
    var quote = quoteOf(document.getSelection());
    if (!quote || box.matches(":popover-open")) {
      bubble.hidden = true;
      release();
      return;
    }
    pending = quote;
    bubble.hidden = false;
    bubble.style.left = Math.min(quote.rect.right, window.innerWidth - bubble.offsetWidth - 8) + "px";
    bubble.style.top = quote.rect.bottom + 6 + "px";
  }

  function busy() {
    return pointerDown || !bubble.hidden || box.matches(":popover-open") || (queued() && document.documentElement.getAttribute("data-hold-reload") === "true");
  }

  function release() {
    if (held && !busy()) location.reload();
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
    if (saveError) meta.append(" · " + saveError);
    var link = a && a.exact && !entry.ranges.length && revisionLink(a);
    if (link) meta.append(" · ", link);
    var i = a ? editable(a) : -1;
    renderThread(box.querySelector(".annot-thread"), a ? (i < 0 ? a.thread : a.thread.slice(0, i)) : []);
    textarea.placeholder = a && i < 0 ? "Follow up" : "What should change here?";
  }

  function openBox(entry, quote) {
    current = entry || null;
    saveError = "";
    var a = entry ? entry.a : null, i = a ? editable(a) : -1;
    fillBox(entry, quote);
    textarea.value = i >= 0 ? a.thread[i].text : "";
    box.querySelector(".annot-delete").hidden = !a;
    bubble.hidden = true;
    tip.hidden = true;
    var rect = entry && entry.ranges.length ? rectOf(entry) : quote ? quote.rect : null;
    box.classList.toggle("annot-box-centered", !rect);
    // Focusing the textarea drops the native selection: the quote stays marked instead.
    CSS.highlights.set("annot-pending", new Highlight(...(quote ? rangesOf(quote.start, quote.end) : [])));
    box.showPopover();
    // Placed once shown: a hidden popover has no size to clamp against.
    box.style.left = rect ? Math.max(8, Math.min(rect.left, window.innerWidth - box.offsetWidth - 8)) + "px" : "";
    box.style.top = rect ? Math.max(8, Math.min(rect.bottom + 8, window.innerHeight - box.offsetHeight - 8)) + "px" : "";
    draw();
    textarea.focus();
  }

  function closeBox() {
    saveError = "";
    if (box.matches(":popover-open")) box.hidePopover();
    CSS.highlights.delete("annot-pending");
    current = null;
    pending = null;
    draw();
    release();
  }

  function submit() {
    var text = textarea.value.trim();
    // A closed box has nothing to save: its textarea may still hold a deleted entry's text.
    if (!text || saving || !box.matches(":popover-open")) return;
    var list = doc.annotations.slice();
    if (current) {
      list = list.map(function (a) {
        return a.id === current.a.id ? Object.assign({}, a, { thread: withReader(a.thread, text) }) : a;
      });
    } else {
      list.push({ exact: pending ? pending.exact : null, prefix: pending ? pending.prefix : null, suffix: pending ? pending.suffix : null, thread: [{ by: "reader", text: text }] });
    }
    // Closed once saved, not before: a reload deferred behind the open box would otherwise
    // cut the PUT short. pending stays until then, so a retry keeps its anchor.
    write(list);
  }

  function remove() {
    if (!current || saving) return;
    var id = current.a.id;
    write(doc.annotations.filter(function (a) {
      return a.id !== id;
    }));
  }

  // The box stays focused during the PUT: one write at a time, or a double Ctrl-Enter
  // sends the entry twice.
  function write(list) {
    saving = true;
    saveError = "";
    save(list)
      .then(closeBox, failed)
      .finally(function () {
        saving = false;
      });
  }

  // The comment is still in the textarea: reopen rather than lose it behind an alert.
  function failed(err) {
    console.error(err);
    saveError = "Save failed: " + err.message;
    meta.textContent = saveError;
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
      if (!e || !e.a.thread.length || box.matches(":popover-open")) {
        tip.hidden = true;
        return;
      }
      renderThread(tipThread, e.a.thread);
      tip.hidden = false;
      // A thread taller than the cap is faded out at the bottom; the click opens it whole.
      tip.classList.toggle("annot-tip-cut", tipThread.scrollHeight > tip.clientHeight);
      tip.style.left = Math.min(ev.clientX + 12, window.innerWidth - tip.offsetWidth - 8) + "px";
      // Below the pointer, or above it when the thread would run off the bottom.
      var top = ev.clientY + 16;
      if (top + tip.offsetHeight > window.innerHeight - 8) top = Math.max(8, ev.clientY - 16 - tip.offsetHeight);
      tip.style.top = top + "px";
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
    tipThread = document.createElement("div");
    tipThread.className = "annot-thread";
    tip.append(tipThread);

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
    var mode = document.getElementById("annotation-mode");
    if (mode) {
      mode.addEventListener("click", function () { setTrigger(manual() ? "auto" : "manual"); });
      document.getElementById("annotation-go").addEventListener("click", function () { sendReady("POST"); });
      document.getElementById("annotation-cancel").addEventListener("click", function () { sendReady("DELETE"); });
    }
    // Timers are throttled in a hidden tab: catch up on return.
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") refresh().catch(console.error);
    });
    // Hold turned off in the settings menu: a reload held for the batch runs now.
    new MutationObserver(function () { release(); }).observe(document.documentElement, { attributeFilter: ["data-hold-reload"] });

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

    window.addEventListener("gld:annotations", function () {
      refresh().catch(console.error);
    });
    document.addEventListener("gld:reload", function (ev) {
      if (!busy()) return;
      held = true;
      ev.preventDefault();
    });
    ["pointerdown", "pointerup", "pointercancel"].forEach(function (type) {
      document.addEventListener(type, function (ev) {
        // Main button only: a context-menu press may never report its release.
        pointerDown = type === "pointerdown" && ev.button === 0;
        release();
      });
    });
    // A window switch mid-press loses the pointerup.
    window.addEventListener("blur", function () {
      pointerDown = false;
      release();
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
      // So is a multi-click (ev.detail counts them), handled on mouseup.
      if (ev.detail >= 2 || !document.getSelection().isCollapsed) return;
      var e = entryAt(ev.clientX, ev.clientY);
      if (e) openBox(e, null);
    });
    // A selection started by a double-click (a word, or words dragged from one) is the
    // annotation itself, capture on or off: the box opens without the bubble stop. Armed
    // on the press that selects the word (detail 2), opened on the release, wherever it
    // lands; dblclick would not fire once the pointer has moved.
    article.addEventListener("mousedown", function (ev) {
      wordSelect = ev.detail >= 2 && ev.button === 0;
      if (wordSelect && !wanted()) capture(true);
    });
    document.addEventListener("mouseup", function () {
      if (!wordSelect) return;
      wordSelect = false;
      if (box.matches(":popover-open")) return;
      pending = quoteOf(document.getSelection());
      if (pending) openBox(null, pending);
    });
  }

  function capture(on) {
    remember(on);
    var button = document.getElementById("annotate-toggle");
    button.setAttribute("aria-pressed", String(wanted()));
    if (!on) bubble.hidden = true;
    draw();
    release();
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
    button.setAttribute("aria-pressed", String(wanted()));
    button.addEventListener("click", function () {
      capture(!wanted());
    });
    load().catch(console.error);
  });
})();
