//go:build !unix

package execx

import "os/exec"

func stopAsGroup(cmd *exec.Cmd) { cmd.WaitDelay = killDelay }

func killGroup(*exec.Cmd) {}
