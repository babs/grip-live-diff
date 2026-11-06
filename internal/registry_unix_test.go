//go:build unix

package internal

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An entry that is not a regular file is skipped: a FIFO would block every scan, a directory
// of a dead PID cannot be removed and would log on each one. Not parallel: it captures the log.
func TestScanRegistrySkipsNotARegularFile(t *testing.T) {
	logged := captureLog(t)

	dir := t.TempDir()
	live := livePID(t)
	if err := syscall.Mkfifo(filepath.Join(dir, fmt.Sprintf("%d.json", live)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, fmt.Sprintf("%d.json/x", exitedPID(t)), "x")

	got := make(chan []instance, 1)
	go func() { got <- scanRegistry(dir) }()
	select {
	case in := <-got:
		if len(in) != 0 {
			t.Fatalf("expected no instance, got %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the scan not to block on a FIFO")
	}
	scanRegistry(dir)
	if logged.Len() != 0 {
		t.Fatalf("expected nothing logged, got %q", logged.String())
	}
}

// TestServeHelper is an instance that listens to the real signals, for TestSignalRemovesTheFile.
func TestServeHelper(t *testing.T) {
	reg, docs := os.Getenv("GLD_TEST_SERVE_REGISTRY"), os.Getenv("GLD_TEST_SERVE_DOCS")
	if reg == "" {
		t.Skip("helper process only")
	}
	s := NewServer("localhost", 0, false, false, false, NewParser(), "")
	s.UseRegistry(reg)
	if err := s.Serve(docs); !errors.As(err, new(Stopped)) {
		t.Fatal(err)
	}
}

// 013-AC5
func TestSignalRemovesTheFile(t *testing.T) {
	t.Parallel()

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()

			reg, docs := t.TempDir(), t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelper$") //nolint:gosec // the test binary itself
			cmd.Env = append(os.Environ(), "GLD_TEST_SERVE_REGISTRY="+reg, "GLD_TEST_SERVE_DOCS="+docs)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			started := make(chan struct{})
			go func() {
				sc := bufio.NewScanner(stdout)
				for sc.Scan() {
					if strings.Contains(sc.Text(), "Starting server") {
						close(started)
						break
					}
				}
				for sc.Scan() { //nolint:revive // drain, so the child never blocks on its output
				}
			}()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("no start line within 10 s")
			}
			if _, ok := readInstanceFile(t, reg, cmd.Process.Pid); !ok {
				t.Fatal("expected the registry file at the start line")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("expected the instance to stop cleanly, got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the instance did not stop within 10 s")
			}
			if _, ok := readInstanceFile(t, reg, cmd.Process.Pid); ok {
				t.Fatalf("expected the registry file removed on %v", sig)
			}
		})
	}
}
