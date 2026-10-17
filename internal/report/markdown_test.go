package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/scenario"
)

// Evidence quotes helm and the cluster, so a chart controls part of it. It must not reach
// report.md as raw HTML, or split one evidence item over several lines.
func TestMarkdownEscapesEvidence(t *testing.T) {
	r := &Report{Results: []scenario.Result{{
		Scenario: "chart-upgrade",
		Verdict:  scenario.Fail,
		Findings: []scenario.Finding{{
			ID:       "HB-CU-001",
			Title:    "Upgrading fails",
			Evidence: []string{"Error: <img src=x onerror=alert(1)>\n## Not a heading"},
		}},
	}}}
	md := r.Markdown()
	if strings.Contains(md, "<img") {
		t.Errorf("raw HTML from evidence in report.md:\n%s", md)
	}
	if strings.Contains(md, "\n## Not a heading") {
		t.Errorf("evidence started a new Markdown line:\n%s", md)
	}
	if !strings.Contains(md, "- Error: &lt;img src=x onerror=alert(1)&gt; ## Not a heading\n") {
		t.Errorf("escaped evidence missing:\n%s", md)
	}
}

// A lone carriage return is a line ending in CommonMark, so a chart's fail message must not use it
// to start a heading in the report (review item 2).
func TestMarkdownEscapesCarriageReturn(t *testing.T) {
	r := &Report{Results: []scenario.Result{{
		Scenario: "chart-upgrade", Verdict: scenario.Fail,
		Findings: []scenario.Finding{{ID: "HB-CU-001", Title: "x", Evidence: []string{"bad value\r## Injected heading"}}},
	}}}
	md := r.Markdown()
	if strings.Contains(md, "\r") {
		t.Errorf("carriage return reached report.md:\n%q", md)
	}
	if strings.Contains(md, "## Injected heading") && !strings.Contains(md, "value ## Injected heading") {
		t.Errorf("carriage return started a new block:\n%s", md)
	}
}

// Every field Write persists goes through Hide, so no field leaks a secret value, URL credential or
// local path around the runner's masking (review item 1).
func TestWriteRedactsEveryField(t *testing.T) {
	runner := &execx.Runner{Secrets: []string{"setval"}, Paths: map[string]string{"/home/zztester": "~"}}
	r := &Report{
		Hide:        runner.Hide,
		Chart:       "https://u:cpw@charts.example.com/c.tgz",
		ValuesFiles: []string{"https://u:vpw@values.example.com/v.yaml", "/home/zztester/vals.yaml"},
		Commands:    []execx.Command{{Line: "helm install --set token=setval /home/zztester/chart"}},
		Results: []scenario.Result{{
			Scenario: "chart-upgrade", Verdict: scenario.Fail, Summary: "failed at /home/zztester/chart",
			Findings: []scenario.Finding{{
				ID: "HB-CU-001", Title: "x",
				Evidence:  []string{"fetch https://u:epw@e.example/a.tgz failed; chart /home/zztester/chart"},
				Reproduce: []string{"helm pull https://u:rpw@r.example/c.tgz"},
			}},
		}},
	}
	dir := t.TempDir()
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"cpw", "vpw", "epw", "rpw", "setval", "zztester"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s leaks %q:\n%s", name, leak, b)
			}
		}
	}
}

// A chart URL can carry credentials; report files are shared, the terminal is not.
func TestWriteHidesChartURLCredentials(t *testing.T) {
	dir := t.TempDir()
	r := &Report{Chart: "https://u:hunter2@charts.example.com/c-1.0.0.tgz"}
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "hunter2") {
			t.Errorf("%s contains the chart URL's password:\n%s", name, b)
		}
	}
}
