package internal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/titanous/json5"
)

const (
	sidecarSuffix = ".annotations.json5"
	// Beside the sidecar, one copy of the file per version that has annotations against it.
	revisionsSuffix = ".annotations.d"
	sidecarVersion  = 2
	// A sidecar is a few dozen comments; anything bigger is not a browser talking.
	maxSidecarBytes = 1 << 20

	byReader = "reader"
	byAgent  = "agent"
)

// sidecarHeader tells the agent, in the file itself, how to take part; rewritten on
// every save, so it survives a tool that drops comments.
const sidecarHeader = `// grip-live-diff annotations for %s. JSON5: comments and trailing commas are fine.
//
// An entry is a thread between the reader and you, the agent. It is a request while its last
// message is the reader's, until you answer it or delete the entry. To take part:
//   - take an exclusive flock on this file while you edit it; the preview does the same;
//   - answer by appending { by: "agent", at: <RFC 3339>, text } to thread; once the request is
//     handled, set status: "done" as well, or delete the entry;
//   - a reader message after yours reopens the entry (status is dropped): handle it again;
//   - never edit or remove reader messages, id or file_hash;
//   - if you rewrite an annotated passage, put the new wording in exact so the mark follows it;
//   - exact / prefix / suffix quote the rendered text; lines is a hint into the markdown source,
//     recomputed on every save from the preview, so trust exact first; exact: null = whole document;
//   - revisions maps a file_hash to a copy of the file as it was when those entries were written;
//     read one only for an entry whose exact is gone from the file. Managed by the preview.
//
// Entry: { id, exact, prefix, suffix, lines: [start, end] | null, file_hash, status?: "done",
//          thread: [{ by: "reader" | "agent", at: <RFC 3339> | null, text }] }
`

// message is one turn of an entry's thread. At is nil on an agent message that did not
// date itself: the server cannot know when the agent wrote.
type message struct {
	By   string  `json:"by"`
	At   *string `json:"at"`
	Text string  `json:"text"`
}

// annotation is one entry of the sidecar. Exact, Prefix and Suffix follow the W3C
// TextQuoteSelector; Lines is a hint into the markdown source, recomputed at every write.
type annotation struct {
	ID       string  `json:"id"`
	Exact    *string `json:"exact"`
	Prefix   *string `json:"prefix"`
	Suffix   *string `json:"suffix"`
	Lines    *[2]int `json:"lines"`
	FileHash string  `json:"file_hash"`
	// Written by the agent, never by the browser.
	Status string    `json:"status,omitempty"`
	Thread []message `json:"thread"`
	// Version 1 fields, read for conversion only; upgrade clears them so they are never
	// written back.
	Comment string `json:"comment,omitempty"`
	Created string `json:"created,omitempty"`
	Reply   string `json:"reply,omitempty"`
}

// upgrade turns a version 1 entry (comment / created / reply) into a thread; a version 2
// entry is returned as is, with a non-nil thread so it encodes as [].
func upgrade(a annotation) annotation {
	if a.Thread == nil {
		a.Thread = []message{}
		if a.Comment != "" || a.Reply != "" || a.Created != "" {
			var at *string
			if created := a.Created; created != "" {
				at = &created
			}
			a.Thread = append(a.Thread, message{By: byReader, At: at, Text: a.Comment})
			if a.Reply != "" {
				a.Thread = append(a.Thread, message{By: byAgent, Text: a.Reply})
			}
		}
	}
	a.Comment, a.Created, a.Reply = "", "", ""
	return a
}

type sidecar struct {
	Version int    `json:"version"`
	File    string `json:"file"`
	// file_hash -> path of the copy, relative to the sidecar's directory. Rebuilt from
	// disk at every save; what the browser or the agent put there is ignored.
	Revisions   map[string]string `json:"revisions,omitempty"`
	Annotations []annotation      `json:"annotations"`
}

// annotationsResponse adds the hash of the markdown the browser is looking at, so it can
// tell a comment written against an older version.
type annotationsResponse struct {
	sidecar
	Hash string `json:"hash"`
}

func fileHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newAnnotationID() string {
	return strings.ToLower(rand.Text()[:8])
}

// sidecarPath maps a markdown URL path to its sidecar on disk, confined to dir the same
// way http.Dir confines its opens.
func sidecarPath(dir http.Dir, target string) string {
	clean := path.Clean("/" + target)
	name := strings.TrimSuffix(clean, path.Ext(clean)) + sidecarSuffix
	return filepath.Join(string(dir), filepath.FromSlash(name))
}

// isMarkdownTarget accepts only a markdown path of this server: no other host, no other
// file type, so neither a redirect nor a sidecar can ever point elsewhere.
func isMarkdownTarget(target string, regex *regexp.Regexp) bool {
	return strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && regex.MatchString(target)
}

// isRevisionTarget tells a kept copy of an annotated version from a document: a copy is
// read-only, it cannot carry annotations of its own.
func isRevisionTarget(target string) bool {
	return strings.Contains(target, revisionsSuffix+"/")
}

// revisionsDir is the directory holding the copies of target's annotated versions.
func revisionsDir(dir http.Dir, target string) string {
	return strings.TrimSuffix(sidecarPath(dir, target), sidecarSuffix) + revisionsSuffix
}

// revisionName is the file name of the copy for hash: twelve hex characters are plenty
// for the handful of versions one file carries.
func revisionName(hash string) string {
	hex := strings.TrimPrefix(hash, "sha256:")
	if len(hex) < 12 || !revisionFile.MatchString(hex[:12]+".md") {
		return ""
	}
	return hex[:12] + ".md"
}

// The only shape a copy's name takes; anything else in the directory is not ours.
var revisionFile = regexp.MustCompile(`^[0-9a-f]{12}\.md$`)

// syncRevisions makes the copies on disk match the entries: the current content is kept
// when an entry refers to it and no copy exists yet, a copy no entry refers to is removed,
// the directory goes when empty. Returns the map for the sidecar. Runs under the sidecar
// lock. A hash with no copy and not the current content stays without one: the server
// has no other version in hand.
func syncRevisions(revDir string, entries []annotation, source []byte, hash string) (map[string]string, error) {
	wanted := make(map[string]string, len(entries))
	for _, a := range entries {
		if name := revisionName(a.FileHash); name != "" {
			wanted[name] = a.FileHash
		}
	}
	kept := map[string]string{}
	rel := filepath.Base(revDir)

	if names, err := os.ReadDir(revDir); err == nil {
		for _, e := range names {
			if !revisionFile.MatchString(e.Name()) {
				continue
			}
			if h, ok := wanted[e.Name()]; ok {
				kept[h] = rel + "/" + e.Name()
				continue
			}
			if err := os.Remove(filepath.Join(revDir, e.Name())); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if name := revisionName(hash); name != "" && wanted[name] == hash && kept[hash] == "" {
		if err := os.MkdirAll(revDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(revDir, name), source, 0o644); err != nil { //nolint:gosec // a copy of a served file
			return nil, err
		}
		kept[hash] = rel + "/" + name
	}

	if len(kept) == 0 {
		// Only ever ours to remove when it holds nothing else: Remove refuses a non-empty dir.
		if err := os.Remove(revDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Println("revisions:", err)
		}
		return nil, nil
	}
	return kept, nil
}

// sameOrigin refuses a page from another origin: the server listens on every interface.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// emptySidecar has a non-nil list, so it encodes as [] and not null. The name is folded to
// one line: it is quoted inside the header comment, where a newline would end the comment.
func emptySidecar(target string) sidecar {
	name := strings.Join(strings.Fields(path.Base(target)), " ")
	return sidecar{Version: sidecarVersion, File: name, Annotations: []annotation{}}
}

func decodeSidecar(f *os.File, target string) (sidecar, error) {
	sc := emptySidecar(target)
	data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes))
	if err != nil {
		return sc, err
	}
	// A freshly created file is empty: that is the "no annotations yet" sidecar.
	if len(strings.TrimSpace(string(data))) == 0 {
		return sc, nil
	}
	// JSON5, not JSON: an agent round-tripping through a JSON5 library writes bare keys
	// and single quotes, and the header above the document is a comment.
	if err := json5.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("decode %s: %w", f.Name(), err)
	}
	sc.Version, sc.File = sidecarVersion, path.Base(target)
	if sc.Annotations == nil {
		sc.Annotations = []annotation{}
	}
	for i, a := range sc.Annotations {
		sc.Annotations[i] = upgrade(a)
	}
	return sc, nil
}

// readSidecar returns the annotations of target, an empty list when it has none.
func readSidecar(dir http.Dir, target string) (sidecar, error) {
	p := sidecarPath(dir, target)
	f, err := os.Open(p) //nolint:gosec // p is confined to dir by sidecarPath
	if errors.Is(err, os.ErrNotExist) {
		return emptySidecar(target), nil
	}
	if err != nil {
		return sidecar{}, err
	}
	//nolint:errcheck
	defer f.Close()
	if err := lockFile(f, false); err != nil {
		return sidecar{}, fmt.Errorf("lock %s: %w", p, err)
	}
	//nolint:errcheck
	defer unlockFile(f)
	return decodeSidecar(f, target)
}

// writeSidecar replaces the annotations of target with incoming, merged with what is on
// disk under an exclusive lock. The write is in place, not temp + rename: a rename would
// swap the inode under an agent waiting on the lock.
func writeSidecar(dir http.Dir, target string, source []byte, incoming []annotation) (sidecar, error) {
	p := sidecarPath(dir, target)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // idem
	if err != nil {
		return sidecar{}, err
	}
	//nolint:errcheck
	defer f.Close()
	if err := lockFile(f, true); err != nil {
		return sidecar{}, fmt.Errorf("lock %s: %w", p, err)
	}
	//nolint:errcheck
	defer unlockFile(f)

	stored, err := decodeSidecar(f, target)
	if err != nil {
		return sidecar{}, err
	}
	stored.Annotations = mergeAnnotations(stored.Annotations, incoming, source)
	stored.Revisions, err = syncRevisions(revisionsDir(dir, target), stored.Annotations, source, fileHash(source))
	if err != nil {
		return sidecar{}, err
	}

	if len(stored.Annotations) == 0 {
		// Still under the lock, so nobody reads a sidecar that is about to vanish.
		if err := os.Remove(p); err != nil {
			return sidecar{}, err
		}
		return stored, nil
	}

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return sidecar{}, err
	}
	if err := f.Truncate(0); err != nil {
		return sidecar{}, err
	}
	header := fmt.Sprintf(sidecarHeader, stored.File)
	if _, err := f.WriteAt(append([]byte(header), append(data, '\n')...), 0); err != nil {
		return sidecar{}, err
	}
	return stored, f.Sync()
}

// mergeAnnotations applies the browser's view of the list to what is on disk. The browser
// owns the reader messages and the list membership; the agent owns everything else on an
// existing entry (status, its messages, and the quote it re-anchors), so those always come
// from disk.
func mergeAnnotations(stored, incoming []annotation, source []byte) []annotation {
	known := make(map[string]annotation, len(stored))
	for _, a := range stored {
		known[a.ID] = a
	}

	now := time.Now().Format(time.RFC3339)
	hash := fileHash(source)
	out := make([]annotation, 0, len(incoming))
	seen := make(map[string]bool, len(incoming))
	for _, in := range incoming {
		in = upgrade(in)
		var a annotation
		switch old, ok := known[in.ID]; {
		case in.ID != "" && seen[in.ID]:
			continue
		case in.ID == "":
			a = in
			a.ID, a.FileHash, a.Status = newAnnotationID(), hash, ""
			// An entry with nothing said is not a request.
			if a.Thread, _ = mergeThread(nil, in.Thread, now); len(a.Thread) == 0 {
				continue
			}
		case !ok:
			// Gone from disk since the page loaded: the agent handled it, do not bring it back.
			continue
		default:
			a = old
			var reopened bool
			if a.Thread, reopened = mergeThread(old.Thread, in.Thread, now); reopened {
				a.Status = ""
			}
		}
		a.Lines = sourceLines(source, a.Exact)
		seen[a.ID] = true
		out = append(out, a)
	}
	return out
}

// mergeThread applies the browser's reader messages to the stored thread. Reader messages
// are matched by rank: the browser may have loaded the page before the agent answered, so
// positions in the two lists do not line up, but the reader's own sequence only grows.
// Edits keep their date; new messages are dated now and appended, which reopens the entry.
func mergeThread(stored, incoming []message, now string) ([]message, bool) {
	var readers []message
	for _, m := range incoming {
		if m.By == byReader {
			readers = append(readers, m)
		}
	}
	out := make([]message, 0, len(stored)+len(readers))
	rank := 0
	for _, m := range stored {
		if m.By == byReader && rank < len(readers) {
			m.Text = readers[rank].Text
			rank++
		}
		out = append(out, m)
	}
	for _, m := range readers[rank:] {
		at := now
		out = append(out, message{By: byReader, At: &at, Text: m.Text})
	}
	return out, rank < len(readers)
}

// sourceLines locates exact in the markdown source and returns its 1-based line span.
// Rendered text has lost the emphasis markers and the line wrapping, so both sides are
// compared with those folded away. Best effort: nil when not found.
func sourceLines(source []byte, exact *string) *[2]int {
	if exact == nil || strings.TrimSpace(*exact) == "" {
		return nil
	}
	src, offsets := foldMarkdown(string(source))
	needle, _ := foldMarkdown(*exact)
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return nil
	}
	// ponytail: first occurrence; disambiguate with prefix/suffix if it ever matters.
	idx := strings.Index(src, needle)
	if idx < 0 {
		return nil
	}
	start, end := offsets[idx], offsets[idx+len(needle)-1]
	lines := [2]int{
		1 + strings.Count(string(source[:start]), "\n"),
		1 + strings.Count(string(source[:end]), "\n"),
	}
	return &lines
}

// foldMarkdown drops emphasis markers and collapses whitespace runs to one space; the
// returned offsets map every byte of the folded string back to the original.
func foldMarkdown(s string) (string, []int) {
	var b strings.Builder
	offsets := make([]int, 0, len(s))
	space := false
	for i, r := range s {
		switch {
		case strings.ContainsRune("*_`~", r):
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			offsets = append(offsets, i)
		}
		space = false
		n := b.Len()
		b.WriteRune(r)
		for ; n < b.Len(); n++ {
			offsets = append(offsets, i)
		}
	}
	return b.String(), offsets
}

// handleAnnotations serves the sidecar of a markdown file: GET returns it (empty when
// absent) with the current file hash, PUT replaces its list.
func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request, dir http.Dir, regex *regexp.Regexp) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	target := r.URL.Query().Get("path")
	if !isMarkdownTarget(target, regex) || (r.Method == http.MethodPut && isRevisionTarget(target)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	content, err := readToString(dir, target)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	var sc sidecar
	switch r.Method {
	case http.MethodGet:
		sc, err = readSidecar(dir, target)
	case http.MethodPut:
		var body struct {
			Annotations []annotation `json:"annotations"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSidecarBytes)).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		sc, err = writeSidecar(dir, target, content, body.Annotations)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		log.Println(err)
		http.Error(w, "sidecar unreadable", http.StatusInternalServerError)
		return
	}

	setNoCacheHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(annotationsResponse{sidecar: sc, Hash: fileHash(content)}); err != nil {
		log.Println(err)
	}
}
