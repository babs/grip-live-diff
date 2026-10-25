package internal

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

// Live reload: the page holds a websocket open, naming its markdown path, the hash it was
// rendered from and the local files it embeds; the server writes "reload" on it when one of
// those changes, "annotations" when only the page's sidecar or kept revisions did, nothing
// otherwise. Adapted from
// github.com/aarol/reload (MIT, Aaro Luomanen) so the watcher can tell files apart, which
// upstream cannot.

const reloadEndpoint = "/reload_ws"

// A burst of events from one save (editor temp file, rename, write) is one reload.
const reloadDebounce = 100 * time.Millisecond

const (
	msgReload      = "reload"
	msgAnnotations = "annotations"
)

// reloadScript is what the page runs; the websocket closing means the server went away, so
// it retries and reloads once it is back. A sidecar change is handed to annotate.js as a
// DOM event: the page stays, the marks are refetched. It waits for the body, whose [src]
// elements are the resources; the embedded assets are never a file on disk. A reload is
// first offered as a cancelable gld:reload event: annotate.js cancels it while the reader
// is selecting or typing a comment, and reloads itself once done.
const reloadScript = `<script>
  function reload() {
    if (document.dispatchEvent(new Event("gld:reload", { cancelable: true }))) location.reload()
  }
  function retry() { setTimeout(function () { listen(true) }, 1000) }
  function listen(isRetry) {
    var protocol = location.protocol === "https:" ? "wss://" : "ws://"
    var params = new URLSearchParams({ page: decodeURIComponent(location.pathname), hash: document.documentElement.dataset.fileHash || "" })
    var resources = new Set()
    document.querySelectorAll("[src]").forEach(function (el) {
      var u = new URL(el.src, location.href)
      if (u.origin === location.origin && !u.pathname.startsWith("` + staticPrefix + `")) resources.add(decodeURIComponent(u.pathname))
    })
    resources.forEach(function (res) { params.append("res", res) })
    var ws = new WebSocket(protocol + location.host + "` + reloadEndpoint + `?" + params)
    if (isRetry) ws.onopen = reload
    ws.onmessage = function (msg) {
      if (msg.data === "` + msgReload + `") reload()
      else if (msg.data === "` + msgAnnotations + `") window.dispatchEvent(new Event("gld:annotations"))
    }
    ws.onclose = retry
  }
  document.addEventListener("DOMContentLoaded", function () { listen(false) })
</script>`

const (
	// A reload closes the socket and opens a new one; kqueue reopens every file of a directory
	// it adds.
	releaseAfter = 30 * time.Second
	checkEvery   = 2 * time.Second
	pingEvery    = 30 * time.Second
	pongWait     = 60 * time.Second
	// Security: the server listens on every interface, any client names directories.
	maxDirsPerPage = 10
	maxWatchedDirs = 100
	maxPolledDirs  = 1000 // missing: a stat per check, no descriptor
)

type reloader struct {
	cond     *sync.Cond
	upgrader websocket.Upgrader
	timer    *time.Timer
	dir      http.Dir // set by watch; empty skips the page and hash checks on connect
	// pending holds the URL paths changed in the burst being debounced. seq and last are
	// what a settled burst leaves for the waiters; last is replaced, never mutated.
	pending map[string]struct{}
	seq     uint64
	last    map[string]struct{}

	// The directories the open pages follow, keyed by URL path. The watcher is only called
	// under subMu, which the event loop never takes: on Windows an Add waits on the event loop.
	subMu        sync.Mutex
	watcher      *fsnotify.Watcher // nil until watch
	root         string
	subs         map[string]*subscription
	nWatched     int // entries with watched set
	capLogged    bool
	polledLogged bool
	warned       map[string]struct{} // pages logged for naming too many directories
	stop         chan struct{}

	releaseAfter    time.Duration
	checkEvery      time.Duration
	pingEvery       time.Duration
	pongWait        time.Duration
	maxDirs         int
	maxPolled       int
	beforeHashCheck func() // tests only
}

// subscription counts the pages following a directory. info tells it from another directory
// later at the same path; timer is the pending release.
type subscription struct {
	n        int
	watched  bool
	failed   bool // never retried while the entry lives
	fdLogged bool
	info     os.FileInfo
	timer    *time.Timer
}

func newReloader() *reloader {
	return &reloader{
		cond: sync.NewCond(&sync.Mutex{}),
		// Security: page and hash would let any website probe which files exist here.
		upgrader:     websocket.Upgrader{CheckOrigin: sameOrigin},
		subs:         make(map[string]*subscription),
		warned:       make(map[string]struct{}),
		stop:         make(chan struct{}),
		releaseAfter: releaseAfter,
		checkEvery:   checkEvery,
		pingEvery:    pingEvery,
		pongWait:     pongWait,
		maxDirs:      maxWatchedDirs,
		maxPolled:    maxPolledDirs,
	}
}

// broadcast wakes every waiting websocket with the changed URL path once the burst has
// settled.
func (r *reloader) broadcast(name string) {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	if r.pending == nil {
		r.pending = make(map[string]struct{})
	}
	r.pending[name] = struct{}{}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(reloadDebounce, r.settle)
}

func (r *reloader) settle() {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	r.last, r.pending = r.pending, nil
	r.seq++
	r.cond.Broadcast()
}

// serveWS holds the connection and relays every settled burst that concerns the page until
// it goes away.
func (r *reloader) serveWS(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()
	page, resources := path.Clean("/"+query.Get("page")), query["res"]
	// Before the page check: a foreign page must not learn whether a file exists.
	if !r.upgrader.CheckOrigin(req) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	r.cond.L.Lock()
	dir := r.dir
	r.cond.L.Unlock()
	// Security: a page names what gets watched.
	if dir != "" {
		if isFile, err := isRegularFile(dir, page); !isMarkdownTarget(page, markdownPath) || err != nil || !isFile {
			http.Error(w, "not a markdown file", http.StatusNotFound)
			return
		}
	}
	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	//nolint:errcheck
	defer conn.Close()
	dirs, dropped := pageDirs(page, resources)
	if dropped > 0 {
		r.warnOnce(page, dropped)
	}
	// Before the hash check: a write between the two would be seen by neither.
	defer r.unsubscribe(r.subscribe(dirs))
	if r.beforeHashCheck != nil {
		r.beforeHashCheck()
	}
	closed, gone := false, r.keepAlive(conn)
	go func() {
		<-gone
		r.cond.L.Lock()
		closed = true
		r.cond.Broadcast()
		r.cond.L.Unlock()
	}()
	r.cond.L.Lock()
	seen := r.seq
	_, coming := r.pending[page]
	// The page opens this socket only once loaded: a write that landed since it was rendered
	// has no burst left to catch. Subscribed first, so a later write is not missed either; a
	// burst still debouncing delivers its own reload, or a file written nonstop reloads in a loop.
	if hash := query.Get("hash"); dir != "" && hash != "" && !coming {
		r.cond.L.Unlock()
		if content, err := readToString(dir, page); err == nil && fileHash(content) != hash {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msgReload)); err != nil {
				return
			}
		}
		r.cond.L.Lock()
	}
	for {
		for r.seq == seen && !closed {
			r.cond.Wait()
		}
		if closed {
			r.cond.L.Unlock()
			return
		}
		seen = r.seq
		changed := r.last
		r.cond.L.Unlock()
		if msg := pageMessage(page, resources, changed); msg != "" {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				return
			}
		}
		r.cond.L.Lock()
	}
}

// keepAlive pings the page and returns a channel closed once it went away or missed the
// pongWait. The page never writes, so a read only returns on that.
func (r *reloader) keepAlive(conn *websocket.Conn) <-chan struct{} {
	gone := make(chan struct{})
	//nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(r.pongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(r.pongWait)) })
	go func() {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				break
			}
		}
		close(gone)
	}()
	go func() {
		tick := time.NewTicker(r.pingEvery)
		defer tick.Stop()
		for {
			select {
			case <-gone:
				return
			case <-tick.C:
				//nolint:errcheck // a dead page is caught by the read deadline
				conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(r.pingEvery))
			}
		}
	}()
	return gone
}

// pageMessage is what a burst that changed these URL paths means for page: a reload when
// the page or one of its resources moved, which outranks an annotations refresh when only
// its sidecar or kept revisions did.
func pageMessage(page string, resources []string, changed map[string]struct{}) string {
	if _, ok := changed[page]; ok {
		return msgReload
	}
	for _, res := range resources {
		if _, ok := changed[res]; ok {
			return msgReload
		}
	}
	base := strings.TrimSuffix(page, path.Ext(page))
	if _, ok := changed[base+sidecarSuffix]; ok {
		return msgAnnotations
	}
	revisions := base + revisionsSuffix
	for name := range changed {
		if name == revisions || strings.HasPrefix(name, revisions+"/") {
			return msgAnnotations
		}
	}
	return ""
}

// pageDirs is the URL paths of the directories a page's message depends on, its own first,
// capped; dropped counts the ones past the cap. A resource that could leave the served tree
// once mapped to the file system is ignored.
func pageDirs(page string, resources []string) (dirs []string, dropped int) {
	dirs = []string{path.Dir(page)}
	seen := map[string]struct{}{dirs[0]: {}}
	for _, res := range resources {
		if strings.ContainsAny(res, "\\\x00") {
			continue
		}
		d := path.Dir(path.Clean("/" + res))
		if rel := strings.TrimPrefix(d, "/"); rel != "" && !filepath.IsLocal(filepath.FromSlash(rel)) {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		if len(dirs) == maxDirsPerPage {
			dropped++
			continue
		}
		seen[d] = struct{}{}
		dirs = append(dirs, d)
	}
	return dirs, dropped
}

// warnOnce logs a page naming too many directories, once: it reconnects at every reload.
func (r *reloader) warnOnce(page string, dropped int) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	if _, ok := r.warned[page]; ok {
		return
	}
	r.warned[page] = struct{}{}
	log.Printf("watch: %s follows more than %d directories, %d not watched", page, maxDirsPerPage, dropped)
}

// watch creates the watcher over dir and walks nothing: the pages subscribe what they follow.
func (r *reloader) watch(dir string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	r.cond.L.Lock()
	r.dir = http.Dir(dir)
	r.cond.L.Unlock()
	r.subMu.Lock()
	r.watcher, r.root = w, dir
	r.subMu.Unlock()

	go r.checkLoop()
	// Never takes subMu, see reloader.
	go func() {
		for {
			select {
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Println("watch:", err)
			case e, ok := <-w.Events:
				if !ok {
					return
				}
				if !e.Has(fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename) {
					continue
				}
				rel, err := filepath.Rel(dir, e.Name)
				if err != nil {
					rel = e.Name
				}
				r.broadcast(path.Clean("/" + filepath.ToSlash(rel)))
			}
		}
	}()
	return nil
}

// close stops the check and the watcher; tests only.
func (r *reloader) close() {
	close(r.stop)
	//nolint:errcheck
	r.watcher.Close()
}

// subscribe follows these directories and returns the ones it counted, the only ones to
// unsubscribe. The add runs under the lock: a second page on a directory waits for it too.
func (r *reloader) subscribe(dirs []string) []string {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	if r.watcher == nil {
		return nil
	}
	var counted []string
	for _, d := range dirs {
		sub, ok := r.subs[d]
		if !ok {
			if !r.admit(d) {
				continue
			}
			sub = &subscription{}
			r.subs[d] = sub
			r.addLocked(d, sub)
		}
		if sub.timer != nil {
			sub.timer.Stop()
			sub.timer = nil
		}
		sub.n++
		counted = append(counted, d)
	}
	return counted
}

// admit tells whether a new directory fits its cap: an existing one counts against the
// watched ones, a missing one against the polled ones, so missing directories named by one
// client do not stop the other pages from reloading.
func (r *reloader) admit(d string) bool {
	if info, err := os.Stat(r.fsPath(d)); err == nil && info.IsDir() {
		if r.nWatched < r.maxDirs {
			return true
		}
		if !r.capLogged {
			r.capLogged = true
			log.Printf("watch: %d directories followed already, %s and the next ones not watched", r.maxDirs, d)
		}
		return false
	}
	if len(r.subs)-r.nWatched < r.maxPolled {
		return true
	}
	if !r.polledLogged {
		r.polledLogged = true
		log.Printf("watch: %d missing directories followed already, %s and the next ones not followed", r.maxPolled, d)
	}
	return false
}

// unsubscribe drops one follower of each directory; the last one starts the release delay.
func (r *reloader) unsubscribe(dirs []string) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for _, d := range dirs {
		sub := r.subs[d]
		if sub == nil {
			continue
		}
		if sub.n--; sub.n > 0 {
			continue
		}
		var timer *time.Timer
		// The callback reads timer under subMu: it may fire before this assignment.
		timer = time.AfterFunc(r.releaseAfter, func() { r.release(d, sub, &timer) })
		sub.timer = timer
	}
}

// release stops watching d unless a page came back or another timer replaced this one: a
// timer stopped too late may already wait on the lock.
func (r *reloader) release(d string, sub *subscription, timer **time.Timer) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	if r.subs[d] != sub || sub.n > 0 || sub.timer != *timer {
		return
	}
	delete(r.subs, d)
	if sub.watched {
		//nolint:errcheck
		r.watcher.Remove(r.fsPath(d))
		r.nWatched--
		r.capLogged = false
	} else {
		r.polledLogged = false
	}
}

func (r *reloader) checkLoop() {
	tick := time.NewTicker(r.checkEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
			r.check()
		}
	}
}

// check drops the watch of a directory gone or replaced (a renamed parent leaves the watch on
// the old directory), and adds one that appeared, broadcasting its files written before the
// add. A directory recreated between two checks may reuse the inode: the watch list tells.
func (r *reloader) check() {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	live := make(map[string]struct{})
	for _, p := range r.watcher.WatchList() {
		live[p] = struct{}{}
	}
	for d, sub := range r.subs {
		if sub.n == 0 || sub.failed {
			continue
		}
		p := r.fsPath(d)
		if sub.watched {
			_, listed := live[p]
			if info, err := os.Stat(p); listed && err == nil && os.SameFile(info, sub.info) {
				continue
			}
			//nolint:errcheck
			r.watcher.Remove(p)
			sub.watched, sub.info = false, nil
			r.nWatched--
		}
		if r.addLocked(d, sub); !sub.watched {
			continue
		}
		// ponytail: every file is broadcast, so a directory back from EMFILE reloads its pages
		// with no real change; compare mtimes with the add if it ever matters.
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				r.broadcast(path.Join(d, e.Name()))
			}
		}
	}
}

// addLocked watches d if it exists and a slot is free; otherwise it stays polled. A failed Add
// is removed, which closes what kqueue opened; out of descriptors, the next check tries again.
func (r *reloader) addLocked(d string, sub *subscription) {
	p := r.fsPath(d)
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() || r.nWatched >= r.maxDirs {
		return
	}
	if err := r.watcher.Add(p); err != nil {
		//nolint:errcheck
		r.watcher.Remove(p)
		if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
			if !sub.fdLogged {
				sub.fdLogged = true
				log.Println("watch:", err)
			}
			return
		}
		sub.failed = true
		log.Println("watch:", err)
		return
	}
	sub.watched, sub.info = true, info
	r.nWatched++
}

func (r *reloader) fsPath(d string) string {
	return filepath.Join(r.root, filepath.FromSlash(path.Clean("/"+d)))
}
