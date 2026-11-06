package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Instance registry: one file per running instance in a directory shared by every instance of
// the user, so an instance knows which of its pages another one has open, and whose agent a
// reader message is for.

const (
	registryEvery = 15 * time.Second
	// Four heartbeats: an instance missing them is stopped or asleep.
	registryLive = 60 * time.Second
	// A PID, a port, a root and the open pages; anything bigger is not an instance's.
	maxInstanceBytes = 1 << 20
)

// The only shape an instance file takes; a temp file of replaceFile starts with a dot.
var instanceFileName = regexp.MustCompile(`^([0-9]+)\.json$`)

// instance is one file of the registry. Pages are sidecar keys (pageKey).
type instance struct {
	PID    int64    `json:"pid"`
	Port   int      `json:"port"`
	Root   string   `json:"root"`
	SeenAt string   `json:"seen_at"`
	Pages  []string `json:"pages"`
}

// DefaultRegistryDir is the instances directory under the user's cache directory, "" when
// there is none: routing is then off.
func DefaultRegistryDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		log.Printf("instance registry off: %v", err)
		return ""
	}
	return filepath.Join(dir, "grip-live-diff", "instances")
}

// pidAlive is processAlive for a PID read from a file. Out of range, Kill would truncate the
// PID to another, live, process.
func pidAlive(pid int64) bool {
	return pid > 0 && pid <= math.MaxInt32 && processAlive(int(pid))
}

// resolveDir makes d absolute and follows its symlinks; what cannot be resolved stays as is.
func resolveDir(d string) string {
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	if real, err := filepath.EvalSymlinks(d); err == nil {
		d = real
	}
	return d
}

// pageKey names the sidecar of target alike in every instance: its directory resolved, the
// file name as served, which sidecarPath does not resolve either.
func pageKey(dir http.Dir, target string) string {
	p := filepath.Join(string(dir), filepath.FromSlash(path.Clean("/"+target)))
	return filepath.Join(resolveDir(filepath.Dir(p)), filepath.Base(p))
}

// instanceRoot is the tree the agent of an instance watches: the start argument, or its
// directory when it names a file.
func instanceRoot(arg string) string {
	if arg == "" {
		arg = "."
	}
	if info, err := os.Stat(arg); err == nil && !info.IsDir() {
		arg = filepath.Dir(arg)
	}
	return resolveDir(arg)
}

// under tells whether key is under root.
func under(root, key string) bool {
	rel, err := filepath.Rel(root, key)
	return err == nil && filepath.IsLocal(rel)
}

func readInstance(p string) (instance, error) {
	var in instance
	data, err := readLimited(p, maxInstanceBytes)
	if err == nil {
		err = json.Unmarshal(data, &in)
	}
	return in, err
}

// scanRegistry returns the instances of dir whose PID runs, by port, and removes the files of
// the others. A file that does not parse is skipped: it may be written by a newer version.
func scanRegistry(dir string) []instance {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []instance
	for _, e := range entries {
		m := instanceFileName.FindStringSubmatch(e.Name())
		// Not a file an instance writes: a FIFO would block the open, a directory the removal.
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		pid, _ := strconv.ParseInt(m[1], 10, 64)
		if !pidAlive(pid) {
			// Another instance may purge it at the same time.
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Println("registry:", err)
			}
			continue
		}
		if in, err := readInstance(p); err == nil && in.PID == pid {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// fresh tells an instance that wrote its heartbeat within liveFor from one stopped or asleep; a
// seen_at as far ahead of the clock is no heartbeat either.
func fresh(in instance, now time.Time, liveFor time.Duration) bool {
	t, err := time.Parse(time.RFC3339, in.SeenAt)
	d := now.Sub(t)
	return err == nil && d < liveFor && d > -liveFor
}

// liveOf keeps the instances of all that wrote their heartbeat within liveFor.
func liveOf(all []instance, now time.Time, liveFor time.Duration) []instance {
	var out []instance
	for _, in := range all {
		if fresh(in, now, liveFor) {
			out = append(out, in)
		}
	}
	return out
}

// peers is one read of the registry: the live instances, self included, by port. self is 0
// when routing is off; live is then empty.
type peers struct {
	self int
	live []instance
}

// owner is the instance whose agent handles a reader message routed to via: via itself when
// live and its root contains key, else the live instance of lowest port whose root does, else 0.
func (p peers) owner(key string, via int) int {
	first := 0
	for _, in := range p.live {
		if !under(in.Root, key) {
			continue
		}
		if in.Port == via {
			return via
		}
		if first == 0 {
			first = in.Port
		}
	}
	return first
}

// owns tells whether the instance on port owns a message on key routed to via. Routing off
// (self 0) owns all; no port owns nothing.
func (p peers) owns(key string, via, port int) bool {
	return p.self == 0 || (port != 0 && p.owner(key, via) == port)
}

// targets is where the reader can send a message on key: this instance when its root contains
// key, then the other instances that have the page open and whose root contains it.
func (p peers) targets(key string) []int {
	out := []int{}
	for _, in := range p.live {
		if in.Port == p.self && under(in.Root, key) {
			out = append(out, in.Port)
		}
	}
	for _, in := range p.live {
		if in.Port != p.self && under(in.Root, key) && slices.Contains(in.Pages, key) {
			out = append(out, in.Port)
		}
	}
	return out
}

// ports lists the ports of the live instances, so the page can tell a gone via.
func (p peers) ports() []int {
	out := make([]int, 0, len(p.live))
	for _, in := range p.live {
		out = append(out, in.Port)
	}
	return out
}

// pick is the via a new reader message gets when the reader asked for asked: asked when it is
// a target, else the first target, else the owner, else 0.
func (p peers) pick(key string, asked int) int {
	t := p.targets(key)
	switch {
	case asked != 0 && slices.Contains(t, asked):
		return asked
	case len(t) > 0:
		return t[0]
	}
	return p.owner(key, 0)
}

// routingInfo is what the page needs of a registry read for one file.
type routingInfo struct {
	Self    int   `json:"self"`
	Targets []int `json:"targets"`
	Live    []int `json:"live"`
}

func (p peers) info(self int, key string) routingInfo {
	return routingInfo{Self: self, Targets: p.targets(key), Live: p.ports()}
}

// registry keeps the file of this instance and reads the others.
type registry struct {
	dir   string
	pid   int64
	port  int
	root  string
	every time.Duration
	// pages maps the URL paths open in this instance to their sidecar key; notify tells the
	// sockets of a page to refetch its annotations.
	pages  func() map[string]string
	notify func(page string)
	out    io.Writer

	mu         sync.Mutex
	closed     bool
	failLogged bool
	written    atomic.Int64        // unix nanoseconds of the last heartbeat on disk; read unlocked
	warned     map[string]struct{} // sidecar key + PID of the other instance
	targets    map[string][]int    // URL path -> targets at the last read
	live       []int               // ports at the last read; nil before the first
	stop       chan struct{}
}

func newRegistry(dir string, port int, root string, out io.Writer) *registry {
	return &registry{
		dir:     dir,
		pid:     int64(os.Getpid()),
		port:    port,
		root:    root,
		every:   registryEvery,
		pages:   func() map[string]string { return nil },
		notify:  func(string) {},
		out:     out,
		warned:  make(map[string]struct{}),
		targets: make(map[string][]int),
		stop:    make(chan struct{}),
	}
}

func (g *registry) path() string {
	return filepath.Join(g.dir, strconv.FormatInt(g.pid, 10)+".json")
}

// self is this instance as the others read it, with the pages open.
func (g *registry) self(open map[string]string) instance {
	keys := slices.Compact(slices.Sorted(maps.Values(open)))
	if keys == nil {
		keys = []string{} // [] in JSON, not null
	}
	return instance{PID: g.pid, Port: g.port, Root: g.root, SeenAt: time.Now().Format(time.RFC3339), Pages: keys}
}

// start writes the file of this instance, once the files of dead instances are removed. An
// error turns routing off.
func (g *registry) start() error {
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(g.dir, 0o700); err != nil {
		return err
	}
	scanRegistry(g.dir)
	return g.write(g.self(g.pages()))
}

func (g *registry) write(me instance) error {
	data, err := json.MarshalIndent(me, "", "  ")
	if err != nil {
		return err
	}
	if err := replaceFile(g.path(), 0o600, append(data, '\n')); err != nil {
		return err
	}
	g.written.Store(time.Now().UnixNano())
	return nil
}

// read returns the live instances: the others from their files, this one as it stands.
func (g *registry) read() peers {
	return g.readWith(g.self(g.pages()))
}

// readWith is read with this instance as me. A heartbeat not on disk for registryLive turns
// routing off: the others and pending no longer see this instance, a via to it would go elsewhere.
func (g *registry) readWith(me instance) peers {
	if time.Since(time.Unix(0, g.written.Load())) >= registryLive {
		return peers{}
	}
	live := []instance{me}
	for _, in := range liveOf(scanRegistry(g.dir), time.Now(), registryLive) {
		if in.PID != g.pid {
			live = append(live, in)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Port < live[j].Port })
	return peers{self: g.port, live: live}
}

// tick rewrites the file of this instance, reads the others, warns of a page newly shared and
// tells a page whose targets changed, or every page when the live instances changed: a message
// may be routed to an instance that never had the page.
func (g *registry) tick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	open := g.pages()
	me := g.self(open)
	// Logged once per run of failures: past registryLive, the others no longer see this instance.
	switch err := g.write(me); {
	case err != nil && !g.failLogged:
		g.failLogged = true
		log.Println("registry:", err)
	case err == nil && g.failLogged:
		g.failLogged = false
		log.Println("registry: heartbeat written again")
	}
	p := g.readWith(me)
	live := p.ports()
	liveChanged := g.live != nil && !slices.Equal(g.live, live)
	g.live = live
	for page, key := range open {
		for _, in := range p.live {
			if in.PID == g.pid || !slices.Contains(in.Pages, key) {
				continue
			}
			id := key + "\x00" + strconv.FormatInt(in.PID, 10)
			if _, ok := g.warned[id]; ok {
				continue
			}
			g.warned[id] = struct{}{}
			rel, err := filepath.Rel(g.root, key)
			if err != nil {
				rel = key
			}
			_, _ = fmt.Fprintf(g.out, "⚠ %s also open in :%d (pid %d)\n", rel, in.Port, in.PID)
		}
		// First seen: the page fetched its targets as it loaded; only a banner is worth a refetch.
		t := p.targets(key)
		prev, seen := g.targets[page]
		changed := seen && (liveChanged || !slices.Equal(prev, t))
		if changed || (!seen && len(t) > 1) {
			g.notify(page)
		}
		g.targets[page] = t
	}
	for page := range g.targets {
		if _, ok := open[page]; !ok {
			delete(g.targets, page)
		}
	}
}

func (g *registry) run() {
	t := time.NewTicker(g.every)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			g.tick()
		}
	}
}

// close stops the heartbeat and removes the file of this instance.
func (g *registry) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.stop)
	if err := os.Remove(g.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Println("registry:", err)
	}
}
