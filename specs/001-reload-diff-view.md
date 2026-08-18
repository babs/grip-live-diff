# 001 — Highlight changes since the initially loaded version

**Status**: shipped
**Requested by**: Damien Degois
**Date**: 2026-08-01

## Problem

`go-grip` auto-reloads the browser whenever the watched `.md` file changes on disk. After a reload the
page simply looks different — the reader has to hunt for what actually moved. When proofreading a long
document, or when an editor/AI rewrites a paragraph in place, a full re-read is the only way to know
what changed. There is no way to see the delta between what was on screen and what is now on disk.

## Solution

A toggle button in the preview: **"Show changes"**. When enabled, the document is still rendered
normally (headings, code, tables, mermaid) but every passage that differs from the reference version is
highlighted inline — insertions in green, deletions struck through in red — down to the word level
inside a modified line, not just whole-line granularity.

Two references are available, switchable from the same control:

- **since open** — the version served when the file was first opened (cumulative);
- **last edit** — the content as it was just before the most recent change (what just moved);
- **last commit** — the file as committed in `git HEAD`, when it is served from a work tree.

The button carries a marker when the file on disk differs from the initial version, so a reload makes
it obvious that *something* changed even before opening the diff.

A second action, **"Mark as read"**, promotes the current disk content to the new reference, clearing
the highlights and starting a fresh comparison from here.

## Scope

- Two per-file reference snapshots held by the server, in memory for the lifetime of the process: the
  **baseline** (first render of that path) and the **previous** distinct content observed before the
  current one.
- A third reference read from git on demand: `git show HEAD:<file>`, offered only when the served
  directory is a git work tree (probed once at start-up).
- Diff mode reachable as `?diff=open` / `?diff=last` / `?diff=head` on the same URL, so the auto-reload
  (which re-requests the same URL) keeps the reader in diff mode across file changes.
- Inline word-level highlighting rendered *on the HTML output*, preserving the normal rendering.
- Reset action to re-baseline the current file.
- Visual state on the toggle when disk ≠ baseline.
- Light and dark theme styling, consistent with the existing theme switch.

## Out of scope

- Diff against an arbitrary revision, branch or the index — `HEAD` only.
- Full iteration history / ring buffer of the last N versions, timestamps, arbitrary
  version-to-version comparison. Exactly two references are kept.
- Persisting baselines across server restarts.
- Per-browser-tab or per-user baselines — go-grip is a single-user local preview tool; the baseline is
  process-global per file path.
- Side-by-side / split view.
- Navigation between changes (next/previous change), change counter.
- Diffing non-markdown files served by the file server.
- Highlighting *inside* mermaid diagrams, MathJax formulas, media elements — those are compared as a
  whole and flagged as changed, never annotated internally (annotating them would break their
  rendering).
- Attributing a formatting-only change to the text it wraps: dropping the emphasis from `**bold**`
  leaves the words identical, so the diff marks the surrounding words rather than the styling itself.
  Doing better means rebuilding the annotated document from a tree instead of a token stream.

## Acceptance criteria

- [ ] Given a `.md` file opened once and then modified on disk, when the page is loaded with
      `?diff=open`, then the changed passages are wrapped in `<ins>` / `<del>` elements and visibly
      highlighted.
- [ ] Given a line where only a few words changed, when diff mode is on, then only those words are
      highlighted — the untouched words of the same line are not.
- [ ] Given a file unchanged since it was first opened, when diff mode is on, then the rendering is
      identical to normal mode and a "no changes" indication is shown.
- [ ] Given a file changed on disk, when the page is loaded in normal mode, then the toggle button
      shows a changed-state marker.
- [ ] Given a file edited twice since it was opened, when `?diff=open` is loaded then both edits are
      highlighted, and when `?diff=last` is loaded then only the second edit is.
- [ ] Given a page reloaded (F5 or auto-reload) without any content change, then the "last edit"
      reference does not move — a reload alone never consumes an iteration.
- [ ] Given diff mode is on, when the file changes again on disk and the browser auto-reloads, then the
      page comes back in the same diff mode, `?diff=open` still comparing against the original
      baseline and `?diff=last` now against the content from just before that change.
- [ ] Given diff mode is on, when "Mark as read" is clicked, then the page returns to normal mode and a
      subsequent diff shows no changes.
- [ ] A document containing a mermaid block, a fenced code block and a MathJax formula still renders
      correctly in diff mode (diagram drawn, syntax colours intact, formula typeset).
- [ ] Deleted content appears in the diff even when a whole paragraph or list item was removed.
- [ ] Given a file served from a git work tree and edited without committing, when `?diff=head` is
      loaded, then the uncommitted changes are highlighted and "Mark as read" is not offered.
- [ ] Given a file that has never been committed, when `?diff=head` is loaded, then the page states it
      has no committed version instead of reporting the whole document as new.
- [ ] Given a directory that is not a git work tree, then the toggle never offers the commit state.
- [ ] `go test ./...` green; the diff engine is covered by table-driven tests including intra-line
      changes, block insertion, block deletion and the unchanged case.

## Phases

### Phase 1 — Diff engine (`internal/htmldiff.go`)

- Work: HTML-aware word diff. Tokenise both HTML strings into tag tokens, word tokens and atomic
  tokens (`<pre class="mermaid">…</pre>`, `<script>`, `<style>`, math spans). Map tokens to runes,
  diff with `github.com/sergi/go-diff/diffmatchpatch`, then rebuild the new HTML with contiguous
  changed word runs wrapped in `<ins class="gg-ins">` / `<del class="gg-del">`. Tags from the deleted
  side are dropped so the output stays well-formed; their text survives inside `<del>`.
- **Data model impact**: none.
- **DoD**: `go test ./internal/ -run Diff` green — table-driven cases covering: unchanged input returns
  the input byte-for-byte; a two-word edit inside a sentence highlights only those words; an added
  paragraph is fully `<ins>`; a removed paragraph appears as `<del>`; a mermaid block is never
  annotated internally.

### Phase 2 — Reference store + server wiring

- Work: concurrency-safe store on `Server`, `map[string]{baseline, prev, cur []byte}`. On every `.md`
  render: if the path is unknown, all three are set to the served content; else if the content differs
  from `cur`, `prev = cur` then `cur = content` — an unchanged re-request never shifts `prev`.
  `?diff=open` / `?diff=last` render the chosen reference and the current content through the parser
  and pass the annotated HTML to the template. `POST /__baseline?path=…` resets all three to the
  current content. Template gains `DiffMode`, `HasChanges`, `DiffEmpty` and the toggle labels.
- **Data model impact**: none (in-memory, process lifetime).
- **DoD**: `go test ./internal/` green with `httptest` cases — first GET stores the three snapshots; GET
  with `?diff=open` after an on-disk edit returns a body containing `gg-ins`; after two successive
  edits `?diff=last` highlights only the second while `?diff=open` highlights both; two GETs without
  any edit in between leave `prev` untouched; the same GET without `diff` returns no diff markup; the
  reset endpoint clears the changed state.

### Phase 3 — UI

- Work: toggle button next to the theme switch cycling off → `?diff=open` → `?diff=last` → off,
  changed-state marker, "Mark as read" button visible in diff mode, `ins`/`del` styling for light and
  dark themes, a discreet banner in diff mode naming the reference in use ("since open" / "last edit")
  and stating "no changes" when identical.
- **Data model impact**: none.
- **DoD**: manual check on a live `go-grip` — edit a file twice while the page is open, observe the
  marker, cycle through both diff references and confirm "since open" shows both edits and "last edit"
  only the last, confirm word-level highlighting in both themes, confirm the mode survives the
  auto-reload, confirm "Mark as read" clears it.

### Phase 4 — Git HEAD reference

- Work: `?diff=head` reads the reference with `git -C <served dir> show HEAD:./<path>`. The work tree
  is probed once when the handler is built and gates the whole state: outside a repository the toggle
  cycles straight back to off. An untracked file, or a repository without a commit, renders the plain
  document with a banner saying there is nothing to compare with. "Mark as read" is hidden — the git
  reference is not the server's to move.
- **Data model impact**: none.
- **DoD**: `go test ./internal/ -run 'Head|Git'` green — an uncommitted edit is highlighted against
  HEAD, an untracked file reports no reference and carries no diff markup, and a non-git directory
  never exposes the state.

## Data model impact (summary)

None. The baseline store is in-memory and dies with the process.

## Open questions

- [ ] None.

## Decisions

- **Diff mode is a URL query param, not client-side JS state** — the auto-reloader re-requests the same
  URL, so the mode survives file changes for free, and mermaid/MathJax re-initialise normally instead
  of needing re-triggering after a DOM swap.
- **Diff is computed on the rendered HTML, not on the markdown source** — required by the chosen
  "highlight on the rendered document" behaviour.
- **Deleted tags are dropped, deleted text is kept** — keeps the output well-formed at the cost of
  removed blocks being shown inline where they used to start.
- **Exactly two references, no iteration history** — "since open" and "last edit" cover the two real
  questions; a ring buffer would add a version selector, a memory cap and a purge policy for a case
  that has not come up.
- **An iteration is a *distinct content* observed on a request, not a request** — otherwise an F5 or a
  spurious watcher event would silently make "last edit" mean "nothing".
- **New dependency `github.com/sergi/go-diff`** — pure Go, no transitive dependencies; replaces ~200
  lines of hand-written LCS.
