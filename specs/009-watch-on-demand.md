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
- On connect, the socket subscribes, before the hash check of 007, the directory of `page` and the
  directory of each `res`. The sidecar sits in the page's directory; the kept revisions are only
  written by the request that also writes the sidecar, so `.annotations.d` needs no subscription.
- The socket is refused when `page` is not an existing markdown file of the served tree, the check
  the page and the annotations already apply. A foreign `Origin` is refused before that check, so it
  learns nothing about which files exist.
- A `res` that does not stay under the served root once cleaned, `\` included, is ignored.
- At most 10 directories per socket, the page's first, and 100 for the server, counting every
  directory with a subscription or in its 30 s grace; a new directory beyond is not watched and is
  logged once. A page may still join a directory already counted.
- Subscriptions are counted per directory: pages open in several directories keep all of them
  watched. A directory is added on its first subscriber and removed 30 s after its last one left, so
  a reload does not reopen it.
- A check every 2 s reconciles the followed directories with the disk:
  - one that was missing and now exists is added, and the files already in it are broadcast, so its
    resources reload the page;
  - one that is watched but gone, or replaced by another directory (removed, renamed, or a parent
    renamed), loses its watch and goes back to missing.
- A directory that cannot be watched is logged once and the page does not reload; the server keeps
  running. Out of descriptors is logged once too, and is not final: the 2 s check tries again.
- The server notices a closed socket at once and drops its subscriptions; the other pages get
  nothing. A connection that stops answering is dropped: a ping every 30 s, gone after 60 s without
  an answer.
- A directory argument serves that directory and opens its `README.md`, or its listing when there is
  none. `docs` and `docs/` behave the same; a missing `docs/` is an error at start.

## Out of scope

- Ignore rules (`.gitignore`, defaults, `--ignore`): nothing is walked, nothing needs ignoring.
- The directory listing: `http.FileServer` reads one directory per request.
- A watched directory that holds many files still costs one descriptor per file on macOS.
- Watching the nearest existing parent of a missing resource directory: the 2 s check covers it.
- Reacting to a directory created or removed faster than the 2 s check.
- Windows: two spellings of one directory (`/img`, `/Img`) are two subscriptions on one watch;
  releasing one stops the other's reloads until the page reconnects.
- Symbolic links. Today the walk does not follow them: a root that is a link watches nothing, a
  linked subdirectory is skipped. After this change Linux follows them; on macOS kqueue reports the
  events under the target path, which no page names, so nothing reloads, as today.
- Binding to all interfaces, as today.

## Acceptance criteria

- [ ] Given a tree of 68,000 files, the server listens in under 1 s (manual run).
- [ ] Given the server started and no page open, no directory is watched.
- [ ] Given `doc.md` open, writing a file in `sub/` (not a resource) sends nothing, and `sub/` is not
      watched.
- [ ] Given `sub/doc.md` open, writing it reloads it.
- [ ] Given `doc.md` open with `img/a.png`, writing `img/a.png` reloads it.
- [ ] Given `docs/doc.md` open with `/assets/img/a.png` and no `assets/`, creating
      `assets/img/a.png` reloads the page within 3 s.
- [ ] Given `doc.md` open with `img/a.png` and no `img/`, creating `img/a.png` reloads the page
      within 3 s.
- [ ] Given `doc.md` open with `img/a.png`, removing then recreating `img/` and writing `img/a.png`
      reloads the page within 3 s.
- [ ] Given `docs/doc.md` open with `/assets/img/a.png`, renaming `assets/` then recreating
      `assets/img/a.png` reloads the page within 3 s, and writes in the renamed directory send
      nothing.
- [ ] Given two pages in one directory, closing one keeps the directory watched; closing both keeps
      it watched for 30 s, then releases it.
- [ ] Given two sockets on one page, closing one sends nothing on the other.
- [ ] Given a connection that stops answering pings, its subscriptions are dropped.
- [ ] Given `page` not an existing markdown file, the handshake is refused; given a foreign
      `Origin`, it is refused the same way whether the page exists or not.
- [ ] Given `res` values outside the root or holding `\`, nothing outside the root is watched.
- [ ] Given 20 resource directories, the page's and 9 of them are watched; given 100 directories
      counted, a page in a new directory watches nothing new and still renders, and a page joining a
      counted directory follows it.
- [ ] Given an unwatchable directory, the page is served and the server keeps running.
- [ ] Subscribing happens before the hash check, for every page on a directory, not only the first.
- [ ] `grip-live-diff docs` and `grip-live-diff docs/` serve `docs/` and open `docs/README.md`; a
      missing `docs/` exits with an error.
- [ ] The criteria of 007 still hold.
- [ ] `go test ./...` green, `GOOS=windows go build ./...` green.

## Decisions

- Subscribe per page rather than filter the walk: the cost no longer depends on the tree, and no
  ignore list has to guess which `build/` or `vendor/` holds docs.
- Release after a grace delay rather than at once: a reload closes and reopens the socket, and kqueue
  reopens every file of the directory on each add.
- Poll a missing resource directory rather than watch its nearest existing parent: that parent can
  be any directory of the tree, a large one included, and a stat every 2 s costs no descriptor.
- One 2 s check for every appearance, disappearance and move, rather than reacting to events: the
  event loop only broadcasts and never takes the subscriptions lock, so `Add` can run under that
  lock without the Windows deadlock (there `Add` waits on the goroutine that feeds the event loop).
  A late directory costs up to 2 s.
- Subscribe before the hash check: a write between the check and the add would be lost otherwise.
- Caps of 10 per socket and 100 in total, and the `page` check: the server listens on every
  interface, and without them any client could have the whole tree watched again.
- No subscription for `.annotations.d`: its writes always come with a sidecar write in the page's
  directory.
