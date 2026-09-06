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

// wakeups reports every broadcast of r on a channel, for as long as the test runs.
func wakeups(t *testing.T, r *reloader) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{}, 8)
	go func() {
		for {
			r.cond.L.Lock()
			r.cond.Wait()
			r.cond.L.Unlock()
			ch <- struct{}{}
		}
	}()
	// The goroutine must be waiting before the first write, or the broadcast is missed.
	time.Sleep(50 * time.Millisecond)
	return ch
}

func expectWakeup(t *testing.T, ch <-chan struct{}, want bool, what string) {
	t.Helper()
	select {
	case <-ch:
		if !want {
			t.Fatalf("expected no reload after %s", what)
		}
	case <-time.After(400 * time.Millisecond):
		if want {
			t.Fatalf("expected a reload after %s", what)
		}
	}
}

// A save of the sidecar must not reload the page that just made it; anything else in the
// directory, including a new subdirectory's files, must.
func TestReloadIgnoresTheSidecar(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := newReloader()
	if err := r.watch(dir, func(name string) bool { return strings.HasSuffix(name, sidecarSuffix) }); err != nil {
		t.Fatal(err)
	}
	ch := wakeups(t, r)

	if err := os.WriteFile(filepath.Join(dir, "doc"+sidecarSuffix), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, false, "a sidecar write")

	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, true, "a markdown write")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, true, "a new directory")
	time.Sleep(50 * time.Millisecond) // the new directory is being added to the watcher
	if err := os.WriteFile(filepath.Join(sub, "deep.md"), []byte("# deep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, true, "a write in a new subdirectory")
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

// The page holds a websocket open; a change writes "reload" on it.
func TestReloadWebsocketGetsTheMessage(t *testing.T) {
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
	r.broadcast()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil || string(msg) != "reload" {
		t.Fatalf("expected the reload message, got %q (%v)", msg, err)
	}
}
