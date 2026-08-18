package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// diffFixture serves docPath out of a temp dir and lets the test rewrite it on the fly.
type diffFixture struct {
	t       *testing.T
	dir     string
	handler http.Handler
}

const docPath = "/doc.md"

// Attribute order and spacing are the template's business, not the assertion's.
var markReadDisabled = regexp.MustCompile(`<button[^>]*mark-read[^>]*\bdisabled`)

func newDiffFixture(t *testing.T, content string) *diffFixture {
	t.Helper()

	f := &diffFixture{t: t, dir: t.TempDir()}
	f.handler = NewServer("localhost", 6419, false, false, false, NewParser()).newHandler(http.Dir(f.dir))
	f.write(content)
	return f
}

// newGitDiffFixture commits the initial content, so ?diff=head has a reference. The repo
// must exist before the handler is built: that is when the git work tree is probed.
func newGitDiffFixture(t *testing.T, content string, commit bool) *diffFixture {
	t.Helper()

	f := &diffFixture{t: t, dir: t.TempDir()}
	f.write(content)
	f.git("init", "-q")
	if commit {
		f.git("add", "doc.md")
		f.git("-c", "user.email=go-grip@test", "-c", "user.name=go-grip",
			"-c", "commit.gpgsign=false", "commit", "-q", "-m", "initial")
	}
	f.handler = NewServer("localhost", 6419, false, false, false, NewParser()).newHandler(http.Dir(f.dir))
	return f
}

func (f *diffFixture) git(args ...string) {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", f.dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func (f *diffFixture) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "doc.md"), []byte(content), 0o644); err != nil {
		f.t.Fatalf("write doc.md: %v", err)
	}
}

func (f *diffFixture) get(target string) string {
	f.t.Helper()

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK {
		f.t.Fatalf("GET %s: expected status %d, got %d", target, http.StatusOK, recorder.Code)
	}
	return recorder.Body.String()
}

func (f *diffFixture) reset(path string, mode ...string) *httptest.ResponseRecorder {
	f.t.Helper()

	form := url.Values{"path": {path}}
	if len(mode) > 0 {
		form.Set("diff", mode[0])
	}
	body := strings.NewReader(form.Encode())
	req := httptest.NewRequest(http.MethodPost, "/__baseline", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	return recorder
}

func TestDiffModeAnnotatesChangesSinceOpen(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)

	f.write("# Title\n\nalpha charlie\n")
	body := f.get(docPath + "?diff=open")

	if !strings.Contains(body, `<ins class="gg-ins">charlie</ins>`) {
		t.Fatalf("expected the new word to be marked as inserted, got %q", body)
	}
	if !strings.Contains(body, `<del class="gg-del">bravo`) {
		t.Fatalf("expected the replaced word to be marked as deleted, got %q", body)
	}
	if !strings.Contains(body, "Diff mode: since file open") {
		t.Fatalf("expected the diff banner, got %q", body)
	}
	// The redirect back into the same mode depends on this hidden field.
	if !strings.Contains(body, `name="diff" value="open"`) {
		t.Fatalf("expected the mark-as-read form to carry the diff mode, got %q", body)
	}
	if markReadDisabled.MatchString(body) {
		t.Fatalf("expected an active mark-as-read button when there are changes, got %q", body)
	}
}

func TestNormalModeCarriesNoDiffMarkup(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)

	f.write("# Title\n\nalpha charlie\n")
	body := f.get(docPath)

	if strings.Contains(body, "gg-ins") || strings.Contains(body, "gg-del") {
		t.Fatalf("expected no diff markup outside diff mode, got %q", body)
	}
	if !strings.Contains(body, "diff-toggle-changed") {
		t.Fatalf("expected the toggle to be flagged as changed, got %q", body)
	}
}

func TestDiffLastOnlyShowsTheMostRecentEdit(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")
	f.get(docPath)
	f.write("# Title\n\ndelta charlie\n")
	f.get(docPath)

	since := f.get(docPath + "?diff=open")
	if !strings.Contains(since, `<ins class="gg-ins">delta charlie</ins>`) {
		t.Fatalf("expected both edits against the opened version, got %q", since)
	}

	last := f.get(docPath + "?diff=last")
	if !strings.Contains(last, `<ins class="gg-ins">delta</ins>`) {
		t.Fatalf("expected the last edit to be marked, got %q", last)
	}
	if strings.Contains(last, "charlie</ins>") {
		t.Fatalf("expected the earlier edit not to be marked, got %q", last)
	}
}

// A reload without an edit must not shift the "previous version" reference, otherwise the
// last-edit diff would silently degrade to "nothing changed".
func TestReloadWithoutEditKeepsThePreviousReference(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")
	f.get(docPath)
	f.get(docPath)
	f.get(docPath)

	body := f.get(docPath + "?diff=last")
	if !strings.Contains(body, `<ins class="gg-ins">charlie</ins>`) {
		t.Fatalf("expected the last edit to still be visible after reloads, got %q", body)
	}
}

func TestDiffModeWithoutChangesReportsNothing(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)

	body := f.get(docPath + "?diff=open")
	if !strings.Contains(body, "Diff mode: since file open") {
		t.Fatalf("expected the diff status line, got %q", body)
	}
	if strings.Contains(body, "gg-ins") || strings.Contains(body, "gg-del") {
		t.Fatalf("expected no diff markup for an unchanged file, got %q", body)
	}
	// Nothing to acknowledge: the mark-as-read button must not call for a click.
	if !markReadDisabled.MatchString(body) {
		t.Fatalf("expected a disabled mark-as-read button on an empty diff, got %q", body)
	}
}

func TestDiffHeadComparesWithTheLastCommit(t *testing.T) {
	t.Parallel()

	f := newGitDiffFixture(t, "# Title\n\nalpha bravo\n", true)
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")

	body := f.get(docPath + "?diff=" + diffModeHead)
	if !strings.Contains(body, `<ins class="gg-ins">charlie</ins>`) {
		t.Fatalf("expected the uncommitted change to be marked, got %q", body)
	}
	if !strings.Contains(body, "Diff mode: since last commit") {
		t.Fatalf("expected the commit banner, got %q", body)
	}
	// The git reference is not ours to move.
	if strings.Contains(body, "Mark as read") {
		t.Fatalf("expected no reset button against the commit reference, got %q", body)
	}
}

// A file the repository does not know about has no committed version: say so instead of
// reporting the whole document as new.
func TestDiffHeadOnAnUntrackedFileReportsNoReference(t *testing.T) {
	t.Parallel()

	f := newGitDiffFixture(t, "# Title\n\nalpha bravo\n", false)
	f.get(docPath)

	body := f.get(docPath + "?diff=" + diffModeHead)
	if !strings.Contains(body, "Diff mode: no committed version to compare with") {
		t.Fatalf("expected the missing-reference banner, got %q", body)
	}
	if strings.Contains(body, "gg-ins") || strings.Contains(body, "gg-del") {
		t.Fatalf("expected no diff markup without a reference, got %q", body)
	}
}

// A git that cannot run is a broken reference, not a missing one: the banner must not
// claim the file was never committed.
func TestDiffHeadWithBrokenGitReportsUnreadableReference(t *testing.T) {
	f := newGitDiffFixture(t, "# Title\n\nalpha bravo\n", true)
	f.get(docPath)

	t.Setenv("PATH", "")
	body := f.get(docPath + "?diff=" + diffModeHead)
	if !strings.Contains(body, "Diff mode: commit reference could not be read") {
		t.Fatalf("expected the unreadable-reference banner, got %q", body)
	}
	if strings.Contains(body, "no committed version") {
		t.Fatalf("expected no missing-version claim when git is broken, got %q", body)
	}
}

func TestDiffToggleSkipsTheCommitStateOutsideAGitRepository(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	if isGitWorkTree(http.Dir(f.dir)) {
		t.Skip("TMPDIR sits inside a git work tree: this test needs a directory outside any repository")
	}

	if body := f.get(docPath + "?diff=" + diffModeLast); !strings.Contains(body, `href="`+docPath+`"`) {
		t.Fatalf("expected the toggle to cycle back to the plain document, got %q", body)
	}
	if body := f.get(docPath + "?diff=" + diffModeHead); strings.Contains(body, "diff-status") {
		t.Fatalf("expected the commit reference to be unavailable, got %q", body)
	}
}

func TestResetBaselineClearsTheChangedState(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")
	f.get(docPath)

	recorder := f.reset(docPath)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("expected status %d, got %d", http.StatusSeeOther, recorder.Code)
	}
	if got := recorder.Header().Get("Location"); got != docPath {
		t.Fatalf("expected a redirect back to %s, got %q", docPath, got)
	}

	body := f.get(docPath + "?diff=open")
	// An empty diff disables the mark-as-read button: that is the "all read" signal.
	if !markReadDisabled.MatchString(body) {
		t.Fatalf("expected the reset to clear the changes, got %q", body)
	}
}

func TestResetBaselineStaysInTheCurrentDiffMode(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")
	f.get(docPath)

	for mode, want := range map[string]string{
		diffModeOpen: docPath + "?diff=" + diffModeOpen,
		diffModeLast: docPath + "?diff=" + diffModeLast,
		// Unknown or empty modes fall back to the plain document.
		diffModeHead: docPath,
		"bogus":      docPath,
		"":           docPath,
	} {
		if got := f.reset(docPath, mode).Header().Get("Location"); got != want {
			t.Fatalf("mode %q: expected a redirect to %q, got %q", mode, want, got)
		}
	}
}

// Mermaid, MathJax and highlighted code must survive diff mode: annotating their content
// would stop them from rendering at all.
func TestDiffModeLeavesRenderedBlocksIntact(t *testing.T) {
	t.Parallel()

	const doc = "# Doc\n\n```mermaid\ngraph TD;\nA-->%s;\n```\n\nInline $\\sqrt{%s}$ math.\n\n" +
		"```go\nfmt.Println(\"%s\")\n```\n"

	f := newDiffFixture(t, fmt.Sprintf(doc, "B", "3x-1", "hello"))
	f.get(docPath)

	f.write(fmt.Sprintf(doc, "C", "4x-1", "world"))
	body := f.get(docPath + "?diff=open")

	if !strings.Contains(body, "<pre class=\"mermaid\">graph TD;\nA--&gt;C;\n</pre>") {
		t.Fatalf("expected the new mermaid block to stay intact, got %q", body)
	}
	if !strings.Contains(body, `<span class="math inline">\(\sqrt{4x-1}\)</span>`) {
		t.Fatalf("expected the new math span to stay intact, got %q", body)
	}
	if !strings.Contains(body, `<ins class="gg-ins">&#34;world&#34;</ins>`) {
		t.Fatalf("expected the code change to be annotated, got %q", body)
	}
	for _, opening := range []string{"<pre class=\"mermaid\"><ins", `<span class="math inline"><ins`} {
		if strings.Contains(body, opening) {
			t.Fatalf("expected no annotation inside %q, got %q", opening, body)
		}
	}
}

func TestResetBaselineRefusesCrossOriginRequests(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	f.get(docPath)
	f.write("# Title\n\nalpha charlie\n")
	f.get(docPath)

	body := strings.NewReader(url.Values{"path": {docPath}}.Encode())
	req := httptest.NewRequest(http.MethodPost, "/__baseline", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, recorder.Code)
	}

	if page := f.get(docPath + "?diff=open"); !strings.Contains(page, "gg-ins") {
		t.Fatalf("expected the reference to survive the refused request, got %q", page)
	}
}

func TestResetBaselineRejectsForeignTargets(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")

	for _, target := range []string{"//evil.example/doc.md", "https://evil.example/doc.md", "/doc.txt", "/missing.md"} {
		if code := f.reset(target).Code; code == http.StatusSeeOther {
			t.Fatalf("expected %q to be rejected, got status %d", target, code)
		}
	}

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__baseline?path="+docPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET to be refused, got status %d", recorder.Code)
	}
}

func TestDirectoryListingIgnoresCacheValidators(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	server := NewServer("localhost", 6419, false, false, false, NewParser())
	handler := server.newHandler(http.Dir(tmpDir))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("expected Cache-Control to disable storage, got %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "README.md") {
		t.Fatalf("expected directory listing body to mention README.md, got %q", recorder.Body.String())
	}
}

func TestRegularFileStillSupportsConditionalRequests(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "plain.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write plain.txt: %v", err)
	}

	server := NewServer("localhost", 6419, false, false, false, NewParser())
	handler := server.newHandler(http.Dir(tmpDir))

	req := httptest.NewRequest(http.MethodGet, "/plain.txt", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotModified {
		t.Fatalf("expected status %d, got %d", http.StatusNotModified, recorder.Code)
	}
}

func TestMarkdownResponsesDisableCaching(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	server := NewServer("localhost", 6419, false, false, false, NewParser())
	handler := server.newHandler(http.Dir(tmpDir))

	req := httptest.NewRequest(http.MethodGet, "/README.md", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("expected Cache-Control to disable storage, got %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html" {
		t.Fatalf("expected text/html response, got %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "Hello") {
		t.Fatalf("expected rendered markdown response to contain document content, got %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "<title>README</title>") {
		t.Fatalf("expected filename-based HTML title, got %q", recorder.Body.String())
	}
}

func TestFormatFilenameTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{name: "humanizes separators", filename: "my-guide_v2.md", want: "My Guide V2"},
		{name: "uses basename", filename: "/docs/getting-started.md", want: "Getting Started"},
		{name: "preserves acronym", filename: "README.md", want: "README"},
		{name: "strips uppercase extension", filename: "release_notes.MD", want: "Release Notes"},
		{name: "collapses separators and whitespace", filename: "  release---notes__v2.md", want: "Release Notes V2"},
		{name: "supports unicode", filename: "überblick-plan.md", want: "Überblick Plan"},
		{name: "falls back for empty stem", filename: ".md", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatFilenameTitle(tt.filename); got != tt.want {
				t.Fatalf("formatFilenameTitle(%q) = %q, want %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestFilenameTitleResponses(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	files := []string{"my-guide_v2.md", "unsafe-<script>.md", ".md"}
	for _, filename := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, filename), []byte("# Hello\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
	}

	server := NewServer("localhost", 6419, false, false, false, NewParser())
	handler := server.newHandler(http.Dir(tmpDir))

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "humanized title", path: "/my-guide_v2.md", want: "<title>My Guide V2</title>"},
		{name: "escaped title", path: "/unsafe-%3Cscript%3E.md", want: "<title>Unsafe &lt;script&gt;</title>"},
		{name: "empty title fallback", path: "/.md", want: "<title>" + defaultHTMLTitle + "</title>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), tt.want) {
				t.Fatalf("expected response to contain %q, got %q", tt.want, recorder.Body.String())
			}
		})
	}
}
