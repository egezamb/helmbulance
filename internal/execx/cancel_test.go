//go:build unix

package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gone waits briefly for pid to disappear; a killed child is a zombie until its new parent reaps it.
func gone(pid int) bool {
	for range 40 {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func childPID(t *testing.T, file string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// A helm that is a wrapper script, or runs a plugin, leaves children that hold its output open.
// The deadline must stop all of them, and let helm handle SIGTERM first.
func TestRunStopsTheWholeCommandAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	term, pidFile := filepath.Join(dir, "term"), filepath.Join(dir, "pid")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := (&Runner{}).Run(ctx, "/bin/sh", "-c",
		`trap 'echo term > "$1"; exit 1' TERM; sleep 10 & echo $! > "$2"; wait`, "sh", term, pidFile)
	if err == nil {
		t.Fatal("want an error after the deadline")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want soon after the 300ms deadline", d)
	}
	if b, _ := os.ReadFile(term); strings.TrimSpace(string(b)) != "term" {
		t.Error("the command did not get SIGTERM")
	}
	if pid := childPID(t, pidFile); !gone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child %d still runs after the deadline", pid)
	}
}

// A child that ignores SIGTERM is killed once killDelay has passed.
func TestRunKillsAChildThatIgnoresSIGTERM(t *testing.T) {
	old := killDelay
	killDelay = 500 * time.Millisecond
	defer func() { killDelay = old }()

	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := (&Runner{}).Run(ctx, "/bin/sh", "-c",
		`(trap '' TERM; exec sleep 10) & echo $! > "$1"; wait`, "sh", pidFile)
	if err == nil {
		t.Fatal("want an error after the deadline")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want soon after deadline plus killDelay", d)
	}
	if pid := childPID(t, pidFile); !gone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child %d that ignores SIGTERM still runs", pid)
	}
}
