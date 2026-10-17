// Package report writes the results of a run as JSON (stable, for machines) and Markdown (for
// people). Neither ever contains a Secret value.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/scenario"
)

// SchemaVersion of the JSON report. Bump it on incompatible changes.
const SchemaVersion = 1

// Report is everything a run produced.
type Report struct {
	SchemaVersion int      `json:"schemaVersion"`
	Chart         string   `json:"chart"`
	ValuesFiles   []string `json:"valuesFiles"`
	SetKeys       []string `json:"setKeys"` // keys only, never values
	// ExpectedRotation is what --expected-rotation allowed, so a reader sees what was not reported.
	ExpectedRotation []string          `json:"expectedRotation,omitempty"`
	HelmVersion      string            `json:"helmVersion"`
	Cluster          string            `json:"cluster"`
	Started          time.Time         `json:"started"`
	Duration         string            `json:"duration"`
	Results          []scenario.Result `json:"results"`
	Commands         []execx.Command   `json:"commands"`
}

// Worst returns the overall verdict: FAIL beats INCONCLUSIVE beats PASS. SKIPPED counts for
// nothing, so a run in which every scenario was skipped tested nothing and is INCONCLUSIVE.
func (r *Report) Worst() string {
	passed, inconclusive := false, false
	for _, res := range r.Results {
		switch res.Verdict {
		case scenario.Fail:
			return scenario.Fail
		case scenario.Inconclusive:
			inconclusive = true
		case scenario.Pass:
			passed = true
		}
	}
	if inconclusive || !passed {
		return scenario.Inconclusive
	}
	return scenario.Pass
}

// Write saves report.json and report.md in dir.
func (r *Report) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644)
}

// Markdown renders the report for people.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Helmbulance report: %s\n\n", r.Worst())
	fmt.Fprintf(&b, "- Chart: `%s`\n", r.Chart)
	if len(r.ValuesFiles) > 0 {
		fmt.Fprintf(&b, "- Values files: `%s`\n", strings.Join(r.ValuesFiles, "`, `"))
	}
	if len(r.SetKeys) > 0 {
		fmt.Fprintf(&b, "- Install-time `--set` keys: `%s`\n", strings.Join(r.SetKeys, "`, `"))
	}
	if len(r.ExpectedRotation) > 0 {
		fmt.Fprintf(&b, "- Expected rotations (not reported): `%s`\n", strings.Join(r.ExpectedRotation, "`, `"))
	}
	fmt.Fprintf(&b, "- Helm: %s\n- Sandbox: kind cluster `%s`\n- Duration: %s\n\n", r.HelmVersion, r.Cluster, r.Duration)

	b.WriteString("| Scenario | Verdict | Summary |\n|---|---|---|\n")
	for _, res := range r.Results {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", res.Scenario, res.Verdict, escape(res.Summary))
	}

	for _, res := range r.Results {
		for _, f := range res.Findings {
			fmt.Fprintf(&b, "\n## %s: %s\n\n", f.ID, escape(f.Title))
			fmt.Fprintf(&b, "**Severity:** %s · **Scenario:** `%s`\n\n", f.Severity, res.Scenario)
			b.WriteString("**Evidence**\n\n")
			for _, e := range f.Evidence {
				fmt.Fprintf(&b, "- %s\n", e)
			}
			fmt.Fprintf(&b, "\n**Impact.** %s\n\n**Fix.** %s\n\n**Reproduce**\n\n```sh\n", f.Impact, f.Fix)
			for _, c := range f.Reproduce {
				b.WriteString(c + "\n")
			}
			b.WriteString("```\n")
		}
	}
	return b.String()
}

// escape keeps text from the cluster from breaking a Markdown table or injecting markup.
func escape(s string) string {
	s = strings.NewReplacer("|", "\\|", "\n", " ", "<", "&lt;", ">", "&gt;").Replace(s)
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}
