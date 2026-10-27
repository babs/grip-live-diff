# 011 — GitHub heading ids

**Status**: draft
**Requested by**: DuxLocutus ([#18](https://github.com/babs/grip-live-diff/issues/18))
**Date**: 2026-10-26

## Problem

`parser.WithAutoHeadingID()` uses goldmark's default id generator: it keeps ASCII letters and
digits, turns any ASCII whitespace (tab included), `-` or `_` into `-`, and drops every other byte, multi-byte letters
included. A link written for GitHub misses its heading in gld.

| Heading | gld today | GitHub |
|---|---|---|
| `À trancher` | `-trancher` | `à-trancher` |
| `ARB-010 · Les défauts de l'extraction` | `arb-010--les-dfauts-de-lextraction` | `arb-010--les-défauts-de-lextraction` |
| `À trancher` (second) | `-trancher-1` | `à-trancher-1` |
| `ΟΔΟΣ` | `heading` | `οδοσ` |
| `snake_case  two  spaces` | `snake-case--two--spaces` | `snake_case--two--spaces` |
| `Tab⇥here – en—em` | `tab-here--enem` | `tabhere--enem` |

ASCII headings diverge too: `_` and tabs. In `teams-watch`, 38 of the 63 internal anchors of one
file have no target; `specs/` holds 298 links with a non-ASCII anchor, in 61 files.

## Solution

Option A of the issue: give goldmark a GitHub-compatible `parser.IDs`, one per document, passed
to `md.Convert` with `parser.WithContext(parser.NewContext(parser.WithIDs(…)))`. Goldmark keeps
calling it, on the heading's last source line, as today.

GitHub's rule, checked against `POST /markdown` (2026-10-26) and
[html-pipeline `TableOfContentsFilter`](https://github.com/gjtorikian/html-pipeline/blob/v2.14.3/lib/html/pipeline/toc_filter.rb)
(`/[^\p{Word}\- ]/u` removed, then `' '` → `-`):

- lower case (`strings.ToLower`);
- keep `-` and every rune of Ruby's `\p{Word}`, that is in Go's stdlib tables `unicode.L`,
  `unicode.Nl` (`Ⅻ`), `unicode.Other_Alphabetic` (`Ⓐ`), `unicode.M`, `unicode.Nd`, `unicode.Pc`
  (`_`, `‿`, `＿`) and `unicode.Join_Control` (ZWJ U+200D, kept inside an emoji sequence);
- each U+0020 space becomes one `-`, no collapsing;
- drop everything else: punctuation (`·`, `'`, `’`, `–`, `—`), symbols and emoji, tabs, other
  separators, `Other_Number` (`½`, `²`, `①`);
- no Unicode normalisation: an NFD `é` stays `e` + U+0301;
- duplicates: the first keeps the slug, the next get `-1`, `-2`, …, skipping a value already
  used (`Foo`, `Foo`, `Foo-1`, `Foo` → `foo`, `foo-1`, `foo-1-1`, `foo-2`).

Option B (keep goldmark ids, document ASCII-only anchors) is rejected: gld renders GFM, and the
links fail on `_` as well, which no anchor discipline in the documents avoids.

The browser matches `href="#%C3%A0-trancher"` (goldmark's percent-encoded link) to
`id="à-trancher"` with no script: checked in headless Chrome.

## Scope

- A `parser.IDs` implementation in `internal/parser.go`, a fresh instance per `MdToHTML` call.
- Same fallback as goldmark for an empty slug: `heading`, `heading-1`, … Deliberate divergence:
  GitHub writes an anchor with `id=""` (`!!!`) or no anchor (empty `##`).
- README: one line saying heading ids follow GitHub.

## Out of scope

- Slugging the rendered heading text. Known limit: the source line leaks markup GitHub does not
  see, as today: `[link](http://a.b)` gives `linkhttpab` (GitHub `link`), `H<sub>2</sub>O` gives
  `hsub2subo` (GitHub `h2o`), `x &amp; y` gives `x-amp-y` (GitHub `x--y`), `![alt](a.png)`
  keeps alt and URL (GitHub drops the image), a two-line setext heading uses its last line only. Upgrade path if a user hits it: an
  `ast.Transformer` after inline parsing that slugs the heading's text nodes, maps `:emoji:`
  nodes back to their shortcode and dedupes itself.
- `İ`: Go lower-cases it to `i`, GitHub to `i̇` (`i` + U+0307).
- GitHub's `user-content-` prefix: gld puts the id on the heading itself, a plain `#slug` link
  already lands.
- Footnote ids (`fn:1` in gld, `fn-1` on GitHub).
- Explicit ids (`{#id}` attributes): not enabled.
- A table of contents, permalink icons on headings.

## Acceptance criteria

- [ ] `## À trancher` → `id="à-trancher"`; a second one → `id="à-trancher-1"`.
- [ ] `### ARB-010 · Les défauts de l'extraction` → `id="arb-010--les-défauts-de-lextraction"`.
- [ ] `## 日本語の見出し` → `id="日本語の見出し"`; `## Ελληνικά Кириллица` →
      `id="ελληνικά-кириллица"`; `## ΟΔΟΣ` → `id="οδοσ"`.
- [ ] `## Rocket 🚀 launch` → `id="rocket--launch"`; `## :rocket: shortcode` →
      `id="rocket-shortcode"`.
- [ ] `## snake_case  two  spaces` → `id="snake_case--two--spaces"`.
- [ ] `## C++ & C# -- 2.0 (beta)!` → `id="c--c----20-beta"`; `## ½ ² ① numbers` →
      `id="---numbers"`; `## Tab⇥here – en—em “quotes” ’apos’` (`⇥` a tab) →
      `id="tabhere--enem-quotes-apos"`.
- [ ] `## a Ⅻ b` → `id="a-ⅻ-b"`; `## a Ⓐ b` → `id="a-ⓐ-b"`; `## a‿b＿c` → `id="a‿b＿c"`.
- [ ] `## Family 👨‍👩‍👧 trip` → `id="family-‍‍-trip"`: the emoji go, both ZWJ (U+200D) stay.
- [ ] `Foo`, `Foo`, `Foo-1`, `Foo` → `foo`, `foo-1`, `foo-1-1`, `foo-2`.
- [ ] NFD `Café` (e + U+0301) → `café` with U+0301 kept.
- [ ] Regression guard: two documents rendered in turn do not share the duplicate counter
      (already true, `goldmark.New` runs per `MdToHTML` call).
- [ ] Given `[go](#à-trancher)` and `other.md#à-trancher`, a click lands on the heading
      (headless browser, `:target` and `scrollY`).
- [ ] `go test ./...` green.

## Phases

### Phase 1 — Id generator
- Work: `githubIDs` type (`map[string]bool` + `Generate` + `Put`) in `internal/parser.go`, wired
  through `md.Convert(input, &buf, parser.WithContext(parser.NewContext(parser.WithIDs(…))))`.
  `parser.WithAutoHeadingID()` stays. README line.
- **Data model impact**: none. Ids change for headings with non-ASCII letters, `_`, tabs or
  non-ASCII punctuation; a link written for the stripped form (`#-trancher`) stops landing.
- **DoD**: table-driven `TestMdToHTML_HeadingIDs` in `internal/parser_test.go` with every
  heading of the acceptance criteria, expected values taken from `POST /markdown`. Headless
  browser check of the two link criteria on a built binary.

## Data model impact (summary)

None. No consumer in gld reads heading ids:

- `annotate.js` anchors on quoted text (`exact`, `prefix`, `suffix`), never on ids.
- `minimap.js` strips ids from its clone, whatever their value.
- Diff references keep markdown and render it with the same parser, so both sides get the same
  ids; no rendered HTML is persisted.
- `pkg/footnote` and `pkg/details` write their own ids, not through `parser.IDs`.
- No template, TOC or script reads `location.hash`.

## Open questions

- [ ] None.

## Decisions

- A over B (maintainer): gld previews GFM, and GitHub is where these documents are read once
  pushed.
- Ids from the raw source line, goldmark's `Generate` input (maintainer): rendered-text ids wait
  for a user who hits the limit, see Out of scope.
- `heading` fallback for an empty slug (maintainer), a deliberate divergence from GitHub: every
  heading keeps a valid, non-empty id.
- `parser.IDs` over a transformer: goldmark already calls it for every heading and dedupes
  through it; the change stays one type and one option.
- No new dependency: stdlib `unicode` covers the classes. Hugo has a `github` id type but
  pulling Hugo for it is out of proportion; no small Go module matches GitHub's output.
- Expected values come from GitHub's API, not from
  [`github-slugger`](https://github.com/Flet/github-slugger), which is a reimplementation of
  the rule.
