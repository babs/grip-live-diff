package internal

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Pending is one run of `grip-live-diff pending`: the open entries of Paths whose owner is the
// instance Port / PID, one "<sidecar>\t<entry id>" line each on Out, and in manual mode the
// ready file it may claim. Claim, the PID of the agent, takes that ready file.
type Pending struct {
	Port     int
	PID      int64
	Claim    int64
	Registry string
	Paths    []string
	Out, Err io.Writer
}

// ErrInstanceGone is returned by Run when the PID of the instance is dead.
var ErrInstanceGone = errors.New("instance gone")

// Run prints the lines and tells whether it printed one. A sidecar it cannot read is reported on
// Err and skipped.
func (p Pending) Run() (bool, error) {
	if !pidAlive(p.PID) {
		// Its file, if any, goes with the other dead ones.
		scanRegistry(p.Registry)
		return false, fmt.Errorf("%w: :%d (pid %d), nothing is its own", ErrInstanceGone, p.Port, p.PID)
	}
	sidecars, err := findSidecars(p.Paths, p.Err)
	if err != nil {
		return false, err
	}
	if p.Claim != 0 && len(sidecars) != 1 {
		return false, fmt.Errorf("--claim takes one sidecar, got %d", len(sidecars))
	}
	owner := p.owns
	if p.Claim == 0 {
		owns := p.owns()
		owner = func() func(string, int) bool { return owns }
	}
	found := false
	for _, sc := range sidecars {
		lines, err := pendingLines(sc, owner, p.Claim)
		if err != nil {
			_, _ = fmt.Fprintln(p.Err, err)
			continue
		}
		for _, l := range lines {
			if _, err := fmt.Fprintf(p.Out, "%s\t%s\n", sc, l); err != nil {
				return found, err
			}
			found = true
		}
	}
	return found, nil
}

// owns applies the owner rule for the instance, once the registry is purged. With no file for
// it, the instance runs with routing off and owns everything; a file not seen for registryLive
// is a stopped instance and owns nothing.
func (p Pending) owns() func(key string, via int) bool {
	// A claim reads it under the flock, after a wait the instance may not have survived.
	if !pidAlive(p.PID) {
		return func(string, int) bool { return false }
	}
	all := scanRegistry(p.Registry)
	now := time.Now()
	i := slices.IndexFunc(all, func(in instance) bool { return in.PID == p.PID })
	v := peers{} // routing off
	if i < 0 {
		// Also what another cache dir than the instance's gives: say so, two agents would answer.
		_, _ = fmt.Fprintf(p.Err, "no registry file for pid %d in %s: routing off, every open entry is its own\n", p.PID, p.Registry)
	} else {
		switch in := all[i]; {
		case in.Port != p.Port:
			_, _ = fmt.Fprintf(p.Err, "pid %d serves :%d, not :%d: nothing is its own\n", p.PID, in.Port, p.Port)
			return func(string, int) bool { return false }
		case !fresh(in, now, registryLive):
			_, _ = fmt.Fprintf(p.Err, "instance :%d (pid %d) not seen since %s: nothing is its own\n", p.Port, p.PID, in.SeenAt)
			return func(string, int) bool { return false }
		}
		v = peers{self: p.Port, live: liveOf(all, now, registryLive)}
	}
	return func(key string, via int) bool { return v.owns(key, via, p.Port) }
}

// findSidecars maps each path to sidecars: a directory to the ones under it, a markdown or
// ready file to the sidecar beside it. A directory that cannot be read is reported on errOut and
// skipped.
func findSidecars(paths []string, errOut io.Writer) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			// An auto sidecar goes with its last entry, a ready file with its batch: no entry, as
			// long as their directory stands.
			named := strings.HasSuffix(p, sidecarSuffix) || strings.HasSuffix(p, readySuffix)
			if dir, derr := os.Stat(filepath.Dir(p)); !named || derr != nil || !dir.IsDir() {
				return nil, fmt.Errorf("%s: no such file or directory", p)
			}
		}
		if err == nil && info.IsDir() {
			// The trailing separator makes WalkDir follow a symlinked directory: it Lstats its
			// root.
			err := filepath.WalkDir(p+string(filepath.Separator), func(q string, d fs.DirEntry, err error) error {
				if err != nil {
					// An unreadable entry must not hide the sidecars of the others.
					_, _ = fmt.Fprintln(errOut, err)
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if !d.IsDir() && strings.HasSuffix(q, sidecarSuffix) {
					out = append(out, q)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		switch {
		case strings.HasSuffix(p, sidecarSuffix):
			out = append(out, p)
		case strings.HasSuffix(p, readySuffix):
			out = append(out, sidecarOfReady(p))
		default:
			out = append(out, sidecarOf(p))
		}
	}
	return out, nil
}

// pendingLines is what the instance owns in the sidecar at p: its open entries in auto; in
// manual, "ready" and those entries when the ready file is unclaimed and its own, nothing
// otherwise. With claim, it writes claim as the PID of the ready file first, under the lock.
// owner gives the owner rule, called once the lock is held.
func pendingLines(p string, owner func() func(key string, via int) bool, claim int64) ([]string, error) {
	flag, exclusive := os.O_RDONLY, false
	if claim != 0 {
		flag, exclusive = os.O_RDWR, true
	}
	f, err := lockSidecar(p, flag, exclusive)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer releaseSidecar(f)
	owns := owner()
	data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes))
	if err != nil {
		return nil, err
	}
	// The key is the one of the page: the file name as served, which the name of the sidecar
	// has lost the extension of.
	name := strings.TrimSuffix(filepath.Base(p), sidecarSuffix) + ".md"
	sc, stored, err := parseSidecar(data, "/"+name, p)
	if err != nil {
		return nil, err
	}
	if stored != "" {
		name = filepath.Base(stored)
	}
	key := pageKey(http.Dir(filepath.Dir(p)), name)

	var lines []string
	for _, a := range sc.Annotations {
		if isOpen(a) && owns(key, lastVia(a)) {
			lines = append(lines, a.ID)
		}
	}
	if sc.Trigger != triggerManual {
		if claim != 0 {
			return nil, nil
		}
		return lines, nil
	}
	rp := readyOf(p)
	if regular, _ := isRegular(rp); !regular {
		return nil, nil
	}
	rf, err := loadReady(rp)
	if err != nil || rf.PID != nil || !owns(key, rf.Via) {
		return nil, nil //nolint:nilerr // a ready file that does not parse is no batch to take
	}
	if claim != 0 {
		now := time.Now().Format(time.RFC3339)
		rf.PID, rf.ClaimedAt = &claim, &now
		if err := writeReady(rp, rf); err != nil {
			return nil, err
		}
	}
	return append([]string{"ready"}, lines...), nil
}
