// Auto-update: the interactive bh checks the channel in the background and,
// when a newer release is out, downloads, verifies AND installs it. The
// running process stays on its version; the next start runs the new one, so
// every new conversation opens on the latest release without /update.
//
// It builds on the prefetch: the tick starts at the free github.com redirect
// (no API budget spent while up to date), Prebaixar puts the verified binary
// in the cache, and the install goes through the same stage + installBinary
// path as /update — SHA256 against SHA256SUMS, exec check with -version, the
// running executable kept as .old-N for rollback.
//
// Off on the iSH build (see auto_ish.go): there updating stays manual, by
// /update. Off for "dev" builds too: every tag would look newer.
package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IntervaloAuto is the pause between background checks.
const IntervaloAuto = 2 * time.Minute

// installLockName serializes installs across bh windows: two processes
// renaming the same executable at once would race on the .old-N backup.
const installLockName = "install"

// installOverride lets tests swap the real install (which replaces the binary
// running the tests) for a fake.
var installOverride func(staged string) error

// errNeedsPermission is an install that would need sudo: the background has
// nobody to run it, so the person updates through /update instead.
var errNeedsPermission = errors.New("installing needs elevated permission; run /update")

// runningReports says whether the installed executable already reports
// version: another bh window may have installed it while this one was
// downloading. A var so tests do not exec anything.
var runningReports = func(version string) bool {
	exe, err := executablePath()
	if err != nil {
		return false
	}
	out, err := exec.Command(exe, "-version").CombinedOutput()
	return err == nil && validateVersionOutput(string(out), version) == nil
}

// AutoInstalar is one tick: if the channel has a release newer than installed,
// download it (or reuse the prefetched copy), verify it and install it.
// Returns the version that was installed, or "" when there was nothing to do.
// On a failed install the error is an *ErroAuto carrying the version, so the
// loop stops retrying (and re-downloading) that release.
func AutoInstalar(ctx context.Context, installed string) (string, error) {
	if _, err := Prebaixar(ctx, installed); err != nil {
		return "", err
	}
	tag, err := newestCached(installed)
	if tag == "" || err != nil {
		return "", err
	}
	plan, err := Verificar(ctx, installed)
	if err != nil || plan.Current || plan.Release.Version != tag {
		return "", err
	}
	if _, ok := cachedFor(plan); !ok {
		return "", nil
	}
	dir := prefetchDir()
	release, ok := takeLock(dir, installLockName)
	if !ok {
		return "", nil
	}
	defer release()
	if runningReports(plan.Release.Version) {
		return plan.Release.Version, nil
	}
	staging, err := stage(ctx, plan)
	if err != nil {
		return "", &ErroAuto{Version: plan.Release.Version, Err: err}
	}
	defer removeIfExists(staging)
	if installOverride != nil {
		err = installOverride(staging)
	} else {
		var result installResult
		result, err = installBinary(staging)
		if len(result.commands) > 0 {
			err = errNeedsPermission
		}
	}
	if err != nil {
		return "", &ErroAuto{Version: plan.Release.Version, Err: err}
	}
	return plan.Release.Version, nil
}

// ErroAuto is a failed background install of Version.
type ErroAuto struct {
	Version string
	Err     error
}

func (e *ErroAuto) Error() string { return "auto-update to " + e.Version + ": " + e.Err.Error() }
func (e *ErroAuto) Unwrap() error { return e.Err }

// newestCached returns the newest version sitting in the prefetch cache when
// it is newer than installed. Prebaixar keeps a single version there, so this
// is a lookup, not a search.
func newestCached(installed string) (string, error) {
	entries, err := os.ReadDir(prefetchDir())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	prefix := AssetPlataformaNome() + "."
	best := ""
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		if cmp, err := compareVersions(v, installed); err != nil || cmp <= 0 {
			continue
		}
		if best == "" {
			best = v
		} else if cmp, _ := compareVersions(v, best); cmp > 0 {
			best = v
		}
	}
	return best, nil
}

// LimparBackups deletes the .old-N copies earlier installs left next to the
// executable. Best-effort: on Windows the copy still running (the process
// that installed over itself) is locked and stays until the next start.
func LimparBackups() {
	exe, err := executablePath()
	if err != nil {
		return
	}
	for _, target := range []string{exe, aliasOf(exe)} {
		if target == "" {
			continue
		}
		matches, _ := filepath.Glob(target + ".old*")
		for _, m := range matches {
			removeIfExists(m)
		}
	}
}

// aliasOf is the second copy installed next to the binary (bh ↔
// barroso-harness), or "" when the name is neither.
func aliasOf(exe string) string {
	dir, base := filepath.Dir(exe), strings.ToLower(filepath.Base(exe))
	ext := filepath.Ext(base)
	switch strings.TrimSuffix(base, ext) {
	case "bh":
		return filepath.Join(dir, "barroso-harness"+ext)
	case "barroso-harness":
		return filepath.Join(dir, "bh"+ext)
	}
	return ""
}

// LoopAuto runs AutoInstalar now and then every IntervaloAuto until ctx is
// canceled, calling onInstalled after each install. Silent otherwise: network
// errors, an offline channel and a rate-limited API are normal, and the next
// tick retries; a failed install is not retried until the channel publishes
// another release. After an install the loop compares against the NEW version,
// so a later release installs on top without reinstalling the same one.
func LoopAuto(ctx context.Context, installed string, onInstalled func(version string)) {
	if !AutoDisponivel {
		return
	}
	if _, err := parseVersion(installed); err != nil {
		return
	}
	LimparBackups()
	failed := "" // a release whose install failed is left to /update
	for {
		if failed == "" || channelMovedPast(ctx, failed) {
			v, err := AutoInstalar(ctx, installed)
			var ea *ErroAuto
			switch {
			case errors.As(err, &ea):
				failed = ea.Version
			case err == nil && v != "":
				installed, failed = v, ""
				if onInstalled != nil {
					onInstalled(v)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(IntervaloAuto):
		}
	}
}

// channelMovedPast reports whether the channel now publishes something other
// than the release that failed, through the free redirect.
func channelMovedPast(ctx context.Context, failed string) bool {
	tag, err := TagUltima(ctx)
	return err == nil && tag != failed
}
