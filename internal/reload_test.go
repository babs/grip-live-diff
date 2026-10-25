package internal

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
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

// The watcher reports every changed file of a followed directory as a URL path under the
// served directory, one burst for writes close together; a directory created in it is
// reported, its files are not, nobody follows it.
// Not parallel: a burst is two writes within reloadDebounce, which the load of the
// parallel tests can stretch.
func TestReloadReportsChangedPaths(t *testing.T) {
	dir := t.TempDir()
	r := watchedReloader(t, dir)
	r.subscribe([]string{"/"})
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
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "deep.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectWakeup(t, ch, "a write in a directory nobody follows")
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
	r := watchedReloader(t, dir)
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
	r := watchedReloader(t, dir)
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

// Short delays, so a release or a poll fits in a test; margins are several times them.
const (
	testReleaseAfter = time.Second
	testCheckEvery   = 100 * time.Millisecond
)

// watchedReloader watches dir with the test delays, and stops when the test ends.
func watchedReloader(t *testing.T, dir string) *reloader {
	t.Helper()
	r := newReloader()
	r.releaseAfter, r.checkEvery = testReleaseAfter, testCheckEvery
	if err := r.watch(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	// Runs before close: every add and remove kept the count of watched entries.
	t.Cleanup(func() {
		r.subMu.Lock()
		defer r.subMu.Unlock()
		n := 0
		for _, sub := range r.subs {
			if sub.watched {
				n++
			}
		}
		if n != r.nWatched {
			t.Errorf("expected nWatched %d, got %d", n, r.nWatched)
		}
	})
	return r
}

func serve(t *testing.T, r *reloader) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(r.serveWS))
	t.Cleanup(srv.Close)
	return srv
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// dialPage opens the socket of page with these resources, as the reload script does.
func dialPage(t *testing.T, srv *httptest.Server, page string, res ...string) *websocket.Conn {
	t.Helper()
	conn, err := tryDialPage(srv, page, res...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	return conn
}

// openPage is dialPage for a page that must get messages: it returns once the server
// follows the page's directory and waits for bursts, which the handshake alone does not say.
func openPage(t *testing.T, r *reloader, srv *httptest.Server, page string, res ...string) *websocket.Conn {
	t.Helper()
	d := path.Dir(page)
	before := followers(r, d)
	conn := dialPage(t, srv, page, res...)
	eventually(t, "the page followed", func() bool { return followers(r, d) > before })
	time.Sleep(50 * time.Millisecond) // from subscribing to waiting on the bursts
	return conn
}

func tryDialPage(srv *httptest.Server, page string, res ...string) (*websocket.Conn, error) {
	query := url.Values{"page": {page}, "res": res}.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?"+query, nil)
	if resp != nil {
		resp.Body.Close() //nolint:errcheck
	}
	return conn, err
}

// readMessage returns the next message within d, "" when none came. A read past its
// deadline breaks the connection: nothing can be read on it afterwards.
func readMessage(t *testing.T, conn *websocket.Conn, d time.Duration) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return ""
	}
	return string(msg)
}

func expectMessage(t *testing.T, conn *websocket.Conn, want, what string) {
	t.Helper()
	if got := readMessage(t, conn, 3*time.Second); got != want {
		t.Fatalf("expected %q after %s, got %q", want, what, got)
	}
}

func expectNoMessage(t *testing.T, conn *websocket.Conn, what string) {
	t.Helper()
	if got := readMessage(t, conn, 3*reloadDebounce); got != "" {
		t.Fatalf("expected nothing after %s, got %q", what, got)
	}
}

func isWatched(r *reloader, d string) bool {
	return slices.Contains(r.watcher.WatchList(), r.fsPath(d))
}

func followers(r *reloader, d string) int {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	if sub, ok := r.subs[d]; ok {
		return sub.n
	}
	return 0
}

// eventually polls cond for up to 3 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("expected %s", what)
}

func TestWatchWalksNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "sub/deep/old.md", "x")
	r := watchedReloader(t, dir)
	ch := wakeups(t, r)

	writeFile(t, dir, "a.md", "x")
	writeFile(t, dir, "sub/deep/b.md", "x")
	expectWakeup(t, ch, "writes with no page open")
	if list := r.watcher.WatchList(); len(list) != 0 {
		t.Fatalf("expected nothing watched, got %v", list)
	}
}

func TestPageDirs(t *testing.T) {
	t.Parallel()

	many := make([]string, 20)
	for i := range many {
		many[i] = fmt.Sprintf("/d%02d/a.png", i)
	}
	for _, tc := range []struct {
		page    string
		res     []string
		want    []string
		dropped int
	}{
		{"/doc.md", nil, []string{"/"}, 0},
		{"/sub/doc.md", []string{"/img/a.png", "/img/b.png", "/sub/x.svg"}, []string{"/sub", "/img"}, 0},
		{"/doc.md", []string{"/../../etc/x.png"}, []string{"/", "/etc"}, 0},
		{"/doc.md", []string{`/a\..\..\x.png`, "/b\x00/c.png"}, []string{"/"}, 0},
		{"/doc.md", many, []string{"/", "/d00", "/d01", "/d02", "/d03", "/d04", "/d05", "/d06", "/d07", "/d08"}, 11},
	} {
		if got, dropped := pageDirs(tc.page, tc.res); !slices.Equal(got, tc.want) || dropped != tc.dropped {
			t.Errorf("pageDirs(%s, %v): expected %v, %d dropped, got %v, %d", tc.page, tc.res, tc.want, tc.dropped, got, dropped)
		}
	}
}

func TestPageFollowsItsDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "sub/doc.md", "# doc\n")
	writeFile(t, dir, "img/a.png", "x")
	writeFile(t, dir, "other/x.md", "x")
	r := watchedReloader(t, dir)
	srv := serve(t, r)
	conn := openPage(t, r, srv, "/sub/doc.md", "/img/a.png")

	writeFile(t, dir, "sub/doc.md", "# doc 2\n")
	expectMessage(t, conn, msgReload, "a write of the page")
	writeFile(t, dir, "img/a.png", "y")
	expectMessage(t, conn, msgReload, "a write of a resource")
	writeFile(t, dir, "other/x.md", "y")
	expectNoMessage(t, conn, "a write in another directory")
	if isWatched(r, "/other") {
		t.Fatal("expected /other not watched")
	}
}

// A resource directory created after the page connected is added by the check, next to the
// page or where nobody watches its parent.
func TestMissingResourceDirectoryIsPolled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ page, res string }{
		{"/doc.md", "/img/a.png"},
		{"/docs/doc.md", "/assets/img/a.png"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, tc.page, "# doc\n")
		r := watchedReloader(t, dir)
		srv := serve(t, r)
		conn := openPage(t, r, srv, tc.page, tc.res)

		writeFile(t, dir, tc.res, "x")
		expectMessage(t, conn, msgReload, tc.res+" written in a directory created later")
	}
}

// A directory removed, or whose parent was renamed, loses its watch: the check drops it and
// adds the directory recreated at its path; writes in the renamed one reach nobody.
func TestReplacedDirectoryIsWatchedAgain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ replace, old string }{
		{"assets/img", ""},
		{"assets", "assets.old"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, "docs/doc.md", "# doc\n")
		writeFile(t, dir, "assets/img/a.png", "x")
		r := watchedReloader(t, dir)
		srv := serve(t, r)
		openPage(t, r, srv, "/docs/doc.md", "/assets/img/a.png")

		var err error
		if tc.old == "" {
			err = os.RemoveAll(filepath.Join(dir, tc.replace))
		} else {
			err = os.Rename(filepath.Join(dir, tc.replace), filepath.Join(dir, tc.old))
		}
		if err != nil {
			t.Fatal(err)
		}
		// A read past its deadline breaks a gorilla connection: a new page listens.
		time.Sleep(3 * reloadDebounce)
		conn := openPage(t, r, srv, "/docs/doc.md", "/assets/img/a.png")
		writeFile(t, dir, "assets/img/a.png", "y")
		expectMessage(t, conn, msgReload, "a resource written after "+tc.replace+" was replaced")
		eventually(t, "the new /assets/img watched", func() bool {
			r.subMu.Lock()
			defer r.subMu.Unlock()
			info, err := os.Stat(filepath.Join(dir, "assets", "img"))
			sub := r.subs["/assets/img"]
			return err == nil && sub.watched && os.SameFile(info, sub.info) && isWatched(r, "/assets/img")
		})
		if tc.old != "" {
			time.Sleep(3 * reloadDebounce)
			again := openPage(t, r, srv, "/docs/doc.md", "/assets/img/a.png")
			writeFile(t, dir, tc.old+"/img/a.png", "z")
			expectNoMessage(t, again, "a write in the renamed directory")
		}
	}
}

// Two sockets on one page: closing one sends nothing on the other. A second page keeps the
// directory watched once both are closed; closing it too keeps it watched for the grace delay,
// then releases it.
func TestDirectoryReleasedAfterLastPage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "doc.md", "# doc\n")
	writeFile(t, dir, "other.md", "# other\n")
	r := watchedReloader(t, dir)
	srv := serve(t, r)
	a := openPage(t, r, srv, "/doc.md")
	b := openPage(t, r, srv, "/doc.md")
	other := openPage(t, r, srv, "/other.md")

	writeFile(t, dir, "doc.md", "# doc 2\n")
	expectMessage(t, a, msgReload, "a write, on the first socket")
	expectMessage(t, b, msgReload, "a write, on the second socket")
	a.Close() //nolint:errcheck
	expectNoMessage(t, b, "the other socket closing")
	b.Close() //nolint:errcheck
	eventually(t, "one follower", func() bool { return followers(r, "/") == 1 })
	if !isWatched(r, "/") {
		t.Fatal("expected / watched while /other.md is open")
	}
	other.Close() //nolint:errcheck
	eventually(t, "no follower", func() bool { return followers(r, "/") == 0 })
	time.Sleep(testReleaseAfter / 4)
	if !isWatched(r, "/") {
		t.Fatal("expected / still watched during the grace delay")
	}
	time.Sleep(testReleaseAfter * 3)
	if isWatched(r, "/") {
		t.Fatal("expected / released after the grace delay")
	}
}

// A timer that fired while a page came back and left again, and waited on the lock, finds
// another timer in its place and releases nothing.
func TestReleaseIgnoresAStaleTimer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := watchedReloader(t, dir)
	r.unsubscribe(r.subscribe([]string{"/"}))
	r.subMu.Lock()
	sub := r.subs["/"]
	stale := sub.timer
	r.subMu.Unlock()
	r.unsubscribe(r.subscribe([]string{"/"}))

	r.release("/", sub, &stale)
	if !isWatched(r, "/") {
		t.Fatal("expected / still watched: the timer is stale")
	}
	r.subMu.Lock()
	current := sub.timer
	r.subMu.Unlock()
	r.release("/", sub, &current)
	if isWatched(r, "/") {
		t.Fatal("expected / released by the current timer")
	}
}

// Every page on a directory, the second one included, runs its hash check with the
// directory watched.
func TestSubscribeBeforeHashCheck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.md", "a")
	writeFile(t, dir, "b.md", "b")
	r := watchedReloader(t, dir)
	checks := make(chan bool, 2)
	r.beforeHashCheck = func() { checks <- isWatched(r, "/") }
	srv := serve(t, r)

	dialPage(t, srv, "/a.md")
	dialPage(t, srv, "/b.md")
	for range 2 {
		select {
		case watched := <-checks:
			if !watched {
				t.Fatal("expected / watched at the hash check")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("expected two hash checks")
		}
	}
}

// A page that stops answering pings is dropped with its directories; one that answers stays.
func TestDeadConnectionIsDropped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "doc.md", "# doc\n")
	writeFile(t, dir, "live/doc.md", "# live\n")
	r := watchedReloader(t, dir)
	r.pingEvery, r.pongWait = 50*time.Millisecond, 200*time.Millisecond
	srv := serve(t, r)

	live := dialPage(t, srv, "/live/doc.md")
	go func() { // reading is what answers the pings
		for {
			if _, _, err := live.NextReader(); err != nil {
				return
			}
		}
	}()
	dialPage(t, srv, "/doc.md") // never read: its pings go unanswered
	eventually(t, "one follower", func() bool { return followers(r, "/") == 1 })
	eventually(t, "the silent page dropped", func() bool { return followers(r, "/") == 0 })
	time.Sleep(4 * r.pongWait)
	if n := followers(r, "/live"); n != 1 {
		t.Fatalf("expected the page answering pings still followed, got %d", n)
	}
}

func TestRefusesAPageThatIsNotMarkdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "doc.md", "# doc\n")
	writeFile(t, dir, "a.png", "x")
	writeFile(t, dir, "sub/x.md", "x")
	r := watchedReloader(t, dir)
	srv := serve(t, r)

	for _, page := range []string{"/sub/", "/sub", "/a.png", "/missing.md"} {
		if conn, err := tryDialPage(srv, page); err == nil {
			conn.Close() //nolint:errcheck
			t.Errorf("page %s: expected the handshake refused", page)
		}
	}
	dialPage(t, srv, "/doc.md")

	// A foreign page gets the same answer whether the file exists or not.
	for _, page := range []string{"/doc.md", "/missing.md"} {
		query := url.Values{"page": {page}}.Encode()
		_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?"+query,
			http.Header{"Origin": {"http://evil.example"}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("page %s from a foreign origin: expected 403, got %v (%v)", page, resp, err)
		}
		if resp != nil {
			resp.Body.Close() //nolint:errcheck
		}
	}
}

// captureLog sends the log to a buffer for the test; not parallel, the log is global.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestGlobalDirectoryCap(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "new/doc.md", "# doc\n")
	for _, d := range []string{"a", "b", "c"} {
		writeFile(t, dir, d+"/x", "x")
	}
	r := watchedReloader(t, dir)
	r.maxDirs = 3
	r.subscribe([]string{"/a", "/b", "/c"})
	srv := serve(t, r)

	conn := dialPage(t, srv, "/new/doc.md")
	dialPage(t, srv, "/new/doc.md")
	time.Sleep(50 * time.Millisecond)
	if followers(r, "/new") != 0 || isWatched(r, "/new") {
		t.Fatal("expected /new not followed past the cap")
	}
	if n := strings.Count(logs.String(), "directories followed already"); n != 1 {
		t.Fatalf("expected the cap logged once for two pages, got %q", logs.String())
	}
	expectNoMessage(t, conn, "a page over the cap")
}

func TestMissingDirectoriesDoNotFillTheCap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/doc.md", "# doc\n")
	r := watchedReloader(t, dir)
	r.maxDirs = 2
	r.subscribe([]string{"/m1", "/m2", "/m3", "/m4", "/m5"})
	srv := serve(t, r)

	conn := openPage(t, r, srv, "/sub/doc.md")
	if !isWatched(r, "/sub") {
		t.Fatal("expected /sub watched, missing directories counted apart")
	}
	writeFile(t, dir, "sub/doc.md", "# doc 2\n")
	expectMessage(t, conn, msgReload, "a page next to five missing directories")
}

func TestPolledDirectoryCap(t *testing.T) {
	logs := captureLog(t)
	r := watchedReloader(t, t.TempDir())
	r.maxPolled = 3

	if counted := r.subscribe([]string{"/m1", "/m2", "/m3", "/m4", "/m5"}); len(counted) != 3 {
		t.Fatalf("expected 3 missing directories followed, got %v", counted)
	}
	if n := strings.Count(logs.String(), "missing directories followed already"); n != 1 {
		t.Fatalf("expected the polled cap logged once, got %q", logs.String())
	}
}

// A missing directory that appears while the watched cap is reached stays polled, and is
// watched once a slot frees.
func TestAppearingDirectoryWaitsForASlot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "e/x", "x")
	r := watchedReloader(t, dir)
	r.maxDirs = 1

	held := r.subscribe([]string{"/e"})
	r.subscribe([]string{"/img"})
	writeFile(t, dir, "img/a.png", "a")
	time.Sleep(3 * testCheckEvery)
	if isWatched(r, "/img") {
		t.Fatal("expected /img polled while the cap is reached")
	}
	r.unsubscribe(held)
	eventually(t, "/img watched once /e is released", func() bool { return isWatched(r, "/img") })
}

// A page over the cap still renders and reloads for what is counted; leaving, it does not
// drop a directory it was not counted on, even one another page follows since.
func TestCappedDirectoryIsNotUnsubscribed(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "d/a.md", "a")
	writeFile(t, dir, "f/b.md", "b")
	writeFile(t, dir, "f/c.md", "c")
	writeFile(t, dir, "e/x", "x")
	r := watchedReloader(t, dir)
	r.maxDirs = 2
	srv := serve(t, r)

	e := r.subscribe([]string{"/e"})
	dialPage(t, srv, "/d/a.md")
	eventually(t, "/d followed", func() bool { return followers(r, "/d") == 1 })
	b := dialPage(t, srv, "/f/b.md")
	time.Sleep(50 * time.Millisecond)
	if followers(r, "/f") != 0 || !strings.Contains(logs.String(), "/f and the next ones not watched") {
		t.Fatalf("expected /f over the cap and logged, log %q", logs.String())
	}

	r.unsubscribe(e)
	eventually(t, "/e released", func() bool { return !isWatched(r, "/e") })
	dialPage(t, srv, "/f/c.md")
	eventually(t, "/f followed by the page under the cap", func() bool { return followers(r, "/f") == 1 })
	b.Close() //nolint:errcheck
	time.Sleep(100 * time.Millisecond)
	if n := followers(r, "/f"); n != 1 {
		t.Fatalf("expected /f still followed once after the capped page left, got %d", n)
	}
}

func TestUnwatchableDirectoryIsLogged(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("mode 000 does not lock this user out")
	}
	logs := captureLog(t)
	dir := t.TempDir()
	writeFile(t, dir, "doc.md", "# doc\n")
	writeFile(t, dir, "two.md", "# two\n")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) //nolint:errcheck,gosec
	r := watchedReloader(t, dir)
	srv := serve(t, r)

	conn := openPage(t, r, srv, "/doc.md", "/locked/a.png")
	openPage(t, r, srv, "/two.md", "/locked/a.png")
	eventually(t, "/locked followed twice", func() bool { return followers(r, "/locked") == 2 })
	eventually(t, "/locked failed", func() bool {
		r.subMu.Lock()
		defer r.subMu.Unlock()
		return r.subs["/locked"].failed
	})
	if n := strings.Count(logs.String(), "watch:"); n != 1 {
		t.Fatalf("expected one log line, got %d: %q", n, logs.String())
	}
	writeFile(t, dir, "doc.md", "# doc 2\n")
	expectMessage(t, conn, msgReload, "a write of the page next to an unwatchable directory")
}
