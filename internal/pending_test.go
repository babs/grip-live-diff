package internal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pendingFixture is a tree with one sidecar and a registry where A and B are live on it.
type pendingFixture struct {
	t          *testing.T
	dir, reg   string
	pidA, pidB int64
	sidecar    string
	dying      *exec.Cmd // an instance a test kills
}

func newPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	pf := &pendingFixture{t: t, dir: resolveDir(t.TempDir()), reg: t.TempDir(), pidA: livePID(t), pidB: livePID(t)}
	pf.sidecar = filepath.Join(pf.dir, "doc.annotations.json5")
	writeInstance(t, pf.reg, instance{PID: pf.pidA, Port: portA, Root: pf.dir})
	writeInstance(t, pf.reg, instance{PID: pf.pidB, Port: portB, Root: pf.dir})
	return pf
}

// write puts the sidecar on disk: open entries routed to A, to B, to a gone port and to none,
// then one answered and one closed, both routed to A.
func (pf *pendingFixture) write(trigger string) {
	pf.t.Helper()
	open := func(id string, via int) string {
		key := ""
		if via != 0 {
			key = fmt.Sprintf(", via: %d", via)
		}
		return fmt.Sprintf(`{id: %q, exact: null, file_hash: "", thread: [{by: "reader", at: null, text: "fix"%s}]}`, id, key)
	}
	entries := []string{
		open("toA", portA), open("toB", portB), open("gone", 7000), open("unrouted", 0),
		fmt.Sprintf(`{id: "answered", exact: null, file_hash: "", thread: [{by: "reader", at: null, text: "fix", via: %d}, {by: "agent", at: null, text: "ok"}]}`, portA),
		fmt.Sprintf(`{id: "closed", exact: null, file_hash: "", status: "done", thread: [{by: "reader", at: null, text: "fix", via: %d}]}`, portA),
	}
	body := fmt.Sprintf("// header\n{version: 2, file: \"doc.md\", trigger: %q, annotations: [%s]}", trigger, strings.Join(entries, ",\n"))
	writeFile(pf.t, pf.dir, filepath.Base(pf.sidecar), body)
}

func (pf *pendingFixture) ready(content string) {
	pf.t.Helper()
	writeFile(pf.t, pf.dir, readyName, content)
}

// pending is the command for port and pid on paths, with buffers for its two outputs.
func (pf *pendingFixture) pending(port int, pid, claim int64, paths ...string) (Pending, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return Pending{Port: port, PID: pid, Claim: claim, Registry: pf.reg, Paths: paths, Out: &out, Err: &errOut}, &out, &errOut
}

// run calls pending for port and pid on the tree, and returns the ids it listed.
func (pf *pendingFixture) run(port int, pid, claim int64) ([]string, bool, string) {
	pf.t.Helper()
	cmd, out, errOut := pf.pending(port, pid, claim, pf.dir)
	found, err := cmd.Run()
	if errors.Is(err, ErrInstanceGone) {
		return nil, false, err.Error()
	}
	if err != nil {
		pf.t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		sc, id, ok := strings.Cut(line, "\t")
		if !ok || sc != pf.sidecar {
			pf.t.Fatalf("expected <sidecar>\\t<id>, got %q", line)
		}
		ids = append(ids, id)
	}
	if found != (len(ids) > 0) {
		pf.t.Fatalf("expected found to tell a line was printed, got %v for %v", found, ids)
	}
	return ids, found, errOut.String()
}

// 013-AC12, 013-AC6 (pending)
func TestPending(t *testing.T) {
	t.Parallel()

	pf := newPendingFixture(t)
	pf.write(triggerAuto)
	dead := int64(exitedPID(t))
	writeInstance(t, pf.reg, instance{PID: dead, Port: portC, Root: pf.dir})
	noFile := livePID(t)

	for _, tc := range []struct {
		name string
		port int
		pid  int64
		want []string
	}{
		{"A: its own, the gone and the unrouted", portA, pf.pidA, []string{"toA", "gone", "unrouted"}},
		{"B: its own only", portB, pf.pidB, []string{"toB"}},
		{"a dead PID", portC, dead, nil},
		{"a port that is not the PID's", portB, pf.pidA, nil},
		{"routing off: no file, live PID", portC, noFile, []string{"toA", "toB", "gone", "unrouted"}},
	} {
		ids, _, _ := pf.run(tc.port, tc.pid, 0)
		if !reflect.DeepEqual(ids, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, ids)
		}
	}
	if _, _, stderr := pf.run(portC, noFile, 0); !strings.Contains(stderr, "routing off") {
		t.Errorf("expected stderr to say routing is off, got %q", stderr)
	}
	if _, _, stderr := pf.run(portA, pf.pidA, 0); stderr != "" {
		t.Errorf("expected nothing on stderr for a registered instance, got %q", stderr)
	}
	if _, _, stderr := pf.run(portC, dead, 0); !strings.Contains(stderr, "gone") {
		t.Errorf("expected stderr to say the instance is gone, got %q", stderr)
	}
	if _, ok := readInstanceFile(t, pf.reg, int(dead)); ok {
		t.Error("expected pending to remove the file of a dead PID")
	}

	// A stopped A: its file is there, not seen for a minute.
	writeInstance(t, pf.reg, instance{PID: pf.pidA, Port: portA, Root: pf.dir, SeenAt: time.Now().Add(-2 * registryLive).Format(time.RFC3339)})
	if ids, found, stderr := pf.run(portA, pf.pidA, 0); found || !strings.Contains(stderr, "not seen") {
		t.Errorf("expected a stopped instance to own nothing, got %v %q", ids, stderr)
	}
	// B is then the lowest live port.
	if ids, _, _ := pf.run(portB, pf.pidB, 0); !reflect.DeepEqual(ids, []string{"toA", "toB", "gone", "unrouted"}) {
		t.Errorf("expected B to own everything once A is stopped, got %v", ids)
	}
}

// 013-AC12: a sidecar that does not parse is reported and skipped, the others still listed.
func TestPendingSkipsABadSidecar(t *testing.T) {
	t.Parallel()

	pf := newPendingFixture(t)
	pf.write(triggerAuto)
	// Walked before doc.annotations.json5.
	writeFile(t, pf.dir, "aa.annotations.json5", "{ bad")
	ids, found, stderr := pf.run(portA, pf.pidA, 0)
	if !found || !reflect.DeepEqual(ids, []string{"toA", "gone", "unrouted"}) || !strings.Contains(stderr, "aa.annotations.json5") {
		t.Fatalf("expected A's entries listed and the bad sidecar on stderr, got %v %v %q", ids, found, stderr)
	}
}

// 013-AC12: the key of a sidecar is its directory resolved, whatever path names it.
func TestPendingResolvesThePath(t *testing.T) {
	pf := newPendingFixture(t)
	pf.write(triggerAuto)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(pf.dir, link); err != nil {
		t.Skip("no symlink here:", err)
	}
	t.Chdir(filepath.Dir(pf.dir))
	for _, path := range []string{link, filepath.Base(pf.dir)} {
		cmd, out, errOut := pf.pending(portB, pf.pidB, 0, path)
		found, err := cmd.Run()
		if err != nil || !found || !strings.HasSuffix(out.String(), "\ttoB\n") {
			t.Errorf("%s: expected B's entry, got %v %v %q %q", path, found, err, out.String(), errOut.String())
		}
	}
}

// 013-AC12: manual mode lists the batch only for its owner, unclaimed.
func TestPendingManual(t *testing.T) {
	t.Parallel()

	pf := newPendingFixture(t)
	pf.write(triggerManual)
	if ids, _, _ := pf.run(portA, pf.pidA, 0); ids != nil {
		t.Fatalf("expected nothing without a ready file, got %v", ids)
	}
	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, portA))
	if ids, _, _ := pf.run(portA, pf.pidA, 0); !reflect.DeepEqual(ids, []string{"ready", "toA", "gone", "unrouted"}) {
		t.Fatalf("expected the batch and A's entries, got %v", ids)
	}
	if ids, _, _ := pf.run(portB, pf.pidB, 0); ids != nil {
		t.Fatalf("expected nothing for B, got %v", ids)
	}
	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: %d, via: %d}`, os.Getpid(), portA))
	if ids, _, _ := pf.run(portA, pf.pidA, 0); ids != nil {
		t.Fatalf("expected nothing once claimed, got %v", ids)
	}
	// A gone via is the lowest port's.
	pf.ready(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: 7000}`)
	if ids, _, _ := pf.run(portA, pf.pidA, 0); len(ids) == 0 || ids[0] != "ready" {
		t.Fatalf("expected A to own a batch whose via is gone, got %v", ids)
	}
}

// 013-AC12
func TestPendingClaim(t *testing.T) {
	t.Parallel()

	pf := newPendingFixture(t)
	pf.write(triggerManual)
	agent := livePID(t)
	readyFile := filepath.Join(pf.dir, readyName)

	// Replaced by a batch of B between pending and the claim.
	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, portB))
	before, _ := os.ReadFile(readyFile) //nolint:gosec // the test's own file
	cmd, out, _ := pf.pending(portA, pf.pidA, agent, pf.sidecar)
	found, err := cmd.Run()
	after, _ := os.ReadFile(readyFile) //nolint:gosec // idem
	if err != nil || found || out.Len() != 0 || !bytes.Equal(before, after) {
		t.Fatalf("expected the claim of B's batch refused and nothing written, got %v %v %q %q", found, err, out.String(), after)
	}

	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, portA))
	out.Reset()
	found, err = cmd.Run()
	if err != nil || !found {
		t.Fatalf("expected the claim of A's batch to succeed, got %v %v", found, err)
	}
	if want := pf.sidecar + "\tready\n" + pf.sidecar + "\ttoA\n" + pf.sidecar + "\tgone\n" + pf.sidecar + "\tunrouted\n"; out.String() != want {
		t.Fatalf("expected the lines of pending, got %q", out.String())
	}
	rf, err := loadReady(readyFile)
	if err != nil || rf.PID == nil || *rf.PID != agent || rf.ClaimedAt == nil || rf.Via != portA || rf.SentAt == nil {
		t.Fatalf("expected pid, claimed_at written and via kept, got %+v %v", rf, err)
	}

	// Claimed already: a second claim fails.
	if found, _ := cmd.Run(); found {
		t.Fatal("expected a second claim refused")
	}
	two, _, _ := pf.pending(portA, pf.pidA, agent, pf.dir, pf.sidecar)
	if _, err := two.Run(); err == nil {
		t.Fatal("expected --claim on two sidecars refused")
	}

	// The trigger is checked again under the lock: in auto, nothing is claimed.
	pf.write(triggerAuto)
	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, portA))
	before, _ = os.ReadFile(readyFile) //nolint:gosec // idem
	auto, autoOut, _ := pf.pending(portA, pf.pidA, agent, pf.sidecar)
	found, err = auto.Run()
	after, _ = os.ReadFile(readyFile) //nolint:gosec // idem
	if err != nil || found || autoOut.Len() != 0 || !bytes.Equal(before, after) {
		t.Fatalf("expected the claim refused and nothing written in auto, got %v %v %q %q", found, err, autoOut.String(), after)
	}
}

// 013-AC12: --claim takes the exclusive lock of the sidecar, so it waits for a reader.
func TestPendingClaimWaitsForTheLock(t *testing.T) {
	t.Parallel()

	pf := newPendingFixture(t)
	pf.write(triggerManual)
	pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, portA))
	agent := livePID(t)
	held, err := lockSidecar(pf.sidecar, os.O_RDONLY, false)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := pf.pending(portA, pf.pidA, agent, pf.sidecar)
	done := make(chan bool, 1)
	go func() {
		found, _ := cmd.Run()
		done <- found
	}()
	select {
	case <-done:
		releaseSidecar(held)
		t.Fatal("expected the claim to wait while the sidecar is locked")
	case <-time.After(200 * time.Millisecond):
	}
	releaseSidecar(held)
	if found := <-done; !found {
		t.Fatal("expected the claim to succeed once the lock is released")
	}
	rf, err := loadReady(filepath.Join(pf.dir, readyName))
	if err != nil || rf.PID == nil || *rf.PID != agent {
		t.Fatalf("expected the pid written, got %+v %v", rf, err)
	}
}

// 013-AC12: --claim reads the registry once it holds the lock: what changed while it waited
// decides, not what it read before.
func TestPendingClaimReadsTheRegistryUnderTheLock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		via    int
		port   int
		setup  func(pf *pendingFixture) int64 // before the claim, returns the claiming PID
		change func(pf *pendingFixture)       // while the claim waits
	}{
		{"a lower port registers and takes a gone via", 7000, portB,
			func(pf *pendingFixture) int64 {
				if err := os.Remove(filepath.Join(pf.reg, strconv.FormatInt(pf.pidA, 10)+".json")); err != nil {
					pf.t.Fatal(err)
				}
				return pf.pidB
			},
			func(pf *pendingFixture) {
				writeInstance(pf.t, pf.reg, instance{PID: pf.pidA, Port: portA, Root: pf.dir})
			}},
		{"its own file goes stale", portA, portA,
			func(pf *pendingFixture) int64 { return pf.pidA },
			func(pf *pendingFixture) {
				writeInstance(pf.t, pf.reg, instance{PID: pf.pidA, Port: portA, Root: pf.dir, SeenAt: time.Now().Add(-2 * registryLive).Format(time.RFC3339)})
			}},
		{"it dies, its file purged", portA, portA,
			func(pf *pendingFixture) int64 {
				pf.dying = exec.Command(os.Args[0], "-test.run=^TestSleepHelper$") //nolint:gosec // the test binary itself
				pf.dying.Env = append(os.Environ(), "GLD_TEST_SLEEP=1")
				if err := pf.dying.Start(); err != nil {
					pf.t.Fatal(err)
				}
				pf.t.Cleanup(func() { _ = pf.dying.Process.Kill() })
				pid := int64(pf.dying.Process.Pid)
				if err := os.Remove(filepath.Join(pf.reg, strconv.FormatInt(pf.pidA, 10)+".json")); err != nil {
					pf.t.Fatal(err)
				}
				writeInstance(pf.t, pf.reg, instance{PID: pid, Port: portA, Root: pf.dir})
				return pid
			},
			func(pf *pendingFixture) {
				_ = pf.dying.Process.Kill()
				_ = pf.dying.Wait()
			}},
		{"its file names another port", portA, portA,
			func(pf *pendingFixture) int64 { return pf.pidA },
			func(pf *pendingFixture) {
				writeInstance(pf.t, pf.reg, instance{PID: pf.pidA, Port: portC, Root: pf.dir})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pf := newPendingFixture(t)
			pf.write(triggerManual)
			pf.ready(fmt.Sprintf(`{sent_at: "2026-11-04T10:00:00Z", pid: null, via: %d}`, tc.via))
			pid := tc.setup(pf)
			held, err := lockSidecar(pf.sidecar, os.O_RDONLY, false)
			if err != nil {
				t.Fatal(err)
			}
			cmd, _, _ := pf.pending(tc.port, pid, livePID(t), pf.sidecar)
			done := make(chan bool, 1)
			go func() {
				found, _ := cmd.Run()
				done <- found
			}()
			time.Sleep(200 * time.Millisecond)
			tc.change(pf)
			releaseSidecar(held)
			if found := <-done; found {
				t.Fatal("expected the claim refused")
			}
			if rf, err := loadReady(filepath.Join(pf.dir, readyName)); err != nil || rf.PID != nil {
				t.Fatalf("expected no pid written, got %+v %v", rf, err)
			}
		})
	}
}

func TestFindSidecarsSkipsAnUnreadableDirectory(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced on a non-root user")
	}
	dir := t.TempDir()
	writeFile(t, dir, "ok/a.annotations.json5", "{}")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // let TempDir clean up
	var errOut bytes.Buffer
	got, err := findSidecars([]string{dir}, &errOut)
	want := []string{filepath.Join(dir, "ok", "a.annotations.json5")}
	if err != nil || !reflect.DeepEqual(got, want) || !strings.Contains(errOut.String(), "locked") {
		t.Fatalf("expected %v and the directory on stderr, got %v %v %q", want, got, err, errOut.String())
	}
}

func TestFindSidecars(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.annotations.json5", "{}")
	writeFile(t, dir, "sub/b.annotations.json5", "{}")
	writeFile(t, dir, "sub/b.md", "# b")
	got, err := findSidecars([]string{
		dir,
		filepath.Join(dir, "sub", "b.md"),
		filepath.Join(dir, "sub", "b.annotations.ready"),
	}, io.Discard)
	want := []string{
		filepath.Join(dir, "a.annotations.json5"),
		filepath.Join(dir, "sub", "b.annotations.json5"),
		filepath.Join(dir, "sub", "b.annotations.json5"),
		filepath.Join(dir, "sub", "b.annotations.json5"),
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v %v", want, got, err)
	}
	// A missing sidecar or ready file is no entry yet; a missing directory or markdown file, or
	// the directory of a sidecar, is an error.
	for _, missing := range []string{"missing.annotations.json5", "missing.annotations.ready"} {
		if got, err := findSidecars([]string{filepath.Join(dir, missing)}, io.Discard); err != nil || len(got) != 1 {
			t.Errorf("%s: expected its sidecar, got %v %v", missing, got, err)
		}
	}
	for _, missing := range []string{"missing", "missing.md", "nodir/a.annotations.json5", "nodir/a.annotations.ready"} {
		if _, err := findSidecars([]string{filepath.Join(dir, missing)}, io.Discard); err == nil {
			t.Errorf("%s: expected an error", missing)
		}
	}
}
