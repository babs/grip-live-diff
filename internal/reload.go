package internal

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
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
// elements are the resources; the embedded assets are never a file on disk.
const reloadScript = `<script>
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
    if (isRetry) ws.onopen = function () { location.reload() }
    ws.onmessage = function (msg) {
      if (msg.data === "` + msgReload + `") location.reload()
      else if (msg.data === "` + msgAnnotations + `") window.dispatchEvent(new Event("gld:annotations"))
    }
    ws.onclose = retry
  }
  document.addEventListener("DOMContentLoaded", function () { listen(false) })
</script>`

type reloader struct {
	cond     *sync.Cond
	upgrader websocket.Upgrader
	timer    *time.Timer
	dir      http.Dir // set by watch; empty skips the hash check on connect
	// pending holds the URL paths changed in the burst being debounced. seq and last are
	// what a settled burst leaves for the waiters; last is replaced, never mutated.
	pending map[string]struct{}
	seq     uint64
	last    map[string]struct{}
}

func newReloader() *reloader {
	return &reloader{
		cond: sync.NewCond(&sync.Mutex{}),
		// Security: page and hash would let any website probe which files exist here.
		upgrader: websocket.Upgrader{CheckOrigin: sameOrigin},
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
// ponytail: a page that left is only noticed at the next change it follows, when the write fails.
func (r *reloader) serveWS(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()
	page, resources := path.Clean("/"+query.Get("page")), query["res"]
	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	//nolint:errcheck
	defer conn.Close()
	r.cond.L.Lock()
	seen, dir := r.seq, r.dir
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
		for r.seq == seen {
			r.cond.Wait()
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

// watch follows dir and every directory under it, present or created later, and
// broadcasts the URL path of every changed file.
func (r *reloader) watch(dir string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	r.cond.L.Lock()
	r.dir = http.Dir(dir)
	r.cond.L.Unlock()
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(p)
		}
		return nil
	})
	if err != nil {
		//nolint:errcheck
		w.Close()
		return err
	}

	go func() {
		defer w.Close() //nolint:errcheck
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
				if e.Has(fsnotify.Create) {
					if info, err := os.Stat(e.Name); err == nil && info.IsDir() {
						//nolint:errcheck
						w.Add(e.Name)
					}
				}
				rel, err := filepath.Rel(dir, e.Name)
				if err != nil {
					rel = e.Name
				}
				if !e.Has(fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename) {
					continue
				}
				r.broadcast("/" + filepath.ToSlash(rel))
			}
		}
	}()
	return nil
}
