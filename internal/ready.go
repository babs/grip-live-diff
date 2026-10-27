package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/titanous/json5"
)

const (
	readySuffix = ".annotations.ready"
	// Two fields and the agent's claimed_at; anything bigger is not ours or the agent's.
	maxReadyBytes = 4 << 10

	triggerAuto   = "auto"
	triggerManual = "manual"

	readyNone    = "none"
	readySent    = "sent"
	readyWorking = "working"
	readyGone    = "gone"
)

var (
	errNotManual   = errors.New("trigger is not manual")
	errNothingOpen = errors.New("no open entry")
)

// readyFile is what the preview writes when the reader sends a batch; the agent sets PID
// (and claimed_at, not read here) when it takes it.
type readyFile struct {
	SentAt *string `json:"sent_at"`
	// int64, not int: a 32-bit build would refuse a large PID and read the file as sent.
	PID *int64 `json:"pid"`
}

// readyState is the ready file as the page shows it.
type readyState struct {
	State  string  `json:"state"`
	PID    *int64  `json:"pid"`
	SentAt *string `json:"sent_at"`
}

// readyPath is the ready file beside target's sidecar, confined like it.
func readyPath(dir http.Dir, target string) string {
	return strings.TrimSuffix(sidecarPath(dir, target), sidecarSuffix) + readySuffix
}

// readReady reports the state of the ready file at p. A file that does not parse reads as
// sent: the agent writes it too, a bad write of its own must not break the page.
func readReady(p string) readyState {
	switch regular, err := isRegular(p); {
	case errors.Is(err, os.ErrNotExist):
		return readyState{State: readyNone}
	case err != nil, !regular:
		return readyState{State: readySent}
	}
	var rf readyFile
	f, err := os.Open(p) //nolint:gosec // p is confined to dir by readyPath
	if err == nil {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(f, maxReadyBytes))
		_ = f.Close()
		if err == nil {
			err = json5.Unmarshal(data, &rf)
		}
	}
	if err != nil {
		return readyState{State: readySent}
	}
	st := readyState{PID: rf.PID, SentAt: rf.SentAt}
	switch {
	case rf.PID == nil:
		st.State = readySent
	// Out of range, Kill would truncate the PID to another, live, process.
	case *rf.PID > 0 && *rf.PID <= math.MaxInt32 && processAlive(int(*rf.PID)):
		st.State = readyWorking
	default:
		st.State = readyGone
	}
	return st
}

// isRegular is checked before any open: opening a FIFO blocks until a writer comes, and a
// send would hang holding the sidecar lock. ponytail: Lstat then Open, a swap in between wins.
func isRegular(p string) (bool, error) {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular(), err
}

// isOpen tells a request from a handled entry: the reader spoke last and the agent did not
// close it.
func isOpen(a annotation) bool {
	return a.Status != "done" && len(a.Thread) > 0 && a.Thread[len(a.Thread)-1].By == byReader
}

// sendReady writes the ready file of target, under the sidecar's lock, the one the agent
// claims under. A batch already sent or worked on is left as is.
func sendReady(dir http.Dir, target string) (readyState, error) {
	// O_RDWR: on NFS, flock is emulated by fcntl, whose exclusive lock needs a writable fd.
	f, err := lockSidecar(sidecarPath(dir, target), os.O_RDWR, true)
	if errors.Is(err, os.ErrNotExist) {
		return readyState{}, errNotManual
	}
	if err != nil {
		return readyState{}, err
	}
	defer releaseSidecar(f)

	sc, err := decodeSidecar(f, target)
	if err != nil {
		return readyState{}, err
	}
	if sc.Trigger != triggerManual {
		return readyState{}, errNotManual
	}
	open := false
	for _, a := range sc.Annotations {
		open = open || isOpen(a)
	}
	if !open {
		return readyState{}, errNothingOpen
	}

	rp := readyPath(dir, target)
	// Not a regular file is not a batch of ours: replaced, the rename does not follow it.
	if regular, _ := isRegular(rp); regular {
		if st := readReady(rp); st.State == readySent || st.State == readyWorking {
			return st, nil
		}
	}
	now := time.Now().Format(time.RFC3339)
	data, err := json.Marshal(readyFile{SentAt: &now})
	if err != nil {
		return readyState{}, err
	}
	if err := replaceFile(rp, "."+filepath.Base(rp)+"-*", 0o644, append(data, '\n')); err != nil {
		return readyState{}, fmt.Errorf("write %s: %w", rp, err)
	}
	return readyState{State: readySent, SentAt: &now}, nil
}

// cancelReady removes the ready file under the sidecar's lock: unlocked, an agent claiming
// at that moment would write the cancelled batch back.
func cancelReady(dir http.Dir, target string) error {
	f, err := lockSidecar(sidecarPath(dir, target), os.O_RDWR, true)
	switch {
	case err == nil:
		defer releaseSidecar(f)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return removeReady(readyPath(dir, target))
}

// removeReady deletes the ready file; already gone is fine.
func removeReady(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// handleReady sends (POST) or cancels (DELETE) the batch of a markdown file in manual mode.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request, dir http.Dir, regex *regexp.Regexp) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	target := r.URL.Query().Get("path")
	if !isMarkdownTarget(target, regex) || isRevisionTarget(target) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if _, err := readToString(dir, target); err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	var st readyState
	var err error
	switch r.Method {
	case http.MethodPost:
		st, err = sendReady(dir, target)
	case http.MethodDelete:
		st = readyState{State: readyNone}
		err = cancelReady(dir, target)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case errors.Is(err, errNotManual), errors.Is(err, errNothingOpen):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		log.Println(err)
		http.Error(w, "ready file not written", http.StatusInternalServerError)
		return
	}

	setNoCacheHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		log.Println(err)
	}
}
