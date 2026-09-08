package internal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sidecarName = "doc.annotations.json5"

func (f *diffFixture) annotations(method, body string, origin ...string) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, "/__annotations?path="+docPath, reader)
	} else {
		req = httptest.NewRequest(method, "/__annotations?path="+docPath, nil)
	}
	if len(origin) > 0 {
		req.Header.Set("Origin", origin[0])
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	return recorder
}

// put replaces the list and returns the stored entries.
func (f *diffFixture) put(entries string) annotationsResponse {
	f.t.Helper()

	rec := f.annotations(http.MethodPut, `{"annotations":`+entries+`}`)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("PUT: expected 200, got %d (%s)", rec.Code, rec.Body)
	}
	var resp annotationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("PUT: decode response: %v (%s)", err, rec.Body)
	}
	return resp
}

func (f *diffFixture) sidecar() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, sidecarName))
	if err != nil {
		f.t.Fatalf("read sidecar: %v", err)
	}
	return string(data)
}

func str(s string) *string { return &s }

func TestAnnotationsEnrichNewEntries(t *testing.T) {
	t.Parallel()

	content := "# Title\n\nalpha bravo\n"
	f := newDiffFixture(t, content)
	before := time.Now().Add(-time.Second)

	// The browser may claim a status or an agent message on a new entry: neither is kept.
	resp := f.put(`[{"exact":"alpha bravo","prefix":"","suffix":"","status":"done","thread":[{"by":"reader","at":"1999-01-01T00:00:00Z","text":"rephrase"},{"by":"agent","text":"forged"}]}]`)
	if len(resp.Annotations) != 1 {
		t.Fatalf("expected one entry, got %+v", resp.Annotations)
	}
	a := resp.Annotations[0]
	if len(a.ID) != 8 {
		t.Fatalf("expected an 8-character id, got %q", a.ID)
	}
	if len(a.Thread) != 1 || a.Thread[0].By != "reader" || a.Thread[0].Text != "rephrase" || a.Thread[0].At == nil {
		t.Fatalf("expected a thread of one dated reader message, got %+v", a.Thread)
	}
	created, err := time.Parse(time.RFC3339, *a.Thread[0].At)
	if err != nil || created.Before(before) {
		t.Fatalf("expected a fresh RFC 3339 timestamp, got %q (%v)", *a.Thread[0].At, err)
	}
	if a.Status != "" {
		t.Fatalf("expected no status on a new entry, got %q", a.Status)
	}
	if want := fileHash([]byte(content)); a.FileHash != want || resp.Hash != want || !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("expected file_hash and hash %q, got %q and %q", want, a.FileHash, resp.Hash)
	}
	if a.Lines == nil || *a.Lines != [2]int{3, 3} {
		t.Fatalf("expected lines [3 3], got %v", a.Lines)
	}
	if a.Exact == nil || *a.Exact != "alpha bravo" {
		t.Fatalf("expected the quote to be kept, got %+v", a)
	}
	if resp.Version != 2 || resp.File != "doc.md" {
		t.Fatalf("expected version 2 for doc.md, got %d %q", resp.Version, resp.File)
	}

	stored := f.sidecar()
	if !strings.Contains(stored, `"id": "`+a.ID+`"`) || strings.Contains(stored, `"status"`) || strings.Contains(stored, `"by": "agent"`) {
		t.Fatalf("expected the sidecar to carry the entry without agent fields, got %s", stored)
	}
	for _, legacy := range []string{`"comment"`, `"created"`, `"updated"`, `"reply"`} {
		if strings.Contains(stored, legacy) {
			t.Fatalf("expected no version 1 field %s in the sidecar, got %s", legacy, stored)
		}
	}
	if !strings.HasPrefix(stored, "// grip-live-diff annotations for doc.md") || !strings.Contains(stored, "flock") {
		t.Fatalf("expected the agent protocol as a header comment, got %s", stored)
	}

	rec := f.annotations(http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hash":"`+hashOf(content)+`"`) {
		t.Fatalf("expected GET to return the sidecar with the hash, got %d %s", rec.Code, rec.Body)
	}
}

func hashOf(content string) string { return fileHash([]byte(content)) }

func TestAnnotationsLinesFollowTheRenderedText(t *testing.T) {
	t.Parallel()

	source := []byte("# Title\n\nsome **bold** text\nwraps over\ntwo lines\n\n```\ncode here\n```\n")
	cases := []struct {
		exact *string
		want  *[2]int
	}{
		{str("some bold text"), &[2]int{3, 3}},
		{str("text wraps over two"), &[2]int{3, 5}},
		{str("code here"), &[2]int{8, 8}},
		{str("gone"), nil},
		{str("   "), nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := sourceLines(source, c.exact)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%v: expected no lines, got %v", c.exact, *got)
		case c.want != nil && (got == nil || *got != *c.want):
			t.Errorf("%q: expected %v, got %v", *c.exact, *c.want, got)
		}
	}
}

func reader(text string) message { return message{By: "reader", Text: text} }

func TestAnnotationsEditKeepsTheAgentFields(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n\ncharlie delta\n")
	first := f.put(`[{"exact":"alpha bravo","thread":[{"by":"reader","text":"rephrase"}]}]`).Annotations[0]

	// The agent answers, re-anchors the quote, and the file moves on.
	agent := strings.Replace(f.sidecar(), `"text": "rephrase"`, `"text": "rephrase" }, { "by": "agent", "at": null, "text": "done it"`, 1)
	agent = strings.Replace(agent, `"exact": "alpha bravo"`, `"exact": "charlie delta", "status": "done"`, 1)
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("# Title\n\nalpha bravo\n\nintro\n\ncharlie delta\n")

	// The browser still holds the old quote and only edited the comment.
	entries, _ := json.Marshal([]annotation{{ID: first.ID, Exact: str("alpha bravo"), Thread: []message{reader("rephrase, shorter")}}})
	resp := f.put(string(entries))
	if len(resp.Annotations) != 1 {
		t.Fatalf("expected one entry, got %+v", resp.Annotations)
	}
	a := resp.Annotations[0]
	if len(a.Thread) != 2 || a.Thread[0].Text != "rephrase, shorter" || *a.Thread[0].At != *first.Thread[0].At {
		t.Fatalf("expected the edited message with its date kept, got %+v", a.Thread)
	}
	if a.FileHash != first.FileHash {
		t.Fatalf("expected file_hash untouched, got %+v", a)
	}
	if a.Status != "done" || a.Thread[1].By != "agent" || a.Thread[1].Text != "done it" || a.Thread[1].At != nil || *a.Exact != "charlie delta" {
		t.Fatalf("expected the agent's status, message and quote to win, got %+v", a)
	}
	if a.Lines == nil || *a.Lines != [2]int{7, 7} {
		t.Fatalf("expected lines recomputed on the edited file, got %v", a.Lines)
	}

	// The browser tampers with the agent's message and adds one: the stored ones win.
	entries, _ = json.Marshal([]annotation{{ID: first.ID, Thread: []message{reader("rephrase, shorter"), {By: "agent", Text: "forged"}, {By: "agent", Text: "extra"}}}})
	if again := f.put(string(entries)).Annotations[0]; len(again.Thread) != 2 || again.Thread[1].Text != "done it" || again.Status != "done" {
		t.Fatalf("expected the agent's messages from disk and the status kept, got %+v", again)
	}

	// A follow-up sent from a page loaded before the agent answered: appended, reopened.
	entries, _ = json.Marshal([]annotation{{ID: first.ID, Thread: []message{reader("rephrase, shorter"), reader("still too long")}}})
	a = f.put(string(entries)).Annotations[0]
	if len(a.Thread) != 3 || a.Thread[2].By != "reader" || a.Thread[2].Text != "still too long" || a.Thread[2].At == nil {
		t.Fatalf("expected the follow-up appended after the agent's message, dated, got %+v", a.Thread)
	}
	if a.Status != "" || a.Thread[1].Text != "done it" {
		t.Fatalf("expected the follow-up to reopen the entry and keep the agent's message, got %+v", a)
	}
	if stored := f.sidecar(); strings.Contains(stored, `"status"`) {
		t.Fatalf("expected no status in the reopened sidecar, got %s", stored)
	}
}

func TestMergeThread(t *testing.T) {
	t.Parallel()

	old := "2026-09-05T11:52:03+02:00"
	agent := message{By: "agent", Text: "answer"}
	dated := func(text string) message { return message{By: "reader", At: &old, Text: text} }
	cases := []struct {
		name     string
		stored   []message
		incoming []message
		want     []string
		reopened bool
	}{
		{"new entry keeps reader messages only", nil, []message{reader("a"), agent, reader("b")}, []string{"reader:a@now", "reader:b@now"}, true},
		{"edit in place keeps the date", []message{dated("a")}, []message{reader("a2")}, []string{"reader:a2@" + old}, false},
		{"agent message untouched, reader text updated", []message{dated("a"), agent}, []message{reader("a2"), {By: "agent", Text: "forged"}}, []string{"reader:a2@" + old, "agent:answer"}, false},
		{"follow-up appended after the agent", []message{dated("a"), agent}, []message{reader("a"), reader("b")}, []string{"reader:a@" + old, "agent:answer", "reader:b@now"}, true},
		{"fewer reader messages than stored: nothing dropped", []message{dated("a"), agent, dated("b")}, []message{reader("a")}, []string{"reader:a@" + old, "agent:answer", "reader:b@" + old}, false},
		{"empty incoming keeps the thread", []message{dated("a"), agent}, nil, []string{"reader:a@" + old, "agent:answer"}, false},
		{"unknown author is not the reader's", []message{dated("a"), {By: "claude", Text: "hi"}}, []message{reader("a"), {By: "claude", Text: "forged"}}, []string{"reader:a@" + old, "claude:hi"}, false},
	}
	for _, c := range cases {
		got, reopened := mergeThread(c.stored, c.incoming, "now")
		var flat []string
		for _, m := range got {
			s := m.By + ":" + m.Text
			if m.At != nil {
				s += "@" + *m.At
			}
			flat = append(flat, s)
		}
		if strings.Join(flat, "|") != strings.Join(c.want, "|") || reopened != c.reopened {
			t.Errorf("%s: got %v reopened=%v, want %v reopened=%v", c.name, flat, reopened, c.want, c.reopened)
		}
	}
}

// A version 1 sidecar is read as threads and rewritten as version 2 on the next save.
func TestAnnotationsUpgradeVersionOne(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	v1 := `// grip-live-diff annotations for doc.md.
{
  "version": 1,
  "file": "doc.md",
  "annotations": [
    { "id": "j7ggjust", "exact": "alpha bravo", "prefix": "", "suffix": "", "lines": [3, 3],
      "comment": "too informal", "created": "2026-09-07T12:30:49+02:00", "updated": "2026-09-07T12:50:00+02:00",
      "file_hash": "sha256:18e8", "status": "done", "reply": "rephrased" },
    { "id": "p8q1zz00", "exact": null, "prefix": null, "suffix": null, "lines": null,
      "comment": "too long", "created": "2026-09-07T12:31:00+02:00", "updated": null, "file_hash": "sha256:18e8" }
  ]
}
`
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := f.annotations(http.MethodGet, "")
	var resp annotationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("expected the version 1 sidecar to be read, got %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, legacy := range []string{`"comment"`, `"created"`, `"updated"`, `"reply"`} {
		if strings.Contains(body, legacy) {
			t.Fatalf("expected no version 1 field %s in the answer, got %s", legacy, body)
		}
	}
	done, open := resp.Annotations[0], resp.Annotations[1]
	if resp.Version != 2 || len(done.Thread) != 2 || done.Status != "done" {
		t.Fatalf("expected a version 2 answer with a two-message done thread, got %+v", resp)
	}
	if m := done.Thread[0]; m.By != "reader" || m.Text != "too informal" || m.At == nil || *m.At != "2026-09-07T12:30:49+02:00" {
		t.Fatalf("expected the comment as a reader message dated created, got %+v", m)
	}
	if m := done.Thread[1]; m.By != "agent" || m.Text != "rephrased" || m.At != nil {
		t.Fatalf("expected the reply as an undated agent message, got %+v", m)
	}
	if len(open.Thread) != 1 || open.Thread[0].Text != "too long" || open.Status != "" {
		t.Fatalf("expected the unanswered entry as a one-message open thread, got %+v", open)
	}

	entries, _ := json.Marshal(resp.Annotations)
	f.put(string(entries))
	stored := f.sidecar()
	if !strings.Contains(stored, `"version": 2`) || !strings.Contains(stored, `"by": "agent"`) || !strings.Contains(stored, `thread: [{ by:`) {
		t.Fatalf("expected a version 2 sidecar with the thread header, got %s", stored)
	}
	if strings.Contains(stored, `"comment"`) || strings.Contains(stored, `"reply"`) {
		t.Fatalf("expected the version 1 fields gone from the sidecar, got %s", stored)
	}
}

func TestAnnotationsDropWhatTheBrowserDropped(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	two := f.put(`[{"exact":"alpha","thread":[{"by":"reader","text":"one"}]},{"exact":"bravo","thread":[{"by":"reader","text":"two"}]}]`).Annotations
	if len(two) != 2 {
		t.Fatalf("expected two entries, got %+v", two)
	}

	// A duplicated id must not be written twice, a deleted one must not come back, a new
	// entry with nothing said (or only an agent message) must not be created.
	entries, _ := json.Marshal([]annotation{{ID: two[1].ID, Thread: []message{reader("two")}}, {ID: two[1].ID, Thread: []message{reader("two")}}, {ID: "unknown0", Thread: []message{reader("resurrected?")}}, {Exact: str("alpha")}, {Exact: str("alpha"), Thread: []message{{By: "agent", Text: "forged"}}}})
	kept := f.put(string(entries)).Annotations
	if len(kept) != 1 || kept[0].ID != two[1].ID {
		t.Fatalf("expected only the second entry to remain, once, got %+v", kept)
	}

	if empty := f.put(`[]`).Annotations; len(empty) != 0 {
		t.Fatalf("expected an empty list, got %+v", empty)
	}
	if _, err := os.Stat(filepath.Join(f.dir, sidecarName)); !os.IsNotExist(err) {
		t.Fatalf("expected the sidecar to be removed once empty, got %v", err)
	}
	rec := f.annotations(http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"annotations":[]`) {
		t.Fatalf("expected GET without a sidecar to answer an empty list, got %d %s", rec.Code, rec.Body)
	}
}

func TestAnnotationsRefuseForeignRequests(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")

	if rec := f.annotations(http.MethodPut, `{"annotations":[]}`, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected a cross-origin PUT to be refused with 403, got %d", rec.Code)
	}
	if rec := f.annotations(http.MethodGet, "", "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected a cross-origin GET to be refused with 403, got %d", rec.Code)
	}
	if rec := f.annotations(http.MethodPut, `{"annotations":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected a broken body to be refused with 400, got %d", rec.Code)
	}
	if rec := f.annotations(http.MethodPost, `{"annotations":[]}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected POST to be refused with 405, got %d", rec.Code)
	}
	for target, code := range map[string]int{"//evil.example/doc.md": 400, "/doc.txt": 400, "/missing.md": 404} {
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/__annotations?path="+target, strings.NewReader(`{"annotations":[]}`)))
		if rec.Code != code {
			t.Errorf("expected %q to be refused with %d, got %d", target, code, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, sidecarName)); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar after refused requests, got %v", err)
	}
	// http.Dir folds ".." away; the sidecar must land where the markdown is read from.
	if got, want := sidecarPath(http.Dir(f.dir), "/../doc.md"), filepath.Join(f.dir, sidecarName); got != want {
		t.Fatalf("expected the sidecar confined to the served directory, got %q", got)
	}
}

func TestPageCarriesTheAnnotationControls(t *testing.T) {
	t.Parallel()

	content := "# Title\n\nalpha bravo\n"
	f := newDiffFixture(t, content)

	body := f.get(docPath)
	if !strings.Contains(body, `id="annotate-toggle"`) {
		t.Fatalf("expected the capture toggle on every page, got %q", body)
	}
	if !strings.Contains(body, `data-file-hash="`+hashOf(content)+`"`) {
		t.Fatalf("expected the page to carry the file hash, got %q", body)
	}
	if !strings.Contains(body, `class="toolbar-row annotations-nav" hidden`) {
		t.Fatalf("expected the navigation row hidden without annotations, got %q", body)
	}
	for _, want := range []string{`<script src="/static/js/annotate.js"></script>`, `<link rel="stylesheet" href="/static/css/annotate.css" />`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q on every page, got %q", want, body)
		}
	}

	f.put(`[{"exact":"alpha","thread":[{"by":"reader","text":"one"}]}]`)
	body = f.get(docPath)
	if strings.Contains(body, `class="toolbar-row annotations-nav" hidden`) || !strings.Contains(body, `class="toolbar-row annotations-nav"`) {
		t.Fatalf("expected the navigation row shown with annotations, got %q", body)
	}
}

// An agent that rewrites the file through a JSON5 library (bare keys, single quotes,
// trailing commas, no header) must still be read, and the header must come back.
func TestAnnotationsReadWhatAJSON5LibraryWrites(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	first := f.put(`[{"exact":"alpha bravo","thread":[{"by":"reader","text":"rephrase"}]}]`).Annotations[0]

	agent := `/* rewritten by an agent */
{
  version: 2,
  file: 'doc.md',
  annotations: [
    { id: '` + first.ID + `', exact: 'alpha', prefix: null, suffix: null, lines: [3, 3],
      file_hash: '` + first.FileHash + `', status: 'done',
      thread: [
        { by: 'reader', at: '` + *first.Thread[0].At + `', text: 'rephrase' },
        { by: 'agent', at: '2026-09-07T12:50:00+02:00', text: "it's shorter", },
      ], },
  ],
}
`
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := f.annotations(http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"it's shorter"`) {
		t.Fatalf("expected the JSON5 sidecar to be read, got %d %s", rec.Code, rec.Body)
	}

	entries, _ := json.Marshal([]annotation{{ID: first.ID, Thread: []message{reader("rephrase")}}})
	if a := f.put(string(entries)).Annotations[0]; a.Status != "done" || *a.Exact != "alpha" {
		t.Fatalf("expected the agent's JSON5 edit to be merged, got %+v", a)
	}
	if stored := f.sidecar(); !strings.HasPrefix(stored, "// grip-live-diff annotations") {
		t.Fatalf("expected the header to be written back, got %s", stored)
	}
}

func TestSidecarNameStaysOnOneLine(t *testing.T) {
	t.Parallel()

	if got := emptySidecar("/odd\nname.md").File; got != "odd name.md" {
		t.Fatalf("expected the newline folded away from the header, got %q", got)
	}
}

// Every version with entries against it is kept beside the sidecar, and only those.
func TestAnnotationsKeepTheAnnotatedVersions(t *testing.T) {
	t.Parallel()

	v1 := "# Title\n\nalpha bravo\n"
	f := newDiffFixture(t, v1)
	revDir := filepath.Join(f.dir, "doc.annotations.d")
	nameOf := func(content string) string { return revisionName(hashOf(content)) }

	resp := f.put(`[{"exact":"alpha","thread":[{"by":"reader","text":"one"}]}]`)
	first := resp.Annotations[0]
	if got, err := os.ReadFile(filepath.Join(revDir, nameOf(v1))); err != nil || string(got) != v1 {
		t.Fatalf("expected a copy of the annotated version, got %q (%v)", got, err)
	}
	if resp.Revisions[hashOf(v1)] != "doc.annotations.d/"+nameOf(v1) || len(resp.Revisions) != 1 {
		t.Fatalf("expected revisions to map the hash to the copy, got %v", resp.Revisions)
	}
	if !strings.Contains(f.sidecar(), `"revisions"`) {
		t.Fatalf("expected revisions in the sidecar, got %s", f.sidecar())
	}

	// A second entry on the same version: still one copy. A forged map is ignored.
	entries, _ := json.Marshal(map[string]any{"revisions": map[string]string{"sha256:forged": "../../etc/passwd"}, "annotations": []annotation{first, {Exact: str("bravo"), Thread: []message{reader("two")}}}})
	rec := f.annotations(http.MethodPut, string(entries))
	var second annotationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if names, _ := os.ReadDir(revDir); len(names) != 1 || len(second.Revisions) != 1 {
		t.Fatalf("expected one copy for one version, got %d files, %v", len(names), second.Revisions)
	}

	// The file moves on and gets a third entry: two versions kept.
	v2 := "# Title\n\nalpha bravo\n\ncharlie\n"
	f.write(v2)
	entries, _ = json.Marshal(append(second.Annotations, annotation{Exact: str("charlie"), Thread: []message{reader("three")}}))
	third := f.put(string(entries))
	if names, _ := os.ReadDir(revDir); len(names) != 2 || len(third.Revisions) != 2 || third.Revisions[hashOf(v2)] != "doc.annotations.d/"+nameOf(v2) {
		t.Fatalf("expected two versions kept, got %d files, %v", len(names), third.Revisions)
	}

	// The two entries on the first version go: its copy goes with them.
	entries, _ = json.Marshal(third.Annotations[2:])
	fourth := f.put(string(entries))
	if _, err := os.Stat(filepath.Join(revDir, nameOf(v1))); !os.IsNotExist(err) {
		t.Fatalf("expected the unreferenced copy removed, got %v", err)
	}
	if len(fourth.Revisions) != 1 || fourth.Revisions[hashOf(v1)] != "" {
		t.Fatalf("expected only the second version left, got %v", fourth.Revisions)
	}

	// Last entry deleted: sidecar and directory gone.
	f.put(`[]`)
	for _, p := range []string{filepath.Join(f.dir, sidecarName), revDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, got %v", p, err)
		}
	}
}

func TestRevisionName(t *testing.T) {
	t.Parallel()

	for hash, want := range map[string]string{
		"sha256:0123456789abcdef": "0123456789ab.md",
		"sha256:../../etc/passwd": "",
		"sha256:short":            "",
		"":                        "",
		"0123456789abcdef":        "0123456789ab.md",
	} {
		if got := revisionName(hash); got != want {
			t.Errorf("%q: expected %q, got %q", hash, want, got)
		}
	}
}

// A copy is not a document: it cannot be annotated, and a hash the server cannot back
// (a version 1 entry whose version is gone) simply has no copy.
func TestAnnotationsRevisionsEdgeCases(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha\n")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/__annotations?path=/doc.annotations.d/abcdef012345.md", strings.NewReader(`{"annotations":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected PUT on a revision to be refused with 400, got %d", rec.Code)
	}

	v1 := `{"version": 1, "file": "doc.md", "annotations": [{"id": "old00000", "exact": "gone", "prefix": "", "suffix": "", "lines": null,
	  "comment": "lost version", "created": "2026-09-07T12:00:00+02:00", "updated": null, "file_hash": "sha256:00000000000000000000000000000000"}]}`
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = f.annotations(http.MethodGet, "")
	var got annotationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Revisions) != 0 {
		t.Fatalf("expected the entry without a revision, got %d %s", rec.Code, rec.Body)
	}
	entries, _ := json.Marshal(got.Annotations)
	if saved := f.put(string(entries)); len(saved.Revisions) != 0 || len(saved.Annotations) != 1 {
		t.Fatalf("expected no copy for a version the server never saw, got %+v", saved)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "doc.annotations.d")); !os.IsNotExist(err) {
		t.Fatalf("expected no revisions directory, got %v", err)
	}
}
