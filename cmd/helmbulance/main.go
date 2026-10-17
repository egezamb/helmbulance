// Command helmbulance runs day-two scenarios against a Helm chart in a disposable kind cluster.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/helm"
	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/matrix"
	"github.com/egezamb/helmbulance/internal/oracle"
	"github.com/egezamb/helmbulance/internal/report"
	"github.com/egezamb/helmbulance/internal/sandbox"
	"github.com/egezamb/helmbulance/internal/scan"
	"github.com/egezamb/helmbulance/internal/scenario"
)

// Exit codes.
const (
	exitClean      = 0
	exitFindings   = 1
	exitError      = 2
	exitIncomplete = 3 // a scenario could not finish, e.g. the chart did not install
)

var scenarios = []scenario.Scenario{
	scenario.UpgradeValues{},
	scenario.ChartUpgrade{},
	scenario.ZeroDowntime{},
	scenario.Reinstall{},
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitError)
	}
	switch os.Args[1] {
	case "test":
		os.Exit(test(os.Args[2:]))
	case "matrix":
		os.Exit(matrixCmd(os.Args[2:]))
	case "scan":
		os.Exit(scanCmd(os.Args[2:]))
	case "list-scenarios":
		for _, s := range scenarios {
			fmt.Printf("%-24s %s\n", s.Name(), s.Description())
		}
	default:
		usage()
		os.Exit(exitError)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  helmbulance test <chart> [-f values.yaml]... [--set key=value]... [flags]
  helmbulance matrix <chart> [-f values.yaml]... [--set key=value]... [--k8s 1.34,1.37]
  helmbulance scan <chart-dir> [-f values.yaml]... [--from-version 1.2.3 [--from repo/chart]] [--strict]
  helmbulance list-scenarios
`)
}

func test(args []string) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var files, sets, only, rotations multiFlag
	fs.Var(&files, "f", "values file (repeatable)")
	fs.Var(&rotations, "expected-rotation", "Secret key the chart changes on purpose, as secret/key with * globs, e.g. '*-webhook-tls/*' (repeatable); it is named in the summary, not reported")
	fs.Var(&sets, "set", "install-time value, key=value (repeatable). Values of four or more characters are hidden in reports. Upgrades test what happens when it is left out.")
	fs.Var(&only, "scenario", "run only this scenario (repeatable)")
	cluster := fs.String("cluster", "", "reuse a sandbox cluster created earlier by helmbulance")
	keep := fs.Bool("keep-cluster", false, "keep the sandbox cluster after the run")
	nodeImage := fs.String("node-image", "", "kind node image")
	timeout := fs.Duration("timeout", 10*time.Minute, "timeout for each install or upgrade")
	out := fs.String("report-dir", "helmbulance-report", "where to write report.md and report.json")
	from := fs.String("from", "", "chart to upgrade from in chart-upgrade (repo/chart, oci://… or a path); defaults to the chart under test")
	fromVersion := fs.String("from-version", "", "chart version to upgrade from; enables the chart-upgrade scenario. A local chart also needs --from")
	trafficTarget := fs.String("traffic-target", "", "Service and port that zero-downtime sends requests to (service:port); default: the most HTTP-like Service port")
	trafficPath := fs.String("traffic-path", "/", "path that zero-downtime requests; it must answer 2xx")
	allowRemote := fs.Bool("allow-remote", false, "run a chart from a repository, OCI registry or URL; it runs without the third-party protections, which arrive in v0.3")

	// Accept the chart path before or after the flags.
	var chart string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		chart, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if chart == "" && fs.NArg() > 0 {
		chart = fs.Arg(0)
	}
	if chart == "" {
		usage()
		return exitError
	}
	if refuseRemote(*allowRemote, chart, *from) || refuseFromVersion(chart, *from, *fromVersion, localVersion) {
		return exitError
	}
	if err := checkRotations(rotations); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	selected, err := pick(only)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	if *fromVersion == "" && len(only) == 0 {
		selected = without(selected, "chart-upgrade")
	}

	runner := execx.NewSandboxRunner()
	runner.Secrets = execx.SetValues(sets)
	started := time.Now()

	var c *sandbox.Cluster
	if *cluster != "" {
		c, err = sandbox.Open(*cluster)
	} else {
		fmt.Println("Creating sandbox cluster…")
		c, err = sandbox.Create(ctx, runner, *nodeImage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	if !*keep && *cluster == "" {
		defer func() {
			fmt.Println("Deleting sandbox cluster…")
			if err := c.Delete(context.Background(), runner); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
	} else {
		fmt.Printf("Sandbox cluster: %s (reuse with --cluster %s)\n", c.Name, c.Name)
	}

	hasher, err := oracle.NewHasher()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	h := &helm.Client{Runner: runner, Kubeconfig: c.Kubeconfig, Timeout: *timeout}
	env := &scenario.Env{
		Runner: runner,
		Helm:   h,
		Kube:   &kube.Client{Runner: runner, Kubeconfig: c.Kubeconfig},
		Hasher: hasher,
		Chart:  chart,
		Values: helm.Values{Files: files, Sets: sets},
		Log:    os.Stderr,

		From:          *from,
		FromVersion:   *fromVersion,
		TrafficTarget: *trafficTarget,
		TrafficPath:   *trafficPath,

		ExpectedRotation: rotations,
	}

	rep := &report.Report{
		SchemaVersion:    report.SchemaVersion,
		Chart:            chart,
		ValuesFiles:      files,
		SetKeys:          execx.SetKeys(sets),
		ExpectedRotation: rotations,
		HelmVersion:      h.Version(ctx),
		Cluster:          c.Name,
		Started:          started,
	}
	for _, s := range selected {
		fmt.Printf("▶ %s\n", s.Name())
		res := s.Run(ctx, env)
		fmt.Printf("  %s: %s\n", res.Verdict, res.Summary)
		rep.Results = append(rep.Results, res)
	}
	rep.Duration = time.Since(started).Round(time.Second).String()
	rep.Commands = runner.Transcript()

	if err := rep.Write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	fmt.Printf("Report: %s/report.md\n", *out)

	switch rep.Worst() {
	case scenario.Fail:
		return exitFindings
	case scenario.Inconclusive:
		return exitIncomplete
	}
	return exitClean
}

func pick(names []string) ([]scenario.Scenario, error) {
	if len(names) == 0 {
		return scenarios, nil
	}
	var out []scenario.Scenario
	for _, n := range names {
		found := false
		for _, s := range scenarios {
			if s.Name() == n {
				out = append(out, s)
				found = true
			}
		}
		if !found {
			return nil, errors.New("unknown scenario: " + n)
		}
	}
	return out, nil
}

// refuseRemote stops a run on a chart that is not on disk unless --allow-remote was given. A chart
// from a repository, OCI registry or URL is someone else's, and the protections for third-party
// charts (architecture §4) do not exist yet: a privileged pod in the sandbox is root on the Docker
// host. This guards against running one by accident; it is not a protection.
func refuseRemote(allow bool, refs ...string) bool {
	for _, ref := range refs {
		if ref == "" || helm.IsLocal(ref) {
			continue
		}
		if helm.IsPath(ref) {
			fmt.Fprintf(os.Stderr, "path not found: %s\n", ref)
			return true
		}
		if !allow {
			fmt.Fprintf(os.Stderr, "%s is not a chart on disk. Charts from a repository, OCI registry or URL run without the third-party protections, which arrive in v0.3.\nRead the chart first; if you trust it, pass --allow-remote (flags go before the chart).\n", ref)
			return true
		}
		fmt.Fprintf(os.Stderr, "warning: running %s without the third-party protections (--allow-remote)\n", ref)
	}
	return false
}

// refuseFromVersion stops a run whose --from-version cannot take effect. helm ignores --version for
// a chart on disk, so without --from a local chart would be upgraded to itself and the upgrade
// would pass without testing anything, and a local --from installs the version in its Chart.yaml
// whatever --from-version says. versionOf reads that version.
func refuseFromVersion(chart, from, fromVersion string, versionOf func(string) (string, error)) bool {
	if fromVersion == "" {
		return false
	}
	if from != "" {
		if !helm.IsLocal(from) {
			return false
		}
		v, err := versionOf(from)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not read the version of --from %s: %v\n", from, err)
			return true
		}
		// helm accepts a leading v on --version, so v1.0.0 and 1.0.0 name the same chart.
		if strings.TrimPrefix(v, "v") != strings.TrimPrefix(fromVersion, "v") {
			fmt.Fprintf(os.Stderr, "--from %s is version %s, not --from-version %s: helm ignores --version for a path.\n", from, v, fromVersion)
			return true
		}
		return false
	}
	if !helm.IsLocal(chart) {
		return false
	}
	fmt.Fprintf(os.Stderr, "--from-version %s needs --from with a local chart: helm ignores --version for a path, so %s would be upgraded to itself.\nGive the earlier chart, e.g. --from ./old-chart or --from repo/chart.\n", fromVersion, chart)
	return true
}

// localVersion reads the version of a chart on disk, outside the run's transcript.
func localVersion(ref string) (string, error) {
	h := &helm.Client{Runner: execx.NewSandboxRunner(), Timeout: time.Minute}
	return h.ChartVersion(context.Background(), ref)
}

func without(list []scenario.Scenario, name string) []scenario.Scenario {
	var out []scenario.Scenario
	for _, s := range list {
		if s.Name() != name {
			out = append(out, s)
		}
	}
	return out
}

// matrixCmd installs the chart on several Kubernetes versions, one fresh cluster each.
func matrixCmd(args []string) int {
	fs := flag.NewFlagSet("matrix", flag.ContinueOnError)
	var files, sets multiFlag
	fs.Var(&files, "f", "values file (repeatable)")
	fs.Var(&sets, "set", "value, key=value (repeatable). Values of four or more characters are hidden in reports.")
	versions := fs.String("k8s", strings.Join(matrix.Versions(), ","), "Kubernetes versions to test, comma-separated")
	timeout := fs.Duration("timeout", 10*time.Minute, "timeout for each install")
	out := fs.String("report-dir", "helmbulance-report", "where to write report.md and report.json")
	allowRemote := fs.Bool("allow-remote", false, "run a chart from a repository, OCI registry or URL; it runs without the third-party protections, which arrive in v0.3")
	var chart string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		chart, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if chart == "" && fs.NArg() > 0 {
		chart = fs.Arg(0)
	}
	if chart == "" {
		usage()
		return exitError
	}
	if refuseRemote(*allowRemote, chart) {
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := execx.NewSandboxRunner()
	runner.Secrets = execx.SetValues(sets)
	started := time.Now()
	rep := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Chart:         chart,
		ValuesFiles:   files,
		SetKeys:       execx.SetKeys(sets),
		HelmVersion:   (&helm.Client{Runner: runner}).Version(ctx),
		Cluster:       "one per Kubernetes version",
		Started:       started,
	}
	opts := matrix.Options{Runner: runner, Chart: chart, Values: helm.Values{Files: files, Sets: sets}, Timeout: *timeout, Log: os.Stdout}
	for _, v := range strings.Split(*versions, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		fmt.Printf("▶ Kubernetes %s\n", v)
		res := matrix.Run(ctx, v, opts)
		fmt.Printf("  %s: %s\n", res.Verdict, res.Summary)
		rep.Results = append(rep.Results, res)
	}
	attributed := matrix.Attribute(rep.Results)
	for i, res := range attributed {
		if res.Verdict != rep.Results[i].Verdict {
			fmt.Printf("%s is now %s: %s\n", res.Scenario, res.Verdict, res.Summary)
		}
	}
	rep.Results = attributed
	rep.Duration = time.Since(started).Round(time.Second).String()
	rep.Commands = runner.Transcript()
	if err := rep.Write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	fmt.Printf("Report: %s/report.md\n", *out)
	switch rep.Worst() {
	case scenario.Fail:
		return exitFindings
	case scenario.Inconclusive:
		return exitIncomplete
	}
	return exitClean
}

// scanCmd runs the static pre-screen. It reports candidates, not findings, so it exits 0 unless
// --strict is given.
func scanCmd(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	var files, sets multiFlag
	fs.Var(&files, "f", "values file (repeatable)")
	fs.Var(&sets, "set", "value, key=value (repeatable). Values of four or more characters are hidden in reports.")
	from := fs.String("from", "", "chart to compare against for immutable fields (repo/chart, oci://… or a path); defaults to the chart")
	fromVersion := fs.String("from-version", "", "version users upgrade from; enables the immutable-field check. A local chart also needs --from")
	strict := fs.Bool("strict", false, "exit 1 when there are candidates")
	out := fs.String("report-dir", "helmbulance-report", "where to write report.md and report.json")
	var chart string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		chart, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if chart == "" && fs.NArg() > 0 {
		chart = fs.Arg(0)
	}
	if chart == "" {
		usage()
		return exitError
	}
	if refuseFromVersion(chart, *from, *fromVersion, localVersion) {
		return exitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runner := execx.NewSandboxRunner()
	runner.Secrets = execx.SetValues(sets)
	started := time.Now()
	rep := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Chart:         chart,
		ValuesFiles:   files,
		SetKeys:       execx.SetKeys(sets),
		HelmVersion:   (&helm.Client{Runner: runner}).Version(ctx),
		Cluster:       "none (static scan)",
		Started:       started,
	}
	rep.Results = scan.Run(ctx, scan.Options{Runner: runner, Chart: chart, Values: helm.Values{Files: files, Sets: sets}, From: *from, FromVersion: *fromVersion})
	for _, r := range rep.Results {
		fmt.Printf("%-30s %s: %s\n", r.Scenario, r.Verdict, r.Summary)
	}
	rep.Duration = time.Since(started).Round(time.Second).String()
	rep.Commands = runner.Transcript()
	if err := rep.Write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	fmt.Printf("Report: %s/report.md\n", *out)
	if *strict && rep.Worst() == scenario.Fail {
		return exitFindings
	}
	return exitClean
}

// checkRotations rejects --expected-rotation patterns that could never match a "secret/key"
// reference, so a typo does not silently report a rotation the user meant to allow.
func checkRotations(patterns []string) error {
	for _, p := range patterns {
		secret, key, ok := strings.Cut(p, "/")
		if !ok || secret == "" || key == "" || strings.Contains(key, "/") {
			return fmt.Errorf("--expected-rotation %q: want secret/key, e.g. '*-webhook-tls/tls.crt' or '*-webhook-tls/*'", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("--expected-rotation %q: %v", p, err)
		}
	}
	return nil
}
