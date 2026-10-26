package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// settingsServer serves a one-page tree with its settings file at path.
func settingsServer(t *testing.T, path string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer("localhost", 0, false, false, false, NewParser(), path).newHandler(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv
}

func putSettings(t *testing.T, srv *httptest.Server, origin, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/__settings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func getPage(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/doc.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /doc.md: %d %v", resp.StatusCode, err)
	}
	return string(body)
}

// 010-AC14: no settings file, or one with missing / invalid keys, renders the defaults.
func TestSettingsDefaults(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" = no file
		want    []string
	}{
		{"no file", "", []string{`data-width="normal"`, `data-theme-setting="system"`, `data-hold-reload="false"`, `media="(prefers-color-scheme: dark)"`}},
		{"garbage", "not json", []string{`data-width="normal"`, `data-theme-setting="system"`}},
		{"invalid value keeps the others", `{"width":"huge","theme":"dark","hold_reload":true}`, []string{`data-width="normal"`, `data-theme-setting="dark"`, `data-hold-reload="true"`, `<body class="markdown-body" data-theme="dark">`}},
		{"wrong type keeps the others", `{"width":5,"theme":"light"}`, []string{`data-width="normal"`, `data-theme-setting="light"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			page := getPage(t, settingsServer(t, path))
			for _, w := range tt.want {
				if !strings.Contains(page, w) {
					t.Errorf("page lacks %s", w)
				}
			}
		})
	}
}

// 010-AC15: a width saved through one instance renders on another port at first paint.
func TestSettingsSharedAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grip-live-diff", "settings.json")
	a, b := settingsServer(t, path), settingsServer(t, path)
	if code := putSettings(t, a, "", `{"width":"wide","theme":"dark","hold_reload":true}`); code != http.StatusOK {
		t.Fatalf("PUT on A: %d", code)
	}
	page := getPage(t, b)
	for _, w := range []string{`data-width="wide"`, `data-theme-setting="dark"`, `data-hold-reload="true"`, `id="highlight-dark" media="all"`, `id="highlight-light" media="not all"`} {
		if !strings.Contains(page, w) {
			t.Errorf("page from B lacks %s", w)
		}
	}
}

// 010-AC16: foreign origin, bad enum, oversize body are refused and leave the file alone;
// unknown keys are dropped.
func TestSettingsPut(t *testing.T) {
	const stored = `{"width":"full","theme":"light","hold_reload":false}`
	tests := []struct {
		name     string
		method   string
		origin   string
		body     string
		wantCode int
		want     string // file content after, "" = unchanged
	}{
		{"saved", http.MethodPut, "", `{"width":"wide","theme":"dark","hold_reload":true}`, http.StatusOK, `"width": "wide"`},
		{"foreign origin", http.MethodPut, "http://evil.example", `{"width":"wide","theme":"dark"}`, http.StatusForbidden, ""},
		{"bad width", http.MethodPut, "", `{"width":"huge","theme":"dark"}`, http.StatusBadRequest, ""},
		{"bad theme", http.MethodPut, "", `{"width":"wide","theme":"blue"}`, http.StatusBadRequest, ""},
		{"missing key", http.MethodPut, "", `{"theme":"dark"}`, http.StatusBadRequest, ""},
		{"over the cap", http.MethodPut, "", `{"width":"wide","theme":"dark","x":"` + strings.Repeat("a", maxSettingsBytes) + `"}`, http.StatusBadRequest, ""},
		{"padding past the cap after a valid object", http.MethodPut, "", `{"width":"wide","theme":"dark"}` + strings.Repeat(" ", 3*maxSettingsBytes), http.StatusBadRequest, ""},
		{"trailing garbage", http.MethodPut, "", `{"width":"wide","theme":"dark"}garbage`, http.StatusBadRequest, ""},
		{"two objects", http.MethodPut, "", `{"width":"wide","theme":"dark"}{"width":"normal","theme":"light"}`, http.StatusBadRequest, ""},
		{"trailing whitespace", http.MethodPut, "", "{\"width\":\"wide\",\"theme\":\"dark\"}\n \t\r\n", http.StatusOK, `"width": "wide"`},
		{"unknown key dropped", http.MethodPut, "", `{"width":"wide","theme":"dark","evil":"x"}`, http.StatusOK, `"width": "wide"`},
		{"wrong method", http.MethodPost, "", `{"width":"wide","theme":"dark"}`, http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(path, []byte(stored), 0o644); err != nil {
				t.Fatal(err)
			}
			srv := settingsServer(t, path)
			var code int
			if tt.method == http.MethodPut {
				code = putSettings(t, srv, tt.origin, tt.body)
			} else {
				resp, err := srv.Client().Post(srv.URL+"/__settings", "application/json", strings.NewReader(tt.body)) //nolint:noctx // test
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				code = resp.StatusCode
			}
			if code != tt.wantCode {
				t.Fatalf("status %d, want %d", code, tt.wantCode)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == "" && string(got) != stored:
				t.Errorf("file changed: %s", got)
			case tt.want != "" && !strings.Contains(string(got), tt.want):
				t.Errorf("file %s lacks %s", got, tt.want)
			}
			if strings.Contains(string(got), "evil") {
				t.Errorf("unknown key written: %s", got)
			}
			// Atomic write: the temp file is renamed, never left behind.
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("dir holds %d entries, want only settings.json", len(entries))
			}
		})
	}
}

// 010-AC17: a read-only config dir fails the save, not the server.
func TestSettingsReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced on a non-root user")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // let TempDir clean up
	srv := settingsServer(t, filepath.Join(dir, "settings.json"))
	if code := putSettings(t, srv, "", `{"width":"wide","theme":"system"}`); code < 500 {
		t.Fatalf("PUT into read-only dir: %d, want 5xx", code)
	}
	if page := getPage(t, srv); !strings.Contains(page, `data-width="normal"`) {
		t.Error("page after failed save lacks the default width")
	}
}

// 010-AC18: width and theme live in the gear menu, not on the toolbar.
func TestSettingsMenu(t *testing.T) {
	page := getPage(t, settingsServer(t, filepath.Join(t.TempDir(), "settings.json")))
	for _, gone := range []string{`id="width-toggle"`, `id="theme-toggle"`} {
		if strings.Contains(page, gone) {
			t.Errorf("page still has %s", gone)
		}
	}
	want := []string{`popovertarget="settings-menu"`, `id="settings-menu"`, `aria-label="Settings"`}
	for _, v := range settingsWidths {
		want = append(want, `name="settings-width" value="`+v+`"`)
	}
	for _, v := range settingsThemes {
		want = append(want, `name="settings-theme" value="`+v+`"`)
	}
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("page lacks %s", w)
		}
	}
}

func TestDefaultSettingsPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got, want := DefaultSettingsPath(), filepath.Join(dir, "grip-live-diff", "settings.json"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if runtime.GOOS != "linux" {
		return
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got := DefaultSettingsPath(); got != "" {
		t.Errorf("no config dir: got %q, want empty", got)
	}
}

func TestSaveSettingsErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the settings path
	}{
		{"no path", func(*testing.T, string) string { return "" }},
		{"target is a directory", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "settings.json")
			if err := os.MkdirAll(filepath.Join(p, "child"), 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"parent is a file", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "file")
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(p, "grip-live-diff", "settings.json")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := saveSettings(tt.setup(t, dir), loadSettings("")); err == nil {
				t.Fatal("want an error")
			}
			if left, _ := filepath.Glob(filepath.Join(dir, ".settings-*.json")); len(left) != 0 {
				t.Errorf("temp files left: %v", left)
			}
		})
	}
}
