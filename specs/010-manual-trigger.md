# 010 — Manual trigger and settings

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-26

## Problem

An agent monitoring the sidecar acts on every save. The reader writes five comments in a row; the
agent starts on the first one, rewrites the markdown, the page reloads, and the reader is still
typing the second. The edits come one by one, each one a reload, and the agent works without the
rest of the batch in view.

Width and theme are kept in `localStorage`, which is per origin: each instance on its own port
starts back at the defaults.

## Solution

Each document gets a trigger mode, **auto** (today's behaviour, the default) or **manual**. In
manual, the reader writes comments without the agent reacting, then clicks **Go**. The preview
drops a signal file next to the sidecar; the agent, told so by the sidecar header, handles every
open entry in one pass and removes the signal file when done. The page shows the batch as sent
until then.

A settings menu holds width, theme and the new "hold reload while sent". Settings are saved in one
file under the user's config directory, shared by every instance whatever its port.

## Scope

### Settings

- `settings.json` in `<os.UserConfigDir>/grip-live-diff/` (`~/.config/grip-live-diff/` on Linux).
  Keys: `width` (`normal` | `wide` | `full`), `theme` (`system` | `light` | `dark`), `hold_reload`
  (bool). Missing file or key: `normal`, `system`, `false`.
- The server reads the file on each page render and puts the values in the page, so the first paint
  is already at the right width and theme: no fetch, no flash.
- `PUT /__settings` saves the whole set: same-origin check, size cap, values validated against the
  enums, unknown keys dropped, written to a temp file then renamed. Last writer wins.
- A gear button in the toolbar opens the menu. The width and theme buttons leave the toolbar.
- A change applies at once to the page that made it. Other open pages pick it up on their next load.
- A save that fails (read-only config dir) keeps the change on the page and logs on the server.

### Manual trigger

- The sidecar gains a top-level `trigger: "auto" | "manual"`. Absent means `auto`. The reader sets
  it; the agent reads it and never writes it.
- A toolbar button next to the annotation arrows switches the mode of the current document and
  saves it to the sidecar. In manual mode a **Go** button shows next to it.
- **Go** writes `<name>.annotations.ready` beside the sidecar, JSON5
  `{ sent_at: <RFC 3339>, pid: null }`. Disabled when no entry is open (last message the reader's).
- The agent claims the batch: under the sidecar flock, if `pid` is null it writes its own PID and
  `claimed_at`, otherwise another agent holds it and it leaves the batch alone. The PID is the one of
  the agent's long-lived process, not of the shell it runs a command in (in Claude Code, the shell's
  `$PPID`).
- The page shows the state of the ready file: "sent" (no `pid`), "agent working · pid N" (`pid` set
  and alive), "agent gone · pid N" (`pid` set, no such process). The server checks liveness when it
  answers the GET, on its own host.
- While the batch is sent or worked on, clicking the button offers to cancel: cancel removes the
  file. "Agent gone" offers Go again, which rewrites the file with `pid: null`. The state follows the
  disk: the agent removing the file turns it back into Go.
- Switching back to auto removes a pending ready file: in auto the agent acts anyway.
- The sidecar header gains a paragraph: when `trigger` is `"manual"`, do not handle entries on a
  sidecar change; wait for the ready file, claim it with your PID, handle every open entry, then
  delete the ready file last.
- The page refreshes its annotation state when the ready file appears or goes, as it does for the
  sidecar (same `annotations` message, no reload).
- Setting `hold_reload`: when on and the ready file exists, a reload due to the markdown waits until
  the file is gone, then runs once. Uses the hold of [008](008-reload-held-while-annotating.md).

### Docs

- README: settings menu and file, manual mode, ready file, toolbar table updated.

## Out of scope

- Moving minimap visibility, annotation capture and the last diff reference out of `localStorage`.
- Importing the width and theme already in `localStorage`: they fall back to the defaults once.
- Pushing a settings change live to the other open pages.
- A CLI flag or env var for the config path.
- A global trigger mode (CLI flag, per directory): the mode is per document.
- Liveness of an agent in another PID namespace (container, remote host): its PID means nothing
  to the server and reads as gone; cancel covers it.
- Taking a batch back from a live agent: cancel removes the file, the agent is not told.
- Telling the agent which entries make up the batch: every open entry at the time it reads the
  sidecar is the batch.

## Acceptance criteria

- [x] **AC1** Given a sidecar without `trigger`, the API reports `auto` and the toolbar shows auto.
- [x] **AC2** Given auto, when the reader switches to manual, then the sidecar holds
      `trigger: "manual"` and the entries are unchanged.
- [x] **AC3** Given manual, a save of entries keeps `trigger`; a PUT without `trigger` keeps the
      stored value.
- [x] **AC4** Given manual and an open entry, when the reader clicks Go, then
      `<name>.annotations.ready` exists with `sent_at` an RFC 3339 time and `pid: null`.
- [x] **AC5** Given manual and no open entry, Go is disabled and the API refuses the request.
- [x] **AC6** Given auto, the API refuses to write the ready file.
- [x] **AC7** Given the ready file, a second Go writes nothing; cancel removes the file.
- [x] **AC8** Given the ready file, when it is removed on disk, then the page shows Go again within
      the reload latency, without reloading (headless run).
- [x] **AC9** Given the ready file, switching to auto removes it.
- [x] **AC10** The sidecar header states the manual rule: wait for the ready file, claim it with
      the PID of the long-lived agent process under the flock, leave it if already claimed, handle
      every open entry, delete the ready file last.
- [x] **AC11** Given `hold_reload` on and the ready file present, writing the markdown does not
      reload the page; removing the ready file reloads it once. Given it off, the markdown write
      reloads at once (headless run).
- [x] **AC12** The ready file of a kept revision (`.annotations.d/`) or outside the served tree is
      refused, as for the sidecar.
- [x] **AC13** `go test ./...` green, `GOOS=windows go build ./...` green.
- [x] **AC14** Given no settings file, a page renders at `normal` width with the browser's theme.
- [x] **AC15** Given width `wide` saved from an instance on port A, a page from an instance on port
      B renders wide on first paint.
- [x] **AC16** `PUT /__settings` refuses a foreign `Origin`, an unknown enum value and a body over
      the cap; unknown keys are not written.
- [x] **AC17** Given a read-only config dir, changing width applies to the page and the server keeps
      running.
- [x] **AC18** The toolbar has no width or theme button; the gear menu sets both.
- [x] **AC19** Given a ready file with `pid` of a live process, GET reports it working; with the PID
      of an exited process, gone; with `pid: null`, sent.
- [x] **AC20** Given "agent gone", Go rewrites the ready file with `pid: null` and a new `sent_at`.
- [x] **AC21** Given a ready file the browser cannot parse (an agent wrote garbage), GET reports it
      sent and the page still renders.

## Phases

### Phase 1 — Settings
- Work: settings file read/write, values in the rendered page, `PUT /__settings`, gear menu, width
  and theme moved into it, `localStorage` dropped for both.
- **Covers**: AC14, AC15, AC16, AC17, AC18
- **Data model impact**: new file `<UserConfigDir>/grip-live-diff/settings.json`.
- **DoD**: `go test ./...` green (table tests: defaults, validation, origin, cap, atomic write);
  headless run on two ports showing AC15.

### Phase 2 — Mode and ready file, server side
- Work: `trigger` in the sidecar struct, kept across PUTs; header paragraph; `POST` (write) and
  `DELETE` (cancel) on `/__annotations/ready?path=`, same origin and path checks; the annotations GET reports
  the ready state (sent / working / gone, liveness by signal 0 on Unix, `OpenProcess` on Windows);
  switching to auto removes the file; `pageMessage` routes the ready file to `annotations`.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC9, AC10, AC12, AC13, AC19, AC20, AC21
- **Data model impact**: sidecar gains `trigger`; new file `<name>.annotations.ready`.
- **DoD**: `go test ./...` green with table tests for each AC above; `GOOS=windows go build ./...`.

### Phase 3 — Trigger UI and hold
- Work: mode button, Go / sent / working / gone / cancel, refresh on the `annotations` message,
  `hold_reload` in the menu and the hold itself. README.
- **Covers**: AC8, AC11
- **Data model impact**: none
- **DoD**: headless run on a test port: switch mode, Go, remove the ready file by hand, see Go come
  back; hold on and off against a markdown write.

## Data model impact (summary)

- Sidecar: optional top-level `trigger`. Version stays 2: a reader without it reads `auto`.
- New file `<name>.annotations.ready`, created by the preview, removed by the agent or the preview.
- New file `<UserConfigDir>/grip-live-diff/settings.json`.

## Open questions

- None.

## Decisions

- Mode in the sidecar, not in the settings: it is per document, and the agent already reads that
  file.
- Suffix `.ready`, not `.go`: a `.go` file breaks `go build` in a Go repository.
- The agent deletes the ready file at the end, not the start: the page can show when the batch is
  done.
- Go refused with no open entry: an empty batch would wake the agent for nothing.
- PID claim instead of a timeout: the page tells a dead agent from a slow one, no clock to tune.
- Claim under the sidecar flock rather than a lock of its own: one lock for the agent to know.
- An unparsable ready file reads as sent: the file is the agent's to write too.
- A ready file in auto is stale: every auto save and every mode switch drops it, best effort.
  A failed removal is logged and does not fail the save, which already stands.
- Settings on the server, not in `localStorage`: `localStorage` is per port.
- Settings injected at render rather than fetched: no flash of the wrong theme or width.
- `theme: system` as default: today an unset theme follows the browser.
- No import of the old `localStorage` values: two clicks to set them again.
- Any client that can reach the server can change the settings: it listens on every interface, as
  for the annotations. The values are three enums, nothing executes from them.
