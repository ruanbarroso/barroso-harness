// Package update queries the distribution channel and replaces the installed
// binary with the newest release.
//
// CHANNEL. Distribution is a Release in the public GitHub repository
// ruanbarroso/barroso-harness, published by the deployer (see
// .deploy/release.json). There is no dl.barroso.tec.br: the host does not
// resolve in DNS, so the source is the GitHub API.
//
// AUTHENTICATION. The repository is public, so no token is needed. When
// GITHUB_TOKEN or TOKEN_GITHUB is set (in that order) it is sent anyway: it
// lifts GitHub's anonymous rate limit. The token NEVER lives in config.json.
//
// VERIFICATION. Each binary's SHA256 comes from the SHA256SUMS published
// alongside the release, and the downloaded asset is checked before touching
// the running executable. Only after verification is the staged binary run
// (:cmd -version) to prove it runs on the current platform. Nothing is
// installed without these three proofs.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RepoSlug is the private releases repository. Fixed on purpose: the
// barroso-harness /update may only update itself from its own channel.
const RepoSlug = "ruanbarroso/barroso-harness"

const (
	originGitHub = "https://api.github.com"
	// manifestCap limits the API's JSON response. A release has a few KB of
	// metadata; 1 MiB is generous slack that still prevents an endless read
	// if the endpoint ever returns something unexpected.
	manifestCap = 1 << 20
	// assetCap is the cap for a downloaded binary. Current releases are
	// ~11 MB; 64 MiB allows growth without opening a hole to drain the server.
	assetCap = 64 << 20
	// httpTimeout covers the handshake and the download. An 11 MB download on
	// a poor connection needs slack; 5 min is the cap that does not look
	// "stuck".
	httpTimeout = 5 * time.Minute
)

// assetPrefix is the start of every release binary name. Used to tell a
// binary from metadata (SHA256SUMS) and to derive the platform.
const assetPrefix = "barroso-harness-"

// sha256SumsName is the checksum-list asset, published by the deployer.
const sha256SumsName = "SHA256SUMS"

// Lancamento is the release resolved in the channel: the tag and its assets.
type Lancamento struct {
	Version string
	TagName string
	Assets  []Asset
}

// Asset is a release file. URL is the GitHub API URL, which needs the
// Accept: application/octet-stream header to return bytes instead of JSON.
type Asset struct {
	Name  string
	URL   string
	Bytes int64
}

// Som carries an asset's declared checksum, read from SHA256SUMS. Empty means
// "the channel published no checksum for this file".
type Som struct {
	SHA256 string
}

// envToken returns the token from the environment, in the order documented
// in the package header. Empty string when there is none.
func envToken() string {
	for _, name := range []string{"GITHUB_TOKEN", "TOKEN_GITHUB"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// setAuth sends the environment token when there is one. The repository is
// public, so a request without it still works.
func setAuth(req *http.Request) {
	if token := envToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// BuscarUltima fetches the most recent published release from the channel.
//
// Pre-releases and drafts are excluded: /update delivers a stable version,
// and a draft is not always complete. If the channel ever publishes only
// pre-releases, the error message explains it — instead of installing
// something unstable.
func BuscarUltima(ctx context.Context) (Lancamento, error) {
	client := &http.Client{Timeout: httpTimeout}
	u := originGitHub + "/repos/" + RepoSlug + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Lancamento{}, fmt.Errorf("build channel request: %w", err)
	}
	setAuth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return Lancamento{}, fmt.Errorf("query channel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if envToken() == "" {
			return Lancamento{}, fmt.Errorf("channel refused the request (HTTP %s): GitHub's anonymous rate limit may be exhausted; set GITHUB_TOKEN to raise it", resp.Status)
		}
		return Lancamento{}, fmt.Errorf("channel rejected the token (HTTP %s): check the token permissions", resp.Status)
	}
	if resp.StatusCode == http.StatusNotFound {
		return Lancamento{}, fmt.Errorf("channel not found (HTTP %s): %s has no releases", resp.Status, RepoSlug)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Lancamento{}, fmt.Errorf("channel responded HTTP %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, manifestCap+1))
	if err != nil {
		return Lancamento{}, fmt.Errorf("read channel response: %w", err)
	}
	if len(raw) > manifestCap {
		return Lancamento{}, fmt.Errorf("channel response exceeds %d bytes", manifestCap)
	}

	var releases []releaseAPI
	if err := json.Unmarshal(raw, &releases); err != nil {
		return Lancamento{}, fmt.Errorf("invalid channel response: %w", err)
	}

	return pickNewest(releases)
}

// releaseAPI is the part of a release's JSON tree the package uses, and
// assetAPI a file inside it. They are named types — not anonymous structs
// inside BuscarUltima — so tests can feed pickNewest without starting an
// HTTP server.
type releaseAPI struct {
	TagName    string     `json:"tag_name"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	Assets     []assetAPI `json:"assets"`
}

type assetAPI struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// releaseFor builds a minimal, valid release for this platform, so tests do
// not repeat the mandatory asset in every case.
func releaseFor(tag string) releaseAPI {
	return releaseAPI{
		TagName: tag,
		Assets: []assetAPI{
			{Name: AssetPlataformaNome(), Size: 1024, BrowserDownloadURL: "https://example.invalid/bin"},
		},
	}
}

// pickNewest returns the release with the highest VERSION among the stable
// ones that have a binary for this platform. NOT the first in the list: the
// API order guarantees nothing — it comes in descending CREATION order, and a
// republish or retag can put an older version ahead of the newest (seen on
// the real channel: bh.9 listed before bh.10). Trusting the position would
// make /update offer a DOWNGRADE to someone already on the newest.
//
// Drafts and pre-releases are excluded: /update delivers a stable version. A
// tag outside the vX.Y.Z[-suffix] form is skipped, not fatal — a stray tag in
// the channel must not block everyone's update.
func pickNewest(releases []releaseAPI) (Lancamento, error) {
	var best Lancamento
	hasBest := false
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		// The tag is validated HERE, before becoming a candidate — not only
		// in the comparison. Otherwise a malformed tag that is the FIRST
		// eligible one would win by default (no comparison knocks it out)
		// and the channel would return "nightly-build" as the latest version.
		if _, err := parseVersion(r.TagName); err != nil {
			continue
		}
		if len(r.Assets) == 0 {
			continue
		}
		rel := Lancamento{Version: r.TagName, TagName: r.TagName}
		for _, a := range r.Assets {
			// The API ASSET URL comes first, and the order is not a detail: in
			// a PRIVATE repository browser_download_url returns 404 "Not Found"
			// on asset download, while the API URL returns the bytes with
			// Accept: application/octet-stream. Measured A/B on the real
			// channel on 2026-10-18, same Accept and same Bearer on both URLs,
			// varying only the kind: browser 404, API 200. User-Agent does not
			// change the result.
			//
			// Do NOT record a redirect explanation here: it was tested and is
			// FALSE. The API URL also redirects (302 to
			// release-assets.githubusercontent.com) and Go's default client
			// follows it and gets 200 — if the header died on the hop, the API
			// URL would fail the same way. The browser 404 has no explained
			// mechanism, and the choice does not depend on it.
			u := a.URL
			if u == "" {
				u = a.BrowserDownloadURL
			}
			rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: u, Bytes: a.Size})
		}
		if _, err := rel.AssetDaPlataforma(); err != nil {
			continue
		}
		if !hasBest {
			best, hasBest = rel, true
			continue
		}
		// The tag was already validated above, so compareVersions cannot fail
		// here; if it did, it would be a bug in this package. The previous
		// eligible release stays the winner.
		cmp, err := compareVersions(rel.Version, best.Version)
		if err != nil {
			continue
		}
		if cmp > 0 {
			best = rel
		}
	}
	if !hasBest {
		return Lancamento{}, errors.New("channel has no published release with a binary for this platform")
	}
	return best, nil
}

// MaisNovaQue reports whether the channel release is newer than the installed
// version. Exported because the CLI wants the answer before offering any
// download: if it is not newer, /update should not even ask.
func (l Lancamento) MaisNovaQue(installed string) (bool, error) {
	cmp, err := compareVersions(l.Version, installed)
	if err != nil {
		return false, err
	}
	return cmp > 0, nil
}

// AssetDaPlataforma picks this platform's asset by NAME — the GitHub API does
// not declare os/arch per asset, so the platform is in the name:
// barroso-harness-<goos>-<goarch>[.exe].
func (l Lancamento) AssetDaPlataforma() (Asset, error) {
	target := AssetPlataformaNome()
	// Exact match first; if the name ever gains an extra suffix, the prefix
	// still matches without accepting another platform's binary.
	for _, a := range l.Assets {
		if a.Name == target {
			return a, nil
		}
	}
	for _, a := range l.Assets {
		if strings.HasPrefix(a.Name, target+".") {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no binary for this platform (%s)", l.Version, target)
}

// AssetSomas returns the SHA256SUMS asset, if the release published it.
func (l Lancamento) AssetSomas() (Asset, bool) {
	for _, a := range l.Assets {
		if a.Name == sha256SumsName {
			return a, true
		}
	}
	return Asset{}, false
}

// ListarSomas downloads and parses SHA256SUMS. Format: "<hex>  <name>" per
// line, the same that sha256sum emits. Malformed lines are ignored — one
// extra future metadata line must not break the update.
func ListarSomas(ctx context.Context, asset Asset) (map[string]string, error) {
	if asset.URL == "" {
		return nil, errors.New("checksums asset has no URL")
	}
	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("SHA256SUMS responded HTTP %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sums[strings.TrimPrefix(fields[len(fields)-1], "*")] = strings.ToLower(fields[0])
	}
	return sums, nil
}

// versionParts is a version split into parts for comparison: three numbers
// and the suffix after the hyphen, split into identifiers (the deployer
// publishes v0.1.0-bh.10; "bh" and "10" are separate identifiers).
type versionParts struct {
	nums      [3]int
	suffix    []string
	hasSuffix bool
}

var versionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// parseVersion validates and splits a tag. A malformed tag is rejected:
// comparing the incomparable would yield a downgrade or refuse a legitimate
// update.
func parseVersion(v string) (versionParts, error) {
	v = strings.TrimSpace(v)
	if !versionPattern.MatchString(v) {
		return versionParts{}, fmt.Errorf("version %q is not in the form vX.Y.Z[-suffix]", v)
	}
	var c versionParts
	body := strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(body, '-'); i >= 0 {
		c.hasSuffix = true
		c.suffix = strings.Split(body[i+1:], ".")
		body = body[:i]
	}
	for i, p := range strings.SplitN(body, ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return versionParts{}, fmt.Errorf("version %q has invalid number %q", v, p)
		}
		c.nums[i] = n
	}
	return c, nil
}

// compareVersions returns -1, 0 or 1.
//
// The fix this function exists for: comparing the suffix as TEXT says that
// "bh.10" < "bh.9", because '1' < '9' at the first character. The result is
// the program refusing the newest update (or worse, offering a downgrade).
// The suffix is compared identifier by identifier, numerically when both
// sides are numbers.
func compareVersions(a, b string) (int, error) {
	ca, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	cb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range ca.nums {
		if ca.nums[i] != cb.nums[i] {
			if ca.nums[i] < cb.nums[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	// No suffix beats a suffix: v0.1.0 > v0.1.0-bh.10 (the stable release is
	// newer than its own preview). That is the semver convention.
	if ca.hasSuffix != cb.hasSuffix {
		if ca.hasSuffix {
			return -1, nil
		}
		return 1, nil
	}
	for i := 0; i < len(ca.suffix) || i < len(cb.suffix); i++ {
		var sa, sb string
		if i < len(ca.suffix) {
			sa = ca.suffix[i]
		}
		if i < len(cb.suffix) {
			sb = cb.suffix[i]
		}
		if sa == sb {
			continue
		}
		na, errA := strconv.Atoi(sa)
		nb, errB := strconv.Atoi(sb)
		if errA == nil && errB == nil {
			if na < nb {
				return -1, nil
			}
			return 1, nil
		}
		if sa < sb {
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

// ── helpers shared by both install_*.go ───────────────────────────────────

var errPermissaoManual = fmt.Errorf("manual permission fallback: %w", os.ErrPermission)

func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

func removeIfExists(name string) {
	if name != "" {
		_ = os.Remove(name)
	}
}

// validateVersionOutput checks that the staged binary reported the expected
// version. Without this proof, a download that returned HTML (an error page)
// would be installed as if it were an executable.
func validateVersionOutput(output, expected string) error {
	if !strings.Contains(output, expected) {
		return fmt.Errorf("staged binary reported %q, expected %s",
			strings.TrimSpace(output), expected)
	}
	return nil
}
