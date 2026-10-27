package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
)

// Three enums and a bool: anything bigger is not the settings menu talking.
const maxSettingsBytes = 4 << 10

var (
	settingsWidths = []string{"normal", "wide", "full"}
	settingsThemes = []string{"system", "light", "dark"}
)

// settings is shared by every instance, whatever its port. Decoding into it drops unknown keys.
type settings struct {
	Width      string `json:"width"`
	Theme      string `json:"theme"`
	HoldReload bool   `json:"hold_reload"`
}

func (st settings) valid() bool {
	return slices.Contains(settingsWidths, st.Width) && slices.Contains(settingsThemes, st.Theme)
}

// Media is the media attribute of the light or dark stylesheet, so the first paint is
// already in the stored theme.
func (st settings) Media(mode string) string {
	switch st.Theme {
	case "system":
		return "(prefers-color-scheme: " + mode + ")"
	case mode:
		return "all"
	}
	return "not all"
}

// DefaultSettingsPath is settings.json under the user's config directory, "" when there is
// none: settings are then not persisted.
func DefaultSettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Printf("settings not persisted: %v", err)
		return ""
	}
	return filepath.Join(dir, "grip-live-diff", "settings.json")
}

// loadSettings falls back to the default for each key missing or invalid, so one bad value
// does not reset the others.
func loadSettings(path string) settings {
	var st settings
	if path != "" {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // path is ours, not the client's
			// A type error still fills the other fields; the checks below cover the rest.
			_ = json.Unmarshal(data, &st)
		}
	}
	if !slices.Contains(settingsWidths, st.Width) {
		st.Width = settingsWidths[0]
	}
	if !slices.Contains(settingsThemes, st.Theme) {
		st.Theme = settingsThemes[0]
	}
	return st
}

// saveSettings replaces the file whole, never in place.
func saveSettings(path string, st settings) error {
	if path == "" {
		return fmt.Errorf("save settings: no user config directory")
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a config dir, readable like any other
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := replaceFile(path, ".settings-*.json", 0o644, append(data, '\n')); err != nil {
		return fmt.Errorf("save settings to %s: %w", path, err)
	}
	return nil
}

// replaceFile writes data through a temp file named after pattern in path's directory, so a
// reader never sees a half-written file. CreateTemp makes it 0600, hence the chmod to mode.
func replaceFile(path, pattern string, mode os.FileMode, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// handleSettings saves the whole set on PUT and answers with what was saved.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	var st settings
	if err := decodeJSONBody(w, r, maxSettingsBytes, &st); err != nil || !st.valid() {
		http.Error(w, "invalid settings", http.StatusBadRequest)
		return
	}
	if err := saveSettings(s.settingsPath, st); err != nil {
		log.Println(err)
		http.Error(w, "settings not saved", http.StatusInternalServerError)
		return
	}

	setNoCacheHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		log.Println(err)
	}
}
