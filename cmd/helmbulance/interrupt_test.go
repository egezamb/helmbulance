//go:build unix

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The first Ctrl-C (or SIGTERM) cancels the run so cleanup can start; a second one must quit at
// once, since cleanup can take minutes. Signals go to the whole test binary, so this runs in a
// child process.
func TestInterruptContext(t *testing.T) {
	signals := map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM}
	if name := os.Getenv("HB_INTERRUPT_CHILD"); name != "" {
		signal := signals[name]
		ctx, cancel := interruptContext()
		defer cancel()
		_ = syscall.Kill(os.Getpid(), signal)
		select {
		case <-ctx.Done():
			os.Stdout.WriteString("cancelled\n")
		case <-time.After(5 * time.Second):
			os.Exit(3) // the first Ctrl-C did not cancel
		}
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), signal)
		time.Sleep(5 * time.Second)
		os.Exit(4) // the second signal did not quit
	}

	for name, signal := range signals {
		cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptContext$")
		cmd.Env = append(os.Environ(), "HB_INTERRUPT_CHILD="+name)
		out, err := cmd.Output()
		ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		// Without the handler the first signal would kill the child too, so check it cancelled first.
		if !strings.Contains(string(out), "cancelled") || !ok || !ws.Signaled() || ws.Signal() != signal {
			t.Errorf("%s: child ended with %v, output %q; want the first %s to cancel and the second to kill (exit 3: first did not cancel, 4: second did not quit)", name, err, out, name)
		}
	}
}
