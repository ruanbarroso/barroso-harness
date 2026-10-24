//go:build !windows

package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

// treeAttrs puts a hook in its own process group, so killing -pgid reaches
// every descendant (a grandchild holding stdout included) and never the agent.
// Local copy of internal/tools/proctree_other.go.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree ends the process group led by pid.
func killTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}
