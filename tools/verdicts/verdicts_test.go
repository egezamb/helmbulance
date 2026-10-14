package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for helmbulance: with VERDICTS_FAKE set it acts as the
// fake below and never runs the tests.
func TestMain(m *testing.M) {
	if dir := os.Getenv("VERDICTS_FAKE"); dir != "" {
		os.Exit(fakeHelmbulance(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeFinding struct {
	ID       string   `json:"id"`
	Evidence []string `json:"evidence"`
}

type fakeResult struct {
	Scenario string        `json:"scenario"`
	Verdict  string        `json:"verdict"`
	Findings []fakeFinding `json:"findings"`
}

// fakeHelmbulance answers by the name of the chart. It appends its arguments to dir/calls, and
// counts the calls per chart so that "flaky" can answer differently the second time.
func fakeHelmbulance(dir string, args []string) int {
	calls, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 2
	}
	calls.WriteString(strings.Join(args, " ") + "\n")
	calls.Close()

	chart := filepath.Base(args[1])
	counter := filepath.Join(dir, chart+".count")
	data, _ := os.ReadFile(counter)
	n, _ := strconv.Atoi(string(data))
	n++
	os.WriteFile(counter, []byte(strconv.Itoa(n)), 0o644)

	defect := fakeResult{Scenario: "s", Verdict: "FAIL", Findings: []fakeFinding{{ID: "HB-T-001", Evidence: []string{"Secret/a changed"}}}}
	clean := fakeResult{Scenario: "s", Verdict: "PASS"}
	var result fakeResult
	exit := 0
	switch chart {
	case "defect":
		result, exit = defect, 1
	case "fixed":
		result = clean
	case "flaky":
		result = clean
		if n > 1 {
			result, exit = defect, 1
		}
	case "stuck":
		result, exit = fakeResult{Scenario: "s", Verdict: "INCONCLUSIVE"}, 3
	default: // "silent": an error before any report
		return 2
	}

	reportDir := args[slices.Index(args, "--report-dir")+1]
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return 2
	}
	report, _ := json.Marshal(map[string]any{"results": []fakeResult{result}})
	if err := os.WriteFile(filepath.Join(reportDir, "report.json"), report, 0o644); err != nil {
		return 2
	}
	return exit
}

func TestCompare(t *testing.T) {
	defect := Case{
		Name:     "defect",
		Exit:     1,
		Verdicts: map[string]string{"s": "FAIL"},
		Findings: map[string][]string{"HB-T-001": {"Secret/a"}},
	}
	fixed := Case{Name: "fixed", Exit: 0, Verdicts: map[string]string{"s": "PASS"}}
	report := func(exit int, verdict string, findings map[string][][]string) Observed {
		if findings == nil {
			findings = map[string][][]string{}
		}
		return Observed{Exit: exit, Report: true, Verdicts: map[string]string{"s": verdict}, Findings: findings}
	}
	found := map[string][][]string{"HB-T-001": {{"Secret/a changed"}}}

	tests := []struct {
		name string
		c    Case
		obs  Observed
		want []string
	}{
		{"defect found", defect, report(1, "FAIL", found), nil},
		{"fixed twin clean", fixed, report(0, "PASS", nil), nil},
		{"finding missed", defect, report(0, "PASS", nil), []string{"exit 0, want 1", "s is PASS, want FAIL", "missing HB-T-001"}},
		{
			// Another finding keeps the exit code at 1; only the ID set shows the miss.
			"finding missed behind another", defect,
			report(1, "FAIL", map[string][][]string{"HB-T-002": {{"x"}}}),
			[]string{"missing HB-T-001", "unexpected HB-T-002"},
		},
		{"false positive on the twin", fixed, report(1, "FAIL", found), []string{"exit 1, want 0", "s is FAIL, want PASS", "unexpected HB-T-001"}},
		{
			"finding about another object", defect,
			report(1, "FAIL", map[string][][]string{"HB-T-001": {{"Secret/b changed"}}}),
			[]string{`HB-T-001 is not about "Secret/a"`},
		},
		{
			// The second finding has the ID of the first and is about something else.
			"one of two findings about another object", defect,
			report(1, "FAIL", map[string][][]string{"HB-T-001": {{"Secret/a changed"}, {"Secret/b changed"}}}),
			[]string{`HB-T-001 is not about "Secret/a"`},
		},
		{"skipped is not a pass", fixed, report(0, "SKIPPED", nil), []string{"s is SKIPPED, want PASS"}},
		{"inconclusive is not a pass", fixed, report(3, "INCONCLUSIVE", nil), []string{"exit 3, want 0", "s is INCONCLUSIVE, want PASS"}},
		{
			"scenario did not run", fixed,
			Observed{Report: true, Verdicts: map[string]string{}, Findings: map[string][][]string{}},
			[]string{"s did not run"},
		},
		{
			"scenario not in the table", fixed,
			Observed{Report: true, Verdicts: map[string]string{"s": "PASS", "t": "PASS"}, Findings: map[string][][]string{}},
			[]string{"t ran (PASS) and is not in the table"},
		},
		{"no report", fixed, Observed{Exit: 0}, []string{"exit 0 and no report.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.c, tt.obs); !slices.Equal(got, tt.want) {
				t.Errorf("Compare:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func pairTruth() Truth {
	return Truth{Cases: []Case{
		{
			Name: "defect", Pair: "p", Role: roleDefect, Command: "test", Args: []string{"charts/defect"},
			Exit: 1, Verdicts: map[string]string{"s": "FAIL"}, Findings: map[string][]string{"HB-T-001": {"Secret/a"}},
		},
		{
			Name: "fixed", Pair: "p", Role: roleFixed, Command: "test", Args: []string{"charts/fixed"},
			Exit: 0, Verdicts: map[string]string{"s": "PASS"},
		},
	}}
}

func TestValidate(t *testing.T) {
	if problems := pairTruth().Validate(); len(problems) > 0 {
		t.Fatalf("a valid table has problems: %q", problems)
	}
	tests := []struct {
		name   string
		change func(*Truth)
		want   string
	}{
		{"same name twice", func(tr *Truth) { tr.Cases[1].Name = "defect" }, "defect: listed twice"},
		{"unknown command", func(tr *Truth) { tr.Cases[0].Command = "lint" }, `command "lint" is not test, scan or matrix`},
		{"unknown role", func(tr *Truth) { tr.Cases[0].Role = "bad" }, `role "bad" is not defect, fixed or incomplete`},
		{"no verdicts", func(tr *Truth) { tr.Cases[1].Verdicts = nil }, "fixed: no verdicts"},
		{"defect without finding", func(tr *Truth) { tr.Cases[0].Findings = nil }, "a defect case expects at least one finding"},
		{"defect without twin", func(tr *Truth) { tr.Cases = tr.Cases[:1] }, "pair p: no fixed twin"},
		{"twin without defect", func(tr *Truth) { tr.Cases = tr.Cases[1:] }, "pair p: fixed has no defect case"},
		{"two defects in a pair", func(tr *Truth) {
			second := tr.Cases[0]
			second.Name = "defect-2"
			tr.Cases = append(tr.Cases, second)
		}, "pair p: 2 defect cases, want one"},
		{"incomplete case with a finding", func(tr *Truth) {
			tr.Cases = append(tr.Cases, Case{
				Name: "stuck", Role: roleIncomplete, Command: "test", Args: []string{"charts/stuck"},
				Exit: 3, Verdicts: map[string]string{"s": "INCONCLUSIVE"}, Findings: map[string][]string{"HB-T-001": nil},
			})
		}, "stuck: an incomplete case has no pair, no findings and exit 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := pairTruth()
			tt.change(&tr)
			if problems := strings.Join(tr.Validate(), "; "); !strings.Contains(problems, tt.want) {
				t.Errorf("Validate: %q, want it to contain %q", problems, tt.want)
			}
		})
	}
}

func TestCoverage(t *testing.T) {
	tr := pairTruth()
	if problems := tr.Coverage([]string{"HB-T-001"}); len(problems) > 0 {
		t.Fatalf("a covered table has problems: %q", problems)
	}

	// A new finding ID in the source needs a pair or a reason.
	problems := tr.Coverage([]string{"HB-T-001", "HB-T-002"})
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "HB-T-002 has no defect case with a fixed twin") {
		t.Errorf("new ID: %q", problems)
	}
	tr.Uncovered = map[string]string{"HB-T-002": "no fixture yet"}
	if problems := tr.Coverage([]string{"HB-T-001", "HB-T-002"}); len(problems) > 0 {
		t.Errorf("an ID listed under uncovered still has problems: %q", problems)
	}

	// The list can only shrink: an entry must go once a pair covers the ID, or once the ID is gone.
	tr.Uncovered = map[string]string{"HB-T-001": "stale", "HB-T-009": "gone"}
	want := []string{"HB-T-001 is covered now; remove it from uncovered", "HB-T-009 is listed under uncovered but is not in the source"}
	if problems := tr.Coverage([]string{"HB-T-001"}); !slices.Equal(problems, want) {
		t.Errorf("stale entries:\n got %q\nwant %q", problems, want)
	}

	// A twin that expects the same finding does not tell the defect apart.
	tr = pairTruth()
	tr.Cases[1].Findings = map[string][]string{"HB-T-001": nil}
	if tr.Covered()["HB-T-001"] {
		t.Error("HB-T-001 counts as covered although the fixed twin expects it too")
	}

	// An ID the source no longer has cannot be expected.
	tr = pairTruth()
	want = []string{"defect expects HB-T-001, which is not in the source"}
	if problems := tr.Coverage(nil); !slices.Equal(problems, want) {
		t.Errorf("renamed ID:\n got %q\nwant %q", problems, want)
	}
}

func TestScore(t *testing.T) {
	tr := pairTruth()
	tr.Cases = append(tr.Cases, Case{Name: "other", Pair: "q", Role: roleFixed, Verdicts: map[string]string{"s": "PASS"}})
	runs := []Run{
		{Case: "defect", Attempt: 1, Findings: []string{"HB-T-001"}, signature: "a"},
		{Case: "defect", Attempt: 2, signature: "b"},
		{Case: "fixed", Attempt: 1, signature: "a"},
		{Case: "fixed", Attempt: 2, Findings: []string{"HB-T-001"}, signature: "b"},
		{Case: "other", Attempt: 1, Findings: []string{"HB-T-001"}, signature: "a"},
		{Case: "other", Attempt: 2, Findings: []string{"HB-T-001"}, signature: "a"},
	}
	scores, unstable := Score(tr, runs, []string{"HB-T-001"})
	want := FindingScore{ID: "HB-T-001", Expected: 2, Found: 1, Twins: 2, Clean: 1, Stray: 2}
	if len(scores) != 1 || scores[0] != want {
		t.Errorf("scores:\n got %+v\nwant %+v", scores, want)
	}
	if !slices.Equal(unstable, []string{"defect", "fixed"}) {
		t.Errorf("unstable: %q, want defect and fixed", unstable)
	}
}

func TestSourceIDs(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a", "a.go"), "package a\n\n// HB-T-009 in a comment does not count.\nconst id = \"HB-T-001\"\n\nvar other = `HB-T-002`\n\nvar text = \"see HB-T-003\"\n")
	write(t, filepath.Join(dir, "a", "a_test.go"), "package a\n\nconst testOnly = \"HB-T-004\"\n")
	ids, err := SourceIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"HB-T-001", "HB-T-002"}; !slices.Equal(ids, want) {
		t.Errorf("SourceIDs: %q, want %q", ids, want)
	}
}

// The table in the repository must load, cover the source, and name charts that exist.
func TestRepositoryTable(t *testing.T) {
	root := filepath.Join("..", "..")
	truth, err := LoadTruth(filepath.Join(root, "testdata", "verdicts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := SourceIDs(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no finding IDs found in internal/")
	}
	for _, problem := range truth.Coverage(ids) {
		t.Error(problem)
	}
	for _, c := range truth.Cases {
		for _, arg := range c.Args {
			if !strings.HasPrefix(arg, "testdata/") {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, arg, "Chart.yaml")); err != nil {
				t.Errorf("%s: %s is not a chart: %v", c.Name, arg, err)
			}
		}
	}
}

const fakeTable = `
cases:
  - name: defect
    pair: p
    role: defect
    command: test
    args: [charts/defect, --set, "key={key}"]
    exit: 1
    verdicts: {s: FAIL}
    findings:
      HB-T-001: [Secret/a]
  - name: fixed
    pair: p
    role: fixed
    command: matrix
    args: [charts/fixed]
    exit: 0
    verdicts: {s: PASS}
  - name: stuck
    role: incomplete
    command: test
    args: [charts/stuck, --timeout, 45s]
    exit: 3
    verdicts: {s: INCONCLUSIVE}
`

// runFake runs the tool against the fake helmbulance and returns its exit code, its output and the
// directory with the scorecard and the fake's calls.
func runFake(t *testing.T, table, source string, args ...string) (int, string, string) {
	t.Helper()
	return runFakeIn(t, t.TempDir(), table, source, args...)
}

// runFakeIn is runFake in a directory the caller made, which may already hold results under out/.
func runFakeIn(t *testing.T, dir, table, source string, args ...string) (int, string, string) {
	t.Helper()
	write(t, filepath.Join(dir, "verdicts.yaml"), table)
	write(t, filepath.Join(dir, "src", "ids.go"), source)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERDICTS_FAKE", dir)
	var stdout, stderr bytes.Buffer
	args = append([]string{
		"-truth", filepath.Join(dir, "verdicts.yaml"), "-source", filepath.Join(dir, "src"),
		"-bin", self, "-out", filepath.Join(dir, "out"),
	}, args...)
	code := run(args, &stdout, &stderr)
	return code, stdout.String() + stderr.String(), dir
}

const fakeSource = "package ids\n\nconst id = \"HB-T-001\"\n"

func TestRun(t *testing.T) {
	code, out, dir := runFake(t, fakeTable, fakeSource, "-cluster", "c1", "-k8s", "1.30")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"ok    defect (exit 1) HB-T-001", "ok    fixed (exit 0)", "ok    stuck (exit 3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// test cases get the cluster and a default timeout, matrix cases the Kubernetes version.
	calls := strings.Split(strings.TrimSpace(read(t, filepath.Join(dir, "calls"))), "\n")
	wantCalls := []string{
		"test charts/defect --set key=" + strings.Repeat("?", 64) + " --cluster c1 --timeout 3m --report-dir " + filepath.Join(dir, "out", "defect"),
		"matrix charts/fixed --k8s 1.30 --report-dir " + filepath.Join(dir, "out", "fixed"),
		"test charts/stuck --timeout 45s --cluster c1 --report-dir " + filepath.Join(dir, "out", "stuck"),
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls: %q", calls)
	}
	for i, call := range calls {
		if i == 0 {
			// The key is random: 64 hex digits.
			fields := strings.Fields(call)
			key := strings.TrimPrefix(fields[3], "key=")
			if raw, err := hex.DecodeString(key); err != nil || len(raw) != 32 {
				t.Errorf("key %q is not 64 hex digits", key)
			}
			call = strings.Replace(call, key, strings.Repeat("?", 64), 1)
		}
		if call != wantCalls[i] {
			t.Errorf("call %d:\n got %s\nwant %s", i, call, wantCalls[i])
		}
	}

	var card Scorecard
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "out", "scorecard.json"))), &card); err != nil {
		t.Fatal(err)
	}
	want := FindingScore{ID: "HB-T-001", Expected: 1, Found: 1, Twins: 1, Clean: 1}
	if len(card.Findings) != 1 || card.Findings[0] != want {
		t.Errorf("scorecard findings: %+v, want %+v", card.Findings, want)
	}
	md := read(t, filepath.Join(dir, "out", "scorecard.md"))
	for _, want := range []string{"3 of 3 runs as expected", "| HB-T-001 | 1 of 1 | 1 of 1 | 0 |", "| stuck | incomplete | 3 |  | as expected |"} {
		if !strings.Contains(md, want) {
			t.Errorf("scorecard.md lacks %q:\n%s", want, md)
		}
	}
}

func TestRunFails(t *testing.T) {
	onlyPair := strings.Split(fakeTable, "  - name: stuck")[0]
	tests := []struct {
		name   string
		table  string
		source string
		args   []string
		code   int
		want   string
	}{
		{
			// The defect chart is swapped for one that reports nothing: a missed finding.
			"missed finding",
			strings.Replace(onlyPair, "charts/defect", "charts/fixed", 1), fakeSource, nil,
			1, "FAIL  defect: exit 0, want 1; s is PASS, want FAIL; missing HB-T-001",
		},
		{
			"false positive on the twin",
			strings.Replace(onlyPair, "charts/fixed", "charts/defect", 1), fakeSource, nil,
			1, "FAIL  fixed: exit 1, want 0; s is FAIL, want PASS; unexpected HB-T-001",
		},
		{
			"no report",
			strings.Replace(onlyPair, "charts/fixed", "charts/silent", 1), fakeSource, nil,
			1, "FAIL  fixed: exit 2 and no report.json",
		},
		{
			"runs disagree",
			strings.Replace(onlyPair, "charts/fixed", "charts/flaky", 1), fakeSource, []string{"-repeat", "2"},
			1, "FAIL  fixed: its runs did not agree with each other",
		},
		{
			"finding ID without a pair",
			onlyPair, fakeSource + "\nconst next = \"HB-T-002\"\n", nil,
			1, "FAIL  coverage: HB-T-002 has no defect case with a fixed twin",
		},
		{"unknown case", onlyPair, fakeSource, []string{"-only", "nope"}, 2, "not every name is a case"},
		{"unknown field in the table", onlyPair + "    exti: 0\n", fakeSource, nil, 2, "field exti not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, _ := runFake(t, tt.table, tt.source, tt.args...)
			if code != tt.code {
				t.Errorf("exit %d, want %d", code, tt.code)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, out)
			}
		})
	}
}

// A second run into the same directory must not read the report of the first.
func TestRunIgnoresStaleReport(t *testing.T) {
	onlyPair := strings.Split(fakeTable, "  - name: stuck")[0]
	dir := t.TempDir()
	if code, out, _ := runFakeIn(t, dir, onlyPair, fakeSource); code != 0 {
		t.Fatalf("first run: exit %d:\n%s", code, out)
	}
	// The twin now ends before it writes a report.
	code, out, _ := runFakeIn(t, dir, strings.Replace(onlyPair, "charts/fixed", "charts/silent", 1), fakeSource)
	if want := "FAIL  fixed: exit 2 and no report.json"; code != 1 || !strings.Contains(out, want) {
		t.Errorf("second run: exit %d, want 1 and %q:\n%s", code, want, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
