package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// Observed is what one run of a case produced.
type Observed struct {
	Exit     int
	Report   bool                  // a report.json was written
	Verdicts map[string]string     // scenario to verdict
	Findings map[string][][]string // finding ID to the evidence lines of each finding with that ID
}

// ReadReport fills in what a report.json says. A missing file leaves Report false.
func ReadReport(path string, exit int) (Observed, error) {
	obs := Observed{Exit: exit, Verdicts: map[string]string{}, Findings: map[string][][]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return obs, nil
	}
	if err != nil {
		return obs, err
	}
	var rep struct {
		Results []struct {
			Scenario string `json:"scenario"`
			Verdict  string `json:"verdict"`
			Findings []struct {
				ID       string   `json:"id"`
				Evidence []string `json:"evidence"`
			} `json:"findings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return obs, fmt.Errorf("%s: %w", path, err)
	}
	obs.Report = true
	for _, r := range rep.Results {
		obs.Verdicts[r.Scenario] = r.Verdict
		for _, f := range r.Findings {
			obs.Findings[f.ID] = append(obs.Findings[f.ID], f.Evidence)
		}
	}
	return obs, nil
}

// IDs returns the finding IDs, sorted.
func (o Observed) IDs() []string {
	ids := make([]string, 0, len(o.Findings))
	for id := range o.Findings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Signature is the outcome of a run in one line, to tell whether two runs of a case agree.
func (o Observed) Signature() string {
	var verdicts []string
	for s, v := range o.Verdicts {
		verdicts = append(verdicts, s+"="+v)
	}
	sort.Strings(verdicts)
	return fmt.Sprintf("exit %d; %s; %s", o.Exit, strings.Join(verdicts, ","), strings.Join(o.IDs(), ","))
}

// Compare lists how obs differs from what c expects. Empty means the run is as expected.
func Compare(c Case, obs Observed) []string {
	if !obs.Report {
		// A run that wrote no report is never "no findings".
		return []string{fmt.Sprintf("exit %d and no report.json", obs.Exit)}
	}
	var diff []string
	if obs.Exit != c.Exit {
		diff = append(diff, fmt.Sprintf("exit %d, want %d", obs.Exit, c.Exit))
	}
	for _, s := range sortedKeys(c.Verdicts) {
		switch got, ok := obs.Verdicts[s]; {
		case !ok:
			diff = append(diff, s+" did not run")
		case got != c.Verdicts[s]:
			diff = append(diff, fmt.Sprintf("%s is %s, want %s", s, got, c.Verdicts[s]))
		}
	}
	for _, s := range sortedKeys(obs.Verdicts) {
		if _, ok := c.Verdicts[s]; !ok {
			diff = append(diff, fmt.Sprintf("%s ran (%s) and is not in the table", s, obs.Verdicts[s]))
		}
	}
	for _, id := range sortedKeys(c.Findings) {
		found, ok := obs.Findings[id]
		if !ok {
			diff = append(diff, "missing "+id)
			continue
		}
		// Every finding with the ID has to be about the object, not only one of them.
		for _, want := range c.Findings[id] {
			for _, evidence := range found {
				if !strings.Contains(strings.Join(evidence, "\n"), want) {
					diff = append(diff, fmt.Sprintf("%s is not about %q", id, want))
					break
				}
			}
		}
	}
	for _, id := range obs.IDs() {
		if _, ok := c.Findings[id]; !ok {
			diff = append(diff, "unexpected "+id)
		}
	}
	return diff
}

// Run is the result of one run of a case.
type Run struct {
	Case     string            `json:"case"`
	Attempt  int               `json:"attempt"`
	Exit     int               `json:"exit"`
	Verdicts map[string]string `json:"verdicts"`
	Findings []string          `json:"findings"`
	Differs  []string          `json:"differs,omitempty"`
	Log      string            `json:"log"`

	signature string
}

// FindingScore says how one finding ID did across the runs.
type FindingScore struct {
	ID string `json:"id"`
	// Expected counts the runs of cases that expect the finding, Found those that gave it.
	Expected int `json:"expected"`
	Found    int `json:"found"`
	// Twins counts the runs of fixed twins of those cases that must not give it, Clean those that
	// did not.
	Twins int `json:"twins"`
	Clean int `json:"clean"`
	// Stray counts the runs of every other case that gave it.
	Stray int `json:"stray"`
	// Uncovered is the reason from the table when no pair covers the ID.
	Uncovered string `json:"uncovered,omitempty"`
}

// Scorecard is the result of a whole run of the table.
type Scorecard struct {
	Helm     string         `json:"helm"`
	K8s      string         `json:"k8s"`
	Repeat   int            `json:"repeat"`
	Runs     []Run          `json:"runs"`
	Findings []FindingScore `json:"findings"`
	// Unstable lists the cases whose runs did not agree with each other.
	Unstable []string `json:"unstable,omitempty"`
	// Coverage lists the finding IDs the table should cover and does not.
	Coverage []string `json:"coverage,omitempty"`
}

// Score builds the per-finding table from the runs. ids are all IDs to list: those in the source
// and those in the table.
func Score(t Truth, runs []Run, ids []string) ([]FindingScore, []string) {
	byName := map[string]Case{}
	for _, c := range t.Cases {
		byName[c.Name] = c
	}
	// twinOf[id] holds the pairs whose defect case expects id.
	twinOf := map[string]map[string]bool{}
	for _, c := range t.Cases {
		if c.Role == roleDefect {
			for id := range c.Findings {
				if twinOf[id] == nil {
					twinOf[id] = map[string]bool{}
				}
				twinOf[id][c.Pair] = true
			}
		}
	}
	var scores []FindingScore
	for _, id := range ids {
		s := FindingScore{ID: id, Uncovered: t.Uncovered[id]}
		for _, r := range runs {
			c := byName[r.Case]
			_, expected := c.Findings[id]
			found := slices.Contains(r.Findings, id)
			switch {
			case expected:
				s.Expected++
				if found {
					s.Found++
				}
			case c.Role == roleFixed && twinOf[id][c.Pair]:
				s.Twins++
				if !found {
					s.Clean++
				}
			case found:
				s.Stray++
			}
		}
		scores = append(scores, s)
	}

	signatures := map[string]map[string]bool{}
	for _, r := range runs {
		if signatures[r.Case] == nil {
			signatures[r.Case] = map[string]bool{}
		}
		signatures[r.Case][r.signature] = true
	}
	var unstable []string
	for name, set := range signatures {
		if len(set) > 1 {
			unstable = append(unstable, name)
		}
	}
	sort.Strings(unstable)
	return scores, unstable
}

// OK reports whether every run was as expected, every case was stable and the table covers the
// source.
func (s Scorecard) OK() bool {
	for _, r := range s.Runs {
		if len(r.Differs) > 0 {
			return false
		}
	}
	return len(s.Unstable) == 0 && len(s.Coverage) == 0
}

// Markdown renders the scorecard for a job summary.
func (s Scorecard) Markdown(t Truth) string {
	var b strings.Builder
	asExpected := 0
	for _, r := range s.Runs {
		if len(r.Differs) == 0 {
			asExpected++
		}
	}
	fmt.Fprintf(&b, "### Verdict scorecard\n\n%d of %d runs as expected", asExpected, len(s.Runs))
	if s.Repeat > 1 {
		fmt.Fprintf(&b, " (every case %d times)", s.Repeat)
	}
	fmt.Fprintf(&b, ". %s, Kubernetes %s for the matrix cases.\n\n", s.Helm, s.K8s)
	if len(s.Unstable) > 0 {
		fmt.Fprintf(&b, "**Unstable:** the runs of %s did not agree with each other.\n\n", strings.Join(s.Unstable, ", "))
	}
	for _, c := range s.Coverage {
		fmt.Fprintf(&b, "**Coverage:** %s.\n\n", c)
	}

	b.WriteString("| Finding | Found where expected | Absent in the fixed twin | Seen elsewhere |\n|---|---|---|---|\n")
	for _, f := range s.Findings {
		if f.Uncovered != "" {
			fmt.Fprintf(&b, "| %s | no pair yet: %s | | %d |\n", f.ID, f.Uncovered, f.Stray)
			continue
		}
		fmt.Fprintf(&b, "| %s | %d of %d | %d of %d | %d |\n", f.ID, f.Found, f.Expected, f.Clean, f.Twins, f.Stray)
	}

	role := map[string]string{}
	for _, c := range t.Cases {
		role[c.Name] = c.Role
	}
	b.WriteString("\n| Case | Role | Exit | Findings | Result |\n|---|---|---|---|---|\n")
	for _, r := range s.Runs {
		name := r.Case
		if s.Repeat > 1 {
			name = fmt.Sprintf("%s (run %d)", r.Case, r.Attempt)
		}
		result := "as expected"
		if len(r.Differs) > 0 {
			result = "**differs:** " + strings.Join(r.Differs, "; ")
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", name, role[r.Case], r.Exit, strings.Join(r.Findings, " "), result)
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
