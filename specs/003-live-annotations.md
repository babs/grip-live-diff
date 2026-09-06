# 003 — Live annotations

**Status**: shipped
**Requested by**: babs
**Date**: 2026-09-05

## Problem

When an agent is rewriting a document, the reader's feedback has nowhere to go. Reading the preview,
they spot a sentence to rework, a wrong fact, a paragraph to cut; then they have to switch to the chat,
describe where it is ("third paragraph under Usage, the sentence about ports") and what to do with it.
The location gets lost in translation, the agent guesses, and the reader re-reads the whole thing on
the next reload to check. The feedback loop that grip-live-diff is meant to shorten is broken exactly
at the point where it should be tightest.

## Solution

Inline comments in the preview, the way Confluence does them. With the annotation mode on, selecting
text offers to annotate it: a comment box opens, the reader types what they want changed, and the
passage stays marked with a yellow tint and a thick orange underline. A comment can also target the
whole document ("too long", "missing a section on X"). Annotations are saved next to the file, in
`<name>.annotations.json5`, so an agent can read them straight from disk: the quoted text, its position
in the markdown source, the comment, and the version of the file the comment was written against.

Marks are always drawn when the file has annotations; two buttons and keyboard shortcuts walk through
them; hovering a mark shows its comment, clicking it opens the comment for editing or deletion. The
file keeps being edited underneath: annotations follow their text across reloads, and when the agent
rewrites an annotated passage it either updates the quote, answers the comment, or deletes the entry.

The loop: read, annotate, then tell the agent "handle the comments in README.annotations.json". The
agent's replies show up in the same boxes on the next reload.

## Scope

- Marks and navigation are on whenever the sidecar has entries, no toggle needed to see feedback.
- A toggle button in the toolbar (pencil icon) turns **capture** on: selecting text inside the article
  shows an "Annotate" bubble next to the selection; the bubble or the `a` key opens the comment box for
  that selection. The choice is kept in `localStorage`, like the theme. Off, selecting text is just
  selecting text.
- Document-level annotation: a button in the navigation row (and the `A` key) opens a comment box with
  no selection; the entry has `exact: null`, no mark, and comes first in navigation.
- Marks drawn Confluence-style: pale yellow background tint plus a thick (3px) orange underline; the
  active one gets a deeper tint; an entry with `status: done` is grey; an entry re-anchored
  approximately has a dashed underline. Drawn with the CSS Custom Highlight API (`background-color`
  and `text-decoration` are both supported in `::highlight()`), so the rendered DOM is never touched:
  no conflict with diff `ins`/`del` wrapping, mermaid, MathJax or the minimap clone.
- Hover on a mark: a read-only tooltip next to the passage with the comment and, when present, the
  agent's reply. It follows the pointer from mark to mark and goes away when the pointer leaves.
- Click on a mark, or stepping onto it, opens the comment box (native `popover`): comment editable,
  Save / Delete / Cancel, creation date, short hash of the file version it was written against, a
  "file changed since" note when that hash is not the current one, the agent's `reply` when present,
  and the anchoring note ("text not found in the current version", "re-anchored approximately") when
  relevant.
- Navigation: previous / next / document buttons in a toolbar row, rendered `hidden` by the server
  when the sidecar has no entry and unhidden by the browser while capture is on (the document button
  must be reachable for the first comment), plus keys.
  Stepping onto an annotation scrolls it into view and opens its box. Order: document-level entries
  first, then document order, then the orphans (text not found), each with the matching note.
- Sidecar `<dir>/<name-without-.md>.annotations.json5`, read and written by the server under an
  advisory lock (`flock`: shared to read, exclusive to write, the write in place under the lock), so
  an agent that takes the same lock never reads a half-written file or overwrites a save. Removed
  when the last annotation is deleted, so directories do not accumulate empty sidecars.
- JSON5, self-describing: every save writes a header comment that tells the agent how to take part
  (lock, `status` / `reply` or delete, re-anchor by rewriting `exact`, what not to touch) and the
  entry format. Read with a JSON5 parser, so a file rewritten through a JSON5 library (bare keys,
  single quotes, trailing commas, header dropped) is still accepted; the header comes back on the
  next save.
- Server fills what the browser cannot know: `id`, `created`, `file_hash` (sha256 of the markdown at
  creation), `lines` (1-based start/end lines of `exact` in the markdown source, best effort, `null`
  when not found or when `exact` is null), `updated` when a comment changes.
- Anchoring on every load: `exact` searched in the article text (text nodes outside diff `del`,
  mermaid and MathJax containers), `prefix` / `suffix` (32 characters each) to pick among several
  occurrences. No exact hit: fuzzy fallback, Hypothesis-style. Locate `prefix` and `suffix` and anchor
  the text between them when the gap stays under three times the length of `exact`; the mark is then
  dashed and the box says so. Neither: orphan, kept in the sidecar, no mark, last in navigation.
- Agent protocol, in the sidecar header and the README: an entry is a request until it is answered
  or deleted.
  When the agent rewrites an annotated passage it replaces `exact` with the new text; when it has
  handled a request it either sets `status: done` with a `reply`, or deletes the entry. `status` and
  `reply` are optional; absent means open.
- Keyboard: `n` / `p` next and previous, `a` annotate the selection, `A` comment on the document,
  `Escape` closes the box, `Ctrl+Enter` saves. None of them fire while typing in the comment box.
- One endpoint, `/__annotations?path=/doc.md`: `GET` returns the sidecar (empty list when absent)
  plus the current file hash, `PUT` replaces it. Same-origin and path checks identical to
  `/__baseline`.
- A list of the annotations at the foot of the document, in navigation order: quote, comment, the
  agent's reply, and the anchoring note. Clicking an item jumps to its mark and opens its box. It
  sits outside the article, so its text never becomes part of the model an annotation anchors in,
  and it is the printable form of the marks.
- Hovering an item of the list lights its mark in the text, without opening the box or scrolling.
- A save does not reload the page: the live-reload watcher ignores `*.annotations.json5`. The
  marks and the list are redrawn from the server's answer. Any other change in the directory still
  reloads.
- Bubble, tooltip, box and marks hidden on print; the list stays.
- README section: how to use it, the JSON schema, the agent protocol, and the one-line prompt to hand
  the agent.

## Out of scope

- Threads: one `reply` per entry, no back-and-forth. A follow-up is a new annotation.
- Bulk actions ("purge done", "delete all"). The agent, or the reader, deletes entries one by one, or
  the sidecar file itself.
- Annotations on non-markdown files, or on the rendered text of mermaid diagrams and MathJax formulas
  (it does not exist in the source; the bubble does not appear on such a selection).
- Annotation marks in the minimap.
- A CLI to list or export annotations; the JSON is the interface.
- Multi-user, concurrent editing of the sidecar. Last write wins, like the rest of the tool.
- Keeping `lines` fresh between two writes: the agent edits the markdown, `lines` in the sidecar is
  stale until the next save from the browser. The agent is told to trust `exact` first; `lines` is a
  hint.
- Browsers without the CSS Custom Highlight API (Firefox < 140, Safari < 17.2): marks are not drawn and
  the capture toggle is disabled with a title saying why.

## Acceptance criteria

- [x] Given any markdown page, the toolbar carries the capture toggle and the current file hash;
      given a sidecar with entries next to the file, the navigation row is shown; given none, it is
      rendered hidden.
- [x] Given capture is on and the reader selects "the quick brown fox" in a paragraph, an "Annotate"
      bubble appears; clicking it opens a comment box; saving a comment writes
      `<name>.annotations.json5` with one entry whose `exact` is the selected text, `prefix` / `suffix`
      the surrounding text, `lines` the line of that sentence in the source, `file_hash` the sha256
      of the file, and `created` an RFC 3339 timestamp; `status` and `reply` are absent.
- [x] Given the page reloads, the same passage carries the tint and the orange underline again,
      without the DOM of the article having gained any element.
- [x] Given capture is off, selecting text shows no bubble; existing marks are still drawn.
- [x] Given the selection sits in a diff `del`, or spans a mermaid diagram, no bubble appears.
- [x] Given the `A` key (or the document button), a comment box opens with no selection; saving
      writes an entry with `exact: null` and `lines: null`, drawn nowhere, first in navigation.
- [x] Given the pointer hovers a marked passage, a tooltip shows its comment (and reply, if any)
      without opening the editor; moving the pointer off the passage hides it; clicking the passage
      opens the editable box.
- [x] Given three anchored annotations and one document-level one, `n` opens the document-level one
      then the three in document order, `p` walks back; the buttons do the same; the box shows the
      comment, the date and the short hash.
- [x] Given the comment is edited and saved, the sidecar carries the new comment and an `updated`
      timestamp, `created` and `file_hash` untouched.
- [x] Given the last annotation is deleted, the sidecar file no longer exists.
- [x] Given the sidecar is rewritten by hand as JSON5 (comment on top, bare keys, single quotes,
      trailing commas), `GET` returns its entries and the next `PUT` writes the header back.
- [x] Given the sidecar carries `status: done` and a `reply` on an entry, its mark is grey and its box
      shows the reply.
- [x] Given the file is edited so that an annotated sentence is reworded but its surroundings stay,
      the mark lands on the new sentence with a dashed underline and the box says it was re-anchored
      approximately.
- [x] Given the file is edited so that an annotated sentence and its surroundings are gone, the
      annotation is not drawn, comes last in navigation, and its box says the text was not found; the
      entry is still in the sidecar.
- [x] Given the sidecar's `exact` is rewritten by hand to a sentence present in the file, after reload
      the mark is on that sentence.
- [x] Given the file is edited elsewhere (the annotated sentence moved lines), after reload the mark
      is on the same sentence and the box notes the file changed since the comment.
- [x] Given a comment is saved, the page is not reloaded and the marks and list reflect the save;
      given the markdown file is written, the page reloads.
- [x] Given four entries, the foot of the document lists them in navigation order with quote,
      comment, reply and note; clicking the third opens its box and the counter reads 3/4.
- [x] `PUT /__annotations` with a cross-origin `Origin` is refused with 403; a path outside `/*.md`
      is refused with 400; a body that is not the expected JSON is refused with 400.
- [x] Given another process holds an exclusive `flock` on the sidecar, `PUT` waits for it and the
      result contains both writes' intent (the server's read-modify-write happened after the lock was
      released); `GET` does not return a partially written file.
- [x] `go test ./...` green; the endpoint contract and the enrichment (`id`, `created`, `file_hash`,
      `lines`, `updated`, file removal on empty, `status` / `reply` / re-anchored `exact` kept from
      disk on an existing entry, unknown ids dropped, JSON5 read, header written) are pinned by Go
      tests.

## Phases

### Phase 1 — Sidecar endpoint
- Work: `internal/annotations.go`: `GET /__annotations?path=/doc.md` reads the sidecar under a
  shared lock and answers `{"file", "hash", "annotations"}`; `PUT` takes `{"annotations": [...]}`,
  validates origin and path (the checks of `/__baseline` extracted into helpers), reads the markdown,
  enriches each entry (new ones get `id`, `created`, `file_hash`; changed comments get `updated`;
  every entry with an `exact` gets `lines` recomputed against the source; on an existing entry only
  the comment is taken from the browser, `status`, `reply` and the quote come from disk, since the
  agent may have changed them while the page was open; an id unknown on disk is dropped, the agent
  deleted it), writes the sidecar in place under an exclusive `flock`
  (`0644`), removes it when the list is empty, answers with the stored document. `flock` is
  `syscall.Flock` on unix and a no-op on windows (build tags). The page template carries the current
  file hash and `HasAnnotations` (sidecar present with at least one entry).
- **Data model impact**: new file `<name>.annotations.json5` next to each annotated markdown file;
  schema below.
- **DoD**: `go test ./internal/ -run Annotations` green: round trip, enrichment fields, `lines` found
  and `null`, pass-through of `status` / `reply`, empty list removes the file, 403 / 400 paths,
  template contract (toggle always, navigation row only with a non-empty sidecar).

### Phase 2 — Capture and draw
- Work: `defaults/static/js/annotate.js` + `defaults/static/css/annotate.css`. Text model of the
  article with offsets; selection to quote + prefix / suffix; bubble; comment box as a native
  `popover`; save through the endpoint; anchoring (exact, then fuzzy) and drawing through
  `CSS.highlights` on load, with the done / dashed variants; hit-testing of marks under the pointer
  (`caretPositionFromPoint` + `isPointInRange`, throttled to one test per animation frame) for the
  hover tooltip and the click that opens the box; capture toggle and preference; document-level entry.
- **Data model impact**: none.
- **DoD**: scripted browser check on a 20-paragraph document: select, annotate, reload, the sidecar
  matches the criteria above and the mark is back with the article's `childElementCount` unchanged;
  a selection inside a `del` shows no bubble; hovering a mark shows the tooltip and clicking it
  opens the box; a reworded sentence gets the dashed mark; a document
  comment lands with `exact: null`.

### Phase 3 — Navigate, edit, document
- Work: prev / next / document buttons row, keyboard shortcuts, ordering (document-level, anchored,
  orphans), edit and delete in the box, reply display, "file changed since", "re-anchored" and "text
  not found" notes. README section: usage, schema, agent protocol, prompt line.
- **Data model impact**: none.
- **DoD**: browser check: three anchored plus one document-level annotation, `n` / `p` and the
  buttons walk them in the specified order; edit updates the sidecar with `updated`; delete of the
  last one removes the file; an orphaned entry is reachable last with its note; a `done` entry shows
  its reply.

## Data model impact (summary)

One sidecar file per annotated markdown file, `<name>.annotations.json5`, in the same directory.
JSON5 so the file can carry its own instructions; the preview writes strict JSON under the header:

```json5
// grip-live-diff annotations for README.md. JSON5: comments and trailing commas are fine.
// ... how to take part, what not to touch, entry format ...
{
  "version": 1,
  "file": "README.md",
  "annotations": [
    {
      "id": "k3f9x2",
      "exact": "the quick brown fox",
      "prefix": "e sentence before it ends with ",
      "suffix": " jumps over the lazy dog. Then",
      "lines": [42, 42],
      "comment": "Too informal, rephrase.",
      "created": "2026-09-05T11:52:03+02:00",
      "updated": null,
      "file_hash": "sha256:3b2f…",
      "status": "done",
      "reply": "Rephrased as 'the fox'."
    },
    {
      "id": "p8q1zz",
      "exact": null,
      "prefix": null,
      "suffix": null,
      "lines": null,
      "comment": "Whole document: too long, cut the history section.",
      "created": "2026-09-05T11:58:40+02:00",
      "updated": null,
      "file_hash": "sha256:3b2f…"
    }
  ]
}
```

`exact`, `prefix`, `suffix` follow the W3C Web Annotation TextQuoteSelector so the anchor survives
edits elsewhere in the file. `lines` is a convenience for the agent, recomputed at every write from
the browser; the agent should trust `exact` first. `status` (`done`) and `reply` are written by the
agent, never by the UI; absent means open.

The comment is free text typed by the reader of a local, single-user preview; no personal data is
collected beyond what they choose to write. Same posture as `/__baseline`: no auth, same-origin check.

## Open questions

- [x] None.

## Decisions

- Marks always drawn, toggle governs capture only — asked, chosen over "one switch for everything":
  feedback in flight must be visible without turning anything on, while selecting to copy must stay
  clean.
- `status` / `reply` optional, written by the agent only, delete stays the other exit — asked, chosen
  over a flat schema (no visibility on what the agent did) and a mandatory workflow (needs purge
  tooling).
- Agent re-anchors by updating `exact`, fuzzy anchoring in the client as a safety net, orphans kept —
  asked, chosen over purging on save, which could silently drop an unhandled request.
- Document-level entries with `exact: null` — asked; global feedback otherwise goes back to the chat
  and leaves no trace.
- Bare letters `n` / `p` / `a` / `A` — asked; nothing else on the page uses letters, chords collide
  with browser and window-manager bindings.
- CSS Custom Highlight API rather than wrapping ranges in `<mark>` — a range that crosses element
  boundaries cannot be wrapped without rebuilding the DOM, and the diff already wraps text. The price
  is that hover and click cannot use DOM events on the mark; they are hit-tested from the pointer
  position against the annotation ranges, which is cheap for a few dozen entries.
- Hover shows, click edits — asked; reading a comment must not cost a click, and a pile of open
  editors on a long document would be noise.
- Confluence look (tint + thick orange underline) rather than a bare underline — the tint makes a
  one-word annotation findable, the underline tells it apart from a diff insertion, which is also a
  background colour.
- Reads and writes both go through `/__annotations` so both honour the lock — asked (flock before
  writing); a locked write is only worth it if the read side respects it too. In-place write under
  the lock rather than temp + rename, because a rename swaps the inode under a waiter's lock.
- The agent side of the protocol takes the same lock (`flock README.annotations.json -c '...'` or
  the language equivalent); documented in the README, not enforced.
- Full-list replace on every save rather than per-entry CRUD — single user, a few dozen entries, one
  atomic write.
- `lines` recomputed at every write, `file_hash` frozen at creation — the hash says which version the
  comment was about, the lines say where the text was at the last save.
- Sidecar removed when empty — a `.annotations.json5` with `[]` next to every file ever annotated is
  litter.
- JSON5 with a protocol header rather than plain JSON — asked; the agent that opens the file finds
  the rules in it, no README lookup. Parsed with `titanous/json5` (a stdlib-only fork of
  `encoding/json`, tagged) because a JSON5 library on the agent side emits bare keys and single
  quotes, which a comment stripper would not survive.
- The live-reload middleware `aarol/reload` is replaced by an internal copy (MIT, attributed) with a
  filter — asked, once the flash on every save was seen; upstream keeps its broadcast private, so a
  filter could not be added from outside. The reload script moves into the template, which drops the
  response-buffering middleware and two dependencies.
- List at the foot of the document — asked after phase 3; the marks are scattered, the list is the
  overview, and the only form that prints.
