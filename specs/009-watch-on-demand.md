# 009 — Watch on demand

**Status**: shipped
**Requested by**: DuxLocutus
**Date**: 2026-10-22

Supersedes the watcher of [007](007-reload-scoped-to-page.md); its routing is unchanged.

## Problem

At start the server walks the whole served tree and adds every directory to the watcher before it
listens. On macOS the kqueue backend of fsnotify also opens one descriptor per file of each watched
directory. A tree holding `node_modules`, `vendor`, `var/cache` or `target` costs minutes, then
fails. On a tree of 68,000 files, the start takes 185 s, then the server exits on
`watch .: "…/persist-take.ts": too many open files in system`. The
descriptors are counted against `kern.maxfiles`, so other processes run short too.

Since 007 a page only reacts to its markdown, its resources, its sidecar and its kept revisions:
the rest of the tree is watched for nothing.

Before this change, a directory argument with a trailing slash fails: `grip-live-diff docs/` serves
`docs/` and opens the URL `/docs`, that is `docs/docs`: a 404.

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
- At most 10 directories per socket, the page's first. For the server, at most 100 watched
  directories, counting those in their 30 s grace, and 1,000 missing ones, polled by the 2 s check;
  a new directory beyond its cap is not followed and is logged once. A page may still join a
  directory already followed. A missing directory that appears while 100 are watched stays polled,
  and is watched once a slot frees.
- Subscriptions are counted per directory: pages open in several directories keep all of them
  watched. A directory is added on its first subscriber and removed 30 s after its last one left, so
  a reload does not reopen it.
- A check every 2 s reconciles the followed directories with the disk:
  - one that was missing and now exists is added, and the files already in it are broadcast, so its
    resources reload the page;
  - one that is watched but gone, or replaced by another directory (removed, renamed, or a parent
    renamed), loses its watch and goes back to missing.
- A directory that cannot be watched is logged once and the page does not reload; the server keeps
  running. Running out of descriptors is logged once too, and is not final: the 2 s check tries
  again.
- The server notices a closed socket at once and drops its subscriptions; the other pages get
  nothing. The server pings every 30 s and drops a connection that has not answered for 60 s.
- A directory argument `docs` serves the parent of `docs/`, as before, and opens `docs/README.md`,
  or the listing of `docs/` when there is none. Links leaving `docs/` (`../assets/a.png`) are
  served. `docs` and `docs/` behave the same; a missing `docs/` is an error at start.

## Out of scope

- Ignore rules (`.gitignore`, defaults, `--ignore`): nothing is walked, nothing needs ignoring.
- The directory listing: `http.FileServer` reads one directory per request.
- A watched directory that holds many files still costs one descriptor per file on macOS.
- Watching the nearest existing parent of a missing resource directory: the 2 s check covers it.
- Reacting to a directory created or removed faster than the 2 s check.
- Windows: two spellings of one directory (`/img`, `/Img`) are two subscriptions on one watch;
  releasing one stops the other's reloads until the page reconnects.
- Symbolic links. Before this change, the walk does not follow them: a root that is a link watches nothing, a
  linked subdirectory is skipped. After this change Linux follows them; on macOS kqueue reports the
  events under the target path, which no page names, so nothing reloads, as before.
- Binding to all interfaces, as before.

## Acceptance criteria

- [x] Given a tree of 68,000 files, the server listens in under 1 s (manual run).
- [x] Given the server started and no page open, no directory is watched.
- [x] Given `doc.md` open, writing a file in `sub/` (not a resource) sends nothing, and `sub/` is not
      watched.
- [x] Given `sub/doc.md` open, writing it reloads it.
- [x] Given `doc.md` open with `img/a.png`, writing `img/a.png` reloads it.
- [x] Given `docs/doc.md` open with `/assets/img/a.png` and no `assets/`, creating
      `assets/img/a.png` reloads the page within 3 s.
- [x] Given `doc.md` open with `img/a.png` and no `img/`, creating `img/a.png` reloads the page
      within 3 s.
- [x] Given `doc.md` open with `img/a.png`, removing then recreating `img/` and writing `img/a.png`
      reloads the page within 3 s.
- [x] Given `docs/doc.md` open with `/assets/img/a.png`, renaming `assets/` then recreating
      `assets/img/a.png` reloads the page within 3 s, and writes in the renamed directory send
      nothing.
- [x] Given two pages in one directory, closing one keeps the directory watched; closing both keeps
      it watched for 30 s, then releases it.
- [x] Given two sockets on one page, closing one sends nothing on the other.
- [x] Given a connection that stops answering pings, its subscriptions are dropped.
- [x] Given `page` not an existing markdown file, the handshake is refused; given a foreign
      `Origin`, it is refused the same way whether the page exists or not.
- [x] Given `res` values outside the root or holding `\`, nothing outside the root is watched.
- [x] Given 20 resource directories, the page's and 9 of them are watched; given 100 watched
      directories, a page in a new existing directory watches nothing new and still renders, and a
      page joining a watched directory follows it.
- [x] Given 100 sockets naming only missing directories, a page in an existing directory still
      reloads; past 1,000 missing directories, a new one is not followed.
- [x] Given 100 watched directories, a missing directory that appears stays polled, then is watched
      once a watched one is released.
- [x] Given an unwatchable directory, the page is served and the server keeps running.
- [x] Subscribing happens before the hash check, for every page on a directory, not only the first.
- [x] `grip-live-diff docs` and `grip-live-diff docs/` serve the parent of `docs/` and open
      `docs/README.md`, or the listing of `docs/` when there is none; `../assets/a.png` from
      `docs/README.md` is served; a missing `docs/` exits with an error.
- [x] The criteria of 007 still hold.
- [x] `go test ./...` green, `GOOS=windows go build ./...` green.

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
- Caps of 10 directories per socket, 100 watched and 1,000 missing, and the `page` check: the
  server listens on every interface, and without them any client could have the whole tree watched
  again.
- Missing directories counted apart from watched ones (PR review): a missing directory costs a
  `stat` per check and no descriptor. Counted in the 100, the missing directories named by one client
  would stop the other pages from reloading.
- A directory argument serves its parent (PR review): the links leaving the directory (`../`) stay
  served. Nothing is watched at start, so the larger root costs no watch. The whole parent is
  exposed on the network, as before.
- No subscription for `.annotations.d`: its writes always come with a sidecar write in the page's
  directory.
