// release.go implements self-update against the project's GitHub releases.
//
// Replacing your own executable is the kind of thing that has to be either
// completely right or not attempted, so every step is checked:
//
//  1. the release's version must parse and be strictly newer than this build
//  2. the asset must match this exact platform
//  3. its SHA-256 must match the checksums.txt published in the same release
//  4. the downloaded file must run and report the version it claims to be
//  5. only then does it replace the current binary, atomically
//
// A build that is not a released version (a local `make build`, which stamps
// a commit hash) is never replaced silently; it takes --force.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The repository releases are published from. Overridable at build time so a
// fork does not have to patch source.
var (
	releaseOwner = "Flohs"
	releaseRepo  = "anamnesia-open-source"
	// releaseAPIBase is overridden by tests to point at a local server.
	releaseAPIBase = "https://api.github.com"
)

// maxAssetBytes caps a download. The binary is around 20 MB; this leaves room
// without letting a bad response consume the disk.
const maxAssetBytes = 200 << 20

// checksumsAsset is the file listing each asset's SHA-256.
const checksumsAsset = "checksums.txt"

// ghRelease is the subset of GitHub's release payload we use.
type ghRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// assetURL returns the download URL for a named asset.
func (r *ghRelease) assetURL(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, true
		}
	}
	return "", false
}

// platformAsset is the asset name for the running platform. It matches what
// `make release` produces and what the release workflow uploads.
func platformAsset() string {
	name := fmt.Sprintf("anamnesia-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// httpGet performs a GET with the headers GitHub expects.
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "anamnesia/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	// A token is not required for public releases, but honouring one avoids
	// the unauthenticated rate limit on shared networks and in CI.
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return (&http.Client{Timeout: 60 * time.Second}).Do(req)
}

// latestRelease fetches the newest published release.
func latestRelease(ctx context.Context) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", releaseAPIBase, releaseOwner, releaseRepo)
	res, err := httpGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("ask GitHub for the latest release: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s/%s has no published releases yet", releaseOwner, releaseRepo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub rate-limited this check; try again later, or set GITHUB_TOKEN")
	default:
		return nil, fmt.Errorf("GitHub returned %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("parse GitHub's reply: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("GitHub returned a release with no tag")
	}
	return &rel, nil
}

// ─── versions ────────────────────────────────────────────────────────

// semver is a parsed version. Only what is needed to order releases.
type semver struct {
	major, minor, patch int
	pre                 string // prerelease suffix, empty for a final release
}

func (v semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

// parseVersion reads "v1.2.3", "1.2.3" or "1.2.3-rc1".
//
// It deliberately fails on anything else, including the bare commit hash that
// `make build` stamps into a local build. An unparseable version means "not a
// released build", which is a state self-update must not guess about.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, false
	}
	var pre string
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

// compareVersions orders two versions: -1 if a < b, 0 if equal, 1 if a > b.
// A prerelease sorts before the matching final release, per semver.
func compareVersions(a, b semver) int {
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "": // a is final, b is a prerelease of the same numbers
		return 1
	case b.pre == "":
		return -1
	case a.pre < b.pre:
		return -1
	default:
		return 1
	}
}

// ─── download and verify ─────────────────────────────────────────────

// fetchToFile downloads url into dir and returns the path, along with the
// SHA-256 of what was written.
func fetchToFile(ctx context.Context, url, dir, name string) (string, string, error) {
	res, err := httpGet(ctx, url)
	if err != nil {
		return "", "", fmt.Errorf("download %s: %w", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download %s: %s", name, res.Status)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(res.Body, maxAssetBytes)); err != nil {
		return "", "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return "", "", err
	}
	return path, hex.EncodeToString(sum.Sum(nil)), nil
}

// expectedChecksum reads one asset's hash out of a checksums.txt body.
// Lines look like "<hex>  <filename>", as produced by sha256sum.
func expectedChecksum(body, asset string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		// The filename may carry a leading '*' from binary-mode output.
		if strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// downloadRelease fetches this platform's binary, verifies it, and returns the
// path to the verified file. The caller installs it.
func downloadRelease(ctx context.Context, rel *ghRelease, dir string, out io.Writer) (string, error) {
	asset := platformAsset()
	binURL, ok := rel.assetURL(asset)
	if !ok {
		return "", fmt.Errorf("release %s has no asset for %s/%s (looked for %q)",
			rel.TagName, runtime.GOOS, runtime.GOARCH, asset)
	}
	sumsURL, ok := rel.assetURL(checksumsAsset)
	if !ok {
		return "", fmt.Errorf("release %s publishes no %s, so the download cannot be verified",
			rel.TagName, checksumsAsset)
	}

	fmt.Fprintf(out, "  downloading %s %s\n", asset, rel.TagName)
	binPath, gotSum, err := fetchToFile(ctx, binURL, dir, asset)
	if err != nil {
		return "", err
	}
	sumsPath, _, err := fetchToFile(ctx, sumsURL, dir, checksumsAsset)
	if err != nil {
		return "", err
	}
	sumsBody, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	wantSum, ok := expectedChecksum(string(sumsBody), asset)
	if !ok {
		return "", fmt.Errorf("%s does not list %s", checksumsAsset, asset)
	}
	if gotSum != wantSum {
		return "", fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s\nRefusing to install it",
			asset, wantSum, gotSum)
	}
	fmt.Fprintln(out, "  checksum verified")

	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", err
	}
	// Last check: the thing we downloaded has to be a working binary that
	// agrees about which version it is.
	if err := verifyDownloadedVersion(ctx, binPath, rel.TagName); err != nil {
		return "", err
	}
	return binPath, nil
}

// verifyDownloadedVersion runs the downloaded binary's `version` command.
func verifyDownloadedVersion(ctx context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	got := strings.TrimSpace(string(out))
	want, ok := parseVersion(tag)
	if !ok {
		return nil // nothing to compare against
	}
	if !strings.Contains(got, want.String()) && !strings.Contains(got, tag) {
		return fmt.Errorf("the downloaded binary reports %q, which does not match release %s", got, tag)
	}
	return nil
}

// installBinary replaces dest with the file at src.
//
// The replacement is a rename within the destination directory, so it is
// atomic and never leaves a half-written executable. Replacing a running
// binary this way is safe on Unix: the running process keeps the old inode.
func installBinary(src, dest string) error {
	dir := filepath.Dir(dest)
	staged := filepath.Join(dir, ".anamnesia-update-"+filepath.Base(dest))

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o755)
	if info, err := os.Stat(dest); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(staged, data, mode); err != nil {
		return installPermissionError(dest, err)
	}
	if err := os.Chmod(staged, mode); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, dest); err != nil {
		_ = os.Remove(staged)
		return installPermissionError(dest, err)
	}
	return nil
}

// installPermissionError turns an opaque EACCES into the actual next step.
// Anamnesia does not escalate privileges on its own.
func installPermissionError(dest string, cause error) error {
	if !os.IsPermission(cause) {
		return fmt.Errorf("install to %s: %w", dest, cause)
	}
	return fmt.Errorf(`cannot write to %s: %w

That location needs elevated permissions. Either:
  sudo anamnesia update
or install somewhere you own and update there instead`, dest, cause)
}

// ─── the self-update step ────────────────────────────────────────────

// selfUpdateResult reports what happened, so the caller can decide whether to
// hand off to the new binary.
type selfUpdateResult struct {
	Checked   bool
	Latest    string // tag of the newest release, when known
	Replaced  bool
	SelfPath  string
	NoteLines []string
}

// selfUpdate compares this build against the latest release and, when it is
// older, installs the new one. force allows replacing an unreleased build.
func selfUpdate(ctx context.Context, out io.Writer, force bool) (selfUpdateResult, error) {
	res := selfUpdateResult{Checked: true}

	rel, err := latestRelease(ctx)
	if err != nil {
		return res, err
	}
	res.Latest = rel.TagName

	latest, ok := parseVersion(rel.TagName)
	if !ok {
		return res, fmt.Errorf("the latest release is tagged %q, which is not a version this can compare", rel.TagName)
	}
	current, currentOK := parseVersion(version)

	switch {
	case !currentOK && !force:
		fmt.Fprintf(out, "  this build is %q, not a released version; latest release is %s\n", version, rel.TagName)
		fmt.Fprintln(out, "  pass --force to replace it anyway")
		return res, nil
	case currentOK && compareVersions(current, latest) >= 0:
		fmt.Fprintf(out, "  already on the latest release (%s)\n", rel.TagName)
		return res, nil
	}

	if currentOK {
		fmt.Fprintf(out, "  a newer release is available: %s (running %s)\n", rel.TagName, current)
	}

	self, err := selfPath()
	if err != nil {
		return res, err
	}
	res.SelfPath = self

	tmp, err := os.MkdirTemp("", "anamnesia-update-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	verified, err := downloadRelease(ctx, rel, tmp, out)
	if err != nil {
		return res, err
	}
	if err := installBinary(verified, self); err != nil {
		return res, err
	}
	res.Replaced = true
	fmt.Fprintf(out, "  replaced %s with %s\n", self, rel.TagName)
	return res, nil
}

// checkOnly reports whether a newer release exists, changing nothing.
func checkOnly(ctx context.Context, out io.Writer) error {
	rel, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed: %s\n", version)
	fmt.Fprintf(out, "latest:    %s\n", rel.TagName)
	if rel.HTMLURL != "" {
		fmt.Fprintf(out, "notes:     %s\n", rel.HTMLURL)
	}

	latest, latestOK := parseVersion(rel.TagName)
	current, currentOK := parseVersion(version)
	switch {
	case !latestOK:
		fmt.Fprintf(out, "\nThe release tag %q is not a comparable version.\n", rel.TagName)
	case !currentOK:
		fmt.Fprintf(out, "\nThis is not a released build, so there is nothing to compare.\n")
		fmt.Fprintln(out, "Run `anamnesia update --force` to install the latest release.")
	case compareVersions(current, latest) < 0:
		fmt.Fprintf(out, "\nAn update is available. Run `anamnesia update`.\n")
	default:
		fmt.Fprintln(out, "\nUp to date.")
	}
	if _, ok := rel.assetURL(platformAsset()); latestOK && !ok {
		fmt.Fprintf(out, "\nNote: that release has no binary for %s/%s.\n", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}
