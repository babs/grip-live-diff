package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const readyName = "doc.annotations.ready"

func (f *diffFixture) ready(method string, origin ...string) *httptest.ResponseRecorder {
	f.t.Helper()

	req := httptest.NewRequest(method, "/__annotations/ready?path="+docPath, nil)
	if len(origin) > 0 {
		req.Header.Set("Origin", origin[0])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// writeSidecarFile writes, as an agent would, a sidecar of one entry whose thread ends with
// a message by last ("done": the reader's, then closed by the agent); trigger "" leaves the
// key out.
func (f *diffFixture) writeSidecarFile(trigger, last string) {
	f.t.Helper()

	key := ""
	if trigger != "" {
		key = fmt.Sprintf("trigger: %q,", trigger)
	}
	thread := `{by: "reader", at: null, text: "fix"}`
	status := ""
	switch last {
	case byAgent:
		thread += `, {by: "agent", at: null, text: "done"}`
	case "done":
		status = `status: "done",`
	}
	body := fmt.Sprintf(`{version: 2, file: "doc.md", %s annotations: [{id: "a1b2c3d4", exact: null, file_hash: "", %s thread: [%s]}]}`, key, status, thread)
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *diffFixture) writeReady(content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, readyName), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// readyContent is the ready file on disk, "" when absent.
func (f *diffFixture) readyContent() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, readyName))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *diffFixture) getAnnotations() annotationsResponse {
	f.t.Helper()
	rec := f.annotations(http.MethodGet, "")
	var resp annotationsResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		f.t.Fatalf("GET: expected 200 and JSON, got %d %s", rec.Code, rec.Body)
	}
	return resp
}

// exitedPID is the PID of a process that ran and was reaped: nothing runs under it.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestTriggerIsKeptAcrossSaves(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")

	// 010-AC1: no sidecar, and a sidecar without trigger, read as auto.
	if resp := f.getAnnotations(); resp.Trigger != triggerAuto || resp.Ready.State != readyNone {
		t.Fatalf("expected auto and no ready file without a sidecar, got %q %+v", resp.Trigger, resp.Ready)
	}
	// An agent breaking the rule with a value of its own reads as auto too.
	for _, trigger := range []string{"", "Manual", "weekly"} {
		f.writeSidecarFile(trigger, byReader)
		if resp := f.getAnnotations(); resp.Trigger != triggerAuto {
			t.Fatalf("expected trigger %q to read as auto, got %q", trigger, resp.Trigger)
		}
	}

	entries := f.put(`[{"exact":"alpha bravo","thread":[{"by":"reader","text":"rephrase"}]}]`).Annotations
	if !strings.Contains(f.sidecar(), `"trigger": "auto"`) {
		t.Fatalf("expected the sidecar to state auto, got %s", f.sidecar())
	}

	// 010-AC2: switching to manual with the current list leaves the entries as they were.
	list, _ := json.Marshal(entries)
	rec := f.annotations(http.MethodPut, `{"trigger":"manual","annotations":`+string(list)+`}`)
	var resp annotationsResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("expected the switch to manual to succeed, got %d %s", rec.Code, rec.Body)
	}
	if resp.Trigger != triggerManual || !reflect.DeepEqual(resp.Annotations, entries) {
		t.Fatalf("expected manual with the entries unchanged, got %q %+v", resp.Trigger, resp.Annotations)
	}
	if !strings.Contains(f.sidecar(), `"trigger": "manual"`) {
		t.Fatalf("expected the sidecar to state manual, got %s", f.sidecar())
	}

	// 010-AC3: a save without trigger keeps it.
	reply := `[{"id":"` + entries[0].ID + `","thread":[{"by":"reader","text":"rephrase"},{"by":"reader","text":"and shorter"}]}]`
	if got := f.put(reply); got.Trigger != triggerManual || len(got.Annotations[0].Thread) != 2 {
		t.Fatalf("expected the save to keep manual, got %q %+v", got.Trigger, got.Annotations)
	}

	// An invalid trigger writes nothing.
	before := f.sidecar()
	for _, body := range []string{`{"trigger":"often","annotations":[]}`, `{"trigger":"","annotations":[]}`, `{"trigger":1,"annotations":[]}`} {
		if rec := f.annotations(http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", body, rec.Code)
		}
	}
	if f.sidecar() != before {
		t.Fatalf("expected a refused trigger to leave the sidecar, got %s", f.sidecar())
	}

	// A manual sidecar with no entry left still holds the mode.
	if got := f.put(`[]`); got.Trigger != triggerManual {
		t.Fatalf("expected manual to survive an empty list, got %q", got.Trigger)
	}
	if resp := f.getAnnotations(); resp.Trigger != triggerManual {
		t.Fatalf("expected the empty manual sidecar kept on disk, got %q", resp.Trigger)
	}
}

// The mode button sends trigger alone: the list must survive it; an explicit [] still empties.
func TestTriggerOnlySaveKeepsTheEntries(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	entries := f.put(`[{"exact":"alpha bravo","thread":[{"by":"reader","text":"rephrase"}]}]`).Annotations
	for _, body := range []string{`{"trigger":"manual"}`, `{"trigger":"auto","annotations":null}`} {
		rec := f.annotations(http.MethodPut, body)
		var resp annotationsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || !reflect.DeepEqual(resp.Annotations, entries) {
			t.Fatalf("%s: expected the entries unchanged, got %d %s", body, rec.Code, rec.Body)
		}
		if !strings.Contains(f.sidecar(), entries[0].ID) || len(resp.Revisions) != 1 {
			t.Fatalf("%s: expected the entry and its revision kept on disk, got %s", body, f.sidecar())
		}
	}
	if got := f.put(`[]`); len(got.Annotations) != 0 {
		t.Fatalf("expected an explicit empty list to empty it, got %+v", got.Annotations)
	}
	if _, err := os.Stat(filepath.Join(f.dir, sidecarName)); !os.IsNotExist(err) {
		t.Fatalf("expected the empty auto sidecar removed, got %v", err)
	}
}

// 010-AC9
func TestSwitchingToAutoRemovesTheReadyFile(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	if rec := f.ready(http.MethodPost); rec.Code != http.StatusOK || f.readyContent() == "" {
		t.Fatalf("expected the batch sent, got %d %s", rec.Code, rec.Body)
	}
	// A save in manual leaves it.
	f.put(`[{"id":"a1b2c3d4","thread":[{"by":"reader","text":"fix"}]}]`)
	if f.readyContent() == "" {
		t.Fatal("expected a save in manual to keep the ready file")
	}
	// Manual again (a double click, a second tab) is not a switch.
	if rec := f.annotations(http.MethodPut, `{"trigger":"manual"}`); rec.Code != http.StatusOK || f.readyContent() == "" {
		t.Fatalf("expected manual over manual to keep the ready file, got %d", rec.Code)
	}
	rec := f.annotations(http.MethodPut, `{"trigger":"auto","annotations":[{"id":"a1b2c3d4","thread":[{"by":"reader","text":"fix"}]}]}`)
	if rec.Code != http.StatusOK || f.readyContent() != "" {
		t.Fatalf("expected the switch to auto to remove the ready file, got %d %q", rec.Code, f.readyContent())
	}
	if !strings.Contains(rec.Body.String(), `"ready":{"state":"none"`) {
		t.Fatalf("expected the response to report no ready file, got %s", rec.Body)
	}
}

// 010-AC9 on the empty-list path: the sidecar goes, the batch with it.
func TestSwitchingToAutoWithNoEntryRemovesTheReadyFile(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	if rec := f.ready(http.MethodPost); rec.Code != http.StatusOK || f.readyContent() == "" {
		t.Fatalf("expected the batch sent, got %d %s", rec.Code, rec.Body)
	}
	if rec := f.annotations(http.MethodPut, `{"trigger":"auto","annotations":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("expected the switch to answer 200, got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, sidecarName)); !errors.Is(err, os.ErrNotExist) || f.readyContent() != "" {
		t.Fatalf("expected the sidecar and the ready file gone, got %v %q", err, f.readyContent())
	}
}

// A ready file in auto is dropped on the switch to manual, so it cannot fire as a batch.
func TestSwitchingToManualDropsAStaleReadyFile(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerAuto, byReader)
	if err := os.WriteFile(filepath.Join(f.dir, readyName), []byte(`{sent_at: "2000-01-01T00:00:00Z", pid: null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := f.annotations(http.MethodPut, `{"trigger":"manual"}`); rec.Code != http.StatusOK || f.readyContent() != "" {
		t.Fatalf("expected the switch to manual to drop the stale ready file, got %d %q", rec.Code, f.readyContent())
	}
}

// A ready file left behind its sidecar is dropped by the next auto save: kept, it would fire as
// a batch nobody sent on the next switch to manual.
func TestAutoSaveDropsAnOrphanReadyFile(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	if rec := f.ready(http.MethodPost); rec.Code != http.StatusOK || f.readyContent() == "" {
		t.Fatalf("expected the batch sent, got %d %s", rec.Code, rec.Body)
	}
	if err := os.Remove(filepath.Join(f.dir, sidecarName)); err != nil {
		t.Fatal(err)
	}
	rec := f.annotations(http.MethodPut, `{"trigger":"auto","annotations":[{"thread":[{"by":"reader","text":"fix"}]}]}`)
	if rec.Code != http.StatusOK || f.readyContent() != "" {
		t.Fatalf("expected the auto save to drop the orphan, got %d %q", rec.Code, f.readyContent())
	}
}

func TestReadyPost(t *testing.T) {
	t.Parallel()

	gone := exitedPID(t)
	for _, tc := range []struct {
		name, trigger, last, ready string
		code                       int
		// The ready file is expected rewritten as a fresh batch; otherwise left as it was.
		fresh bool
	}{
		{"010-AC4 manual, open entry", triggerManual, byReader, "", http.StatusOK, true},
		{"010-AC5 manual, entry answered", triggerManual, byAgent, "", http.StatusConflict, false},
		{"010-AC5 manual, entry closed by the agent", triggerManual, "done", "", http.StatusConflict, false},
		{"010-AC6 auto", triggerAuto, byReader, "", http.StatusConflict, false},
		{"010-AC6 no trigger", "", byReader, "", http.StatusConflict, false},
		{"010-AC7 already sent", triggerManual, byReader, `{"sent_at": "2000-01-01T00:00:00Z", "pid": null}`, http.StatusOK, false},
		{"010-AC7 agent working", triggerManual, byReader, fmt.Sprintf(`{sent_at: "2000-01-01T00:00:00Z", pid: %d}`, os.Getpid()), http.StatusOK, false},
		{"010-AC21 garbage reads as sent", triggerManual, byReader, `not json`, http.StatusOK, false},
		{"010-AC20 agent gone", triggerManual, byReader, fmt.Sprintf(`{sent_at: "2000-01-01T00:00:00Z", pid: %d}`, gone), http.StatusOK, true},
		{"010-AC20 pid 0", triggerManual, byReader, `{sent_at: "2000-01-01T00:00:00Z", pid: 0}`, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newDiffFixture(t, "# Title\n")
			f.writeSidecarFile(tc.trigger, tc.last)
			if tc.ready != "" {
				f.writeReady(tc.ready)
			}
			before := time.Now().Add(-time.Second)
			rec := f.ready(http.MethodPost)
			if rec.Code != tc.code {
				t.Fatalf("expected %d, got %d %s", tc.code, rec.Code, rec.Body)
			}
			got := f.readyContent()
			if !tc.fresh {
				if got != tc.ready {
					t.Fatalf("expected the ready file left as %q, got %q", tc.ready, got)
				}
				return
			}
			var rf map[string]any
			if err := json.Unmarshal([]byte(got), &rf); err != nil {
				t.Fatalf("expected a JSON ready file, got %q", got)
			}
			sent, err := time.Parse(time.RFC3339, fmt.Sprint(rf["sent_at"]))
			if v, ok := rf["pid"]; !ok || v != nil || err != nil || sent.Before(before) {
				t.Fatalf("expected a fresh sent_at and pid null, got %q", got)
			}
			// An agent running as another user must be able to read it.
			if fi, err := os.Stat(filepath.Join(f.dir, readyName)); runtime.GOOS != "windows" && (err != nil || fi.Mode().Perm() != 0o644) {
				t.Fatalf("expected the ready file 0644, got %v %v", fi.Mode(), err)
			}
			var st readyState
			if json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.State != readySent || st.PID != nil {
				t.Fatalf("expected the response to report sent, got %s", rec.Body)
			}
		})
	}
}

// 010-AC7
func TestReadyCancel(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	f.writeReady(fmt.Sprintf(`{sent_at: "2000-01-01T00:00:00Z", pid: %d}`, os.Getpid()))
	for range 2 {
		rec := f.ready(http.MethodDelete)
		if rec.Code != http.StatusOK || f.readyContent() != "" || !strings.Contains(rec.Body.String(), `"state":"none"`) {
			t.Fatalf("expected cancel to remove the ready file, idempotent, got %d %s %q", rec.Code, rec.Body, f.readyContent())
		}
	}
}

// 010-AC19, 010-AC21
func TestReadyState(t *testing.T) {
	t.Parallel()

	gone := exitedPID(t)
	for _, tc := range []struct {
		name, content, want string
	}{
		{"absent", "", readyNone},
		{"pid null", `{"sent_at": "2026-10-26T10:00:00Z", "pid": null}`, readySent},
		{"pid absent, JSON5", `{sent_at: '2026-10-26T10:00:00Z',}`, readySent},
		{"live pid", fmt.Sprintf(`{sent_at: "2026-10-26T10:00:00Z", pid: %d, claimed_at: "2026-10-26T10:01:00Z"}`, os.Getpid()), readyWorking},
		{"exited pid", fmt.Sprintf(`{sent_at: "2026-10-26T10:00:00Z", pid: %d}`, gone), readyGone},
		{"pid 0", `{pid: 0}`, readyGone},
		{"negative pid", `{pid: -1}`, readyGone},
		{"pid past int32", `{pid: 4294967297}`, readyGone},
		{"garbage", `{pid: `, readySent},
		{"empty file", " ", readySent},
		{"pid a string", `{pid: "12"}`, readySent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newDiffFixture(t, "# Title\n")
			if tc.content != "" {
				f.writeReady(tc.content)
			}
			if got := f.getAnnotations().Ready; got.State != tc.want {
				t.Fatalf("expected %s, got %+v", tc.want, got)
			}
			if page := f.get(docPath); !strings.Contains(page, "Title") {
				t.Fatalf("expected the page to render, got %q", page)
			}
		})
	}
}

func TestReadyRefusals(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)

	if rec := f.ready(http.MethodPost, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("expected a cross-origin POST refused with 403, got %d", rec.Code)
	}
	if rec := f.ready(http.MethodDelete, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("expected a cross-origin DELETE refused with 403, got %d", rec.Code)
	}
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		if rec := f.ready(m); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected %s refused with 405, got %d", m, rec.Code)
		}
	}
	// 010-AC12: a kept revision, a path out of the served tree.
	if err := os.MkdirAll(filepath.Join(f.dir, "doc"+revisionsSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "doc"+revisionsSuffix, "0123456789ab.md"), []byte("# Old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for target, code := range map[string]int{
		"/doc" + revisionsSuffix + "/0123456789ab.md": 400,
		"//evil.example/doc.md":                       400,
		"/doc.txt":                                    400,
		"/missing.md":                                 404,
	} {
		for _, m := range []string{http.MethodPost, http.MethodDelete} {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, httptest.NewRequest(m, "/__annotations/ready?path="+target, nil))
			if rec.Code != code {
				t.Errorf("%s %q: expected %d, got %d", m, target, code, rec.Code)
			}
		}
	}
	if f.readyContent() != "" {
		t.Fatalf("expected no ready file after refused requests, got %q", f.readyContent())
	}
	matches, _ := filepath.Glob(filepath.Join(f.dir, "*", "*.ready"))
	if len(matches) != 0 {
		t.Fatalf("expected no ready file beside a revision, got %v", matches)
	}
	// http.Dir folds ".." away; the ready file must stay where the sidecar is.
	if got, want := readyPath(http.Dir(f.dir), "/../doc.md"), filepath.Join(f.dir, readyName); got != want {
		t.Fatalf("expected the ready file confined to the served directory, got %q", got)
	}
}

// 010-AC10
func TestSidecarHeaderStatesTheManualRule(t *testing.T) {
	t.Parallel()

	header := fmt.Sprintf(sidecarHeader, "doc.md")
	flat := strings.ReplaceAll(header, "\n// ", " ")
	for _, want := range []string{
		`"manual"`, "wait for the ready file", "-> .ready", "under the flock of this file, read the ready file",
		"Take your PID before the lock", "long-lived process", "$PPID, read outside flock -c",
		"absent -> the batch was cancelled, nothing to do, do not create it",
		"pid set -> another agent holds the batch, leave it",
		"Handle every open entry", "delete the ready file, last", "file_hash or trigger",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("expected the header to state %q", want)
		}
	}
	paragraph := header[strings.Index(header, "// trigger is"):]
	for _, line := range strings.Split(paragraph[:strings.Index(paragraph, "\n//\n")], "\n") {
		if len(line) > 100 {
			t.Errorf("header line is %d characters: %q", len(line), line)
		}
	}
}

// A sidecar or ready file the server cannot handle fails Go and cancel with 500; a save still stands.
func TestReadyErrors(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	if err := os.WriteFile(filepath.Join(f.dir, sidecarName), []byte(`{trigger: "manual", annotations: [`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := f.ready(http.MethodPost); rec.Code != http.StatusInternalServerError || f.readyContent() != "" {
		t.Fatalf("expected an unreadable sidecar to answer 500, got %d", rec.Code)
	}

	// A ready path that cannot be removed: a directory with something in it.
	f.writeSidecarFile(triggerManual, byReader)
	if err := os.MkdirAll(filepath.Join(f.dir, readyName, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := f.ready(http.MethodDelete); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected a failed cancel to answer 500, got %d", rec.Code)
	}
	// The switch is saved: an undeletable ready path is logged, it does not fail the save.
	for _, body := range []string{`{"trigger":"auto"}`, `{"trigger":"auto"}`} {
		if rec := f.annotations(http.MethodPut, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"trigger":"auto"`) {
			t.Fatalf("expected the save to stand despite the ready path, got %d %s", rec.Code, rec.Body)
		}
		if !strings.Contains(f.sidecar(), `"trigger": "auto"`) {
			t.Fatalf("expected auto saved on disk, got %s", f.sidecar())
		}
	}
}

// A switch to auto that fails to save keeps the batch: the sidecar still says manual.
func TestFailedSwitchToAutoKeepsTheBatch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the process cannot read")
	}

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	if rec := f.ready(http.MethodPost); rec.Code != http.StatusOK {
		t.Fatalf("expected the batch sent, got %d", rec.Code)
	}
	revDir := filepath.Join(f.dir, "doc"+revisionsSuffix)
	if err := os.Mkdir(revDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(revDir, 0o755) }) //nolint:gosec // restored so TempDir can clean it
	before := f.sidecar()
	for _, body := range []string{`{"trigger":"auto"}`, `{"trigger":"auto","annotations":[]}`} {
		if rec := f.annotations(http.MethodPut, body); rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: expected the save to fail on the revisions, got %d", body, rec.Code)
		}
		if f.readyContent() == "" || f.sidecar() != before {
			t.Fatalf("%s: expected the batch and the manual sidecar kept, got %q", body, f.readyContent())
		}
	}
}
