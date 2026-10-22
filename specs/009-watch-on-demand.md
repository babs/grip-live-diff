# 009 — Watch on demand

**Status**: draft
**Requested by**: DuxLocutus
**Date**: 2026-10-22

Supersedes the watcher of [007](007-reload-scoped-to-page.md); its routing is unchanged.

## Problem

At start the server walks the whole served tree and adds every directory to the watcher before it
listens. On macOS the kqueue backend of fsnotify also opens one descriptor per file of each watched
directory. A tree holding `node_modules`, `vendor`, `var/cache` or `target` costs minutes, then
fails: on a tree of 68,000 files, 185 s, then
`watch .: "…/persist-take.ts": too many open files in system`, and the server exits. The
descriptors are counted against `kern.maxfiles`, so other processes run short too.

Since 007 a page only reacts to its markdown, its resources, its sidecar and its kept revisions:
the rest of the tree is watched for nothing.

A directory argument makes it worse: `grip-live-diff docs` serves and watches the parent of `docs`.

## Solution

Nothing is watched at start. A page's socket subscribes the directories its message depends on;
the last socket to leave releases them.

## Scope

- `watch` creates the watcher and walks nothing. The server listens at once.
- On connect, the socket subscribes, before the hash check of 007:
  - the directory of `page`;
  - the directory of each `res`;
  - `<page base>.annotations.d`, when it exists.
- Subscriptions are counted per directory: pages open in several directories keep all of them
  watched. A directory is added on its first subscriber and removed 30 s after its last one left, so
  a reload does not reopen it.
- A directory created later is added when it is subscribed, that is `<page base>.annotations.d`
  created after connect.
- A resource directory missing on connect is checked every 2 s while a page subscribes it. Once it
  exists, it is added, and its resources already present reload the page.
- The server notices a closed socket at once: a reader on the connection ends the handler.
- A directory that cannot be watched is logged; the page is served and does not reload.
- A directory argument serves that directory and opens its `README.md`, or its listing when there is
  none. `docs` and `docs/` behave the same.

## Out of scope

- Ignore rules (`.gitignore`, defaults, `--ignore`): nothing is walked, nothing needs ignoring.
- The directory listing: `http.FileServer` reads one directory per request.
- A watched directory that holds many files still costs one descriptor per file on macOS.
- Watching the nearest existing parent of a missing resource directory: the 2 s check covers it.

## Acceptance criteria

- [ ] Given a tree of 68,000 files, the server listens in under 1 s and opens no descriptor before a
      page connects.
- [ ] Given `doc.md` open, writing a file in `sub/` (not a resource) sends nothing, and `sub/` is not
      watched.
- [ ] Given `sub/doc.md` open, writing it reloads it.
- [ ] Given `doc.md` open with `img/a.png`, writing `img/a.png` reloads it.
- [ ] Given `doc.md` open and no `doc.annotations.d/`, the first kept revision written refreshes the
      marks.
- [ ] Given two pages in one directory, closing one keeps the directory watched; closing both
      releases it 30 s later; a reconnect within 30 s does not re-add it.
- [ ] Given `doc.md` open with `img/a.png` and no `img/`, creating `img/a.png` reloads the page
      within 3 s.
- [ ] Given an unwatchable directory, the page is served and the server keeps running.
- [ ] `grip-live-diff docs` and `grip-live-diff docs/` serve `docs/` and open `docs/README.md`.
- [ ] The criteria of 007 still hold.
- [ ] `go test ./...` green.

## Decisions

- Subscribe per page rather than filter the walk: the cost no longer depends on the tree, and no
  ignore list has to guess which `build/` or `vendor/` holds docs.
- Release after a grace delay rather than at once: a reload closes and reopens the socket, and kqueue
  reopens every file of the directory on each add.
- Poll a missing resource directory rather than watch its nearest existing parent: that parent can
  be any directory of the tree, a large one included, and a stat every 2 s costs no descriptor.
- Subscribe before the hash check: a write between the check and the add would be lost otherwise.
