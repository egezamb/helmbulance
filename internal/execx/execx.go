// Package execx runs external commands (helm, kind, kubectl) and keeps a transcript of them, so a
// report can show exactly what was run. Values passed with --set are masked in the transcript.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Command is one executed command as it appears in a report.
type Command struct {
	Line     string        `json:"line"`
	Duration time.Duration `json:"duration"`
	ExitCode int           `json:"exitCode"`
}

// killDelay is how long a command gets to stop after SIGTERM before it and its children are
// killed. A variable, so tests can shorten it.
var killDelay = 30 * time.Second

// Runner executes commands with a fixed environment and records each one.
type Runner struct {
	Env []string
	// Secrets are values (from --set) that are replaced with *** in error text, since helm can
	// quote them in schema errors and chart fail messages.
	Secrets []string

	mu         sync.Mutex
	transcript []Command
}

// Run executes name with args and returns its stdout. On failure the error includes stderr.
func (r *Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, _, err := r.RunCapture(ctx, name, args...)
	return out, err
}

// RunCapture is Run that also returns stderr, for commands whose warnings matter even when they
// succeed (API deprecation warnings from helm install, for example).
func (r *Runner) RunCapture(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = r.Env
	stopAsGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		killGroup(cmd)
	}
	rec := Command{Line: Mask(name, args), Duration: time.Since(start).Round(time.Millisecond)}
	if cmd.ProcessState != nil {
		rec.ExitCode = cmd.ProcessState.ExitCode()
	}
	r.mu.Lock()
	r.transcript = append(r.transcript, rec)
	r.mu.Unlock()

	errText := []byte(Redact(stderr.String(), r.Secrets))
	if err != nil {
		return stdout.Bytes(), errText, fmt.Errorf("%s: %w: %s", rec.Line, err, r.errorText(stderr.String()))
	}
	return stdout.Bytes(), errText, nil
}

// errorText keeps helm's last "Error: " line, or all of stderr when there is none, with the
// secrets redacted. The "Error: " marker is cut off before redacting, so a value such as
// logLevel=Error cannot hide it from the code that looks for it.
func (r *Runner) errorText(stderr string) string {
	const marker = "Error: "
	stderr = strings.TrimSpace(stderr)
	if i := strings.LastIndex(stderr, marker); i >= 0 && (i == 0 || stderr[i-1] == '\n') {
		return marker + Redact(stderr[i+len(marker):], r.Secrets)
	}
	return Redact(stderr, r.Secrets)
}

// Transcript returns the commands run so far.
func (r *Runner) Transcript() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Command(nil), r.transcript...)
}

// Mask renders a command line with the values of --set style flags replaced, because they often
// carry passwords.
func Mask(name string, args []string) string {
	out := []string{name}
	flag := ""
	for _, a := range args {
		switch {
		case flag != "":
			out = append(out, maskValue(flag, a))
			flag = ""
		case isSetFlag(a):
			out = append(out, a)
			flag = a
		case strings.HasPrefix(a, "--set") && strings.Contains(a, "="):
			f, value, _ := strings.Cut(a, "=")
			out = append(out, f+"="+maskValue(f, value))
		default:
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}

func isSetFlag(a string) bool {
	return a == "--set" || a == "--set-string" || a == "--set-file" || a == "--set-json" || a == "--set-literal"
}

// maskValue turns "a=1,b.c=2" into "a=***,b.c=***". --set-literal and --set-json values are one
// assignment whatever they contain, so everything after the first key is masked.
func maskValue(flag, s string) string {
	switch flag {
	case "--set-json":
		// A value that starts with '{' is one JSON object, not key=value.
		if strings.HasPrefix(strings.TrimSpace(s), "{") {
			return "***"
		}
		if key, _, ok := cutAssignment(s); ok {
			return key + "=***"
		}
		return "***"
	case "--set-literal":
		// Helm reads a --set-literal key up to the first '=', with no escapes.
		if key, _, ok := strings.Cut(s, "="); ok {
			return key + "=***"
		}
		return "***"
	}
	parts := splitAssignments(s)
	for i, p := range parts {
		if key, _, ok := cutAssignment(p); ok {
			parts[i] = key + "=***"
		} else {
			parts[i] = "***"
		}
	}
	return strings.Join(parts, ",")
}
