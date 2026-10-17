//go:build unix

package execx

import (
	"os/exec"
	"syscall"
)

// stopAsGroup runs cmd in its own process group, so a deadline reaches the children of a wrapper
// script or a helm plugin too. The group gets SIGTERM first, so helm can mark the release failed
// instead of leaving it pending; after killDelay the command's pipes are closed.
func stopAsGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = killDelay
}

// killGroup kills whatever is left of the command's group after a cancelled run. The group ID
// cannot be reused while any of its members is alive, so this reaches only our children.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
