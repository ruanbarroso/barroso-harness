//go:build !windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type installResult struct {
	path     string
	commands []string
}

func executablePath() (string, error) {
	name, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return filepath.Abs(resolved)
}

func installBinary(staged string) (installResult, error) {
	target, err := executablePath()
	if err != nil {
		return installResult{}, err
	}
	return installAt(target, staged)
}

// installAt is factored out so tests can replace only files inside TempDir.
func installAt(target, staged string) (installResult, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return installResult{}, err
	}
	staged, err = filepath.Abs(staged)
	if err != nil {
		return installResult{}, err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".barroso-harness-update-*")
	if err != nil {
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("create staging next to the binary: %w", err)
	}
	tmpName := tmp.Name()
	defer removeIfExists(tmpName)

	in, err := os.Open(staged)
	if err != nil {
		_ = tmp.Close()
		return installResult{}, err
	}
	_, copyErr := tmp.ReadFrom(in)
	closeInErr := in.Close()
	if copyErr != nil {
		_ = tmp.Close()
		if isPermissionError(copyErr) {
			return permissionFallback(staged, target)
		}
		return installResult{}, copyErr
	}
	if closeInErr != nil {
		_ = tmp.Close()
		return installResult{}, closeInErr
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return installResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return installResult{}, err
	}

	backup := target + ".old"
	for i := 1; ; i++ {
		if _, err := os.Lstat(backup); os.IsNotExist(err) {
			break
		}
		backup = fmt.Sprintf("%s.old-%d", target, i)
	}
	if err := os.Rename(target, backup); err != nil && !os.IsNotExist(err) {
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("back up previous binary: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Rename(backup, target)
		if isPermissionError(err) {
			return permissionFallback(staged, target)
		}
		return installResult{}, fmt.Errorf("install new binary: %w", err)
	}
	return installResult{path: target}, nil
}

// manualDir holds the copy the printed sudo commands install. It is apart
// from the download staging on purpose: the callers remove the staging file
// once installation returns, and the copy must outlive that. A var so tests
// point it at a TempDir.
var manualDir = func() string { return filepath.Join(stagingRoot(), "manual") }

// permissionFallback copies staged where the user can reach it and returns the
// sudo commands that finish the installation. The error is never nil: nothing
// was installed yet.
func permissionFallback(staged, target string) (installResult, error) {
	dir := manualDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return installResult{}, err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(staged)+"-*")
	if err != nil {
		return installResult{}, err
	}
	fallback := tmp.Name()
	if err := tmp.Close(); err != nil {
		removeIfExists(fallback)
		return installResult{}, err
	}
	if err := copyFile(staged, fallback); err != nil {
		removeIfExists(fallback)
		return installResult{}, err
	}
	alias := filepath.Join(filepath.Dir(target), "bh")
	return installResult{path: fallback, commands: []string{
		fmt.Sprintf("sudo install -m 0755 %q %q", fallback, target),
		fmt.Sprintf("sudo ln -sf %q %q", target, alias),
	}}, errPermissaoManual
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_TRUNC on the source itself would empty it before the copy reads a byte.
	srcInfo, err := in.Stat()
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
		return fmt.Errorf("copy %s onto itself", src)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

func verifyStaged(staged, version string) error {
	cmd := exec.Command(staged, "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run staged binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return validateVersionOutput(string(output), version)
}

func installCommands(result installResult) string { return strings.Join(result.commands, "\n") }
