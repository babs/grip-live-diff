package cmd

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/minio/selfupdate"
	"github.com/spf13/cobra"
	"github.com/ulikunitz/xz"
	"golang.org/x/mod/semver"
)

const (
	githubRepo = "babs/grip-live-diff"
	binaryName = "grip-live-diff"

	// A release asset is one compressed binary: past these sizes it is not ours, and
	// reading it would only be a way to exhaust memory.
	maxMetadataBytes = 4 << 20
	maxAssetBytes    = 64 << 20
	maxBinaryBytes   = 256 << 20
)

// No overall deadline: pulling a release binary over a slow link is legitimate. This
// bounds the part that actually hangs — a server that accepts and never answers. The
// default transport is cloned rather than replaced, or the update would stop working
// behind an HTTP_PROXY.
var httpClient = &http.Client{Transport: proxyAwareTransport()}

// Hosts of the release, as variables so a test can serve them locally.
var (
	apiHost      = "https://api.github.com"
	downloadHost = "https://github.com"
)

func proxyAwareTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

var updateCmd = &cobra.Command{
	Use:          "update",
	Short:        "Self-update to the latest GitHub release",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return selfUpdate()
	},
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

func selfUpdate() error {
	latest, err := latestRelease()
	if err != nil {
		return err
	}

	fmt.Printf("current version: %v, latest release: %v\n", Version, latest)

	switch semver.Compare(latest, Version) {
	case -1:
		fmt.Println("you have a newer version, nothing to do")
		return nil
	case 0:
		fmt.Println("already latest version")
		return nil
	case 1:
		fmt.Println("new version detected, upgrading")
		if Version == devVersion {
			fmt.Println("development build detected, press enter to proceed")
			//nolint:errcheck
			bufio.NewReader(os.Stdin).ReadBytes('\n')
		}
	}

	ext := "xz"
	if runtime.GOOS == "windows" {
		ext = "exe.xz"
	}
	asset := fmt.Sprintf("%s-%s-%s.%s", binaryName, runtime.GOOS, runtime.GOARCH, ext)
	downloadLink := releaseAssetURL(latest, asset)

	options := selfupdate.Options{}
	if err := options.CheckPermissions(); err != nil {
		return fmt.Errorf("won't perform self update: %w\nmanual download: %s", err, downloadLink)
	}

	want, err := publishedChecksum(latest, asset)
	if err != nil {
		return err
	}

	fmt.Printf("downloading %v\n", downloadLink)
	binary, err := downloadVerified(latest, asset, want)
	if err != nil {
		return err
	}

	if err := selfupdate.Apply(bytes.NewReader(binary), options); err != nil {
		return fmt.Errorf("apply update: %w", err)
	}
	fmt.Println("upgrade complete")
	return nil
}

// downloadVerified fetches the asset, refuses it unless it hashes to want, and returns
// the decompressed binary. Nothing downstream may see bytes that failed this check.
func downloadVerified(tag, asset, want string) ([]byte, error) {
	archive, err := fetch(releaseAssetURL(tag, asset), maxAssetBytes)
	if err != nil {
		return nil, fmt.Errorf("download release: %w", err)
	}

	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s: got %x, release publishes %s", asset, got, want)
	}

	r, err := xz.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open xz stream: %w", err)
	}
	// Decompress in full before returning: a bomb must fail here, never as a binary
	// truncated onto the running executable.
	binary, err := readAtMost(r, maxBinaryBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress release: %w", err)
	}
	return binary, nil
}

func latestRelease() (string, error) {
	body, err := fetch(apiHost+"/repos/"+githubRepo+"/releases/latest", maxMetadataBytes)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("parse release payload: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("no release found for %s", githubRepo)
	}
	// Without a comparable tag every branch below would be a coin toss.
	if !semver.IsValid(release.TagName) {
		return "", fmt.Errorf("latest release %q of %s is not a semver tag", release.TagName, githubRepo)
	}
	return release.TagName, nil
}

func releaseAssetURL(tag, asset string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", downloadHost, githubRepo, tag, asset)
}

// publishedChecksum returns the sha256 the release publishes for asset. It travels the
// same channel as the binary, so it guards against a corrupted or swapped asset, not
// against a compromised release: signing the checksum file is the next step up.
func publishedChecksum(tag, asset string) (string, error) {
	url := releaseAssetURL(tag, binaryName+".sha256sum")
	body, err := fetch(url, maxMetadataBytes)
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}

	sum, ok := checksumFor(body, asset)
	if !ok {
		return "", fmt.Errorf("no checksum published for %s in %s", asset, url)
	}
	return sum, nil
}

// checksumFor picks the digest of asset out of a sha256sum listing.
func checksumFor(listing []byte, asset string) (string, bool) {
	for _, line := range strings.Split(string(listing), "\n") {
		// sha256sum writes "<hex>  <name>", and "<hex> *<name>" in binary mode.
		sum, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if strings.TrimLeft(name, " *") == asset {
			return sum, true
		}
	}
	return "", false
}

func fetch(url string, max int64) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return readAtMost(resp.Body, max)
}

// readAtMost reads r in full, or fails: a silently truncated payload would be applied as
// if it were complete.
func readAtMost(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("payload exceeds %d bytes", max)
	}
	return data, nil
}
