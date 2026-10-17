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
	"unicode/utf8"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/scenario"
)

// SchemaVersion of the JSON report. Bump it on incompatible changes.
const SchemaVersion = 1

// Report is everything a run produced.
type Report struct {
	SchemaVersion int               `json:"schemaVersion"`
	Chart         string            `json:"chart"`
	ValuesFiles   []string          `json:"valuesFiles"`
	SetKeys       []string          `json:"setKeys"` // keys only, never values
	HelmVersion   string            `json:"helmVersion"`
	Cluster       string            `json:"cluster"`
	Started       time.Time         `json:"started"`
	Duration      string            `json:"duration"`
	Results       []scenario.Result `json:"results"`
	Commands      []execx.Command   `json:"commands"`

	// Hide masks secret values, URL credentials and local paths. Write applies it to every field,
	// so nothing reaches report.json or report.md around the runner's masking. It is idempotent.
	Hide func(string) string `json:"-"`
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
	// Report files are shared. Mask credentials, local paths and secret values in one place, so no
	// field reaches either file around the runner's masking.
	r.redact()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644)
}

// redact masks every text field of the report. With no Hide func it falls back to redacting the
// chart's URL, as before. Idempotent, so transcript text the runner already masked is unchanged.
func (r *Report) redact() {
	hide := r.Hide
	if hide == nil {
		r.Chart = execx.RedactURLs(r.Chart)
		return
	}
	r.Chart = hide(r.Chart)
	for i, v := range r.ValuesFiles {
		r.ValuesFiles[i] = hide(v)
	}
	for i := range r.Commands {
		r.Commands[i].Line = hide(r.Commands[i].Line)
	}
	for i := range r.Results {
		res := &r.Results[i]
		res.Summary = hide(res.Summary)
		for j := range res.Findings {
			f := &res.Findings[j]
			f.Title, f.Impact, f.Fix = hide(f.Title), hide(f.Impact), hide(f.Fix)
			for k, e := range f.Evidence {
				f.Evidence[k] = hide(e)
			}
			for k, c := range f.Reproduce {
				f.Reproduce[k] = hide(c)
			}
		}
	}
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
				fmt.Fprintf(&b, "- %s\n", escape(e))
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

// escape keeps text from the cluster from breaking a Markdown table or injecting markup. A lone
// carriage return is a line ending in CommonMark too, so it is turned into a space like a newline;
// otherwise a chart's `fail "x\r## Heading"` would start a heading in the report.
func escape(s string) string {
	s = strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;").Replace(s)
	if len(s) > 500 {
		s = s[:500]
		// Do not cut a multi-byte rune in half, which would leave invalid UTF-8.
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}
