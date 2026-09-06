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
	sidecarSuffix  = ".annotations.json5"
	sidecarVersion = 1
	// A sidecar is a few dozen comments; anything bigger is not a browser talking.
	maxSidecarBytes = 1 << 20
)

// sidecarHeader tells the agent, in the file itself, how to take part; rewritten on
// every save, so it survives a tool that drops comments.
const sidecarHeader = `// grip-live-diff annotations for %s. JSON5: comments and trailing commas are fine.
//
// An entry is a reader's request until it is answered or removed. To take part:
//   - take an exclusive flock on this file while you edit it; the preview does the same;
//   - once a request is handled, either set status: "done" with a short reply, or delete the entry;
//   - if you rewrite an annotated passage, put the new wording in exact so the mark follows it;
//   - exact / prefix / suffix quote the rendered text; lines is a hint into the markdown source,
//     recomputed on every save from the preview, so trust exact first; exact: null = whole document;
//   - id, created, file_hash and comment belong to the reader: never change them.
//
// Entry: { id, exact, prefix, suffix, lines: [start, end] | null, comment, created, updated,
//          file_hash, status?: "done", reply?: string }
`

// annotation is one entry of the sidecar. Exact, Prefix and Suffix follow the W3C
// TextQuoteSelector; Lines is a hint into the markdown source, recomputed at every write.
type annotation struct {
	ID       string  `json:"id"`
	Exact    *string `json:"exact"`
	Prefix   *string `json:"prefix"`
	Suffix   *string `json:"suffix"`
	Lines    *[2]int `json:"lines"`
	Comment  string  `json:"comment"`
	Created  string  `json:"created"`
	Updated  *string `json:"updated"`
	FileHash string  `json:"file_hash"`
	// Written by the agent, never by the browser.
	Status string `json:"status,omitempty"`
	Reply  string `json:"reply,omitempty"`
}

type sidecar struct {
	Version     int          `json:"version"`
	File        string       `json:"file"`
	Annotations []annotation `json:"annotations"`
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
// owns the comment and the list membership; the agent owns everything else on an existing
// entry (status, reply, and the quote it re-anchors), so those always come from disk.
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
		var a annotation
		switch old, ok := known[in.ID]; {
		case in.ID != "" && seen[in.ID]:
			continue
		case in.ID == "":
			a = in
			a.ID, a.Created, a.FileHash = newAnnotationID(), now, hash
			a.Updated, a.Status, a.Reply = nil, "", ""
		case !ok:
			// Gone from disk since the page loaded: the agent handled it, do not bring it back.
			continue
		default:
			a = old
			if in.Comment != old.Comment {
				a.Comment, a.Updated = in.Comment, &now
			}
		}
		a.Lines = sourceLines(source, a.Exact)
		seen[a.ID] = true
		out = append(out, a)
	}
	return out
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
	if !isMarkdownTarget(target, regex) {
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
