package internal

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wakeups reports the changed paths of every settled burst of r on a channel, for as long
// as the test runs.
func wakeups(t *testing.T, r *reloader) <-chan map[string]struct{} {
	t.Helper()
	ch := make(chan map[string]struct{}, 8)
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

func expectWakeup(t *testing.T, ch <-chan map[string]struct{}, what string, want ...string) {
	t.Helper()
	select {
	case got := <-ch:
		if len(got) != len(want) {
			t.Fatalf("expected %q after %s, got %v", want, what, got)
		}
		for _, name := range want {
			if _, ok := got[name]; !ok {
				t.Fatalf("expected %q after %s, got %v", want, what, got)
			}
		}
	case <-time.After(400 * time.Millisecond):
		if len(want) > 0 {
			t.Fatalf("expected %q after %s", want, what)
		}
	}
}

// The watcher reports every changed file as a URL path under the served directory, one
// burst for writes close together, including files of a directory created later.
func TestReloadReportsChangedPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := newReloader()
	if err := r.watch(dir); err != nil {
		t.Fatal(err)
	}
	ch := wakeups(t, r)

	if err := os.WriteFile(filepath.Join(dir, "my doc.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, "a markdown write", "/my doc.md")

	for _, name := range []string{"doc" + sidecarSuffix, "doc.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expectWakeup(t, ch, "a burst", "/doc"+sidecarSuffix, "/doc.md")
	expectWakeup(t, ch, "that same burst")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, "a new directory", "/sub")
	time.Sleep(50 * time.Millisecond) // the new directory is being added to the watcher
	if err := os.WriteFile(filepath.Join(sub, "deep.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, "a write in a new subdirectory", "/sub/deep.png")
}

// A page reloads for itself and its resources, refreshes its marks for its own sidecar and
// kept revisions, and ignores everything else in the tree.
func TestPageMessage(t *testing.T) {
	t.Parallel()

	resources := []string{"/img/a.png", "/sub/diagram.svg"}
	for _, tc := range []struct {
		page    string
		changed []string
		want    string
	}{
		{"/doc.md", []string{"/doc.md"}, msgReload},
		{"/doc.md", []string{"/img/a.png"}, msgReload},
		{"/doc.md", []string{"/sub/diagram.svg", "/.git/index"}, msgReload},
		{"/doc.md", []string{"/doc" + sidecarSuffix}, msgAnnotations},
		{"/doc.md", []string{"/doc" + revisionsSuffix}, msgAnnotations},
		{"/doc.md", []string{"/doc" + revisionsSuffix + "/0123456789ab.md"}, msgAnnotations},
		{"/doc.md", []string{"/doc" + sidecarSuffix, "/doc.md"}, msgReload},
		{"/sub/doc.md", []string{"/sub/doc" + sidecarSuffix}, msgAnnotations},
		{"/doc.md", []string{"/other.md"}, ""},
		{"/doc.md", []string{"/other" + sidecarSuffix, "/other" + revisionsSuffix + "/0123456789ab.md"}, ""},
		{"/doc.md", []string{"/img/b.png"}, ""},
		{"/doc.md", []string{"/.doc.md.swp", "/doc.md~", "/4913"}, ""},
		{"/doc.md", []string{"/.git/index", "/.git/objects/ab"}, ""},
		{"/doc.md", []string{"/sub/doc.md", "/sub/doc" + sidecarSuffix}, ""},
		{"/doc.md", []string{"/doc" + revisionsSuffix + "x"}, ""},
		{"/my doc.md", []string{"/my doc" + sidecarSuffix}, msgAnnotations},
	} {
		changed := make(map[string]struct{})
		for _, name := range tc.changed {
			changed[name] = struct{}{}
		}
		if got := pageMessage(tc.page, resources, changed); got != tc.want {
			t.Errorf("page %s, changed %v: expected %q, got %q", tc.page, tc.changed, tc.want, got)
		}
	}
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

// Every reload the script runs is first offered as a cancelable gld:reload: annotate.js
// holds it while the reader is mid-annotation. A bare location.reload() would skip the hold.
func TestReloadScriptOffersEveryReloadAsACancelableEvent(t *testing.T) {
	t.Parallel()

	f := &diffFixture{t: t, dir: t.TempDir()}
	f.handler = NewServer("localhost", 6419, false, false, true, NewParser()).newHandler(http.Dir(f.dir))
	f.write("# Title\n")
	page := f.get(docPath)

	gated := `if (document.dispatchEvent(new Event("gld:reload", { cancelable: true }))) location.reload()`
	if !strings.Contains(page, gated) {
		t.Fatalf("reload script lacks the gated reload %q", gated)
	}
	if n := strings.Count(page, "location.reload()"); n != 1 {
		t.Fatalf("expected the gated call to be the only location.reload(), found %d", n)
	}
}

// The page holds one websocket open; every settled burst that concerns it writes its
// message on it, the others write nothing.
func TestReloadWebsocketGetsTheMessages(t *testing.T) {
	t.Parallel()

	r := newReloader()
	srv := httptest.NewServer(http.HandlerFunc(r.serveWS))
	defer srv.Close()

	query := url.Values{"page": {"/my doc.md"}, "res": {"/a.png"}}.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+reloadEndpoint+"?"+query, nil)
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
	for _, step := range []struct{ changed, want string }{
		{"/other.md", ""},
		{"/my doc" + sidecarSuffix, msgAnnotations},
		{"/b.png", ""},
		{"/a.png", msgReload},
	} {
		r.broadcast(step.changed)
		time.Sleep(reloadDebounce + 50*time.Millisecond) // one burst per step
		if step.want == "" {
			continue
		}
		_, msg, err := conn.ReadMessage()
		if err != nil || string(msg) != step.want {
			t.Fatalf("expected %q after %s on the same connection, got %q (%v)", step.want, step.changed, msg, err)
		}
	}
}

// A page whose file changed between rendering and opening its socket is told to reload at
// once; a page that is current, or sends no hash, waits for the next change that concerns it.
func TestReloadWebsocketCatchesAWriteBeforeConnect(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newReloader()
	if err := r.watch(dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(r.serveWS))
	defer srv.Close()

	for _, tc := range []struct {
		hash string
		want string
	}{
		{fileHash([]byte("# before\n")), msgReload},
		{fileHash([]byte("# now\n")), msgAnnotations},
		{"", msgAnnotations},
	} {
		query := url.Values{"page": {"/doc.md"}, "hash": {tc.hash}}.Encode()
		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		//nolint:errcheck
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond) // the handler must be waiting before the broadcast
		r.broadcast("/doc" + sidecarSuffix)
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil || string(msg) != tc.want {
			t.Fatalf("hash %q: expected %q first, got %q (%v)", tc.hash, tc.want, msg, err)
		}
		//nolint:errcheck
		conn.Close()
	}
}

// A connect during a burst that holds the page leaves the reload to that burst: a file
// written nonstop must not reload the page again at every connect.
func TestReloadWebsocketLeavesAPendingBurstAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newReloader()
	if err := r.watch(dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(r.serveWS))
	defer srv.Close()

	r.broadcast("/doc.md")
	query := url.Values{"page": {"/doc.md"}, "hash": {fileHash([]byte("# before\n"))}}.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck
	defer conn.Close()
	//nolint:errcheck
	resp.Body.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != msgReload {
		t.Fatalf("expected %q, got %q (%v)", msgReload, msg, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(reloadDebounce + 200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.ReadMessage(); err == nil {
		t.Fatalf("expected a single reload, got a second message %q", msg)
	}
}

// Only a page of this server may open the socket.
func TestReloadWebsocketRefusesForeignOrigins(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(newReloader().serveWS))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	for _, tc := range []struct {
		origin string
		ok     bool
	}{
		{"http://" + host, true},
		{"", true},
		{"http://evil.example", false},
		{"http://localhost.evil.example", false},
		{"null", false},
	} {
		header := http.Header{}
		if tc.origin != "" {
			header.Set("Origin", tc.origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial("ws://"+host+"?page=/doc.md", header)
		if resp != nil {
			//nolint:errcheck
			resp.Body.Close()
		}
		if conn != nil {
			//nolint:errcheck
			conn.Close()
		}
		if (err == nil) != tc.ok {
			t.Errorf("origin %q: expected accepted %v, got error %v", tc.origin, tc.ok, err)
		}
	}
}
