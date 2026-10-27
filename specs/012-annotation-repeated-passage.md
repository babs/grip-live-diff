# 012 — Annotations on a repeated passage

**Status**: draft
**Requested by**: DuxLocutus, [issue #19](https://github.com/babs/grip-live-diff/issues/19)
**Date**: 2026-10-27

Fixes the anchoring of [003](003-live-annotations.md). Related: [010](010-manual-trigger.md)
(sidecar header, `trigger`). Line references are to `develop` at `9aa0515`.

## Problem

A page holds the same passage twice, with the same 32 characters on each side: two recipes with
the same ingredient table. The reader annotates « Flour » in the second table. The entry is saved,
but its mark is drawn on the first table, over that table's own mark, and its `lines` points at
the first table too.

- `annotate.js` `quoteOf` (`:545`) stores `CONTEXT = 32` (`:3`) characters of `prefix` and
  `suffix`. Both entries get the same selector. `anchor()` (`:136`) scores with `context()`
  (`:127`) and keeps the first occurrence on a tie.
- `sourceLines()` (`annotations.go:487`, called from `mergeAnnotations` at `:450`) uses
  `strings.Index` on `exact` alone (`:498`), and returns `nil` when it is not found (`:499`).
  `lines` is the first occurrence for every repeated passage, even when the browser anchors
  right: a list item repeated under two headings gets the right mark and `lines` of the first
  heading's item.

Reproduced on 2026-10-26 with the issue's `recipes.md`, headless Chrome driving the real UI
(double-click, Ctrl-Enter): two entries with the same `exact` / `prefix` / `suffix`, both
`lines: [9, 9]`, both ranges of `annot` in table 0.

## Solution

`lines` steers the anchoring. The rendered HTML tells the browser each block's source lines, the
browser sends the selected block's span with a new entry, and both sides rank the occurrences of
`exact` with the same three steps.

- **Render.** An AST transformer sets `data-lines="start-end"` (1-based source lines) on block
  nodes. The span comes from the node's own `Lines()` when it has them, else from its first and
  last *block* descendants that have them (`Type() == ast.TypeBlock`: `Lines()` panics on inline
  nodes). Lines are offset by the front matter:
  - `MdToHTML` strips the front matter only when `RenderTable` succeeds (`parser.go:31-35`);
  - the offset is then the newline count of `input[:len(input)-len(body)]`, delimiters included;
  - otherwise it is 0.
- **Block of a passage.** One definition, used at creation and when anchoring. Take the first text
  node of the trimmed quote: the one holding folded offset `start`, after `quoteOf` drops leading
  whitespace (`annotate.js:557-559`). The block is that node's nearest ancestor carrying
  `data-lines`. `range.startContainer` is not used: a selection can start in the previous block
  or on the newline between two.
- **Create.** `quoteOf` keeps `CONTEXT = 32`. `submit` (`annotate.js:660`, the new entry's
  `list.push`) sends the span of the passage's block as `lines: [start, end]`. No tagged block:
  no `lines`.
- **Rank.** The same order on both sides:
  1. Span. Keep the occurrences whose span overlaps the entry's `lines`. Exactly one: it wins.
     - Browser span of an occurrence: its block's `data-lines`.
     - Server span: the source lines of its first and last byte.
  2. Context. Among the kept occurrences (all of them when none or no `lines`), the highest
     context score wins.
     - Browser: `context()` on the rendered text, as today.
     - Server: its letters-only twin. Walk outward from the occurrence over the source, and inward
       over `prefix` (backwards) and `suffix` (forwards), skipping every non-letter on both sides
       (`unicode.IsLetter`). Count the letters that match before the first mismatch. The score is
       the prefix count plus the suffix count, at most the number of letters in `prefix` and
       `suffix`.
  3. Proximity, then order. The smallest distance to `lines[0]` wins, then document order. The
     distance is 0 when `lines[0]` is inside the occurrence's span, else the line gap to the
     nearest end of that span. Same definition on both sides. No `lines`: document order.
- **Save.** `mergeAnnotations` (`annotations.go:417`) passes the entry's `lines` as it stands
  before recomputing it:
  - new entry: the block span from the browser;
  - existing entry: the stored value, the passage's own span;
  - hand-written entry: whatever the agent wrote.

  `sourceLines` then overwrites `lines` with the chosen occurrence's span. When `exact` is not
  found in the source, `lines` is kept unchanged instead of becoming `null`. Today's `null`
  would drop the browser's span, and the next load falls back to the first occurrence. A quote
  that is not found includes one spanning table cells (`Flour 250 g`), crossing a link, or holding
  `_` in code, an entity or an escape.
  A save that only switches `trigger` (010) does not run `mergeAnnotations`: `lines` is
  untouched, as today.
- **Sidecar header.** The `exact / prefix / suffix` bullet (`annotations.go:49-50`) says `lines` is
  "recomputed on every save from the preview". That becomes "recomputed whenever the preview
  saves the list". The bullet then gains: `lines` also picks among repeated occurrences of
  `exact`, so an agent writing an entry on a repeated passage sets it to where the passage is.
  The 010 paragraph (manual trigger, ready file) is unchanged. In manual mode, an agent's edit to
  `lines` waits on disk like any other edit, and the next list save reads it as the hint.

## Scope

- `internal/parser.go`: the transformer, registered in `MdToHTML`, front matter offset.
- `pkg/highlighting`: the fenced code wrapper renders the node's attributes. Same edit drops the
  stray quote in `overflow-auto""` (`highlighting.go:64`).
- `internal/annotations.go`: `sourceLines(source, exact, prefix, suffix, near)`, its caller in
  `mergeAnnotations`, the header line.
- `defaults/static/js/annotate.js`: block of a passage, `quoteOf` span, `submit` sends `lines`,
  `anchor()` rank.
- `scripts/e2e`: `repeated-passage.js`, and `run.sh` runs it.
- README: `lines` in the schema section.

### Where `data-lines` lands (goldmark v1.7.17 and the `pkg/` renderers)

| Node | Lines | Renders attributes | Result |
|---|---|---|---|
| Paragraph, Heading | own | yes | tagged |
| List, ListItem (`pkg/tasklist` renderer) | from descendants | yes | tagged |
| TextBlock (tight list item content) | own | no tag of its own | gives its list item a span |
| Blockquote | from descendants | yes | tagged |
| Table, TableHeader (`<thead>`), TableRow | from cells | yes | tagged, one row = one source line |
| TableCell | own | yes | not tagged, the row is enough |
| FencedCodeBlock (`pkg/highlighting` renderer) | own (content lines) | after this change | tagged on the wrapper `div` |
| Footnote definition (`pkg/footnote`) | from descendants | yes | tagged, its real source lines |
| Alert (`pkg/alert`) | none | no, `renderAlert` never renders attributes | alert `div` untagged; inner paragraphs carry theirs |
| CodeBlock (indented), HTMLBlock, math block, mermaid | own | no | none, nearest tagged ancestor |

## Out of scope

- Entries already written. Their stored `lines` is the first occurrence, so they stay on it. A
  reader can delete and re-create the entry. An agent can set `lines` to fix it.
- Two occurrences in one block with the same 32-character context on both sides: no step tells
  them apart, the first wins.
- Repeats inside indented code blocks or raw HTML blocks: no attribute there, so step 1 does not
  apply to them.
- Hand-written entry with no `lines`, on occurrences whose contexts differ only in digits or
  punctuation. The browser's context tells them apart, while the server's letters-only score ties
  and takes the first. The first save stores the first occurrence's `lines`, and the mark moves
  there. The header tells agents to set `lines` on a repeated passage.
- Document order in step 3. The browser uses page order and the server uses source order. They
  differ only for footnote definitions, which render at the end and sit mid-source. Footnote
  blocks carry their source span, so step 1 decides before step 3 is reached.
- Which line inside one block. The browser compares block spans and the server compares the
  occurrence's own lines. Inside one wrapped paragraph the server falls back to its letters-only
  context, which link URLs can mislead, so `lines` may name the wrong line inside the block (saved
  `[3, 3]` for a passage on line 4). The mark is unaffected: the browser ranks by block span.
- Drift. Lines added or removed above the passage shift the stored `lines` until the next list
  save re-syncs it. Step 1 can then match another repeat sitting on the old lines. That save stores
  its lines and makes it stick.
- Fuzzy fallback (no exact hit): unchanged.
- Re-anchoring an existing entry from the browser: the quote of an existing entry comes from disk
  (003), unchanged.

## Acceptance criteria

Fixture documents for every criterion are added under `internal/testdata/` (names indicative).

- [ ] Issue repro (`recipes.md`): annotate « Flour » in each table. Entries carry `lines` `[9, 9]`
      and `[20, 20]`; one `annot` range in table 0, one in table 1.
- [ ] `list.md`: `1. Check the logs of the **api** pod.` under two headings, and once more in a
      paragraph. Each mark on its own occurrence, `lines` 5, 10, 15.
- [ ] `three.md`: three paragraphs with the same sentence, the last one annotated. Mark and `lines`
      on the third (line 7).
- [ ] Whole document repeated twice, passage annotated in the second copy: mark and `lines` on the
      second copy.
- [ ] A block with more than 512 identical characters around the passage, copied three times,
      passage annotated in the middle copy: mark and `lines` on the middle copy.
- [ ] `See the docs. Check the logs` and `See the [docs](http://d). Check the logs` as two
      paragraphs, `Check the logs` annotated in the second. It stays on the second after save and
      after reload.
- [ ] Two occurrences in one long paragraph with different context, the second annotated: mark on
      the second (step 2). Asserts the mark only: `lines` inside one block is out of scope.
- [ ] `recipes.md`, select `Flour 250 g` (two cells) in the second table: the browser sends
      `[20, 20]`, `sourceLines` does not find the quote, the sidecar keeps `lines: [20, 20]`, and
      after reload the mark is in table 1.
- [ ] Hand-written entry with no `lines` on a repeated passage: first occurrence, no error. The same
      entry with `lines` set to the second copy: mark and `lines` on the second copy.
- [ ] `recipes.md` behind 4 lines of front matter (`---`, two keys, `---`): rows carry
      `data-lines="13-13"` and `"24-24"`, entries `lines` `[13, 13]` and `[24, 24]`.
- [ ] `go test ./...` green.

## Phases

### Phase 1 — Line attributes

- Work: transformer and front matter offset in `internal/parser.go`, wrapper attributes in
  `pkg/highlighting`.
- **Data model impact**: none; rendered HTML gains `data-lines` on blocks.
- **DoD**: `go test ./...` green, with new cases:
  - `parser_test`: rows, `<thead>`, list items (tight and loose), a fenced block, a footnote
    definition carry their spans; front matter fixture is offset; no panic on a tight list
    item that starts with inline markup (`- **bold** item`), an empty list item (`-`) or an empty
    table cell (`| a |  |`), where a descendant walk that does not stop at block nodes reaches
    inline nodes or none.

### Phase 2 — Server picks the occurrence

- Work: `sourceLines(source []byte, exact, prefix, suffix *string, near *[2]int)` with the rank
  above; caller passes the entry's `lines` before overwriting it; header line.
- **Data model impact**: `lines` values change for repeated passages.
- **DoD**: `go test ./internal/ -run Annotations` green. `TestAnnotationsLinesFollowTheRenderedText`
  is table-driven over the fixtures. `prefix` / `suffix` are given as the browser sends them
  (rendered, folded), so the table and list cases have no source-identical context.
  - issue table, `near` `[20, 20]` → 20, nil → 9;
  - list ×3 → 5, 10, 15;
  - three paragraphs → 7;
  - whole document twice → second copy;
  - block ×3 → middle copy;
  - link-in-prefix pair, `near` on paragraph 2 → paragraph 2;
  - same-paragraph pair on a single line → second, by context;
  - quote not found (`Flour 250 g`), `near` `[20, 20]` → `[20, 20]` kept;
  - current cases, unchanged.

  A `mergeAnnotations` test: a new entry's `lines` is the hint and gets overwritten; an existing
  entry is hinted by its stored `lines`.

### Phase 3 — Browser sends and ranks

- Work: block of a passage, `quoteOf` span, `submit` sends `lines`, `anchor()` rank. README.
- **Data model impact**: none.
- **DoD**: `scripts/e2e/repeated-passage.js`, run by `scripts/e2e/run.sh` next to the 010
  `manual-trigger.js`, drives headless Chrome over CDP through the real UI (double-click,
  Ctrl-Enter) on every fixture. It asserts three things: the `annot` ranges sit in the expected
  block, the sidecar `lines` match, and a reload keeps every mark in place.

## Data model impact (summary)

- Sidecar schema unchanged. `lines` changes role: still a hint for the agent, and now also an input
  to anchoring. The browser sends it at creation, the server reads it before recomputing it, and
  both sides use it to pick among repeated occurrences of `exact`.
- Sidecar header: the `lines` line says it picks among repeats and that an agent writing an entry
  on a repeated passage sets it.
- Rendered HTML: `data-lines="start-end"` on block elements.

## Decisions

- Line hint over a longer context. Growing `prefix` / `suffix` until unique was prototyped: it
  fixes the browser up to its cap (512 per side). A block repeated three times with more than 512
  identical characters around the passage still ties, and every entry on a repeated passage grows
  to up to a kilobyte in the sidecar the agent reads. The line hint costs one attribute and no
  sidecar growth.
- Span first, then context, then proximity, on both sides. The server's context cannot match the
  browser's: the source carries link URLs, inline HTML, `:emoji:` codes and `[^fn]` markers the
  page does not show. With context first, the server can score another occurrence higher, overwrite
  `lines`, and the mark jumps on the next load. Both sides read the span from the same `lines`
  value, so they agree on which block holds the passage. They can still disagree on the line
  inside that block (out of scope). Context only splits occurrences inside one span, or entries
  with no span.
- One attribute, `data-lines="start-end"`, rather than a start and an end attribute: one lookup
  in the browser.
- Spans, not start lines. The stored `lines` is the passage's own span, and the browser compares
  it to block spans. Comparing to block starts alone puts the next block closer than the passage's
  own paragraph.
- Letters-only context in `sourceLines`. On the issue table, the `foldMarkdown` score is 2 for both
  occurrences, because the pipes break the match within a character. Letters-only scores 44 for
  both. Letters and digits fail on ordered lists, because the source's `1.` is not in the rendered
  text.
- `key()` left as is: measured no diff change with `data-lines`.
- No TextPositionSelector: a rendered-text offset breaks on any edit above the passage, an agent
  cannot compute it by hand, and it does not map to the source.
- No occurrence index (`nth`): it breaks when an occurrence is added or removed above, an agent
  rewriting `exact` would not update it, and the source counts occurrences the page does not show
  (link URLs, diff deletions, diagrams).
