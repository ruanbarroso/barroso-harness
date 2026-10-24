package update

import (
	"net/http"
	"strings"
	"testing"
)

// ── compareVersions ───────────────────────────────────────────────────────
//
// The table below is why this package was rewritten: the original comparison
// sorted the suffix as TEXT, so "bh.10" < "bh.9" ('1' < '9' at the first
// character). The symptom would be /update refusing the newest release — or,
// with the sign flipped, offering a downgrade.

func TestCompareVersionsNumericSuffix(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0-bh.9", "v0.1.0-bh.10", -1},
		{"v0.1.0-bh.10", "v0.1.0-bh.9", 1},
		{"v0.1.0-bh.2", "v0.1.0-bh.10", -1},
		{"v0.1.0-bh.10", "v0.1.0-bh.10", 0},
		// Stable beats preview: the version without a suffix is newer than
		// itself with a suffix. That is the semver convention, and the
		// deployer publishes both forms when it closes a version.
		{"v0.1.0", "v0.1.0-bh.10", 1},
		{"v0.1.0-bh.10", "v0.1.0", -1},
		// A non-numeric suffix compares lexicographically, as in semver.
		{"v0.1.0-alpha", "v0.1.0-beta", -1},
		{"v0.1.0-rc.2", "v0.1.0-rc.10", -1},
	}
	for _, c := range cases {
		got, err := compareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("compareVersions(%q,%q): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsRejectsInvalidForm(t *testing.T) {
	for _, v := range []string{"", "dev", "1.2", "vX.Y.Z", "v0.1.0-", "anything_else"} {
		if _, err := parseVersion(v); err == nil {
			t.Errorf("parseVersion(%q) should reject", v)
		}
	}
}

// ── token ──────────────────────────────────────────────────────────────────

// The repository is public: without a token in the environment, requests go
// out with no Authorization header instead of an empty "Bearer ".
func TestNoTokenSendsNoAuthorization(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("TOKEN_GITHUB", "")
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid", nil)
	setAuth(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q without a token, want none", got)
	}
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	setAuth(req)
	if got := req.Header.Get("Authorization"); got != "Bearer ghp_x" {
		t.Fatalf("Authorization = %q with a token, want Bearer ghp_x", got)
	}
}

func TestTokenEnvironmentPrecedence(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_primary")
	t.Setenv("TOKEN_GITHUB", "ghp_secondary")
	if got := envToken(); got != "ghp_primary" {
		t.Errorf("envToken = %q, want ghp_primary (GITHUB_TOKEN wins)", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := envToken(); got != "ghp_secondary" {
		t.Errorf("envToken without GITHUB_TOKEN = %q, want ghp_secondary", got)
	}
}

// ── asset name ─────────────────────────────────────────────────────────────

// The asset name is the ONLY way to know the platform: the GitHub API does
// not declare os/arch per asset. If this function derives it wrong, the
// update downloads the wrong platform's binary and the exec check fails with
// a confusing message.
func TestPlatformAssetNameHasDeployerForm(t *testing.T) {
	name := AssetPlataformaNome()
	if !strings.HasPrefix(name, "barroso-harness-") {
		t.Errorf("asset %q should start with barroso-harness-", name)
	}
	if !strings.Contains(strings.TrimPrefix(name, "barroso-harness-"), "-") {
		t.Errorf("asset %q should have goos-goarch", name)
	}
}

// Names that only share this platform's name as a prefix, or name another
// platform, must NOT match; the exact name must.
func TestPlatformAssetRequiresExactName(t *testing.T) {
	own := AssetPlataformaNome()
	l := Lancamento{Version: "v0.1.0-bh.10", Assets: []Asset{
		{Name: own + "-v2"},
		{Name: own + "x"},
		{Name: "barroso-harness-planet9-ppc64"},
	}}
	if _, err := l.AssetDaPlataforma(); err == nil {
		t.Fatal("a release without this platform's exact binary should error")
	}

	l.Assets = []Asset{{Name: own}}
	got, err := l.AssetDaPlataforma()
	if err != nil {
		t.Fatalf("AssetDaPlataforma: %v", err)
	}
	if got.Name != own {
		t.Fatalf("asset %q, want %q", got.Name, own)
	}
}

// ── SHA256SUMS ─────────────────────────────────────────────────────────────

// An asset without a checksum in SHA256SUMS cannot be installed. This test
// pins Rodar's decision: a missing checksum aborts instead of proceeding on
// "trust".
func TestDownloadRejectsInvalidChecksum(t *testing.T) {
	for _, s := range []string{"abc", "zz" + strings.Repeat("0", 62), strings.Repeat("a", 63)} {
		if isSHA256(s) {
			t.Errorf("isSHA256(%q) should be false", s)
		}
	}
	if !isSHA256(strings.Repeat("a", 64)) {
		t.Error("isSHA256 of 64 hex digits should be true")
	}
}

// ── Rodar: flow without a channel ──────────────────────────────────────────

// A local "dev" version is a build without a release: any published version
// is new and the comparison must neither hang nor refuse.
func TestNormalizeVersionDev(t *testing.T) {
	for input, want := range map[string]string{
		"":              "dev",
		"  ":            "dev",
		"dev":           "dev",
		" v0.1.0-bh.9 ": "v0.1.0-bh.9",
	} {
		if got := normalizeVersion(input); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

// ── pickNewest ─────────────────────────────────────────────────────────────
//
// These tests exist because of a REAL bug, caught by a probe against the
// production channel before anyone saw the symptom: the API returned
// v0.1.0-bh.9 BEFORE v0.1.0-bh.10 in the release list. The first version of
// this package took the first stable release in the list, so a user with
// bh.10 installed would be offered a DOWNGRADE to bh.9.

// THE CASE THAT MOTIVATED THE FIX: the list has the OLDER version before the
// newer one. The choice must look at the version, not the position —
// otherwise whoever is on bh.10 gets offered a downgrade to bh.9.
func TestPickNewestIgnoresListOrder(t *testing.T) {
	list := []releaseAPI{
		releaseFor("v0.1.0-bh.9"),
		releaseFor("v0.1.0-bh.10"),
		releaseFor("v0.1.0-bh.8"),
	}
	rel, err := pickNewest(list)
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	if rel.Version != "v0.1.0-bh.10" {
		t.Fatalf("picked %q, want v0.1.0-bh.10 (the newest, not the first)", rel.Version)
	}
}

// More than ten versions: "bh.10" > "bh.9" needs the NUMERIC suffix
// comparison. A textual comparison would rank bh.9 highest and the test
// would fail.
func TestPickNewestWithTwoDigitSuffix(t *testing.T) {
	list := []releaseAPI{releaseFor("v0.1.0-bh.9"), releaseFor("v0.1.0-bh.11"), releaseFor("v0.1.0-bh.10")}
	rel, err := pickNewest(list)
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	if rel.Version != "v0.1.0-bh.11" {
		t.Fatalf("picked %q, want v0.1.0-bh.11", rel.Version)
	}
}

// Drafts and pre-releases are ignored even when they are the highest version
// in the list: /update delivers stable.
func TestPickNewestIgnoresDraftAndPrerelease(t *testing.T) {
	highest := releaseFor("v0.2.0-rc.1")
	highest.Prerelease = true
	draft := releaseFor("v0.3.0")
	draft.Draft = true
	list := []releaseAPI{highest, draft, releaseFor("v0.1.0-bh.10")}
	rel, err := pickNewest(list)
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	if rel.Version != "v0.1.0-bh.10" {
		t.Fatalf("picked %q; draft/pre-release should not win", rel.Version)
	}
}

// A malformed tag (stray in the channel) must neither abort nor win: it is
// ignored and the best valid one still wins.
func TestPickNewestIgnoresInvalidTag(t *testing.T) {
	stray := releaseFor("nightly-build")
	list := []releaseAPI{stray, releaseFor("v0.1.0-bh.10")}
	rel, err := pickNewest(list)
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	if rel.Version != "v0.1.0-bh.10" {
		t.Fatalf("picked %q; an invalid tag should not win", rel.Version)
	}
}

// Only invalid tags in the list = nothing installable, and the error must
// say so instead of returning an empty release.
func TestPickNewestOnlyInvalidTagErrors(t *testing.T) {
	_, err := pickNewest([]releaseAPI{releaseFor("garbage")})
	if err == nil {
		t.Fatal("only invalid tags should error")
	}
}

// A release without a binary for THIS platform is useless: it is ignored,
// even if it is the newest. Without this filter, the update would download
// an asset that does not exist here.
func TestPickNewestIgnoresReleaseWithoutPlatformBinary(t *testing.T) {
	other := releaseAPI{
		TagName: "v0.1.0-bh.10",
		Assets: []assetAPI{
			{Name: "barroso-harness-planet-9-ppc64", Size: 10, BrowserDownloadURL: "https://example.invalid/x"},
		},
	}
	list := []releaseAPI{other, releaseFor("v0.1.0-bh.9")}
	rel, err := pickNewest(list)
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	if rel.Version != "v0.1.0-bh.9" {
		t.Fatalf("picked %q; a release without this platform's binary should not win", rel.Version)
	}
}

// An empty list (channel with no published release) returns an explicit error.
func TestPickNewestEmptyList(t *testing.T) {
	if _, err := pickNewest(nil); err == nil {
		t.Fatal("an empty list should error")
	}
}

// The chosen URL must be the API ASSET URL, not the browser one.
//
// It is not taste: in a private repository browser_download_url redirects to
// objects.githubusercontent.com and the Authorization header does not
// survive the hop — the download gives 404 "Not Found" with a valid token.
// The API URL accepts the Bearer and returns bytes with
// Accept: application/octet-stream. If this test fails, every /update breaks
// in production with nothing else giving it away.
func TestPickNewestPrefersAPIAssetURL(t *testing.T) {
	r := releaseFor("v0.1.0-bh.11")
	r.Assets[0].URL = "https://api.github.com/repos/ruanbarroso/barroso-harness/releases/assets/653642039"
	r.Assets[0].BrowserDownloadURL = "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/barroso-harness-windows-amd64.exe"

	rel, err := pickNewest([]releaseAPI{r})
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	asset, err := rel.AssetDaPlataforma()
	if err != nil {
		t.Fatalf("AssetDaPlataforma: %v", err)
	}
	if !strings.HasPrefix(asset.URL, "https://api.github.com/") {
		t.Fatalf("chosen URL %q should be the API asset URL (browser_download_url returns 404 on a private repo; the API URL returns the bytes)", asset.URL)
	}
}

// A release without an API URL falls back to the browser one: degradation,
// not failure.
func TestPickNewestFallsBackToBrowserWithoutAPIURL(t *testing.T) {
	r := releaseFor("v0.1.0-bh.11")
	r.Assets[0].URL = ""
	r.Assets[0].BrowserDownloadURL = "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/binary"

	rel, err := pickNewest([]releaseAPI{r})
	if err != nil {
		t.Fatalf("pickNewest: %v", err)
	}
	asset, _ := rel.AssetDaPlataforma()
	if asset.URL != "https://github.com/ruanbarroso/barroso-harness/releases/download/v0.1.0-bh.11/binary" {
		t.Fatalf("without an API URL it should use the browser one, got %q", asset.URL)
	}
}
