package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Confirmar decides whether the update proceeds. It exists so the REPL can
// ask and tests can pin the answer. Nil = proceed without asking (-p mode,
// tests).
type Confirmar func() bool

// Rodar checks the channel, downloads the newest release, verifies and
// installs it. It is the only entry point the CLI calls.
//
// Confirmation comes BEFORE the download: there is no reason to download
// 11 MB only to find out the person did not want to replace the binary.
//
// Possible errors, and what each means:
//   - missing token           → private channel without credentials; nothing done
//   - channel rejected token  → permissions; nothing done
//   - no stable release       → empty channel for this platform; nothing done
//   - checksum mismatch       → corrupted download; staged file removed, binary intact
//   - binary in use           → Windows cannot rename a running executable; exits
//     with the PowerShell command for the person to run
//
// In every failure BEFORE installation, the running executable is left
// untouched. A failed update never leaves the machine without a harness.
//
// installed reports that a new binary is in place: the caller exits so the
// next start runs it.
func Rodar(ctx context.Context, out io.Writer, currentVersion string, confirm Confirmar) (installed bool, err error) {
	rel, err := BuscarUltima(ctx)
	if err != nil {
		return false, fmt.Errorf("update channel unavailable: %w", err)
	}

	cmp, err := compareVersions(currentVersion, rel.Version)
	if err != nil {
		return false, fmt.Errorf("compare versions: %w", err)
	}
	switch cmp {
	case 0:
		fmt.Fprintf(out, "already on the latest version (%s).\n", currentVersion)
		return false, nil
	case 1:
		fmt.Fprintf(out, "installed version (%s) is newer than the published one (%s); nothing to do.\n",
			currentVersion, rel.Version)
		return false, nil
	}

	asset, err := rel.AssetDaPlataforma()
	if err != nil {
		return false, err
	}

	// The checksum comes from the SHA256SUMS published with the release.
	// Without it, the integrity check has no reference — and downloading a
	// binary without checking is exactly the hole this package closes.
	sum := ""
	if sumsAsset, ok := rel.AssetSomas(); ok {
		sums, errSums := ListarSomas(ctx, sumsAsset)
		if errSums != nil {
			return false, fmt.Errorf("could not read the release SHA256SUMS (%v); aborting for safety", errSums)
		}
		sum = sums[asset.Name]
		if sum == "" {
			return false, fmt.Errorf("SHA256SUMS of release %s does not list %s; aborting for safety",
				rel.Version, asset.Name)
		}
	} else {
		return false, errors.New("release did not publish SHA256SUMS; aborting for safety")
	}

	if confirm != nil && !confirm() {
		fmt.Fprintln(out, "update canceled; current installation kept.")
		return false, nil
	}

	plan := Plano{Release: rel, Asset: asset, Sum: sum}
	if _, ok := cachedFor(plan); ok {
		fmt.Fprintf(out, "using %s, already downloaded in the background...\n", rel.Version)
	} else {
		fmt.Fprintf(out, "downloading %s (%s, ~%d MiB)...\n", asset.Name, rel.Version, asset.Bytes/(1<<20))
	}
	staging, err := stage(ctx, plan)
	if err != nil {
		return false, err
	}
	defer removeIfExists(staging)

	result, err := installBinary(staging)
	if err != nil {
		if len(result.commands) > 0 {
			fmt.Fprintln(out, "not enough permission to replace the binary; run the following, then reopen bh:")
			fmt.Fprintln(out, installCommands(result))
			return false, err
		}
		return false, err
	}
	fmt.Fprintf(out, "installed %s; bh will exit now — reopen it to use the new version.\n", rel.Version)
	return true, nil
}

// stagingRoot is where the downloaded binary waits for installation. It lives
// in the user cache — never next to the executable, where a temp file with
// the exec bit could be mistaken for a half-done installation.
func stagingRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		// With no known cache, the executable's own dir accepts a file with
		// an unmistakable name; the installation removes it in the defer.
		if exe, errExe := os.Executable(); errExe == nil {
			return filepath.Dir(exe)
		}
		return "."
	}
	return filepath.Join(dir, "barroso-harness", "updates")
}

// normalizeVersion cleans up "dev" — a local build without a release
// version. In that case any published version is new.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "dev"
	}
	return v
}

// Plano is what Verificar found: either nothing to do (Current), or a newer
// release ready to install (Release, Asset, Sum).
type Plano struct {
	Current bool   // installed version is the newest (or newer)
	Message string // why there is nothing to do, in one line
	Release Lancamento
	Asset   Asset
	Sum     string
}

// Verificar checks the channel without downloading anything: it is the first
// half of Rodar, for a UI that asks before installing (the update dialog).
func Verificar(ctx context.Context, currentVersion string) (Plano, error) {
	rel, err := BuscarUltima(ctx)
	if err != nil {
		return Plano{}, fmt.Errorf("update channel unavailable: %w", err)
	}
	cmp, err := compareVersions(currentVersion, rel.Version)
	if err != nil {
		return Plano{}, fmt.Errorf("compare versions: %w", err)
	}
	switch cmp {
	case 0:
		return Plano{Current: true, Message: "already on the latest version (" + currentVersion + ")."}, nil
	case 1:
		return Plano{Current: true, Message: "installed version (" + currentVersion +
			") is newer than the published one (" + rel.Version + ")."}, nil
	}
	asset, err := rel.AssetDaPlataforma()
	if err != nil {
		return Plano{}, err
	}
	sumsAsset, ok := rel.AssetSomas()
	if !ok {
		return Plano{}, errors.New("release did not publish SHA256SUMS; aborting for safety")
	}
	sums, err := ListarSomas(ctx, sumsAsset)
	if err != nil {
		return Plano{}, fmt.Errorf("could not read the release SHA256SUMS (%v); aborting for safety", err)
	}
	sum := sums[asset.Name]
	if sum == "" {
		return Plano{}, fmt.Errorf("SHA256SUMS of release %s does not list %s; aborting for safety", rel.Version, asset.Name)
	}
	return Plano{Release: rel, Asset: asset, Sum: sum}, nil
}

// Instalar downloads, verifies and installs what Verificar found: the second
// half of Rodar. On a permission failure the error carries the commands the
// person must run.
func Instalar(ctx context.Context, p Plano) error {
	staging, err := stage(ctx, p)
	if err != nil {
		return err
	}
	defer removeIfExists(staging)
	result, err := installBinary(staging)
	if err != nil {
		if len(result.commands) > 0 {
			return fmt.Errorf("%w; run: %s", err, strings.Join(result.commands, " && "))
		}
		return err
	}
	return nil
}

// stage returns a verified binary of the plan's release, ready to install:
// the one prefetched in the background when its SHA256 matches SHA256SUMS,
// otherwise a fresh download. Either way the exec check runs before anything
// is installed. The caller removes the returned file once done.
func stage(ctx context.Context, p Plano) (string, error) {
	staging, ok := cachedFor(p)
	if !ok {
		staging = filepath.Join(stagingRoot(), p.Asset.Name)
		if _, err := Baixar(ctx, p.Asset, p.Sum, staging); err != nil {
			return "", err
		}
	}
	if err := verifyStaged(staging, p.Release.Version); err != nil {
		removeIfExists(staging)
		return "", fmt.Errorf("new binary failed the exec check: %w", err)
	}
	return staging, nil
}
