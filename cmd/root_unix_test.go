//go:build unix

package cmd

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signal still ends the server with 128+n, the code a shell gives a process it killed.
func TestSignalExitCode(t *testing.T) {
	t.Parallel()

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestPendingHelper$") //nolint:gosec // the test binary itself
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+home, "XDG_CONFIG_HOME="+home,
				"GLD_TEST_PENDING_ARGS="+strings.Join([]string{"-b=false", "-p", "0", t.TempDir()}, "\n"))
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
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 128+int(sig) {
				t.Fatalf("expected exit %d, got %v", 128+int(sig), err)
			}
		})
	}
}
