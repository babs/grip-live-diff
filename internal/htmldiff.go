package internal

import (
	"strings"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	insClass = "gg-ins"
	delClass = "gg-del"
	spaces   = " \t\n\r\f\v"
)

type tokenKind uint8

const (
	kindText   tokenKind = iota // words, the only tokens that get annotated
	kindTag                     // markup, passed through untouched
	kindAtomic                  // mermaid/math/script/style, changed as a whole
)

type token struct {
	s    string
	kind tokenKind
}

// DiffHTML returns newHTML with the passages that differ from oldHTML wrapped in
// <ins>/<del>. Equal and inserted tokens concatenate back to newHTML exactly, so the
// document structure is always that of the new version; deletions only ever inject text.
func DiffHTML(oldHTML, newHTML []byte) []byte {
	oldTokens := tokenize(string(oldHTML))
	newTokens := tokenize(string(newHTML))

	v := newVocab()
	dmp := diffmatchpatch.New()
	// Past this budget the algorithm returns a coarser diff. One second is meant for a
	// server under load; a local preview can afford to keep the diff precise.
	dmp.DiffTimeout = 5 * time.Second
	// No semantic cleanup: it is tuned for characters and here one rune is a whole word,
	// so it would swallow the untouched words sitting between two edits of the same line.
	diffs := dmp.DiffMainRunes(v.encode(oldTokens), v.encode(newTokens), false)

	var out strings.Builder
	out.Grow(len(newHTML))
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			for _, t := range v.decode(d.Text) {
				out.WriteString(t.s)
			}
		case diffmatchpatch.DiffInsert:
			writeInserted(&out, v.decode(d.Text))
		case diffmatchpatch.DiffDelete:
			writeDeleted(&out, v.decode(d.Text))
		}
	}
	return []byte(out.String())
}

func writeInserted(out *strings.Builder, tokens []token) {
	var run []token
	for _, t := range tokens {
		// Tags carry the structure of the new document: emitting them inside <ins> would
		// nest the annotation wrongly, so they cut the run instead.
		if t.kind == kindTag || isBlank(t.s) {
			flushInserted(out, run)
			run = run[:0]
			out.WriteString(t.s)
			continue
		}
		run = append(run, t)
	}
	flushInserted(out, run)
}

func writeDeleted(out *strings.Builder, tokens []token) {
	var run []token
	for _, t := range tokens {
		switch {
		case t.kind == kindTag:
			// Dropped: the removed markup has no place in the new document tree.
			continue
		case t.kind == kindAtomic:
			run = append(run, token{atomicInner(t.s), kindText})
		case isBlank(t.s):
			if len(run) > 0 {
				run = append(run, t)
			}
		default:
			run = append(run, t)
		}
	}
	flushDeleted(out, run)
}

// flushInserted keeps the trailing whitespace outside the marker: it belongs to the new
// document, and the highlight should stop at the last visible character.
func flushInserted(out *strings.Builder, run []token) {
	s := joinTokens(run)
	trimmed := strings.TrimRight(s, spaces)
	if trimmed == "" {
		out.WriteString(s)
		return
	}
	out.WriteString(`<ins class="` + insClass + `">` + trimmed + "</ins>" + s[len(trimmed):])
}

// flushDeleted keeps everything inside the marker, whitespace included: removing the
// element must leave the new document byte-for-byte intact, and the space still separates
// the removed words from the surrounding text.
func flushDeleted(out *strings.Builder, run []token) {
	s := joinTokens(run)
	if strings.Trim(s, spaces) == "" {
		return
	}
	out.WriteString(`<del class="` + delClass + `">` + s + "</del>")
}

func joinTokens(run []token) string {
	var b strings.Builder
	for _, t := range run {
		b.WriteString(t.s)
	}
	return b.String()
}

func tokenize(html string) []token {
	var out []token
	for i := 0; i < len(html); {
		if html[i] != '<' {
			next := strings.IndexByte(html[i:], '<')
			if next < 0 {
				next = len(html) - i
			}
			out = appendWords(out, html[i:i+next])
			i += next
			continue
		}

		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			out = appendWords(out, html[i:])
			break
		}
		tag := html[i : i+end+1]
		if stop := atomicEnd(html, i+end+1, tag); stop > 0 {
			out = append(out, token{html[i:stop], kindAtomic})
			i = stop
			continue
		}
		out = append(out, token{tag, kindTag})
		i += end + 1
	}
	return out
}

// appendWords splits text into tokens of one word plus the whitespace that follows it.
func appendWords(out []token, text string) []token {
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && !isSpaceByte(text[j]) {
			j++
		}
		for j < len(text) && isSpaceByte(text[j]) {
			j++
		}
		out = append(out, token{text[i:j], kindText})
		i = j
	}
	return out
}

// atomicEnd returns the offset just past the element opened by openTag when its content
// must never be annotated (annotating it would break mermaid, MathJax or a script), or -1.
func atomicEnd(html string, contentStart int, openTag string) int {
	var name string
	switch {
	case strings.HasPrefix(openTag, "<img"):
		// Void element: without this it would be a plain tag, hence dropped when removed
		// and left unmarked when added — an invisible change.
		return contentStart
	case strings.HasPrefix(openTag, "<script"):
		name = "script"
	case strings.HasPrefix(openTag, "<style"):
		name = "style"
	case strings.HasPrefix(openTag, "<video"):
		name = "video"
	case strings.HasPrefix(openTag, "<audio"):
		name = "audio"
	case strings.HasPrefix(openTag, "<iframe"):
		name = "iframe"
	case strings.HasPrefix(openTag, "<pre") && strings.Contains(openTag, "mermaid"):
		name = "pre"
	case strings.HasPrefix(openTag, "<span") && strings.Contains(openTag, `class="math`):
		name = "span"
	default:
		return -1
	}
	if strings.HasSuffix(openTag, "/>") {
		return contentStart
	}

	closing, opening := "</"+name+">", "<"+name
	for i, depth := contentStart, 1; depth > 0; {
		next := strings.Index(html[i:], closing)
		if next < 0 {
			return -1
		}
		if nested := strings.Index(html[i:i+next], opening); nested >= 0 {
			depth++
			i += nested + len(opening)
			continue
		}
		depth--
		i += next + len(closing)
		if depth == 0 {
			return i
		}
	}
	return -1
}

func atomicInner(s string) string {
	start := strings.IndexByte(s, '>')
	end := strings.LastIndex(s, "</")
	if start >= 0 && end > start {
		return s[start+1 : end]
	}
	return s
}

func isBlank(s string) bool {
	return strings.Trim(s, spaces) == ""
}

func isSpaceByte(b byte) bool {
	return strings.IndexByte(spaces, b) >= 0
}

// vocab maps every distinct token to a rune so a character-level diff becomes a
// token-level one.
type vocab struct {
	ids  map[string]rune
	toks map[rune]token
	next rune
}

func newVocab() *vocab {
	return &vocab{ids: make(map[string]rune), toks: make(map[rune]token), next: 1}
}

func (v *vocab) encode(tokens []token) []rune {
	out := make([]rune, 0, len(tokens))
	for _, t := range tokens {
		r, ok := v.ids[t.s]
		if !ok {
			r = v.next
			v.next++
			// Surrogates are not valid runes: they would collapse to U+FFFD once
			// diffmatchpatch turns the rune slices back into strings.
			if v.next == 0xD800 {
				v.next = 0xE000
			}
			v.ids[t.s] = r
			v.toks[r] = t
		}
		out = append(out, r)
	}
	return out
}

func (v *vocab) decode(s string) []token {
	out := make([]token, 0, len(s))
	for _, r := range s {
		out = append(out, v.toks[r])
	}
	return out
}
