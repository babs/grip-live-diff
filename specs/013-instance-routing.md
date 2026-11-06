# 013 — Instance registry and message routing

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-04

Supersedes the claim of [010](010-manual-trigger.md): the agent claims with `pending --claim`.

## Problem

Two instances can serve the same markdown file: `grip-live-diff -p 6419 docs/` for one agent
session, `grip-live-diff -p 6420 docs/specs/` for another. Both read and write the one sidecar of
the file, `toto.annotations.json5`. Nothing in the sidecar tells which instance a reader message
came from.

Each agent arms a watch on the sidecars of its tree. A comment written in the page of 6420 wakes
the agent of 6419 too: both may answer the same thread. In manual mode the first agent to claim the
ready file takes the batch, whichever instance sent it.

The reader does not know that the page is open in another instance either.

## Solution

Each instance keeps one file in a registry shared by all the instances of the user: its port, its
PID, the tree it was started on and the pages open in it. An instance reads the other files to know
which of its pages are open elsewhere.

The server stamps each new reader message with `via`, the port of the instance whose agent must
answer. When the page is open in another instance too, the page says so and lets the reader choose
the instance. `grip-live-diff pending` tells an agent which open entries are its own: the rule is
computed in one place, by gld, not by each agent's script.

## Scope

### Instance identity

- An instance is its port and its PID. The start line gains the PID:
  `🚀 Starting server: http://localhost:6419/ (pid 1234)`.
- `root` is the argument the instance was started on, resolved (absolute, symlinks resolved): the
  directory, or the directory of the file. Not the served directory: `serveTarget` serves a
  directory from its parent, and the agent watches the argument's tree only. The root of an
  instance contains a file when the sidecar key of the file (Registry) is under it.
- At start: bind the port (`net.Listen`), write the registry file, then print the start line. A
  port already taken stops the start with no line and no file; once the line is printed, the
  agent finds the file.

### Registry

- Directory `<os.UserCacheDir>/grip-live-diff/instances/` (`~/Library/Caches/...` on macOS,
  `~/.cache/...` on Linux). One file per instance, `<pid>.json`:
  `{ pid, port, root, seen_at, pages: [<sidecar key>] }`. `seen_at` is RFC 3339.
- The sidecar key of a page: the directory of the markdown file resolved, joined with the base name
  of the file as served. From a sidecar: its directory resolved, joined with its `file`. Two pages
  share a key exactly when they share a sidecar: a symlinked directory is followed, a symlinked file
  is not, as `sidecarPath` does not follow it.
- An instance writes only its own file, to a temp file then renamed. It reads the files of the
  others, never locks them. The directory and the files are private to the user (0700, 0600).
- Every 15 s the instance rewrites its file (`seen_at`) and reads the others. It rewrites at once
  when its page list changes.
- A page is listed while a socket of the instance follows it, and 30 s after the last one closed,
  so a reload does not drop it. The reloader counts sockets per page for this, beside its count per
  directory. With `--no-reload`, no socket opens: the file is written with `pages: []`.
- An entry is live when its PID is alive (signal 0 on Unix, `OpenProcess` on Windows, as for the
  ready file) and its `seen_at` is under 60 s old.
- At start, and at each read, the instance removes the files whose PID is dead. A file with a live
  PID and an old `seen_at` (process stopped, host asleep) is skipped, not removed. Two instances
  removing the same file: "not found" is fine.
- On SIGINT or SIGTERM, the instance removes its file, then exits.
- A registry that cannot be written (read-only cache dir): logged once, the server runs, routing is
  off: no `via` written, no banner.
- A heartbeat that fails while the server runs: routing is off once the file of the instance is
  `registryLive` (60 s) old, as the others and `pending` no longer see it; on again at the next
  heartbeat written.

### Detection

- A page is shared when another live instance lists the same sidecar key.
- Terminal: the first time a page of the instance is shared with a given other instance, it prints
  `⚠ <path relative to root> also open in :<port> (pid <pid>)`.
- `GET /__annotations` gains `self` (the port of the instance) and `targets`: the instance itself
  if its root contains the file, listed or not; then the other live instances whose root contains
  the file and that list the page, sorted. `live` lists the ports of every live instance, so the
  page can tell a gone `via`. The PUT and the ready POST and DELETE answer with the same fields.
- The server sends `annotations` to the sockets of a page when its `targets` or `live` changes at a
  registry read. The page redraws the banner on each fetch, `poll` included.
- The page shows a banner above the document when `targets` holds a port other than `self`:
  "Also open in :6419. Send to: [:6420 (this page) ▾]". The selector lists `targets`. The choice
  is kept in `sessionStorage` per path, so a reload keeps it; a stored port no longer in `targets`
  falls back to the first of `targets`. The banner goes when no other port is left.
- Each reader message whose `via` is not `self` shows "→ :<via>"; "→ :<via> (gone)" when `via` is
  not in `live`.

### Routing

- The owner of an open entry, from the `via` of its last reader message:
  - `via` names a live instance whose root contains the file: that instance;
  - else (absent, gone, or a port taken again by an instance on another tree): the live instance of
    lowest port whose root contains the file;
  - else none.
- A reader message gains `via`, an integer port. On each new reader message (`mergeThread`,
  appended) the server sets it from the port the PUT body names in `via`: that port if it is in
  `targets`; else the first of `targets`; else the owner rule's instance; else no `via`.
- On a PUT, an open entry whose owner is not the instance its `via` names (gone `via`) gets the
  port of its owner as `via` on its last reader message, not the port the PUT names: an agent may
  already handle it. An entry without `via` is left without: the owner rule gives it the lowest
  port already. The save rewrites the sidecar, which wakes the Monitors.
- An edit of an earlier reader message keeps its `via`. A `via` the browser puts on a message in the
  list is ignored, as `status` is.
- The PUT answers with the sidecar as stored: the page shows the `via` written, not the one asked.
- Go (`POST /__annotations/ready`) takes the chosen port with the same check; the ready file and
  `readyState` gain `via`, and the page shows "Sent → :<via>". Go is refused:
  - when no open entry is owned by that port: "nothing open for :<via>";
  - when a batch is sent or worked on and its owner (the owner rule on its `via`) is another port:
    "batch pending for :<via>".
  When its owner is the same port, it writes nothing, as today; if its `via` is gone, it rewrites
  `via` to that port, so the Monitors wake. `readyFile` keeps `via` across an agent rewrite.
- `grip-live-diff pending --port <port> --pid <pid> <path>…`: `<path>` a sidecar, a markdown file,
  a ready file or a directory (its sidecars, recursively). For each sidecar:
  - trigger auto: one line `<sidecar>\t<entry id>` per open entry owned by the instance;
  - trigger manual: when a ready file stands with `pid` null and its owner is the instance, one line
    `<sidecar>\tready`, then the lines of the open entries owned by the instance.
  The owner of a ready file is the owner rule applied to its `via`. A sidecar that cannot be read
  is reported on stderr and skipped. Exit 0 when a line is printed, 1 when none, 2 when the
  instance is gone, a path does not exist or an argument is wrong: a Monitor that loops on 1 stops.
  A ready file stands for its sidecar. A missing sidecar or ready file whose directory stands is
  no entry yet: an auto sidecar goes with its last entry.
  The instance is
  `--port` and `--pid`:
  - its PID is dead: nothing is owned, stderr says the instance is gone, exit 2;
  - its PID is alive and the registry holds no file for it (registry off: the start writes the file
    before the start line): it owns every open entry, today's behaviour, and says so on stderr;
  - its file is there but not live (stopped, asleep), or names another port: nothing is owned.
  `pending` removes the dead files of the registry, as a start does.
- `grip-live-diff pending … --claim <agent pid> <sidecar>`: under the flock of the sidecar, reads
  the registry and checks again that the trigger is manual and the ready file stands, has `pid`
  null and is owned by the instance, then writes `pid` and `claimed_at` and prints the lines as `pending` does; exit 1 and
  nothing written otherwise. The agent does not take the flock around it.
- The sidecar header tells the agent: run `grip-live-diff pending --port <port> --pid <pid>` on the
  file, with the port and PID of the start line of the instance you started; handle only the entries
  it lists; in manual mode, when it lists `ready`, claim with `--claim <your PID>` and handle the
  entries that call lists, then delete the ready file.

### Docs

- Sidecar header: the paragraph above. No absolute path: the header is committed with the sidecar.
- README: the registry, the banner, `via`, `grip-live-diff pending` and its use in a Monitor script.

## Out of scope

- Instances of different versions on one file: a binary without `via` drops it when it saves the
  sidecar. Every instance runs the same version (`grip-live-diff update`).
- A registry on a shared home (NFS) or instances in another PID namespace (container): a foreign PID
  reads as dead.
- Routing a change written to the sidecar outside the server (editor, other tool): no `via`, the
  owner is the lowest live port.
- A thread owner: each message is routed on its own; a thread may pass from one session to another.
- One ready file per instance: a second batch waits for the first.
- The trigger mode per instance: it stays per document, shared by the instances.
- Pages of an instance with `--no-reload`: it is not seen as having the page open.

## Acceptance criteria

- [x] **AC1** Given an instance started on `docs/specs/`, its file `<pid>.json` exists with its
      port, PID and `root` = `docs/specs` resolved before the start line, which shows the PID;
      `seen_at` is rewritten within 15 s. Given `--no-reload`, the file exists with `pages: []`.
- [x] **AC2** Given a port already taken, the instance stops with no start line and no registry
      file.
- [x] **AC3** Given a page opened, the file lists its sidecar key at once; 30 s after its last
      socket closed, it does not; a reload in between keeps it listed.
- [x] **AC4** Given a registry file whose PID is dead, a start, a read or `pending` removes it;
      given a live PID and an old `seen_at`, no read reports it and none removes it.
- [x] **AC5** Given SIGINT or SIGTERM, the instance removes its file.
- [x] **AC6** Given a read-only registry dir, the server serves pages, logs once, the annotations
      API reports no other target and writes no `via`; `pending` for that instance lists every open
      entry. Given a heartbeat that fails for 60 s while the server runs, routing is off until a
      heartbeat is written again.
- [x] **AC7** Given the same file open in A and B, B serving a parent dir through a symlinked
      directory, A reports `targets` = [A, B] and `self` = A, and prints the warning line once.
      Given a symlinked file, the two pages do not share a key, and A started on the directory of
      the link contains it. Given B whose root does not contain the file, B is not in `targets`.
      Given A with `--no-reload`, A is first of its own `targets`.
- [x] **AC8** Given a page open only in A, whose root contains it, a new reader message through A
      gets `via` = A. Given a file outside the root of A (served through `../`) and B live whose
      root contains it, page open in B or not, `via` = B; with no such instance, no `via`. Given C
      on another tree that took the port of a dead `via`, the owner is not C.
- [x] **AC9** Given `targets` = [A, B], a PUT naming B writes `via` = B; naming a port not in
      `targets` writes `via` = A; a `via` set on a message of the list is ignored; the PUT answer
      holds `self`, `targets` and `live`.
- [x] **AC10** Given an edit of an earlier reader message, its `via` is unchanged. Given an open
      entry whose `via` is gone and whose owner is A, a PUT through B naming B re-stamps its last
      reader message with `via` = A.
- [x] **AC11** Given manual mode, Go naming B writes the ready file with `via` = B and the GET
      reports it; a Go naming A while it stands is refused with the pending port; a Go naming B
      again writes nothing; a Go naming a port that owns no open entry is refused; an agent rewrite
      with `pid` set keeps `via`. Given a ready file whose `via` is gone and whose owner is A, a Go
      naming A rewrites `via` = A.
- [x] **AC12** `pending`: given entries routed to A, to B, gone, and absent, `--port A` lists A's
      and those whose owner is A by the lowest-port rule, exit 0; `--port B` lists only B's;
      given a dead `--pid`, nothing, exit 2; given a missing path or a wrong flag, exit 2; given a
      manual sidecar, `ready` and its entries only
      when the ready file is A's and unclaimed; given A's file with an old `seen_at`, nothing; given
      no file and a live PID, every open entry. `--claim` writes `pid` only when the ready file is
      still A's and unclaimed under the flock: given it replaced by a ready file of B between
      `pending` and `--claim`, exit 1 and nothing written.
- [x] **AC13** The sidecar header names `grip-live-diff pending`, the start line as the source of
      port and PID, the ready file rule, and holds no absolute path.
- [x] **AC14** Given B starts with the page open in A, the banner appears in A's page without a
      reload within 30 s; the selector sends the next message to B, the message shows "→ :B", and
      the choice survives a reload; when B stops, the banner goes within 45 s and the message shows
      "(gone)" once B's file is removed (headless run).
- [x] **AC15** `go test ./...` green, `GOOS=windows go build ./...` green.

## Phases

### Phase 1 — Identity, registry and detection, server side
- Work: bind before the start line, PID in it, `root`, registry write and read loop, page count per
  socket with its grace, purge, removal on signal, `self`, `targets` and `live` in the GET, warning
  line, `annotations` on a change of `targets`.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6 (server part), AC7
- **Data model impact**: new directory `<UserCacheDir>/grip-live-diff/instances/`.
- **DoD**: `go test ./...` green with table tests; `GOOS=windows go build ./...`.

### Phase 2 — Routing and `pending`
- Work: owner rule, `via` on messages and in the ready file, the PUT and Go fields and their
  checks, the re-stamp of a gone `via`, `grip-live-diff pending`, header paragraph.
- **Covers**: AC6 (`pending`), AC8, AC9, AC10, AC11, AC12, AC13
- **Data model impact**: reader message gains `via`; ready file gains `via`.
- **DoD**: `go test ./...` green with table tests for each AC above.

### Phase 3 — Banner and selector
- Work: banner, selector, "→ :port" and "(gone)" on messages, "Sent → :port", the Go refusals.
  README.
- **Covers**: AC14, AC15
- **Data model impact**: none
- **DoD**: headless run on two ports (`scripts/e2e`).

## Data model impact (summary)

- Reader message: optional `via` (integer). Version stays 2: a message without it is unrouted.
- Ready file: optional `via`.
- New directory `<UserCacheDir>/grip-live-diff/instances/`, one `<pid>.json` per instance.

## Open questions

- None.

## Decisions

- Registry out of the served tree: no file created beside each open page, nothing in git, no
  sidecar write per heartbeat, no watch woken.
- One file per instance, not one shared file: no lock, no write conflict.
- No host in the registry: the user cache dir is local; a hostname changes with the network on
  macOS.
- `root` is the start argument: a `via` to an instance whose agent does not watch the file would
  leave the message unhandled.
- `via` stamped by the server: the browser cannot route to an instance that does not have the page.
- Port in `via`, port and PID for `pending`: the port names the instance in the banner; the PID
  keeps an agent whose instance died, or whose port was taken again, from taking messages.
- Choice in the page, not a question from the agent: one decision, where the reader writes; no
  question left open in a second session.
- A gone `via` goes to the live instance of lowest port: one agent answers, not every one. A save
  re-stamps it, so a message routed to a dead instance is not stuck.
- `grip-live-diff pending`: the owner rule, path resolution and liveness live in gld; a Monitor
  script calls it instead of parsing JSON5 and the registry. `--claim` re-checks the owner under the
  flock: the ready file can change between the two calls.
- A re-stamp uses the owner, not the port of the PUT: the owner's agent may already work on it.
- Remove a file on a dead PID only: a stopped or sleeping instance keeps its file and is listed
  again at its next heartbeat.
- 30 s grace for a closed page, the `releaseAfter` of 009: a reload must not flap the banner. The
  grace a comment needs after its tab closes is not one: `via` is stamped when the comment is saved.
