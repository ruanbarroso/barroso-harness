//go:build windows

package runtime

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeAttrs puts a hook in its own process group, so a console signal to the
// hook never reaches the agent.
func treeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// procTree holds a hook and every descendant in a Job Object. taskkill /T
// walks parent PIDs, so it cannot find a grandchild once the hook itself has
// exited; a job keeps the whole tree reachable (and KILL_ON_JOB_CLOSE ends
// whatever is left when the tree is closed).
type procTree struct{ job windows.Handle }

func newProcTree() *procTree {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return &procTree{}
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return &procTree{}
	}
	return &procTree{job: job}
}

// add puts a started process in the tree. Descendants it starts afterwards
// join the job automatically.
func (t *procTree) add(p *os.Process) {
	if t.job == 0 || p == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(t.job, h)
}

// kill ends the process and every descendant.
func (t *procTree) kill(p *os.Process) {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
	if p != nil {
		_ = p.Kill()
	}
}

// close releases the job, ending anything still in it.
func (t *procTree) close() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}
