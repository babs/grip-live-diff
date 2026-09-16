# 007 — Reload scoped to the page

**Status**: shipped
**Requested by**: babs
**Date**: 2026-09-16

Supersedes the watcher rules of [005](005-annotation-threads.md) and [006](006-annotation-revisions.md).

## Problem

The watcher follows the whole served tree and every change reloaded every open page: another
markdown file, an editor swap file, a `git status` writing `.git/index`.

## Solution

Each page tells the server what it shows; a burst only reaches the pages it concerns.

## Scope

- The page opens `/reload_ws?page=<path>&hash=<data-file-hash>&res=<path>…` once loaded; `res` is
  every same-origin `[src]` of the document outside `/static/`, deduplicated.
- The watcher reports changed files as URL paths; a settled burst holds all of them.
- Per page: `reload` when the burst holds the page or one of its resources; otherwise
  `annotations` when it holds the page's sidecar, its `.annotations.d/` directory or a file in it;
  otherwise nothing.
- On connect, the server hashes the file on disk: a hash different from the page's sends `reload`,
  so a write between rendering and connecting is not lost; skipped while a burst holding the page is
  still debouncing, which delivers the reload itself.
- The socket refuses a foreign `Origin`.

## Out of scope

- `srcset` and CSS `url()` resources.
- Case-insensitive file systems: a resource spelled with another case than the file does not match.

## Acceptance criteria

- [x] Given `doc.md` open, writing `other.md`, an unused image, `.doc.md.swp` or `.git/index` does not
      reload it.
- [x] Given `doc.md` open, writing an image it embeds reloads it.
- [x] Given `doc.md` open, writing its sidecar or a kept revision refreshes the marks without reload;
      another file's sidecar does nothing.
- [x] Given the markdown and its sidecar in the same burst, the page reloads once.
- [x] Given the file changed after the page was rendered and before its socket opened, the page
      reloads on connect.
- [x] Given the file written nonstop, connecting does not reload on its own.
- [x] Given a foreign `Origin`, the handshake is refused.
- [x] `go test ./...` green; routing pinned by `TestPageMessage`, connect check by
      `TestReloadWebsocketCatchesAWriteBeforeConnect`.

## Decisions

- Resources listed by the browser from the DOM rather than parsed server-side from the rendered HTML
  — the browser already resolved every relative URL, raw HTML included.
- Routing on the server rather than the client — testable in Go; the client only names what it shows.
