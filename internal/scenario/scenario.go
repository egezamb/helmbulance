// Package scenario defines what a scenario is and what it reports.
package scenario

import (
	"context"
	"io"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/helm"
	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/oracle"
)

// Verdicts. A scenario that cannot finish (install fails, timeout) is Inconclusive, never Fail:
// a finding has to be reproduced, not inferred from a broken run.
const (
	Pass         = "PASS"
	Fail         = "FAIL"
	Inconclusive = "INCONCLUSIVE"
	// Skipped: the chart has nothing this scenario tests (no HTTP Service, say). Not a failure.
	Skipped = "SKIPPED"
)

// Env is what a scenario gets to work with.
type Env struct {
	Runner *execx.Runner
	Helm   *helm.Client
	Kube   *kube.Client
	Hasher *oracle.Hasher
	Chart  string
	Values helm.Values

	// From is the release the chart-upgrade scenario starts from: a chart reference (repo/chart,
	// oci://… or a path) and a version. Empty From means "the same chart reference as Chart".
	From        string
	FromVersion string

	// TrafficTarget ("service:port") and TrafficPath are where zero-downtime sends its requests.
	// Empty picks the Service port that looks most like HTTP, and "/".
	TrafficTarget string
	TrafficPath   string

	// ExpectedRotation lists Secret keys ("secret/key", globs allowed) that the chart changes on
	// purpose, such as a webhook certificate it reissues. They are named, not reported.
	ExpectedRotation []string
	Log              io.Writer
}

// Finding is one reproduced problem.
type Finding struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Severity  string   `json:"severity"`
	Evidence  []string `json:"evidence"`
	Impact    string   `json:"impact"`
	Fix       string   `json:"fix"`
	Reproduce []string `json:"reproduce"`
}

// Result is the outcome of one scenario.
type Result struct {
	Scenario string    `json:"scenario"`
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings,omitempty"`
}

// Scenario is one day-two situation tested against a live release.
type Scenario interface {
	Name() string
	Description() string
	Run(ctx context.Context, env *Env) Result
}
