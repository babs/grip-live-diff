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

	resp := f.put(`[{"exact":"alpha bravo","prefix":"","suffix":"","comment":"rephrase"}]`)
	if len(resp.Annotations) != 1 {
		t.Fatalf("expected one entry, got %+v", resp.Annotations)
	}
	a := resp.Annotations[0]
	if len(a.ID) != 8 {
		t.Fatalf("expected an 8-character id, got %q", a.ID)
	}
	created, err := time.Parse(time.RFC3339, a.Created)
	if err != nil || created.Before(before) {
		t.Fatalf("expected a fresh RFC 3339 created timestamp, got %q (%v)", a.Created, err)
	}
	if a.Updated != nil {
		t.Fatalf("expected no updated timestamp on a new entry, got %q", *a.Updated)
	}
	if want := fileHash([]byte(content)); a.FileHash != want || resp.Hash != want || !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("expected file_hash and hash %q, got %q and %q", want, a.FileHash, resp.Hash)
	}
	if a.Lines == nil || *a.Lines != [2]int{3, 3} {
		t.Fatalf("expected lines [3 3], got %v", a.Lines)
	}
	if a.Comment != "rephrase" || a.Exact == nil || *a.Exact != "alpha bravo" {
		t.Fatalf("expected the quote and comment to be kept, got %+v", a)
	}
	if resp.Version != 1 || resp.File != "doc.md" {
		t.Fatalf("expected version 1 for doc.md, got %d %q", resp.Version, resp.File)
	}

	stored := f.sidecar()
	if !strings.Contains(stored, `"id": "`+a.ID+`"`) || strings.Contains(stored, `"status"`) || strings.Contains(stored, `"reply"`) {
		t.Fatalf("expected the sidecar to carry the entry without agent fields, got %s", stored)
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

func TestAnnotationsEditKeepsTheAgentFields(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n\ncharlie delta\n")
	first := f.put(`[{"exact":"alpha bravo","comment":"rephrase"}]`).Annotations[0]

	// The agent answers, re-anchors the quote, and the file moves on.
	agent := strings.Replace(f.sidecar(), `"comment": "rephrase"`, `"comment": "rephrase", "status": "done", "reply": "done it"`, 1)
	agent = strings.Replace(agent, `"exact": "alpha bravo"`, `"exact": "charlie delta"`, 1)
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("# Title\n\nalpha bravo\n\nintro\n\ncharlie delta\n")

	// The browser still holds the old quote and only edited the comment.
	entries, _ := json.Marshal([]annotation{{ID: first.ID, Exact: str("alpha bravo"), Comment: "rephrase, shorter"}})
	resp := f.put(string(entries))
	if len(resp.Annotations) != 1 {
		t.Fatalf("expected one entry, got %+v", resp.Annotations)
	}
	a := resp.Annotations[0]
	if a.Comment != "rephrase, shorter" || a.Updated == nil {
		t.Fatalf("expected the edited comment with an updated timestamp, got %+v", a)
	}
	if a.Created != first.Created || a.FileHash != first.FileHash {
		t.Fatalf("expected created and file_hash untouched, got %+v", a)
	}
	if a.Status != "done" || a.Reply != "done it" || a.Exact == nil || *a.Exact != "charlie delta" {
		t.Fatalf("expected the agent's status, reply and quote to win, got %+v", a)
	}
	if a.Lines == nil || *a.Lines != [2]int{7, 7} {
		t.Fatalf("expected lines recomputed on the edited file, got %v", a.Lines)
	}

	// Same comment again: not an edit.
	entries, _ = json.Marshal([]annotation{{ID: first.ID, Comment: "rephrase, shorter"}})
	if again := f.put(string(entries)).Annotations[0]; *again.Updated != *a.Updated {
		t.Fatalf("expected an unchanged comment to keep its updated timestamp, got %q then %q", *a.Updated, *again.Updated)
	}
}

func TestAnnotationsDropWhatTheBrowserDropped(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	two := f.put(`[{"exact":"alpha","comment":"one"},{"exact":"bravo","comment":"two"}]`).Annotations
	if len(two) != 2 {
		t.Fatalf("expected two entries, got %+v", two)
	}

	// A duplicated id must not be written twice, a deleted one must not come back.
	entries, _ := json.Marshal([]annotation{{ID: two[1].ID, Comment: "two"}, {ID: two[1].ID, Comment: "two"}, {ID: "unknown0", Comment: "resurrected?"}})
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

	f.put(`[{"exact":"alpha","comment":"one"}]`)
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
	first := f.put(`[{"exact":"alpha bravo","comment":"rephrase"}]`).Annotations[0]

	agent := `/* rewritten by an agent */
{
  version: 1,
  file: 'doc.md',
  annotations: [
    { id: '` + first.ID + `', exact: 'alpha', prefix: null, suffix: null, lines: [3, 3],
      comment: 'rephrase', created: '` + first.Created + `', updated: null,
      file_hash: '` + first.FileHash + `', status: 'done', reply: "it's shorter", },
  ],
}
`
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := f.annotations(http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reply":"it's shorter"`) {
		t.Fatalf("expected the JSON5 sidecar to be read, got %d %s", rec.Code, rec.Body)
	}

	entries, _ := json.Marshal([]annotation{{ID: first.ID, Comment: "rephrase"}})
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
