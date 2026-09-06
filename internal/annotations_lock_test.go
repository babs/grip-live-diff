//go:build unix

package internal

import (
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
	first := f.put(`[{"exact":"alpha bravo","comment":"rephrase"}]`).Annotations[0]

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
		done <- f.put(`[{"id":"` + first.ID + `","comment":"rephrase, shorter"}]`)
	}()

	select {
	case resp := <-done:
		t.Fatalf("expected the save to wait for the lock, got %+v", resp.Annotations)
	case <-time.After(150 * time.Millisecond):
	}

	// The agent answers under its lock, then releases.
	edited := strings.Replace(f.sidecar(), `"comment": "rephrase"`, `"comment": "rephrase", "status": "done"`, 1)
	if _, err := agent.WriteAt([]byte(edited), 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(agent.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case resp := <-done:
		a := resp.Annotations[0]
		if a.Status != "done" || a.Comment != "rephrase, shorter" {
			t.Fatalf("expected the save merged over the agent's edit, got %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the save to proceed once the lock was released")
	}
}
