# 006 — Annotated revisions

**Status**: shipped
**Requested by**: babs, from a thread on spec 005
**Date**: 2026-09-07

Extends [005](005-annotation-threads.md) and [004](004-diff-reference-picker.md).

## Problem

An agent asked to "handle the comments" often rewrites the document wholesale. Entries it did not
re-anchor become orphans: the sidecar still holds the quoted text and 32 characters on each side,
nothing more. The version the reader commented on is gone, it was never committed, and neither the
reader nor the agent can reread the passage in its context or see what changed since the comment.

## Solution

The preview keeps a copy of every version of the file that has annotations against it, next to the
sidecar: `<name>.annotations.d/<hash>.md`, one per version, written when the first comment on that
version is saved and removed when the last entry referring to it is gone. The sidecar lists them under
`revisions`, keyed by `file_hash`, so the agent knows where to look for the context of an orphan. The
diff picker gains one reference per kept revision, "annotated · 12:31", so the reader sees what the
document became since they commented; an orphan's "text not found" note links straight to that diff,
where the lost passage shows struck through.

## Scope

- Directory `<name>.annotations.d/` beside the sidecar, files `<12 hex of the sha256>.md`, the
  markdown as served when the comment was created. Written by `PUT /__annotations` when an entry's
  `file_hash` is the current file's hash and no revision exists for it; never rewritten. `0644` /
  `0755`, under the sidecar's exclusive lock.
- Sidecar `revisions: { "sha256:…": "<name>.annotations.d/<hash>.md" }`, server-owned: values sent
  by the browser are ignored, the map is rebuilt from disk at every save. Header line telling the
  agent what it is and when to read it (an orphan, nothing else).
- Reaping at every save: a revision no entry references is deleted; the directory is removed when
  empty; the sidecar going away takes the directory with it.
- An entry whose `file_hash` is neither the current file nor a kept revision (created by a version 1
  server, or the copy was deleted by hand) simply has no revision; nothing else changes.
- Watcher: changes under `*.annotations.d/` are classified like the sidecar (`annotations`), so
  writing a revision during a save does not reload the page. The classifier receives the path
  relative to the served directory, not the base name.
- `PUT /__annotations` with a path inside a `.annotations.d/` directory is refused with 400: a
  revision is not annotatable. `GET` on such a path renders it as any markdown file, read-only view.
- Diff picker: one reference per kept revision after `HEAD`, labelled "annotated · HH:MM" (the
  copy's modification time, which is when the first entry on that version was saved), selected
  through `?diff=<hash>`; unknown hash falls back to
  *since open* like `head` does without git. Kept in `localStorage` like the others; no key binding.
- Box and list: the "text not found in the current version" note becomes a link to
  `?diff=<hash>` when a revision exists for the entry. In that diff the passage sits in a `del`,
  which anchoring skips: no mark, the strike-through is the mark.
- README: the directory, the `revisions` key, the picker row.

## Out of scope

- Revisions for entries created before this ships (their version is gone).
- A cap on the number of revisions: it equals the number of distinct versions with open entries,
  which the reader controls by deleting entries.
- Storing revisions inside the sidecar (`revisions: {sha: content}`): the agent reads the whole
  sidecar on every pass and a markdown escaped in a JSON string does not diff.
- Restoring a revision over the file.
- Marks drawn inside diff deletions.

## Acceptance criteria

- [x] Given a file with no sidecar, saving a first annotation creates `<name>.annotations.d/<hash>.md`
      equal to the file, and the sidecar carries `revisions` with that hash and path.
- [x] Given two entries against the same version, one revision exists; given entries against two
      versions (the file changed between two saves), two revisions exist.
- [x] Given the browser sends `revisions` with a forged path, the stored map is the one rebuilt from
      disk.
- [x] Given the last entry referring to a revision is deleted, the revision file is gone; given the
      last entry of the sidecar is deleted, the sidecar and the directory are gone.
- [x] Given a version 1 entry whose hash has no copy, `GET` returns it without a revision and no error.
- [x] Given a save that writes a revision while the page is open, the page refreshes its marks and does
      not reload.
- [x] `PUT /__annotations?path=/doc.annotations.d/abc.md` is refused with 400.
- [x] Given one kept revision, the picker shows a fourth reference "annotated · HH:MM"; clicking it
      loads `?diff=<hash>` with the picker marking it active; the diff shows the changes since that
      version; `?diff=<unknown>` shows *since open*.
- [x] Given an orphaned entry with a revision, its note in the box and in the list links to
      `?diff=<hash>`; in that diff the passage is struck through and the entry stays orphaned (no mark).
- [x] `go test ./...` green; revision write, reaping, map rebuild, refusal and classifier pinned by
      Go tests.

## Phases

### Phase 1 — Keep and reap
- Work: `internal/annotations.go`: `Revisions map[string]string` on the sidecar; after the merge,
  write the current content for entries carrying the current hash when missing, delete unreferenced
  files, remove the empty directory; header line. `server.go`: classifier gets the relative path,
  `.annotations.d/` classified as `annotations`; PUT inside it refused.
- **Data model impact**: `revisions` key in the sidecar; new directory beside it.
- **DoD**: `go test ./internal/ -run 'Annotations|Reload'` green on the criteria above.

### Phase 2 — Show
- Work: `?diff=<hash>` resolved to the revision file; picker row per revision (template gets the list
  with time labels); `annotate.js` turns the orphan note into a link. README.
- **Data model impact**: none.
- **DoD**: browser check: annotate, rewrite the paragraph so the entry orphans, the picker offers the
  revision, the note links to it, the diff shows the passage struck through.

## Data model impact (summary)

Sidecar gains a server-owned map:

```json5
{
  "version": 2,
  "file": "README.md",
  "revisions": {
    "sha256:3b2f…": "README.annotations.d/3b2f0c1d9e8a.md"
  },
  "annotations": [ … ]
}
```

Beside it, `README.annotations.d/3b2f0c1d9e8a.md`: the file as it was. Same posture as the sidecar
(local preview, same-origin, no auth); the copy holds nothing the file did not.

## Open questions

- [x] None.

## Decisions

- Files in a directory rather than content in the sidecar — asked, chosen: the agent reads the whole
  sidecar every pass; a copy per version costs it nothing on disk, thousands of tokens inline.
- Revision written only when `file_hash` is the current content — the server has no other version
  in hand; a hash it cannot back is left without a copy rather than guessed from the in-memory diff
  snapshots, which are per process and unbounded in age.
- `revisions` server-owned and rebuilt from disk — the browser has no business naming files; a
  forged path could point outside the directory.
- Picker row per revision rather than one "annotated" reference — two versions with open entries is
  the normal case after a rewrite round; a single row would have to pick one.
- Picker rows listed from the directory, labelled with the copy's mtime — a sidecar read at render
  time would wait on an agent holding the lock (003 renders with a stat for the same reason); the
  copy is written with the first entry, so the two times are the same.
- No mark in `del` — anchoring skips deletions on purpose (003); the strike-through already says
  "this is what you commented on".
