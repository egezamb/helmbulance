// Command verdicts checks Helmbulance's verdicts against a table of cases whose right answer is
// known (testdata/verdicts.yaml): fixture charts with one defect each, their fixed twins, and charts
// on which a run cannot finish. It runs every case with a built helmbulance, compares the exit
// code, the verdict of every scenario and the findings with the table, and writes a scorecard.
//
//	go run ./tools/verdicts [-cluster helmbulance-xxxx] [-only name,name] [-repeat 3]
//
// It exits 1 when a run differs from the table, when two runs of a case disagree, or when the
// table does not cover a finding ID the source can emit. docs/verdict-quality.md has the details.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verdicts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	truthPath := fs.String("truth", "testdata/verdicts.yaml", "the table of cases")
	source := fs.String("source", "internal", "Go source to list the finding IDs from")
	bin := fs.String("bin", "bin/helmbulance", "the helmbulance binary to run")
	out := fs.String("out", "", "directory for logs, reports and the scorecard (default: a new temporary directory)")
	cluster := fs.String("cluster", "", "reuse this sandbox cluster for the test cases")
	k8s := fs.String("k8s", "1.37", "Kubernetes version for the matrix cases")
	only := fs.String("only", "", "run only these cases, comma-separated")
	repeat := fs.Int("repeat", 1, "run every case this many times; the runs must agree")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	truth, err := LoadTruth(*truthPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	sourceIDs, err := SourceIDs(*source)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cases := truth.Cases
	if *only != "" {
		names := strings.Split(*only, ",")
		cases = slices.DeleteFunc(slices.Clone(cases), func(c Case) bool { return !slices.Contains(names, c.Name) })
		if len(cases) != len(names) {
			fmt.Fprintf(stderr, "-only %s: not every name is a case in %s\n", *only, *truthPath)
			return 2
		}
	}
	if *repeat < 1 {
		fmt.Fprintln(stderr, "-repeat must be at least 1")
		return 2
	}
	if *out == "" {
		if *out, err = os.MkdirTemp("", "verdicts-"); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	r := Runner{Bin: *bin, Out: *out, Cluster: *cluster, K8s: *k8s, Key: hex.EncodeToString(key)}

	card := Scorecard{Helm: helmVersion(), K8s: *k8s, Repeat: *repeat, Coverage: truth.Coverage(sourceIDs)}
	for _, c := range cases {
		for attempt := 1; attempt <= *repeat; attempt++ {
			res := r.Run(context.Background(), c, attempt)
			card.Runs = append(card.Runs, res)
			if len(res.Differs) == 0 {
				fmt.Fprintf(stdout, "ok    %s (exit %d) %s\n", c.Name, res.Exit, strings.Join(res.Findings, " "))
			} else {
				fmt.Fprintf(stdout, "FAIL  %s: %s  log: %s\n", c.Name, strings.Join(res.Differs, "; "), res.Log)
			}
		}
	}

	ids := slices.Clone(sourceIDs)
	for _, c := range truth.Cases {
		for id := range c.Findings {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	card.Findings, card.Unstable = Score(truth, card.Runs, ids)
	for _, name := range card.Unstable {
		fmt.Fprintf(stdout, "FAIL  %s: its runs did not agree with each other\n", name)
	}
	for _, c := range card.Coverage {
		fmt.Fprintf(stdout, "FAIL  coverage: %s\n", c)
	}

	data, err := json.MarshalIndent(card, "", "  ")
	if err == nil {
		err = errors.Join(
			os.WriteFile(filepath.Join(*out, "scorecard.json"), append(data, '\n'), 0o644),
			os.WriteFile(filepath.Join(*out, "scorecard.md"), []byte(card.Markdown(truth)), 0o644),
		)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintf(stdout, "Scorecard: %s\n", filepath.Join(*out, "scorecard.md"))
	if !card.OK() {
		return 1
	}
	return 0
}

// Runner runs cases with one helmbulance binary.
type Runner struct {
	Bin     string
	Out     string
	Cluster string
	K8s     string
	Key     string
}

// caseDeadline bounds one run. It is far above any timeout a case sets, so it only ends a run that
// hangs.
const caseDeadline = 30 * time.Minute

// Run runs c once and compares the result with the table.
func (r Runner) Run(ctx context.Context, c Case, attempt int) Run {
	c = r.expand(c)
	name := c.Name
	if attempt > 1 {
		name = fmt.Sprintf("%s.%d", c.Name, attempt)
	}
	dir := filepath.Join(r.Out, name)
	res := Run{Case: c.Name, Attempt: attempt, Log: dir + ".log"}

	args := append([]string{c.Command}, c.Args...)
	switch c.Command {
	case "test":
		if r.Cluster != "" {
			args = append(args, "--cluster", r.Cluster)
		}
		if !slices.Contains(args, "--timeout") {
			args = append(args, "--timeout", "3m")
		}
	case "matrix":
		args = append(args, "--k8s", r.K8s)
	}
	args = append(args, "--report-dir", dir)

	// A report that an earlier run left in the same directory must not stand in for this one.
	if err := os.Remove(filepath.Join(dir, "report.json")); err != nil && !os.IsNotExist(err) {
		res.Exit, res.Differs = -1, []string{err.Error()}
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, caseDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	log, err := os.Create(res.Log)
	if err != nil {
		res.Exit, res.Differs = -1, []string{err.Error()}
		return res
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		res.Exit = exit.ExitCode()
	default:
		res.Exit, res.Differs = -1, []string{"helmbulance did not run to its end: " + err.Error()}
		return res
	}

	obs, err := ReadReport(filepath.Join(dir, "report.json"), res.Exit)
	if err != nil {
		res.Differs = []string{err.Error()}
		return res
	}
	res.Verdicts, res.Findings, res.signature = obs.Verdicts, obs.IDs(), obs.Signature()
	res.Differs = Compare(c, obs)
	return res
}

// expand fills in {key} and {k8s}.
func (r Runner) expand(c Case) Case {
	fill := strings.NewReplacer("{key}", r.Key, "{k8s}", r.K8s).Replace
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = fill(a)
	}
	verdicts := make(map[string]string, len(c.Verdicts))
	for s, v := range c.Verdicts {
		verdicts[fill(s)] = v
	}
	c.Args, c.Verdicts = args, verdicts
	return c
}

func helmVersion() string {
	out, err := exec.Command("helm", "version", "--short").Output()
	if err != nil {
		return "helm (version unknown)"
	}
	return "helm " + strings.TrimSpace(string(out))
}
