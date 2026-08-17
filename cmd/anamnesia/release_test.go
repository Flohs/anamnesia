package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"v1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.3", true},
		{" v0.1.0 ", "0.1.0", true},
		{"v1.2.3-rc1", "1.2.3-rc1", true},
		{"v1.2.3+build7", "1.2.3-build7", true},
		{"v10.0.0", "10.0.0", true},

		// Not released versions. A local build stamps a commit hash, and
		// replacing such a binary silently would be wrong.
		{"dev", "", false},
		{"873d6c5", "", false},
		{"v1.2", "", false},
		{"1.2.3.4", "", false},
		{"", "", false},
		{"v-1.2.3", "", false},
		{"vx.y.z", "", false},
	}
	for _, tc := range tests {
		got, ok := parseVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("parseVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.0", 0},
		{"1.9.0", "1.10.0", -1}, // not string ordering
		{"2.0.0", "10.0.0", -1},
		{"0.2.0", "0.10.0", -1},
		// A prerelease precedes the release with the same numbers.
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"1.0.0-rc2", "1.0.0-rc1", 1},
	}
	for _, tc := range tests {
		a, ok := parseVersion(tc.a)
		if !ok {
			t.Fatalf("cannot parse %q", tc.a)
		}
		b, ok := parseVersion(tc.b)
		if !ok {
			t.Fatalf("cannot parse %q", tc.b)
		}
		if got := compareVersions(a, b); got != tc.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestExpectedChecksum(t *testing.T) {
	body := strings.Join([]string{
		"aaaa1111  anamnesia-darwin-arm64",
		"bbbb2222  anamnesia-linux-amd64",
		"CCCC3333 *anamnesia-linux-arm64", // binary-mode marker
		"garbage line",
		"",
	}, "\n")

	for _, tc := range []struct{ asset, want string }{
		{"anamnesia-darwin-arm64", "aaaa1111"},
		{"anamnesia-linux-amd64", "bbbb2222"},
		{"anamnesia-linux-arm64", "cccc3333"}, // lowercased
	} {
		got, ok := expectedChecksum(body, tc.asset)
		if !ok {
			t.Errorf("no checksum found for %s", tc.asset)
			continue
		}
		if got != tc.want {
			t.Errorf("checksum for %s = %q, want %q", tc.asset, got, tc.want)
		}
	}
	if _, ok := expectedChecksum(body, "anamnesia-windows-amd64"); ok {
		t.Error("found a checksum for an asset that is not listed")
	}
}

func TestPlatformAssetMatchesReleaseNaming(t *testing.T) {
	// The updater and `make release` have to agree on the filename, or an
	// update can never find its own binary.
	want := fmt.Sprintf("anamnesia-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got := platformAsset(); got != want {
		t.Errorf("platformAsset() = %q, want %q", got, want)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Skipf("cannot read Makefile: %v", err)
	}
	if !strings.Contains(string(makefile), "dist/anamnesia-$$os-$$arch") {
		t.Error("the Makefile no longer builds dist/anamnesia-<os>-<arch>; the updater looks for that name")
	}
}

// releaseServer stands in for GitHub, serving one release and its assets.
func releaseServer(t *testing.T, tag string, assets map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	var list []asset
	for name, body := range assets {
		list = append(list, asset{Name: name, URL: srv.URL + "/download/" + name})
		mux.HandleFunc("/download/"+name, func(body []byte) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
		}(body))
	}
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"html_url": "https://example.invalid/releases/" + tag,
			"assets":   list,
		})
	})
	return srv
}

func TestLatestReleaseParsesAssets(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte("binary"),
		checksumsAsset:  []byte("deadbeef  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v9.9.9" {
		t.Errorf("tag = %q", rel.TagName)
	}
	if _, ok := rel.assetURL(platformAsset()); !ok {
		t.Errorf("platform asset missing from %+v", rel.Assets)
	}
	if _, ok := rel.assetURL(checksumsAsset); !ok {
		t.Error("checksums asset missing")
	}
	if _, ok := rel.assetURL("nope"); ok {
		t.Error("found an asset that was never published")
	}
}

// TestDownloadRejectsBadChecksum is the guard that matters most: a download
// whose hash does not match what the release published must never be
// installed.
func TestDownloadRejectsBadChecksum(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte("tampered binary"),
		checksumsAsset:  []byte("0000000000000000000000000000000000000000000000000000000000000000  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloadRelease(context.Background(), rel, t.TempDir(), &strings.Builder{})
	if err == nil {
		t.Fatal("a mismatched checksum was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestDownloadRejectsMissingChecksums(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte("binary"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downloadRelease(context.Background(), rel, t.TempDir(), &strings.Builder{}); err == nil {
		t.Fatal("a release with no checksums.txt was accepted")
	}
}

func TestDownloadRejectsMissingPlatformAsset(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		"anamnesia-plan9-mips": []byte("binary"),
		checksumsAsset:         []byte("aa  anamnesia-plan9-mips\n"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloadRelease(context.Background(), rel, t.TempDir(), &strings.Builder{})
	if err == nil {
		t.Fatal("a release without this platform's asset was accepted")
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("error does not say which platform is missing: %v", err)
	}
}

// TestDownloadAcceptsVerifiedBinary walks the happy path with a real
// executable, so the checksum comparison and the version self-check are both
// exercised rather than mocked.
func TestDownloadAcceptsVerifiedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the stand-in binary")
	}
	script := "#!/bin/sh\necho 'anamnesia 9.9.9 (commit abc, built now)'\n"
	sum := sha256.Sum256([]byte(script))

	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte(script),
		checksumsAsset:  []byte(hex.EncodeToString(sum[:]) + "  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path, err := downloadRelease(context.Background(), rel, t.TempDir(), &strings.Builder{})
	if err != nil {
		t.Fatalf("verified download rejected: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("downloaded binary is not executable: %v", info.Mode())
	}
}

// TestDownloadRejectsVersionMismatch covers the case where the asset is intact
// but is not the version the release claims.
func TestDownloadRejectsVersionMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the stand-in binary")
	}
	script := "#!/bin/sh\necho 'anamnesia 1.0.0'\n"
	sum := sha256.Sum256([]byte(script))

	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte(script),
		checksumsAsset:  []byte(hex.EncodeToString(sum[:]) + "  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	rel, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloadRelease(context.Background(), rel, t.TempDir(), &strings.Builder{})
	if err == nil {
		t.Fatal("a binary reporting the wrong version was accepted")
	}
	if !strings.Contains(err.Error(), "does not match release") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestInstallBinaryReplacesAtomically checks the swap keeps the destination's
// permissions and leaves no staging file behind.
func TestInstallBinaryReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "anamnesia")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := installBinary(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("destination has %q, want %q", got, "new")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("permissions became %v, want 0755", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".anamnesia-update-") {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

// TestSelfUpdateSkipsUnreleasedBuildWithoutForce: a locally built binary
// carries a commit hash, and must not be silently replaced.
func TestSelfUpdateSkipsUnreleasedBuildWithoutForce(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", map[string][]byte{
		platformAsset(): []byte("binary"),
		checksumsAsset:  []byte("aa  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	original := version
	version = "873d6c5"
	t.Cleanup(func() {
		releaseAPIBase = "https://api.github.com"
		version = original
	})

	var out strings.Builder
	res, err := selfUpdate(context.Background(), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replaced {
		t.Error("an unreleased build was replaced without --force")
	}
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("output does not explain how to proceed: %q", out.String())
	}
}

// TestSelfUpdateSkipsWhenCurrent avoids pointless downloads.
func TestSelfUpdateSkipsWhenCurrent(t *testing.T) {
	srv := releaseServer(t, "v1.0.0", map[string][]byte{
		platformAsset(): []byte("binary"),
		checksumsAsset:  []byte("aa  " + platformAsset() + "\n"),
	})
	releaseAPIBase = srv.URL
	original := version
	version = "1.0.0"
	t.Cleanup(func() {
		releaseAPIBase = "https://api.github.com"
		version = original
	})

	var out strings.Builder
	res, err := selfUpdate(context.Background(), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replaced {
		t.Error("replaced the binary despite already being current")
	}
	if !strings.Contains(out.String(), "already on the latest") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestLatestReleaseReportsNoReleases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	releaseAPIBase = srv.URL
	t.Cleanup(func() { releaseAPIBase = "https://api.github.com" })

	_, err := latestRelease(context.Background())
	if err == nil {
		t.Fatal("expected an error when no releases exist")
	}
	if !strings.Contains(err.Error(), "no published releases") {
		t.Errorf("unhelpful error: %v", err)
	}
}
