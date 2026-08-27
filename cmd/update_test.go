package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

func TestChecksumFor(t *testing.T) {
	// Both shapes sha256sum emits, plus the sibling entries it is picked out of.
	listing := []byte(
		"aaa1  grip-live-diff-linux-arm64.xz\n" +
			"bbb2 *grip-live-diff-linux-amd64.xz\n" +
			"ccc3  grip-live-diff-windows-amd64.exe.xz\n" +
			"\n" +
			"garbage-without-a-name\n")

	for _, c := range []struct {
		asset, want string
		ok          bool
	}{
		{"grip-live-diff-linux-amd64.xz", "bbb2", true},
		{"grip-live-diff-linux-arm64.xz", "aaa1", true},
		{"grip-live-diff-windows-amd64.exe.xz", "ccc3", true},
		{"grip-live-diff-darwin-arm64.xz", "", false},
		// A prefix of a listed name must not be mistaken for it.
		{"grip-live-diff-linux-amd64", "", false},
	} {
		got, ok := checksumFor(listing, c.asset)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.asset, got, ok, c.want, c.ok)
		}
	}
}

// xzBytes compresses payload the way release.sh does.
func xzBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadVerified(t *testing.T) {
	payload := []byte("\x7fELF pretend this is the new binary")
	archive := xzBytes(t, payload)
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/asset.xz"):
			//nolint:errcheck
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/corrupt.xz"):
			bad := append([]byte(nil), archive...)
			bad[len(bad)/2] ^= 0xff
			//nolint:errcheck
			w.Write(bad)
		case strings.HasSuffix(r.URL.Path, "/notxz.xz"):
			//nolint:errcheck
			w.Write([]byte("not compressed at all"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := downloadHost
	downloadHost = srv.URL
	defer func() { downloadHost = old }()

	t.Run("matching checksum yields the payload", func(t *testing.T) {
		got, err := downloadVerified("v1.0.0", "asset.xz", good)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("payload mismatch: %q", got)
		}
	})

	// Every way the bytes can be wrong must stop before anything is applied.
	for _, c := range []struct{ name, asset, want, contains string }{
		{"corrupted asset", "corrupt.xz", good, "checksum mismatch"},
		{"asset swapped for another", "asset.xz", strings.Repeat("0", 64), "checksum mismatch"},
		{"missing asset", "gone.xz", good, "download release"},
		{"not an xz stream", "notxz.xz", func() string {
			s := sha256.Sum256([]byte("not compressed at all"))
			return hex.EncodeToString(s[:])
		}(), "xz"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := downloadVerified("v1.0.0", c.asset, c.want)
			if err == nil {
				t.Fatalf("accepted %d bytes instead of failing", len(got))
			}
			if got != nil {
				t.Error("returned bytes alongside the error")
			}
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("error %q does not mention %q", err, c.contains)
			}
		})
	}
}
