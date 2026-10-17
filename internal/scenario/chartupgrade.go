package scenario

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/oracle"
)

// ChartUpgrade installs an earlier version of the chart and upgrades it to the chart under test,
// with the same values, the way users upgrade.
//
// Control: an upgrade from the earlier version to itself. Secrets that change there are
// regenerated on every upgrade and are not blamed on the new version. Treatment: the upgrade to
// the new version. It reports an upgrade that fails, Secret values the new version changes or
// drops, and workloads that are not healthy afterwards.
type ChartUpgrade struct{}

const chartUpgradeNamespace = "hb-chart-upgrade"

func (ChartUpgrade) Name() string { return "chart-upgrade" }

func (ChartUpgrade) Description() string {
	return "Installs an earlier chart version (--from-version) and upgrades to the chart under test with the same values."
}

func (s ChartUpgrade) Run(ctx context.Context, env *Env) (res Result) {
	res.Scenario = s.Name()
	if env.FromVersion == "" {
		res.Verdict = Inconclusive
		res.Summary = "skipped: give --from-version to say which version users upgrade from"
		return res
	}
	from := env.From
	if from == "" {
		from = env.Chart
	}
	const release, ns = "hb-chart", chartUpgradeNamespace
	defer func() {
		if err := env.Helm.Uninstall(ctx, release, ns); err != nil {
			fmt.Fprintf(env.Log, "cleanup: %v\n", err)
		}
		if err := env.Kube.DeleteNamespace(ctx, ns); err != nil {
			fmt.Fprintf(env.Log, "cleanup: %v\n", err)
		}
	}()

	mark := len(env.Runner.Transcript())
	if err := env.Helm.Install(ctx, release, ns, from, env.Values, "--version", env.FromVersion); err != nil {
		return inconclusive(res, "the earlier version did not install", err)
	}
	repro := []string{reproLine(env.Runner.Transcript()[mark].Line)}
	// fromV is the version helm resolved, which differs from --from-version for a range like ~1.2.
	fromV, err := installedFrom(ctx, env, release, ns)
	if err != nil {
		return inconclusive(res, "could not confirm the earlier version", err)
	}

	secrets := newSecretReader(ctx, env, release, ns)
	snapshot := secrets.snapshot
	installed, err := snapshot()
	if err != nil {
		return secretsInconclusive(res, "after install", err)
	}

	// Control: same version again.
	if err := env.Helm.Upgrade(ctx, release, ns, from, env.Values, "--version", env.FromVersion); err != nil {
		return inconclusive(res, "upgrading the earlier version to itself failed", err)
	}
	control, err := snapshot()
	if err != nil {
		return secretsInconclusive(res, "after the control upgrade", err)
	}
	podsBefore, err := env.Kube.Pods(ctx, ns)
	if err != nil {
		return inconclusive(res, "could not read pods", err)
	}
	jobsBefore, err := env.Kube.FailedJobs(ctx, ns)
	if err != nil {
		return inconclusive(res, "could not read Jobs", err)
	}

	// Treatment: the version under test.
	mark = len(env.Runner.Transcript())
	upgradeErr := env.Helm.Upgrade(ctx, release, ns, env.Chart, env.Values)
	repro = append(repro, reproLine(env.Runner.Transcript()[mark].Line))
	if IsEnvironmentError(ctx, upgradeErr) {
		return inconclusive(res, "the upgrade did not finish", upgradeErr)
	}
	if upgradeErr != nil {
		res.Verdict = Fail
		res.Summary = "the upgrade from " + fromV + " fails"
		res.Findings = append(res.Findings, Finding{
			ID:       "HB-CU-001",
			Title:    "Upgrading from " + fromV + " fails",
			Severity: "high",
			Evidence: []string{"`helm upgrade` failed: " + helmMessage(upgradeErr)},
			Impact: "Users on " + fromV + " cannot upgrade with their existing values. A " +
				"failed upgrade can leave the release in a failed state that needs a manual rollback.",
			Fix: "If the error names an immutable field (selector, StatefulSet volumeClaimTemplates or " +
				"serviceName), keep it unchanged or document the migration (for example deleting the " +
				"StatefulSet with --cascade=orphan). If it is a values or schema error, accept the old " +
				"value or document the change in the release notes.",
			Reproduce: repro,
		})
		return res
	}

	// Read pods before the Secret snapshot, which can take seconds to settle: a new pod gets its
	// restart baseline here, so a crash in that wait still counts as after the upgrade.
	podsUpgraded, err := env.Kube.Pods(ctx, ns)
	if err != nil {
		fmt.Fprintf(env.Log, "could not read pods right after the upgrade: %v\n", err)
	}
	upgraded, err := snapshot()
	if err != nil {
		return secretsInconclusive(res, "after the upgrade", err)
	}
	consumers, err := env.Kube.SecretConsumers(ctx, ns)
	if err != nil {
		fmt.Fprintf(env.Log, "could not map Secret consumers: %v\n", err)
	}

	regenerated := oracle.Diff(installed, control)
	// A new version adding keys is normal; only changed and removed keys break existing data.
	// A renamed Secret whose value is carried over loses nothing either.
	rest, moved := oracle.Moved(control, upgraded, withoutRefs(oracle.Diff(control, upgraded), regenerated))
	moved, back := unpairStillRead(moved, consumers)
	changes, added := splitAdded(append(rest, back...))
	if len(changes) > 0 {
		res.Findings = append(res.Findings, Finding{
			ID:       "HB-CU-002",
			Title:    "The new version changes or drops Secret values of an existing release",
			Severity: "high",
			Evidence: describe(changes, consumers),
			Impact: "After the upgrade the application gets different credentials or keys than the " +
				"ones its data was created with, or loses them.",
			Fix: "Keep the Secret names and keys of earlier versions, or migrate the stored value " +
				"(read the old Secret with `lookup` and carry it over) and say so in the release notes.",
			Reproduce: repro,
		})
	}

	if unhealthy := unhealthyAfter(ctx, env, podsBefore, podsUpgraded, jobsBefore); len(unhealthy) > 0 {
		res.Findings = append(res.Findings, Finding{
			ID:        "HB-CU-003",
			Title:     "Workloads are not healthy after the upgrade",
			Severity:  "high",
			Evidence:  unhealthy,
			Impact:    "The upgrade completed, but parts of the application are not ready or keep restarting.",
			Fix:       "Read the pod logs and events in the failure bundle; typical causes are a removed setting the app still needs, a migration that fails, or a changed port or probe.",
			Reproduce: repro,
		})
	}

	if len(regenerated) > 0 {
		fmt.Fprintf(env.Log, "note: %d Secret key(s) change on every upgrade of %s; see upgrade-without-values\n", len(regenerated), fromV)
	}
	if len(res.Findings) > 0 {
		res.Verdict = Fail
		res.Summary = fmt.Sprintf("%d problem(s) upgrading from %s%s", len(res.Findings), fromV, toVersion(ctx, env, release, ns))
	} else {
		res.Verdict = Pass
		res.Summary = "upgraded from " + fromV + toVersion(ctx, env, release, ns) + " with Secrets kept and workloads healthy"
		if noKeys(installed, control, upgraded) {
			res.Summary = "upgraded from " + fromV + toVersion(ctx, env, release, ns) + " with workloads healthy; " + noSecretsTested
		}
	}
	res.Summary = withNotes(res.Summary,
		note("keys added by the new version, not a finding", describe(added, consumers)),
		note("values moved to another key, not a finding", describe(moved, consumers)),
		secrets.notComparedNote())
	return res
}

// unhealthyAfter waits for the release to settle and lists pods that are not ready or restarted
// more than before the upgrade, and Jobs that gave up since. A pod that survived the upgrade is
// compared with its count from before it. A new pod is compared with its count at the first read
// that sees it (upgraded, read as soon as helm --wait returned, or else the first Settle read),
// since restarts during its own startup, before helm --wait returned, are not the upgrade's doing;
// matrix counts the same way.
func unhealthyAfter(ctx context.Context, env *Env, before, upgraded []kube.Pod, jobsBefore []string) []string {
	baseline := map[string]int{}
	for _, p := range before {
		baseline[p.UID] = p.Restarts
	}
	for _, p := range upgraded {
		if _, seen := baseline[p.UID]; !seen {
			baseline[p.UID] = p.Restarts
		}
	}
	return Settle(ctx, func() []string {
		pods, err := env.Kube.Pods(ctx, chartUpgradeNamespace)
		if err != nil {
			return []string{"could not read pods: " + err.Error()}
		}
		for _, p := range pods {
			if _, seen := baseline[p.UID]; !seen {
				baseline[p.UID] = p.Restarts
			}
		}
		jobs, err := env.Kube.FailedJobs(ctx, chartUpgradeNamespace)
		if err != nil {
			return []string{"could not read Jobs: " + err.Error()}
		}
		return append(podProblems(pods, baseline), newlyFailed(jobs, jobsBefore)...)
	})
}

// podProblems lists pods that are not ready and pods that restarted more than their baseline, by
// pod UID, so a Deployment's new pods are checked too. A finished Job pod is left to the Jobs' own
// status, but a Job pod stuck Pending or crash-looping in place is judged here, since its Job stays
// active with no failure for `Settle` to see.
func podProblems(pods []kube.Pod, restartsBefore map[string]int) []string {
	var problems []string
	for _, p := range pods {
		if p.FinishedJob() {
			continue
		}
		if !p.Ready {
			problems = append(problems, fmt.Sprintf("pod `%s` (%s) is not ready", p.Name, p.Owner))
		}
		if n := p.Restarts - restartsBefore[p.UID]; n > 0 {
			problems = append(problems, fmt.Sprintf("pod `%s` (%s) restarted %d more time(s) after the upgrade", p.Name, p.Owner, n))
		}
	}
	sort.Strings(problems)
	return problems
}

// newlyFailed lists the Jobs in now that had not failed before, by "Job/<name>": the text after
// the name, such as a count of failed attempts, can change.
func newlyFailed(now, before []string) []string {
	had := map[string]bool{}
	for _, j := range before {
		name, _, _ := strings.Cut(j, ":")
		had[name] = true
	}
	var out []string
	for _, j := range now {
		if name, _, _ := strings.Cut(j, ":"); !had[name] {
			out = append(out, j)
		}
	}
	return out
}

// helmMessage keeps the part of a helm error that explains it, without the command line.
func helmMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, "Error: "); i >= 0 {
		msg = msg[i+len("Error: "):]
	}
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	return msg
}
