package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Roles of a case.
const (
	roleDefect     = "defect"     // the chart has the defect its findings name
	roleFixed      = "fixed"      // the twin of a defect case, without that defect
	roleIncomplete = "incomplete" // the run cannot finish; the right verdict is INCONCLUSIVE
)

// Truth is the table of cases with a known right answer, and the finding IDs no pair covers yet.
type Truth struct {
	// Uncovered maps a finding ID that has no defect case with a fixed twin to the reason. The
	// coverage check fails for any other uncovered ID, and for a listed ID that is covered, so the
	// list can only shrink.
	Uncovered map[string]string `yaml:"uncovered"`
	Cases     []Case            `yaml:"cases"`
}

// Case is one run of helmbulance whose right answer is known.
type Case struct {
	Name string `yaml:"name"`
	// Pair is shared by a defect case and its fixed twins. Incomplete cases have none.
	Pair    string   `yaml:"pair"`
	Role    string   `yaml:"role"`
	Command string   `yaml:"command"` // test, scan or matrix
	Args    []string `yaml:"args"`    // {key} is a random key, {k8s} the Kubernetes version
	Exit    int      `yaml:"exit"`
	// Verdicts is the verdict of every scenario in the report. {k8s} works in the names too.
	Verdicts map[string]string `yaml:"verdicts"`
	// Findings maps each expected finding ID to texts that its evidence must contain, so a finding
	// has to be about the right object and not only of the right kind.
	Findings map[string][]string `yaml:"findings"`
}

// LoadTruth reads and validates the table.
func LoadTruth(path string) (Truth, error) {
	var t Truth
	f, err := os.Open(path)
	if err != nil {
		return t, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("%s: %w", path, err)
	}
	if problems := t.Validate(); len(problems) > 0 {
		return t, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	return t, nil
}

// Validate lists what is wrong with the table itself.
func (t Truth) Validate() []string {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	seen := map[string]bool{}
	defects, fixed := map[string][]Case{}, map[string][]Case{}
	for _, c := range t.Cases {
		switch {
		case c.Name == "":
			bad("a case has no name")
			continue
		case seen[c.Name]:
			bad("%s: listed twice", c.Name)
		}
		seen[c.Name] = true
		if !slices.Contains([]string{"test", "scan", "matrix"}, c.Command) {
			bad("%s: command %q is not test, scan or matrix", c.Name, c.Command)
		}
		if len(c.Args) == 0 {
			bad("%s: no args", c.Name)
		}
		if len(c.Verdicts) == 0 {
			bad("%s: no verdicts", c.Name)
		}
		switch c.Role {
		case roleDefect:
			defects[c.Pair] = append(defects[c.Pair], c)
			if len(c.Findings) == 0 {
				bad("%s: a defect case expects at least one finding", c.Name)
			}
		case roleFixed:
			fixed[c.Pair] = append(fixed[c.Pair], c)
		case roleIncomplete:
			if c.Pair != "" || len(c.Findings) > 0 || c.Exit != 3 {
				bad("%s: an incomplete case has no pair, no findings and exit 3", c.Name)
			}
		default:
			bad("%s: role %q is not defect, fixed or incomplete", c.Name, c.Role)
		}
		if c.Role != roleIncomplete && c.Pair == "" {
			bad("%s: no pair", c.Name)
		}
	}
	for pair, d := range defects {
		if pair == "" {
			continue
		}
		if len(d) != 1 {
			bad("pair %s: %d defect cases, want one", pair, len(d))
		}
		if len(fixed[pair]) == 0 {
			bad("pair %s: no fixed twin", pair)
		}
	}
	for pair, f := range fixed {
		if pair != "" && len(defects[pair]) == 0 {
			bad("pair %s: %s has no defect case", pair, f[0].Name)
		}
	}
	sort.Strings(problems)
	return problems
}

// Covered returns the finding IDs that a pair tells apart: the defect case expects the finding and
// at least one of its fixed twins does not.
func (t Truth) Covered() map[string]bool {
	covered := map[string]bool{}
	for _, d := range t.Cases {
		if d.Role != roleDefect {
			continue
		}
		for id := range d.Findings {
			for _, f := range t.Cases {
				if _, has := f.Findings[id]; f.Role == roleFixed && f.Pair == d.Pair && !has {
					covered[id] = true
				}
			}
		}
	}
	return covered
}

// Coverage checks the table against the finding IDs the source can emit. Every ID needs a pair
// that tells it apart, or an entry under uncovered with the reason.
func (t Truth) Coverage(sourceIDs []string) []string {
	var problems []string
	covered := t.Covered()
	for _, id := range sourceIDs {
		_, listed := t.Uncovered[id]
		switch {
		case !covered[id] && !listed:
			problems = append(problems, id+" has no defect case with a fixed twin; add a pair, or list it under uncovered with the reason")
		case covered[id] && listed:
			problems = append(problems, id+" is covered now; remove it from uncovered")
		}
	}
	known := func(id string) bool { return slices.Contains(sourceIDs, id) }
	for id := range t.Uncovered {
		if !known(id) {
			problems = append(problems, id+" is listed under uncovered but is not in the source")
		}
	}
	for _, c := range t.Cases {
		for id := range c.Findings {
			if !known(id) {
				problems = append(problems, c.Name+" expects "+id+", which is not in the source")
			}
		}
	}
	sort.Strings(problems)
	return slices.Compact(problems)
}

var findingID = regexp.MustCompile(`^HB-[A-Z0-9]+-[0-9]{3}$`)

// SourceIDs returns every finding ID that appears as a string literal in the Go files under dir,
// tests left out.
func SourceIDs(dir string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && findingID.MatchString(s) {
					seen[s] = true
				}
			}
			return true
		})
		return nil
	})
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, err
}
