//go:build !windows

package runtime

import "os/exec"

func hideWindow(*exec.Cmd) {}
