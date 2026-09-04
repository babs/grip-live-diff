# 002 — Minimap in diff mode

**Status**: in progress
**Requested by**: babs
**Date**: 2026-09-04

## Problem

In diff mode on a long document, the reader has no idea where the changes are. The only way to find
them is to scroll the whole page looking for green and red, and on a document of a few thousand lines
a single edited word is easy to miss entirely.

## Solution

When a diff is on, a VS Code-style minimap sits along the right edge of the window: a scaled-down
rendering of the whole document, with insertions and deletions marked in green and red so they stand
out at a glance. A translucent rectangle shows the part of the document currently on screen. Clicking
anywhere on the minimap jumps there, dragging the rectangle scrolls the page. A toolbar button hides or
shows the minimap and the choice is remembered, like the theme and the page width.

## Scope

- Minimap rendered only when `?diff=…` is active, whatever the reference (open, last, head).
- Scaled rendering of the actual document (same styles, same theme), not a synthetic sketch.
- Full-width green / red marks at the height of every `ins` / `del`, so a one-word change is visible
  even when the scaled text is not.
- Viewport indicator, click-to-jump, drag-to-scroll.
- When the scaled document is taller than the window, the minimap slides with the scroll so the
  current position is always visible (VS Code behaviour).
- Toggle button in the diff toolbar row, preference in `localStorage`, default on.
- Rebuilt when the layout changes (width mode, window resize, mermaid / MathJax finishing).
- Hidden on print and under 940px, where the markdown stylesheet stops centering the article.

## Out of scope

- Minimap outside diff mode.
- Heading / structure markers.
- Hover tooltips, per-change navigation ("next change"), a change counter.
- Live update while typing: the page reloads on every save, the minimap is rebuilt with it.
- Any tuning for pathological documents (tens of thousands of lines): the minimap doubles the DOM
  of the article and that is accepted as the ceiling of this iteration.

## Acceptance criteria

- [ ] Given a document loaded with `?diff=open`, the page carries the minimap script and the toggle
      button; loaded without `?diff`, it carries neither.
- [ ] Given a long document with one insertion and one deletion in diff mode, the minimap shows a green
      mark and a red mark at heights proportional to their position in the document.
- [ ] Given the reader scrolls the page, the viewport rectangle follows and stays inside the minimap.
- [ ] Given a click at the bottom of the minimap, the page scrolls so that the end of the document is
      on screen.
- [ ] Given a drag of the viewport rectangle, the page scrolls with it.
- [ ] Given the toggle is clicked, the minimap disappears; after a reload it stays hidden; clicking
      again brings it back and that also survives a reload.
- [ ] Given the page width is switched (normal / wide / full), the minimap is rebuilt at the new
      proportions and the marks stay aligned with the changes.
- [ ] Given the document contains a mermaid diagram and a MathJax formula, the minimap reflects their
      rendered height, not the source block, and neither is rendered twice by their libraries.
- [ ] Given the dark theme, the minimap is drawn in the dark theme.
- [ ] Nothing in the minimap is focusable or read by assistive technology, and links inside it do
      not navigate.
- [ ] `go test ./...` green; the template contract (script and toggle present only in diff mode) is
      pinned by a Go test.

## Phases

### Phase 1 — Minimap
- Work: `defaults/static/js/minimap.js` clones the rendered article into a fixed `inert` panel on the
  right, scaled by CSS transform to a fixed width, with ids stripped from the clone. Viewport
  rectangle synced on scroll, click-to-jump, drag-to-scroll, proportional sliding when the map is
  taller than the window. Full-width marks computed from the position of every `ins.gg-ins` /
  `del.gg-del` in the original. Rebuild on a debounced `ResizeObserver` of the article. Styles in
  `github-diff.css` (light / dark), hidden in `github-print.css` and under 940px. Script included
  from `layout.html` inside `{{if .DiffMode}}`.
- **Data model impact**: none.
- **DoD**: `go test ./internal/ -run Minimap` green — the script tag is present with `?diff=open` and
  absent without. A scripted browser check on a 250-paragraph document with one insertion and one
  deletion: two marks at the expected heights, `scrollY` lands at the document end after a click at
  the bottom of the map, the viewport rectangle tracks a programmatic scroll, exactly one mermaid
  `<svg>` per diagram outside the minimap panel.

### Phase 2 — Toggle
- Work: button in the diff toolbar row next to "Mark as read", `data-minimap="on|off"` on `<html>`
  driven by `localStorage` key `grip-live-diff-minimap`, applied before first paint like the width
  mode. The minimap is not built at all while off, so hiding it also removes its cost.
- **Data model impact**: none.
- **DoD**: Go test pins the toggle button in diff mode only. Browser check: click → panel gone from
  the DOM, reload → still gone, click → back, reload → still back.

## Data model impact (summary)

None.

## Open questions

- [ ] None.

## Decisions

- Rendering by DOM clone + CSS `transform: scale()` rather than a canvas painter — it is the only way to
  get the real rendering with zero dependency and a few dozen lines; the cost is a second layout of
  the article, accepted for this iteration.
- Fixed minimap width (about 100 px) and a scale derived from the article width, so the map keeps the
  same proportions across the three width modes.
- Change marks drawn from the originals' positions, not from the clone, so they stay exact whatever the
  scaled text looks like.
- `inert` on the panel (native) instead of hand-rolled `aria-hidden` + `tabindex` + `pointer-events`
  juggling; clicks are captured by an overlay on top of the clone.
- Default on: the feature was asked for, hiding it is the exception.
- Preference key and attribute names follow the existing `grip-live-diff-<thing>` / `data-<thing>`
  pattern of theme and width.
