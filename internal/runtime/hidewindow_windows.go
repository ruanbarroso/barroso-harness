//go:build windows

package runtime

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console hook from flashing a window over the TUI.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
