//go:build !windows

package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTree creates an "installed binary" and a "staged" file in a TempDir, so
// installation swaps fake files and never the binary running the tests.
func fakeTree(t *testing.T) (target, staged string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "barroso-harness")
	staged = filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("old-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	return target, staged
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// The happy path: the staged file becomes the target and the previous one is
// kept as .old, so a failed update can be rolled back.
func TestInstallUnixReplacesAndKeepsPrevious(t *testing.T) {
	target, staged := fakeTree(t)

	result, err := installAt(target, staged)
	if err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if result.path != target {
		t.Fatalf("installed path %q, want %q", result.path, target)
	}
	if want := "v0.1.0-bh.9"; readFile(t, target) != want {
		t.Fatalf("target has %q, want %q", readFile(t, target), want)
	}
	// The previous one remains recoverable.
	kept := target + ".old"
	if want := "old-version"; readFile(t, kept) != want {
		t.Fatalf("backup %q has %q, want %q", kept, readFile(t, kept), want)
	}
	// Unix installs by copying into a temporary file before the atomic rename.
	if got := readFile(t, staged); got != "v0.1.0-bh.9" {
		t.Fatalf("staged has %q after install, want original contents", got)
	}
}

// The installed binary must be executable: 0755, not the download's 0700.
func TestInstallUnixLeavesBinaryExecutable(t *testing.T) {
	target, staged := fakeTree(t)
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", mode)
	}
}

// Staging happens in the target's own directory: that is what makes the
// final rename atomic. If it were done in /tmp, a cross-filesystem rename
// would fail with EXDEV — and the target would be left without a binary.
func TestInstallUnixLeavesNoStagingBehind(t *testing.T) {
	target, staged := fakeTree(t)
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".barroso-harness-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("staging was left behind: %v", leftovers)
	}
}

// A second /update does not overwrite the existing backup: the first .old is
// still what rolling back would restore.
func TestInstallUnixDoesNotOverwriteExistingBackup(t *testing.T) {
	target, staged := fakeTree(t)
	previous := target + ".old"
	if err := os.WriteFile(previous, []byte("original-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v0.1.0-bh.2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installAt(target, staged); err != nil {
		t.Fatalf("installAt: %v", err)
	}
	if readFile(t, previous) != "original-backup" {
		t.Fatalf("original backup was overwritten: %q", readFile(t, previous))
	}
	// This install's previous version went to the next free slot.
	if readFile(t, target+".old-1") != "old-version" {
		t.Fatalf("expected the previous one in .old-1, got %q", readFile(t, target+".old-1"))
	}
}

// useManualDir points the sudo fallback's copy at a TempDir for the test.
func useManualDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := manualDir
	manualDir = func() string { return dir }
	t.Cleanup(func() { manualDir = prev })
	return dir
}

// Without write permission on the target's directory (a root-owned binary in
// /usr/local/bin), the installer does not give up: it returns ready-made sudo
// commands — and an error, because nothing was installed yet.
func TestInstallUnixWithoutPermissionReturnsSudoCommands(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: everything is writable, the EACCES path never happens")
	}
	useManualDir(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "barroso-harness")
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Read-only directory: creating the staging file next to the target fails with EACCES.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	result, err := installAt(target, staged)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("without permission it should return a permission error, got %v", err)
	}
	if readFile(t, target) != "old" {
		t.Fatalf("target changed without permission: %q", readFile(t, target))
	}
	if len(result.commands) == 0 {
		t.Fatalf("error without actionable commands: %v", err)
	}
	if got := readFile(t, result.path); got != "new" {
		t.Fatalf("fallback copy %q has %q, want the staged contents", result.path, got)
	}
	info, statErr := os.Stat(result.path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("fallback copy is not executable: %v", info.Mode())
	}
	commands := strings.Join(result.commands, "\n")
	if !strings.Contains(commands, "sudo install -m 0755") {
		t.Fatalf("missing the install command: %q", commands)
	}
	if !strings.Contains(commands, "sudo ln -sf") {
		t.Fatalf("missing the bh shortcut: %q", commands)
	}
	if !strings.Contains(commands, result.path) || !strings.Contains(commands, target) {
		t.Fatalf("commands should mention the copy %q and the target %q: %q", result.path, target, commands)
	}
}

// A fresh download is staged in the update cache under the asset's name. The
// fallback copy must neither truncate that file (copying it onto itself) nor
// be the file the caller removes once installation returns.
func TestInstallUnixFallbackSurvivesStagingRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: everything is writable, the EACCES path never happens")
	}
	cache := useManualDir(t)
	staged := filepath.Join(cache, AssetPlataformaNome())
	if err := os.WriteFile(staged, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "barroso-harness")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	result, err := installAt(target, staged)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("want a permission error, got %v", err)
	}
	if readFile(t, staged) != "new" {
		t.Fatalf("the fallback truncated the staged file: %q", readFile(t, staged))
	}
	if result.path == staged {
		t.Fatalf("fallback copy is the staged file itself: %q", result.path)
	}
	removeIfExists(staged)
	if got := readFile(t, result.path); got != "new" {
		t.Fatalf("fallback copy %q has %q after the staging was removed", result.path, got)
	}
}

// copyFile onto the same file would truncate it before reading.
func TestCopyFileRefusesSameFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(name, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(name, name); err == nil {
		t.Fatal("copying a file onto itself should error")
	}
	if readFile(t, name) != "data" {
		t.Fatalf("source was truncated: %q", readFile(t, name))
	}
}

// The `bh` shortcut is a symlink to the binary. EvalSymlinks resolves the
// real target, so updating through the symlink replaces the actual binary and
// the shortcut keeps working — which is what this test pins.
func TestInstallUnixResolvesShortcutSymlink(t *testing.T) {
	// TempDir may itself sit behind a symlink (/var → /private/var on macOS).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "barroso-harness-real")
	shortcut := filepath.Join(dir, "bh")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, shortcut); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(shortcut)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolved != real {
		t.Fatalf("symlink resolved to %q, want %q", resolved, real)
	}
}
