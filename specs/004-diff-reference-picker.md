# 004 — Diff reference picker

**Status**: shipped
**Requested by**: babs, after feedback from colleagues
**Date**: 2026-09-06

Supersedes the single-button cycle of [001](001-reload-diff-view.md).

## Problem

The `±` button cycled through off, since open, last edit, last commit, and back to off. Going from
the commit reference to since open took two clicks and a pass through the plain page; the icon never
changed, so the current reference was only readable from a second-row label and the next one only from
the tooltip. *Mark as read* was a bare check icon that vanished in the commit mode without a word.
Colleagues found the control hard to learn and hard to predict.

## Solution

Split the two concepts the button was carrying. `±` is now on/off only, and comes back to the reference
used last. With the diff on, a segmented control lists the three references, the current one filled,
one click to any of them. *Mark as read* has its label and is greyed out, with a tooltip saying why,
instead of disappearing. Keyboard: `d` toggles, `1` `2` `3` pick a reference.

## Scope

- Toolbar rows in diff mode: 1. width, `±`, theme; 2. `since open | last edit | HEAD`; 3. a note when
  HEAD cannot be read (untracked file, git unavailable), only then; 4. *Mark as read* and the minimap
  toggle.
- `HEAD` greyed out with a tooltip when the directory is not a git work tree; `?diff=head` there
  falls back to *since open* so a remembered reference never lands on a plain page.
- *Mark as read* disabled against HEAD ("git's to move") and when there is nothing new.
- Last reference kept in `localStorage`; the `±` link is rewritten to it on load when the diff is off.
- `1` `2` `3` work with the diff off as well; the key of the reference already shown does nothing.
- Active reference carries `aria-current="page"`.

## Out of scope

- A dropdown instead of the segmented control (two clicks per change, the problem being fixed).
- Icons instead of text labels: the missing text was the complaint.
- Per-change navigation, change counter.

## Acceptance criteria

- [x] Given `?diff=open`, the picker marks *since open* as active and `aria-current`; the same for
      `last` and `head`.
- [x] Given the diff is off, clicking `±` opens the reference used last, *since open* on first use.
- [x] Given a directory outside git, `HEAD` is greyed out and `?diff=head` shows the *since open* diff.
- [x] Given `?diff=head` on an untracked file, the note row says so and *Mark as read* is disabled.
- [x] Given `?diff=open` with no change since the reference, *Mark as read* is disabled.
- [x] Given the dark theme, the picker and the button follow it.
- [x] Given `d`, the diff toggles; given `2`, the last-edit reference loads; given the key of the
      current reference, nothing reloads.
