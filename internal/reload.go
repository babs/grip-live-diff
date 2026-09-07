package internal

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

// Live reload: the page holds a websocket open, the server writes "reload" on it when a
// watched file changes, "annotations" when only a sidecar did. Adapted from
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
// DOM event: the page stays, the marks are refetched.
const reloadScript = `<script>
  function retry() { setTimeout(function () { listen(true) }, 1000) }
  function listen(isRetry) {
    var protocol = location.protocol === "https:" ? "wss://" : "ws://"
    var ws = new WebSocket(protocol + location.host + "` + reloadEndpoint + `")
    if (isRetry) ws.onopen = function () { location.reload() }
    ws.onmessage = function (msg) {
      if (msg.data === "` + msgReload + `") location.reload()
      else if (msg.data === "` + msgAnnotations + `") window.dispatchEvent(new Event("gld:annotations"))
    }
    ws.onclose = retry
  }
  listen(false)
</script>`

type reloader struct {
	cond     *sync.Cond
	upgrader websocket.Upgrader
	timer    *time.Timer
	// pending is the message of the burst being debounced; a reload outranks an
	// annotations refresh. seq and last are what a settled burst leaves for the waiters.
	pending string
	seq     uint64
	last    string
}

func newReloader() *reloader {
	return &reloader{
		cond: sync.NewCond(&sync.Mutex{}),
		// The page may be opened as 127.0.0.1 while the printed URL says localhost.
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

// broadcast wakes every waiting websocket with msg once the burst has settled.
func (r *reloader) broadcast(msg string) {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	if r.pending != msgReload {
		r.pending = msg
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(reloadDebounce, r.settle)
}

func (r *reloader) settle() {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	r.last, r.pending = r.pending, ""
	r.seq++
	r.cond.Broadcast()
}

// serveWS holds the connection and relays every settled burst until the page goes away.
// ponytail: a page that left is only noticed at the next change, when the write fails.
func (r *reloader) serveWS(w http.ResponseWriter, req *http.Request) {
	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	//nolint:errcheck
	defer conn.Close()
	r.cond.L.Lock()
	seen := r.seq
	for {
		for r.seq == seen {
			r.cond.Wait()
		}
		seen = r.seq
		msg := r.last
		r.cond.L.Unlock()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			return
		}
		r.cond.L.Lock()
	}
}

// watch follows dir and every directory under it, present or created later, and
// broadcasts on any change the message classify gives its file (path relative to dir),
// none to ignore it.
func (r *reloader) watch(dir string, classify func(name string) string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
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
				msg := classify(rel)
				if msg == "" || !e.Has(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) {
					continue
				}
				r.broadcast(msg)
			}
		}
	}()
	return nil
}
