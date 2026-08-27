package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aarol/reload"
	chroma_html "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/babs/grip-live-diff/defaults"
)

const defaultHTMLTitle = "grip-live-diff - markdown preview"

// Diff references selectable through the ?diff= query parameter.
const (
	diffModeOpen = "open" // compare with the version served when the file was first opened
	diffModeLast = "last" // compare with the content as it was before the last change
	diffModeHead = "head" // compare with the file as committed in git HEAD
)

// Past this many distinct files the oldest baseline is dropped: without a bound the
// server retains every document it has ever served for the life of the process.
const maxTrackedPaths = 128

// snapshot holds the two reference versions of a file, plus the last content served.
type snapshot struct {
	baseline []byte
	prev     []byte
	cur      []byte
}

type Server struct {
	parser       *Parser
	boundingBox  bool
	host         string
	port         int
	browser      bool
	enableReload bool

	mu        sync.Mutex
	snapshots map[string]*snapshot
	tracked   []string // insertion order of snapshots, oldest first
}

// gitTimeout bounds the git calls: a slow repository must not hold a request open.
const gitTimeout = 5 * time.Second

// errNoCommittedVersion reports that git ran and has no HEAD version of the file —
// untracked, or the repository has no commit yet.
var errNoCommittedVersion = errors.New("no committed version")

// gitHeadContent returns the file as committed in HEAD. A missing committed version is
// reported as errNoCommittedVersion; any other failure (timeout, git unavailable) is not.
func gitHeadContent(ctx context.Context, dir http.Dir, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// "HEAD:./name" resolves relative to -C, and the "./" also keeps a leading dash in a
	// filename from being read as an option.
	//nolint:gosec // name has already been resolved to a regular file under dir
	out, err := exec.CommandContext(ctx, "git", "-C", string(dir), "show", "HEAD:./"+strings.TrimPrefix(name, "/")).Output()
	if err != nil {
		// An ExitError with a live context is git itself answering "not there"; anything
		// else means the reference could not be read at all.
		var exitErr *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exitErr) {
			err = errNoCommittedVersion
		}
		return nil, fmt.Errorf("git show HEAD of %s: %w", name, err)
	}
	return out, nil
}

func isGitWorkTree(dir http.Dir) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	//nolint:gosec // fixed arguments
	return exec.CommandContext(ctx, "git", "-C", string(dir), "rev-parse", "--is-inside-work-tree").Run() == nil
}

func NewServer(host string, port int, boundingBox bool, browser bool, enableReload bool, parser *Parser) *Server {
	return &Server{
		host:         host,
		port:         port,
		boundingBox:  boundingBox,
		browser:      browser,
		enableReload: enableReload,
		parser:       parser,
		snapshots:    make(map[string]*snapshot),
	}
}

// record updates the references of path and returns them. prev only moves when the
// content actually changed, so a plain reload never consumes an iteration.
func (s *Server) record(path string, content []byte) (baseline, prev []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, ok := s.snapshots[path]
	switch {
	case !ok:
		snap = &snapshot{baseline: content, prev: content, cur: content}
		s.putLocked(path, snap)
	case !bytes.Equal(snap.cur, content):
		snap.prev, snap.cur = snap.cur, content
	}
	return snap.baseline, snap.prev
}

// resetReferences makes content the new comparison point for path.
func (s *Server) resetReferences(path string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(path, &snapshot{baseline: content, prev: content, cur: content})
}

// putLocked stores snap under path, evicting the oldest entry past maxTrackedPaths.
func (s *Server) putLocked(path string, snap *snapshot) {
	if _, known := s.snapshots[path]; !known {
		s.tracked = append(s.tracked, path)
		if len(s.tracked) > maxTrackedPaths {
			delete(s.snapshots, s.tracked[0])
			s.tracked = s.tracked[1:]
		}
	}
	s.snapshots[path] = snap
}

func (s *Server) Serve(file string) error {
	directory := path.Dir(file)
	filename := path.Base(file)

	var reloadMiddleware *reload.Reloader
	if s.enableReload {
		reloadMiddleware = reload.New(directory)
		reloadMiddleware.DebugLog = log.New(io.Discard, "", 0)
		// Fix WebSocket CORS issues for development
		reloadMiddleware.Upgrader.CheckOrigin = func(r *http.Request) bool {
			return true
		}
	}

	dir := http.Dir(directory)
	handler := s.newHandler(dir)

	addr := fmt.Sprintf("http://%s:%d/", s.host, s.port)
	if file == "" {
		// If README.md exists then open README.md at beginning
		readme := "README.md"
		f, err := dir.Open(readme)
		if err == nil {
			//nolint:errcheck
			defer f.Close()
		}
		if err == nil {
			addr, _ = url.JoinPath(addr, readme)
		}
	} else {
		addr, _ = url.JoinPath(addr, filename)
	}

	fmt.Printf("🚀 Starting server: %s\n", addr)

	if s.browser {
		err := Open(addr)
		if err != nil {
			fmt.Println("❌ Error opening browser:", err)
		}
	}

	if s.enableReload {
		handler = reloadMiddleware.Handle(handler)
		fmt.Printf("📡 Auto-reload enabled. Files will trigger browser refresh.\n")
	} else {
		fmt.Printf("🔄 Auto-reload disabled. Use F5 to manually refresh.\n")
	}
	return http.ListenAndServe(fmt.Sprintf(":%d", s.port), handler)
}

func (s *Server) newHandler(dir http.Dir) http.Handler {
	// Probed once: it decides whether the toggle offers the "last commit" reference at all.
	hasGit := isGitWorkTree(dir)

	fileServer := http.FileServer(dir)
	mux := http.NewServeMux()
	mux.Handle("/static/", http.FileServer(http.FS(defaults.StaticFiles)))

	regex := regexp.MustCompile(`(?i)\.md$`)
	mux.HandleFunc("/__baseline", func(w http.ResponseWriter, r *http.Request) {
		s.handleResetBaseline(w, r, dir, regex)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if regex.MatchString(r.URL.Path) {
			isFile, err := isRegularFile(dir, r.URL.Path)
			if err == nil && isFile {
				setNoCacheHeaders(w)

				bytes, err := readToString(dir, r.URL.Path)
				if err != nil {
					log.Fatal(err)
					return
				}
				htmlContent, err := s.parser.MdToHTML(bytes)
				if err != nil {
					log.Fatal(err)
					return
				}

				page, err := s.buildPage(r, dir, hasGit, bytes, htmlContent)
				if err != nil {
					log.Println(err)
				}

				err = serveTemplate(w, page)
				if err != nil {
					log.Fatal(err)
					return
				}
				return
			}
		}

		isDirectory, err := isDirectory(dir, r.URL.Path)
		if err == nil && isDirectory {
			setNoCacheHeaders(w)
			stripCacheValidators(r)
		}

		fileServer.ServeHTTP(w, r)
	})

	return mux
}

func readToString(dir http.Dir, filename string) ([]byte, error) {
	f, err := dir.Open(filename)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck
	defer f.Close()

	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type htmlStruct struct {
	Content      template.HTML
	BoundingBox  bool
	CssCodeLight template.CSS
	CssCodeDark  template.CSS
	Title        string

	Path            string
	DiffMode        string
	HasChanges      bool
	HasGit          bool
	DiffEmpty       bool
	DiffUnavailable bool
	DiffRefMissing  bool
	CanMarkAsRead   bool
	DiffHref        string
	DiffTitle       string
	DiffLabel       string
}

func (s *Server) pageTitle(filename string) string {
	title := formatFilenameTitle(filename)
	if title == "" {
		return defaultHTMLTitle
	}
	return title
}

func formatFilenameTitle(filename string) string {
	filename = path.Base(filename)
	extension := path.Ext(filename)
	if strings.EqualFold(extension, ".md") {
		filename = strings.TrimSuffix(filename, extension)
	}

	filename = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' {
			return ' '
		}
		return r
	}, filename)

	words := strings.Fields(filename)
	for i, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		words[i] = string(unicode.ToUpper(first)) + word[size:]
	}
	return strings.Join(words, " ")
}

// diffControls fills the labels of the toggle, which cycles off → since open → last edit
// → last commit (only inside a git work tree) → off.
func (h *htmlStruct) diffControls() {
	h.CanMarkAsRead = h.DiffMode != diffModeHead // the git reference is not ours to move

	switch h.DiffMode {
	case diffModeOpen:
		h.DiffHref = h.Path + "?diff=" + diffModeLast
		h.DiffTitle = "Comparing with the version opened — switch to the last edit only"
		h.DiffLabel = "Diff mode: since file open"
	case diffModeLast:
		h.DiffHref = h.Path
		h.DiffTitle = "Comparing with the version before the last edit — click to hide changes"
		if h.HasGit {
			h.DiffHref = h.Path + "?diff=" + diffModeHead
			h.DiffTitle = "Comparing with the version before the last edit — switch to the last commit"
		}
		h.DiffLabel = "Diff mode: last edit"
	case diffModeHead:
		h.DiffHref = h.Path
		h.DiffTitle = "Comparing with the last commit — click to hide changes"
		switch {
		case h.DiffRefMissing:
			h.DiffLabel = "Diff mode: no committed version to compare with"
		case h.DiffUnavailable:
			h.DiffLabel = "Diff mode: commit reference could not be read"
		default:
			h.DiffLabel = "Diff mode: since last commit"
		}
	default:
		h.DiffHref = h.Path + "?diff=" + diffModeOpen
		h.DiffTitle = "Show changes since this file was opened"
	}
}

// buildPage records the served content as a reference and, in diff mode, annotates the
// rendered HTML with the changes against the requested reference.
func (s *Server) buildPage(r *http.Request, dir http.Dir, hasGit bool, content, htmlContent []byte) (htmlStruct, error) {
	baseline, prev := s.record(r.URL.Path, content)

	page := htmlStruct{
		Content:      template.HTML(htmlContent), //nolint:gosec // rendered markdown is the payload
		BoundingBox:  s.boundingBox,
		CssCodeLight: template.CSS(getCssCode("github")),
		CssCodeDark:  template.CSS(getCssCode("github-dark")),
		Title:        s.pageTitle(r.URL.Path),
		Path:         r.URL.Path,
		HasChanges:   !bytes.Equal(baseline, content),
		HasGit:       hasGit,
	}

	var reference []byte
	switch r.URL.Query().Get("diff") {
	case diffModeOpen:
		page.DiffMode, reference = diffModeOpen, baseline
	case diffModeLast:
		page.DiffMode, reference = diffModeLast, prev
	case diffModeHead:
		if hasGit {
			page.DiffMode = diffModeHead
			head, err := gitHeadContent(r.Context(), dir, r.URL.Path)
			if err != nil {
				page.DiffUnavailable = true
				page.DiffRefMissing = errors.Is(err, errNoCommittedVersion)
			}
			reference = head
		}
	}

	switch {
	case page.DiffMode == "" || page.DiffUnavailable:
	case bytes.Equal(reference, content):
		page.DiffEmpty = true
	default:
		referenceHTML, err := s.parser.MdToHTML(reference)
		if err != nil {
			// The diff is a convenience: serve the plain document rather than nothing.
			page.DiffMode = ""
			page.diffControls()
			return page, fmt.Errorf("render reference version of %s: %w", r.URL.Path, err)
		}
		page.Content = template.HTML(DiffHTML(referenceHTML, htmlContent)) //nolint:gosec // idem
	}

	page.diffControls()
	return page, nil
}

// handleResetBaseline promotes the content currently on disk to the new comparison point.
func (s *Server) handleResetBaseline(w http.ResponseWriter, r *http.Request, dir http.Dir, regex *regexp.Regexp) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// The server listens on every interface, so a page from another origin must not be
	// able to silently drop the comparison point.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
	}

	target := r.FormValue("path")
	// Only a markdown path of this server is acceptable, so the redirect below can never
	// be pointed at another host.
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || !regex.MatchString(target) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	content, err := readToString(dir, target)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	s.resetReferences(target, content)

	// Come back in the diff mode the reader was in, so marking as read does not
	// silently drop them out of diff view.
	redirect := target
	switch mode := r.FormValue("diff"); mode {
	case diffModeOpen, diffModeLast:
		redirect += "?diff=" + mode
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func serveTemplate(w http.ResponseWriter, html htmlStruct) error {
	w.Header().Set("Content-Type", "text/html")
	tmpl, err := template.ParseFS(defaults.Templates, "templates/layout.html")
	if err != nil {
		return err
	}
	err = tmpl.Execute(w, html)
	return err
}

func getCssCode(style string) string {
	buf := new(strings.Builder)
	formatter := chroma_html.New(chroma_html.WithClasses(true))
	s := styles.Get(style)
	_ = formatter.WriteCSS(buf, s)
	return buf.String()
}

func setNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func stripCacheValidators(r *http.Request) {
	r.Header.Del("If-Modified-Since")
	r.Header.Del("If-None-Match")
}

func isDirectory(dir http.Dir, name string) (bool, error) {
	file, err := dir.Open(name)
	if err != nil {
		return false, err
	}
	//nolint:errcheck
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, err
	}

	return info.IsDir(), nil
}

func isRegularFile(dir http.Dir, name string) (bool, error) {
	file, err := dir.Open(name)
	if err != nil {
		return false, err
	}
	//nolint:errcheck
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, err
	}

	return !info.IsDir(), nil
}
