//go:build windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// treeAttrs puts a hook in its own process group, so killing the tree does
// not reach the agent. Local copy of internal/tools/proctree_windows.go.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killTree ends pid and every descendant.
func killTree(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if k.Run() != nil {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}
