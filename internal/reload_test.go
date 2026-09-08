package internal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wakeups reports the message of every settled burst of r on a channel, for as long as
// the test runs.
func wakeups(t *testing.T, r *reloader) <-chan string {
	t.Helper()
	ch := make(chan string, 8)
	go func() {
		r.cond.L.Lock()
		seen := r.seq
		for {
			for r.seq == seen {
				r.cond.Wait()
			}
			seen = r.seq
			ch <- r.last
		}
	}()
	// The goroutine must be waiting before the first write, or the broadcast is missed.
	time.Sleep(50 * time.Millisecond)
	return ch
}

func expectWakeup(t *testing.T, ch <-chan string, want string, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("expected %q after %s, got %q", want, what, got)
		}
	case <-time.After(400 * time.Millisecond):
		if want != "" {
			t.Fatalf("expected %q after %s", want, what)
		}
	}
}

// A sidecar write refreshes the annotations without reloading the page; anything else in
// the directory, including a new subdirectory's files, reloads, and wins over a sidecar
// write in the same burst.
func TestReloadTellsTheSidecarApart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := newReloader()
	if err := r.watch(dir, classifyChange); err != nil {
		t.Fatal(err)
	}
	ch := wakeups(t, r)

	if err := os.WriteFile(filepath.Join(dir, "doc"+sidecarSuffix), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgAnnotations, "a sidecar write")

	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgReload, "a markdown write")

	for _, order := range [][]string{{"doc" + sidecarSuffix, "doc.md"}, {"doc.md", "doc" + sidecarSuffix}} {
		for _, name := range order {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		expectWakeup(t, ch, msgReload, "a burst of "+strings.Join(order, " then "))
		expectWakeup(t, ch, "", "that same burst")
	}

	// A kept revision is written during a save: it refreshes, like the sidecar.
	rev := filepath.Join(dir, "doc"+revisionsSuffix)
	if err := os.Mkdir(rev, 0o755); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgAnnotations, "the revisions directory")
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(rev, "0123456789ab.md"), []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgAnnotations, "a revision write")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgReload, "a new directory")
	time.Sleep(50 * time.Millisecond) // the new directory is being added to the watcher
	if err := os.WriteFile(filepath.Join(sub, "deep.md"), []byte("# deep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, msgReload, "a write in a new subdirectory")
}

func TestReloadScriptFollowsTheFlag(t *testing.T) {
	t.Parallel()

	for _, on := range []bool{true, false} {
		f := &diffFixture{t: t, dir: t.TempDir()}
		f.handler = NewServer("localhost", 6419, false, false, on, NewParser()).newHandler(http.Dir(f.dir))
		f.write("# Title\n")
		if got := strings.Contains(f.get(docPath), reloadEndpoint); got != on {
			t.Fatalf("reload %v: expected script presence %v, got %v", on, on, got)
		}
	}
}

// The page holds one websocket open; every settled burst writes its message on it.
func TestReloadWebsocketGetsTheMessages(t *testing.T) {
	t.Parallel()

	r := newReloader()
	srv := httptest.NewServer(http.HandlerFunc(r.serveWS))
	defer srv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+reloadEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck
	defer conn.Close()
	//nolint:errcheck
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond) // the handler must be waiting before the broadcast
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{msgAnnotations, msgReload} {
		r.broadcast(want)
		_, msg, err := conn.ReadMessage()
		if err != nil || string(msg) != want {
			t.Fatalf("expected %q on the same connection, got %q (%v)", want, msg, err)
		}
	}
}
