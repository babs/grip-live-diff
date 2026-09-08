# 005 — Annotation threads

**Status**: shipped
**Requested by**: babs, after a first real review loop (four answered annotations on a draft spec)
**Date**: 2026-09-07

Extends [003](003-live-annotations.md): lifts its "one reply per entry, no back-and-forth" exclusion.

## Problem

After the agent has answered, the reader cannot tell what happened. An answered entry is a grey mark
and a grey block: nothing says "done", nothing says who wrote which text, and the answer sits above
the question in the comment box. The reader who disagrees with the answer has no move: editing the
comment leaves `status: done` and the reply in place, so the agent never sees the objection. And the
answers only show up after a manual refresh, because the live reload deliberately ignores the sidecar.
The loop stops one step short of a conversation.

## Solution

An annotation is a thread. Each message says who wrote it, reader or agent, and when; the box, the
list at the foot of the document and the hover tooltip show the thread in order, oldest first, each
message labelled "you" or "agent". The box carries a status badge: open, done, or reopened. When the
last message is the agent's, the box offers a follow-up: the reader types it, the entry is open again,
and the agent picks it up on its next pass. When the agent writes the sidecar, the marks and the
threads refresh in place, without reloading the page and without losing what the reader was typing.

## Scope

- Sidecar schema version 2: `comment`, `reply`, `updated` and `created` are replaced by
  `thread: [{by, at, text}]`. `by` is `reader` or `agent`. `status: done` stays, agent-only. Version 1
  files are read and converted (`comment` becomes the first reader message dated `created`, `reply`
  an agent message with no date); the next save writes version 2.
- Merge rules on `PUT`, extending 003's: the browser owns reader messages and list membership, the
  agent owns its messages and `status`. Reader messages are matched by rank among reader messages
  (the reader's sequence can only grow: edits in place, new ones appended at the end of the thread);
  agent messages always come from disk. A new reader message clears `status`: the request is open
  again. `at` is set by the server on new reader messages and never changed afterwards.
- Header rewritten for the agent: append a message `{by: "agent", at, text}` and set `status: "done"`,
  or delete the entry; never edit or remove reader messages; an entry whose last message is the
  reader's is a pending request, whatever it was before.
- Box: meta line = date of the first message, short hash, status badge, anchoring notes. Below it the
  thread, chronological, each message with its label and time. The textarea at the bottom edits the
  last reader message when it is the last message of the thread (as today), or composes a follow-up
  when the last message is the agent's (placeholder "Follow up"). Older reader messages are
  read-only.
- List at the foot and hover tooltip: the thread in the same order with the same labels; the list
  item carries the badge.
- Badge derivation: `done` when `status` is done; `reopened` when `status` is absent and an agent
  message precedes the last reader message; `open` otherwise. Marks: done grey as today, reopened
  drawn like open.
- Live refresh: the watcher no longer ignores `*.annotations.json5`. A sidecar change broadcasts
  `annotations` instead of `reload`; a burst that also touches any other file broadcasts `reload`.
  The websocket stays open across messages (today it closes after one, and the reconnect path
  reloads the page). On `annotations` the page refetches the sidecar and redraws marks, list and
  box; the textarea keeps its content and the box stays on the same entry by id. If that entry is
  gone from disk, the box closes.
- README: schema v2, the agent protocol, the follow-up.

## Out of scope

- Per-message delete or reorder. The reader deletes the whole entry, as today.
- Editing a reader message once an agent message follows it. The correction is a follow-up.
- Agent identity beyond the `agent` role (no names, no model ids). `by` other than `reader` or
  `agent` is shown verbatim and treated as agent.
- Other statuses (`wontfix`, `question`). The agent asks in a message and leaves the entry open.
- Notification of a refresh (toast, counter flash). The marks and badges change, that is the cue.
- Writing `at` on agent messages from the server. The agent sets it; missing, the UI shows no time.

## Acceptance criteria

- [x] Given a version 1 sidecar with `comment`, `created`, `reply` and `status: done`, `GET` returns
      the entry with `thread: [{by: "reader", at: <created>, text: <comment>}, {by: "agent", at:
      null, text: <reply>}]`, `status: done`, and no `comment` / `reply` / `updated` / `created`
      keys; the next `PUT` writes `"version": 2` and the new header.
- [x] Given a new annotation is saved from the browser, the entry has `thread` with one reader
      message dated now, `id`, `file_hash`, `lines`, and no `status`.
- [x] Given an entry `[reader, agent]` with `status: done` on disk and the browser sends
      `[reader, reader']` (page loaded before the agent wrote), the result is `[reader, agent,
      reader']` with `status` absent, `reader'.at` set by the server.
- [x] Given an entry `[reader]` on disk and the browser sends `[reader-edited]`, the text is replaced
      and `at` is unchanged.
- [x] Given the browser sends an agent message with altered text or one that is not on disk, the
      stored agent messages are kept as they were and the extra one is dropped.
- [x] Given `PUT` on an entry with `status: done` and no new reader message, `status` stays done.
- [x] Given the box is open on an answered entry, the thread reads oldest first: "you" then "agent",
      each with its time (none when `at` is null); the badge reads "done"; the textarea is empty
      with the placeholder "Follow up".
- [x] Given "Follow up" text is saved, the box shows three messages, the badge reads "reopened", the
      mark is no longer grey, the sidecar has the third message and no `status`.
- [x] Given the box is open on an entry whose last message is the reader's, the textarea holds that
      message and saving edits it in place; the thread above shows only the earlier messages.
- [x] Given the list at the foot of the document, each item shows the badge and the thread with
      labels, oldest first; the tooltip shows the same.
- [x] Given the agent rewrites the sidecar (adds a message, sets done) while the page is open, within
      one second the mark turns grey, the list shows the message, and the page did not reload
      (a marker set on `window` before the write is still there, and the scroll position is kept).
- [x] Given the box is open with "not yet saved" typed in the textarea when the agent writes the
      sidecar, the box stays open on the same entry, the thread shows the agent's new message, and
      the textarea still reads "not yet saved".
- [x] Given the agent deletes the entry the box is open on, the box closes and the mark is gone.
- [x] Given the markdown file is written, the page reloads as before; given the markdown and the
      sidecar are written within the same 100 ms burst, the page reloads once.
- [x] Given a save from the page, the resulting `annotations` broadcast does not open the box, move
      the scroll, or change what is drawn.
- [x] Given the server restarts, the page reconnects and reloads once, as today.
- [x] `go test ./...` green; the merge rules (rank matching, reopen, agent messages from disk, `at`
      immutability, v1 conversion, header), the broadcast kind selection and the looping websocket
      are pinned by Go tests.

## Phases

### Phase 1 — Schema v2 and merge
- Work: `internal/annotations.go`: `message{By, At, Text}` and `Thread []message` on the entry;
  `comment` / `reply` / `updated` / `created` dropped from the struct, read from a v1 shadow struct
  and converted; `mergeAnnotations` rewritten to the rank rules above; reopen on new reader message;
  header text for the thread protocol. Tests for every merge rule and the conversion.
- **Data model impact**: sidecar version 2 (see summary). Version 1 files still readable.
- **DoD**: `go test ./internal/ -run Annotations` green, including the v1 fixture in
  `.claude/scratchpad/DRAFT-quota-history-download.annotations.json5` converted as specified.

### Phase 2 — Thread in the UI
- Work: `annotate.js` / `annotate.css`: thread rendering in box, list and tooltip; badge; textarea
  bound to the last reader message or to a follow-up; `submit` builds the thread accordingly. README
  section updated.
- **Data model impact**: none.
- **DoD**: browser check on the scratchpad fixture: four done entries show "you"/"agent" threads
  in order with the badge; a follow-up on the second entry writes a third message, clears `status`,
  turns the mark yellow and the badge to "reopened"; a new annotation still works end to end.

### Phase 3 — Live refresh on sidecar writes
- Work: `internal/reload.go`: broadcast carries a kind (`reload` | `annotations`), `reload` wins
  within a burst, `serveWS` loops until the write fails; `server.go` passes the sidecar test as
  the classifier instead of the ignore filter; the reload script dispatches a DOM event on
  `annotations`; `annotate.js` listens and calls `load()` keeping the textarea and the current entry.
- **Data model impact**: none.
- **DoD**: `go test ./internal/ -run Reload` green (kind selection, burst precedence, two
  messages on one connection); browser check: `flock`-guarded rewrite of the sidecar by a script
  while the box is open, marks and thread update, textarea untouched, no navigation entry added.

## Data model impact (summary)

Sidecar `<name>.annotations.json5`, version 2:

```json5
{
  "version": 2,
  "file": "README.md",
  "annotations": [
    {
      "id": "k3f9x2",
      "exact": "the quick brown fox",
      "prefix": "e sentence before it ends with ",
      "suffix": " jumps over the lazy dog. Then",
      "lines": [42, 42],
      "file_hash": "sha256:3b2f…",
      "thread": [
        { "by": "reader", "at": "2026-09-05T11:52:03+02:00", "text": "Too informal, rephrase." },
        { "by": "agent",  "at": "2026-09-05T12:50:00+02:00", "text": "Rephrased as 'the fox'." },
        { "by": "reader", "at": "2026-09-05T13:02:11+02:00", "text": "Still too informal." }
      ]
    }
  ]
}
```

Removed from version 1: `comment` (first reader message), `reply` (agent message), `created`
(first message's `at`), `updated` (edits do not change `at`; the thread order is the history).
`status: "done"` unchanged, agent-only, absent means open.

`by` is a role, not a person: no new personal data. Same posture as 003, local single-user preview,
same-origin check, no auth.

## Open questions

- [x] None.

## Decisions

- Thread array rather than a `followup` field — a single follow-up caps the loop at one round trip;
  the second round ("not that, I meant…") is certain. The array also gives the author per message,
  which the flat schema could only imply by field name.
- `created` and `updated` dropped — `thread[0].at` is the creation date; an edit that changed `at`
  would lose it, and the order of the array is the history. Fewer fields for the agent to get wrong
  (the fixture shows the agent setting `updated` itself).
- Reader messages matched by rank, not by id — no per-message ids to generate, store and explain;
  the reader's sequence only grows, so rank is stable. Per-message delete would need ids; it is out of
  scope.
- A new reader message clears `status` server-side — `status` stays agent-only in the protocol, the
  server derives the reopen; the agent cannot miss it and the UI cannot forget it.
- Textarea bound to the last reader message or a follow-up, older reader messages read-only — one
  textarea, no per-message editors, and the case where an edit would race an agent answer cannot
  happen: once the agent has spoken, the correction is a follow-up.
- `at` optional on agent messages, not filled by the server — the server cannot know when the agent
  wrote; a wrong time is worse than none.
- Broadcast kind on the existing websocket rather than a second channel or polling — one socket, one
  script, the sidecar case is one branch. The socket looping is the fix that makes any non-reload
  message possible.
- Self-triggered refetch after a save from the page accepted — one extra `GET` with identical data;
  suppressing it needs the page to know which write was its own.
- Box closes when its entry is deleted underneath — the alternative keeps a box on nothing; the
  reader typing a follow-up on an entry the agent is deleting at the same second is rare enough.
