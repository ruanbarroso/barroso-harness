//go:build !windows

package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

// treeAttrs puts a hook in its own process group, so killing -pgid reaches
// every descendant (a grandchild holding stdout included) and never the agent.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// procTree is the hook's process group; the group outlives its leader, so
// it is still reachable after the hook exits.
type procTree struct{}

func newProcTree() *procTree { return &procTree{} }

func (t *procTree) add(*os.Process) {}

// kill ends the process group led by p.
func (t *procTree) kill(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		_ = p.Kill()
	}
}

func (t *procTree) close() {}
