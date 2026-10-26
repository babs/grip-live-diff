//go:build unix

package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An agent holding the lock while it edits the sidecar must be waited for, and its edit
// must be part of what the browser's save is merged with.
func TestAnnotationsWaitForTheAgentLock(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n\nalpha bravo\n")
	first := f.put(`[{"exact":"alpha bravo","thread":[{"by":"reader","text":"rephrase"}]}]`).Annotations[0]

	p := filepath.Join(f.dir, sidecarName)
	agent, err := os.OpenFile(p, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	// Rendering the page must not queue behind the agent.
	rendered := make(chan string, 1)
	go func() { rendered <- f.get(docPath) }()
	select {
	case page := <-rendered:
		if !strings.Contains(page, `class="toolbar-row annotations-nav">`) {
			t.Fatalf("expected the navigation row on a locked sidecar, got %q", page)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected the page to render while the sidecar is locked")
	}

	done := make(chan annotationsResponse, 1)
	go func() {
		done <- f.put(`[{"id":"` + first.ID + `","thread":[{"by":"reader","text":"rephrase, shorter"}]}]`)
	}()

	select {
	case resp := <-done:
		t.Fatalf("expected the save to wait for the lock, got %+v", resp.Annotations)
	case <-time.After(150 * time.Millisecond):
	}

	// The agent answers under its lock, then releases.
	edited := strings.Replace(f.sidecar(), `"file_hash"`, `"status": "done", "file_hash"`, 1)
	if _, err := agent.WriteAt([]byte(edited), 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case resp := <-done:
		a := resp.Annotations[0]
		if a.Status != "done" || a.Thread[0].Text != "rephrase, shorter" {
			t.Fatalf("expected the save merged over the agent's edit, got %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the save to proceed once the lock was released")
	}
}

// A send waits for the agent's lock: the agent claims the batch under it.
func TestReadyWaitsForTheAgentLock(t *testing.T) {
	t.Parallel()

	f := newDiffFixture(t, "# Title\n")
	f.writeSidecarFile(triggerManual, byReader)
	agent, err := os.Open(filepath.Join(f.dir, sidecarName))
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck
	defer agent.Close()
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() { done <- f.ready(http.MethodPost).Code }()
	select {
	case code := <-done:
		t.Fatalf("expected the send to wait for the lock, got %d", code)
	case <-time.After(150 * time.Millisecond):
	}
	if f.readyContent() != "" {
		t.Fatal("expected no ready file while the agent holds the lock")
	}
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != http.StatusOK || f.readyContent() == "" {
			t.Fatalf("expected the batch sent once the lock was released, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the send to proceed once the lock was released")
	}

	// A cancel waits too: unlocked, it would land between the agent's read and its claim.
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	go func() { done <- f.ready(http.MethodDelete).Code }()
	select {
	case code := <-done:
		t.Fatalf("expected the cancel to wait for the lock, got %d", code)
	case <-time.After(150 * time.Millisecond):
	}
	if f.readyContent() == "" {
		t.Fatal("expected the ready file kept while the agent holds the lock")
	}
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != http.StatusOK || f.readyContent() != "" {
			t.Fatalf("expected the batch cancelled once the lock was released, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the cancel to proceed once the lock was released")
	}
}

// A ready path that is not a regular file is never opened: a FIFO would block the GET, and the
// send under the sidecar lock. It reads as sent; a send replaces it, a directory fails.
func TestReadyNotARegularFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p string)
		code  int
	}{
		{"fifo", func(t *testing.T, p string) {
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}, http.StatusOK},
		// Pointing at a claim by a live process: followed, it would read as working.
		{"symlink", func(t *testing.T, p string) {
			target := filepath.Join(filepath.Dir(p), "elsewhere")
			if err := os.WriteFile(target, []byte(fmt.Sprintf(`{pid: %d}`, os.Getpid())), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}, http.StatusOK},
		{"directory", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newDiffFixture(t, "# Title\n")
			f.writeSidecarFile(triggerManual, byReader)
			p := filepath.Join(f.dir, readyName)
			tc.setup(t, p)

			got := make(chan string, 1)
			go func() { got <- f.getAnnotations().Ready.State }()
			select {
			case state := <-got:
				if state != readySent {
					t.Fatalf("expected sent, got %s", state)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("expected GET not to block on the ready path")
			}

			code := make(chan int, 1)
			go func() { code <- f.ready(http.MethodPost).Code }()
			select {
			case c := <-code:
				if c != tc.code {
					t.Fatalf("expected %d, got %d", tc.code, c)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("expected POST not to block on the ready path")
			}
			if tc.code != http.StatusOK {
				return
			}
			// Checked before any read: reading a FIFO left in place would block the test.
			if regular, err := isRegular(p); !regular || err != nil {
				t.Fatalf("expected a regular file in place, got %v", err)
			}
			if !strings.Contains(f.readyContent(), `"pid":null`) {
				t.Fatalf("expected a ready file of ours, got %q", f.readyContent())
			}
			if tc.name == "symlink" {
				if data, _ := os.ReadFile(filepath.Join(f.dir, "elsewhere")); !strings.Contains(string(data), "pid: ") {
					t.Fatalf("expected the link target untouched, got %q", data)
				}
			}
		})
	}
}

// The agent may replace the sidecar (temp + rename, as sed -i or jq > t && mv do) or the
// preview remove it while a request waits for the lock: the request must act on what p names
// once it holds the lock, not on the inode it opened.
func TestLockFollowsTheSidecarReplaced(t *testing.T) {
	t.Parallel()

	handled := `{version: 2, file: "doc.md", trigger: "manual", annotations: [{id: "a1b2c3d4", exact: null, file_hash: "", thread: [{by: "reader", at: null, text: "fix"}, {by: "agent", at: null, text: "done"}]}]}`
	for _, tc := range []struct {
		name string
		// edit runs while the agent holds the lock on the old inode.
		edit  func(t *testing.T, p string)
		send  func(f *diffFixture) *httptest.ResponseRecorder
		check func(t *testing.T, f *diffFixture, rec *httptest.ResponseRecorder)
	}{
		{"send after a rename to a handled sidecar", renameTo(handled), postReady, func(t *testing.T, f *diffFixture, rec *httptest.ResponseRecorder) {
			if rec.Code != http.StatusConflict || f.readyContent() != "" {
				t.Fatalf("expected 409 and no ready file, got %d %q", rec.Code, f.readyContent())
			}
		}},
		{"send after the sidecar was removed", removeFile, postReady, func(t *testing.T, f *diffFixture, rec *httptest.ResponseRecorder) {
			if rec.Code != http.StatusConflict || f.readyContent() != "" {
				t.Fatalf("expected 409 and no ready file, got %d %q", rec.Code, f.readyContent())
			}
		}},
		{"save after a rename", renameTo(handled), func(f *diffFixture) *httptest.ResponseRecorder {
			return f.annotations(http.MethodPut, `{"annotations":[{"id":"a1b2c3d4","thread":[{"by":"reader","text":"fix, shorter"}]}]}`)
		}, func(t *testing.T, f *diffFixture, rec *httptest.ResponseRecorder) {
			if s := f.sidecar(); rec.Code != http.StatusOK || !strings.Contains(s, "fix, shorter") || !strings.Contains(s, `"by": "agent"`) {
				t.Fatalf("expected the save merged into the live file, got %d %s", rec.Code, s)
			}
		}},
		{"save after the sidecar was removed", removeFile, func(f *diffFixture) *httptest.ResponseRecorder {
			return f.annotations(http.MethodPut, `{"trigger":"manual"}`)
		}, func(t *testing.T, f *diffFixture, rec *httptest.ResponseRecorder) {
			if rec.Code != http.StatusOK || !strings.Contains(f.sidecar(), `"trigger": "manual"`) {
				t.Fatalf("expected the save written to a live file, got %d", rec.Code)
			}
		}},
		{"read after a rename", renameTo(handled), func(f *diffFixture) *httptest.ResponseRecorder {
			return f.annotations(http.MethodGet, "")
		}, func(t *testing.T, _ *diffFixture, rec *httptest.ResponseRecorder) {
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"by":"agent"`) {
				t.Fatalf("expected the live file read, got %d %s", rec.Code, rec.Body)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newDiffFixture(t, "# Title\n")
			f.writeSidecarFile(triggerManual, byReader)
			p := filepath.Join(f.dir, sidecarName)
			agent, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer agent.Close()
			if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- tc.send(f) }()
			select {
			case rec := <-done:
				t.Fatalf("expected the request to wait for the lock, got %d", rec.Code)
			case <-time.After(150 * time.Millisecond):
			}
			tc.edit(t, p)
			if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-done:
				tc.check(t, f, rec)
			case <-time.After(2 * time.Second):
				t.Fatal("expected the request to proceed once the lock was released")
			}
		})
	}
}

func postReady(f *diffFixture) *httptest.ResponseRecorder { return f.ready(http.MethodPost) }

func renameTo(content string) func(t *testing.T, p string) {
	return func(t *testing.T, p string) {
		t.Helper()
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p); err != nil {
			t.Fatal(err)
		}
	}
}

func removeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}
