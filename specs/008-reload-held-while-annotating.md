# 008 — Reload held while annotating

**Status**: shipped
**Requested by**: babs
**Date**: 2026-09-17

## Problem

A save on disk reloads the page under the reader: a selection being dragged is lost, the
**Annotate** bubble vanishes, a half-typed comment goes with the box. The agent writes the file
exactly while the reader comments on it.

Two smaller annoyances alongside: a word selected with a double-click still needs the bubble click,
and the page has no way back to the directory listing.

## Solution

- The reload script offers every reload as a cancelable `gld:reload` event. `annotate.js` cancels it
  while the pointer is down, the bubble is visible or a box is open, and calls `location.reload()`
  itself once none of that is left (pointer up, selection dropped, box saved or closed). Save and
  delete close the box after the PUT resolved, so a held reload never cuts it short.
- A selection begun by a double-click opens the box at once, bubble skipped: armed on the `mousedown`
  whose `detail` is 2 or more (the press that selects the word), which also turns capture on, opened on the
  `mouseup`, so a drag extending the selection by words ends in the box too. `dblclick` is not used:
  it does not fire once the pointer has moved.
- While a box is open on a new selection, the quote is drawn in a `annot-pending` highlight: the
  textarea takes the focus, and the native selection with it.
- A house button, first in the toolbar, links to `/`: the listing of the served directory.

## Out of scope

- Keyboard selections (shift+arrows) as a hold: the bubble covers them once the selection exists.
- Restoring the selection after the deferred reload.

## Acceptance criteria

- [x] Pointer down, bubble visible or box open: `gld:reload` is cancelled; the page reloads on
      pointer up, selection collapse, box close respectively.
- [x] Nothing pending: `gld:reload` is not cancelled, the page reloads at once.
- [x] Ctrl-Enter in a box opened on a double-clicked word saves an entry anchored on that word,
      then the held reload runs.
- [x] Capture off, double-click on a word: the pencil lights up on the second press, the box opens on release.
- [x] Box open on a selection: the quote carries the `annot-pending` highlight, gone once closed.
- [x] `--no-reload` and unsupported browsers unchanged: the event has no listener, or is never sent.
- [x] `go test ./...` green.

## Decisions

- A cancelable DOM event rather than a `window` hook: the reload script stays two lines and knows
  nothing of annotations; `annotate.js` owns every state it locks on.
- Hold on the pointer, not only on a non-collapsed selection: a drag has no selection until the first
  character is crossed.
