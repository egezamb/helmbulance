package scenario

import (
	"context"
	"fmt"
	"strings"

	"github.com/egezamb/helmbulance/internal/oracle"
)

// Reinstall checks what comes back after `helm uninstall` and `helm install` with the same values,
// the way operators recover a broken release. It writes a sentinel into every volume, reinstalls,
// and compares the volumes and the release's Secrets. The dangerous case is data that survives
// while the credentials it was written with do not.
type Reinstall struct{}

const reinstallNamespace = "hb-reinstall"

func (Reinstall) Name() string { return "reinstall" }

func (Reinstall) Description() string {
	return "Uninstalls and installs again with the same values, and checks volumes and Secrets."
}

// volume is one PersistentVolumeClaim and what happened to its sentinel.
type volume struct {
	claim, uid, value string
	// state is "kept", "missing" or "replaced" after the reinstall; "deleted" when the claim went
	// away with the release, and "recreated" when the reinstall made a new claim with the same
	// name; "unwritable" when no sentinel could be written.
	state string
}

func (s Reinstall) Run(ctx context.Context, env *Env) (res Result) {
	res.Scenario = s.Name()
	const release, ns = "hb-reinstall", reinstallNamespace
	defer func() {
		// Clean up even after a cancel, so a reused cluster does not keep the release (#11).
		cleanup := context.WithoutCancel(ctx)
		if err := env.Helm.Uninstall(cleanup, release, ns); err != nil {
			fmt.Fprintf(env.Log, "cleanup: %v\n", err)
		}
		if err := env.Kube.DeleteNamespace(cleanup, ns); err != nil {
			fmt.Fprintf(env.Log, "cleanup: %v\n", err)
		}
	}()

	mark := len(env.Runner.Transcript())
	if err := env.Helm.Install(ctx, release, ns, env.Chart, env.Values); err != nil {
		return inconclusive(res, "the chart did not install", err)
	}
	var repro []string
	for _, c := range env.Runner.Transcript()[mark:] {
		repro = append(repro, reproLine(c.Line))
	}

	secrets := newSecretReader(ctx, env, release, ns)
	before, err := secrets.snapshot()
	if err != nil {
		return secretsInconclusive(res, "after install", err)
	}
	volumes, err := s.writeSentinels(ctx, env, ns)
	if err != nil {
		return inconclusive(res, "could not write the sentinels", err)
	}
	// Control: read every sentinel back before anything changes, so a read path that does not work
	// cannot later look like lost data.
	if err := s.readSentinels(ctx, env, ns, volumes, false); err != nil {
		return inconclusive(res, "could not read the sentinels back before the uninstall", err)
	}
	if lost := append(claimsIn(volumes, "missing"), claimsIn(volumes, "replaced")...); len(lost) > 0 {
		return inconclusive(res, "a sentinel did not read back before the uninstall",
			fmt.Errorf("check %s: something rewrites the volume while the release runs", strings.Join(lost, ", ")))
	}

	mark = len(env.Runner.Transcript())
	if err := env.Helm.Uninstall(ctx, release, ns); err != nil {
		return inconclusive(res, "the uninstall failed", err)
	}
	secondInstall := env.Helm.Install(ctx, release, ns, env.Chart, env.Values)
	repro = concat(repro, sentinelRepro("write a marker into each volume before the uninstall", "echo marker > /pvc/marker")...)
	repro = concat(repro, transcriptRepro(env, mark)...)
	repro = concat(repro, sentinelRepro("read it back after the reinstall", "cat /pvc/marker")...)
	if secondInstall != nil {
		// A `helm install --wait` that timed out still applied the release; its pods just never
		// became ready, which is exactly how a reinstall that broke the credentials behind kept data
		// shows up. Collect the evidence before the verdict, so HB-RI-001 is not hidden behind
		// INCONCLUSIVE. Other failures (a render refusal, the chart's webhook, a pull or tool error)
		// are not this case and go to installFailed.
		if timedOutWaiting(ctx, secondInstall) {
			res = s.analyze(ctx, env, res, release, ns, secrets, before, volumes, repro)
			switch {
			case len(res.Findings) > 0:
				res.Findings[0].Evidence = append([]string{
					"The reinstall's `helm install --wait` timed out before the release was ready: " + helmMessage(secondInstall),
				}, res.Findings[0].Evidence...)
				return res
			case res.Verdict == Inconclusive:
				return res
			default:
				return inconclusive(res, "the reinstall did not finish, and no credential change was proven", secondInstall)
			}
		}
		return s.installFailed(ctx, res, secondInstall, repro)
	}
	return s.analyze(ctx, env, res, release, ns, secrets, before, volumes, repro)
}

// analyze compares the release's Secrets and the volume sentinels after the reinstall and builds the
// verdict. It runs after a clean reinstall and, for the evidence it can still gather, after a
// `--wait` timeout, since the release is applied in both.
func (s Reinstall) analyze(ctx context.Context, env *Env, res Result, release, ns string,
	secrets *secretReader, before oracle.SecretSnapshot, volumes []volume, repro []string) Result {
	after, err := secrets.snapshot()
	if err != nil {
		return secretsInconclusive(res, "after the reinstall", err)
	}
	if err := s.readSentinels(ctx, env, ns, volumes, true); err != nil {
		return inconclusive(res, "could not read the sentinels", err)
	}

	consumers, consumersErr := env.Kube.SecretConsumers(ctx, ns)
	if consumersErr != nil {
		fmt.Fprintf(env.Log, "could not map Secret consumers: %v\n", consumersErr)
	}
	stateful, statefulErr := env.Kube.StatefulWorkloads(ctx, ns)
	if statefulErr != nil {
		fmt.Fprintf(env.Log, "could not find stateful workloads: %v\n", statefulErr)
	}
	statefulKnown := statefulErr == nil
	changes, added := splitAdded(oracle.Diff(before, after))
	changes, rotated := splitExpected(changes, env.ExpectedRotation)
	// A claim that could not be written to leaves it unknown whether it kept old data. An unbound
	// claim was never mounted, so it holds none and needs no such check (review item 5).
	if unwritable := claimsIn(volumes, "unwritable"); len(unwritable) > 0 && len(changes) > 0 {
		return inconclusive(res, "Secrets changed, and no sentinel could be written to "+strings.Join(unwritable, ", "),
			fmt.Errorf("without one it is unknown whether those volumes kept data written with the old values"))
	}
	var keptClaims []string
	for _, v := range volumes {
		if v.state == "kept" {
			keptClaims = append(keptClaims, v.claim)
		}
	}
	claimUsers, claimUsersErr := env.Kube.ClaimUsers(ctx, ns, keptClaims)
	if claimUsersErr != nil {
		fmt.Fprintf(env.Log, "could not map claims to workloads: %v\n", claimUsersErr)
	}
	// If a kept volume's readers could not be mapped while Secrets changed, relatedness is unknown,
	// so the result is INCONCLUSIVE, not a PASS that never checked (review item 2).
	if (consumersErr != nil || claimUsersErr != nil) && len(keptClaims) > 0 && len(changes) > 0 {
		return inconclusive(res, "could not tell which kept volumes read the changed Secrets",
			fmt.Errorf("mapping Secret readers or claim users failed, so a credential change next to kept data cannot be ruled in or out"))
	}

	var unrelated, certs []oracle.Change
	res.Findings, unrelated, certs = reinstallFindings(volumes, changes, consumers, stateful, statefulKnown, claimUsers, repro)
	if len(keptClaims) == 0 {
		unrelated = nil // the summary already says new Secrets are expected
	}

	if len(res.Findings) > 0 {
		res.Verdict = Fail
		res.Summary = fmt.Sprintf("%d problem(s) after uninstall and install", len(res.Findings))
	} else {
		res.Verdict = Pass
		res.Summary = reinstallPassSummary(volumes, changes)
	}
	res.Summary = withNotes(res.Summary,
		note("keys added by the reinstall, not a finding", describe(added, consumers)),
		note("certificates reissued, read by a workload that kept data, not a finding", describe(certs, consumers)),
		note("changed, but read by no workload whose volume kept its data", describe(unrelated, consumers)),
		note("rotated as expected (--expected-rotation)", describe(rotated, consumers)),
		note("no sentinel could be written to", claimsIn(volumes, "unwritable")),
		note("not bound, so not checked", claimsIn(volumes, "unbound")),
		secrets.notComparedNote())
	return res
}

// writeSentinels writes a fresh sentinel into every PersistentVolumeClaim in ns.
func (Reinstall) writeSentinels(ctx context.Context, env *Env, ns string) ([]volume, error) {
	claims, err := env.Kube.PVCs(ctx, ns)
	if err != nil {
		return nil, err
	}
	var volumes []volume
	for i, claim := range claims {
		if claim.Phase != "Bound" {
			// Mounting an unbound claim would bind it: the check would change what it measures.
			volumes = append(volumes, volume{claim: claim.Name, uid: claim.UID, state: "unbound"})
			continue
		}
		value, err := newSentinel()
		if err != nil {
			return nil, err
		}
		out, err := env.Kube.RunWithPVC(ctx, ns, sentinelPod("w", i), sentinelImage, claim.Name,
			writeSentinelScript(value), sentinelTimeout)
		if err != nil {
			return nil, err
		}
		v := volume{claim: claim.Name, uid: claim.UID, value: value}
		if strings.TrimSpace(out) != "written" {
			v.state = "unwritable"
		}
		volumes = append(volumes, v)
	}
	return volumes, nil
}

// readSentinels sets each written volume's state from what its sentinel reads back; remove deletes
// the sentinel after reading it.
func (Reinstall) readSentinels(ctx context.Context, env *Env, ns string, volumes []volume, remove bool) error {
	claims, err := env.Kube.PVCs(ctx, ns)
	if err != nil {
		return err
	}
	uids := map[string]string{}
	for _, c := range claims {
		uids[c.Name] = c.UID
	}
	for i := range volumes {
		v := &volumes[i]
		uid, present := uids[v.claim]
		switch {
		case v.state == "unwritable" || v.state == "unbound":
			continue
		case !present:
			v.state = "deleted"
			continue
		case uid != v.uid:
			// helm uninstall deleted the claim and the reinstall made a new, empty one.
			v.state = "recreated"
			continue
		}
		out, err := env.Kube.RunWithPVC(ctx, ns, sentinelPod("r", i), sentinelImage, v.claim,
			readSentinelScript(remove), sentinelTimeout)
		if err != nil {
			return err
		}
		v.state = sentinelState(out, v.value)
	}
	return nil
}

// reinstallRunErrors are error texts that come from the run, not the chart. They match the list in
// #22's IsEnvironmentError, which replaces this once it is merged.
var reinstallRunErrors = []string{
	"context deadline exceeded", "timed out waiting for the condition", "signal: killed",
	"errimagepull", "imagepullbackoff", "toomanyrequests", "connection refused", "i/o timeout",
	"tls handshake timeout",
}

// timedOutWaiting reports whether the second install failed because `helm --wait` timed out with
// the release applied, as opposed to a cancelled run, the chart's own webhook, or a pull or tool
// error. Only then is the post-install evidence worth collecting.
func timedOutWaiting(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false // the whole run was cancelled; collect nothing more
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "failed calling webhook") {
		return false // the chart's own webhook failed: a finding, left to installFailed
	}
	return strings.Contains(msg, "timed out waiting for the condition") ||
		strings.Contains(msg, "context deadline exceeded")
}

// installFailed is the result when the second install fails.
//   - The chart refusing at render time (`fail`, `required`) is safe behaviour, a PASS (D4): it may
//     be asking for an existingSecret, which is what HB-RI-001's fix recommends.
//   - A timeout, a killed helm or a pull error says more about the run: INCONCLUSIVE. A failing
//     call to the chart's own webhook is the chart's fault, though, whatever words it carries.
//   - Anything else is a release that cannot be recreated.
func (Reinstall) installFailed(ctx context.Context, res Result, err error, repro []string) Result {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "execution error at (") {
		res.Verdict = Pass
		res.Summary = "refuses safely on reinstall: " + helmMessage(err)
		return res
	}
	if ctx.Err() != nil {
		return inconclusive(res, "the reinstall did not finish", err)
	}
	if !strings.Contains(msg, "failed calling webhook") {
		for _, s := range reinstallRunErrors {
			if strings.Contains(msg, s) {
				return inconclusive(res, "the reinstall did not finish", err)
			}
		}
	}
	res.Verdict = Fail
	res.Summary = "the release does not install again after an uninstall"
	res.Findings = []Finding{{
		ID:       "HB-RI-003",
		Title:    "The release does not install again after `helm uninstall`",
		Severity: "high",
		Evidence: []string{"The second `helm install` failed: " + helmMessage(err)},
		Impact: "Reinstalling is the usual way out of a broken release. Here it fails, often because " +
			"something kept from the first install (a Secret with `helm.sh/resource-policy: keep`, a " +
			"cluster-scoped object) conflicts with the new one.",
		Fix: "Make kept objects adoptable by the same release (keep Helm's ownership labels and " +
			"annotations), or document the manual cleanup before a reinstall.",
		Reproduce: repro,
	}}
	return res
}

// reinstallFindings turns the volumes and the Secret changes into findings. A changed key matters
// only when a workload that mounts a volume which kept its data reads it; the other changes are
// returned apart, to be named in the summary.
func reinstallFindings(volumes []volume, changes []oracle.Change, consumers map[string][]string,
	stateful map[string]bool, statefulKnown bool, claimUsers map[string][]string, repro []string) (findings []Finding, unrelated, certs []oracle.Change) {
	keepers := map[string]bool{}
	var kept []string
	for _, v := range volumes {
		if v.state != "kept" {
			continue
		}
		line := "`" + v.claim + "`"
		if users := claimUsers[v.claim]; len(users) > 0 {
			line += " (mounted by " + strings.Join(users, ", ") + ")"
		}
		kept = append(kept, line)
		for _, w := range claimUsers[v.claim] {
			keepers[w] = true
		}
	}
	var related []oracle.Change
	for _, c := range changes {
		reads := false
		for _, w := range readers(c, consumers) {
			reads = reads || keepers[w]
		}
		switch {
		case !reads:
			unrelated = append(unrelated, c)
		case keyClass(c) == oracle.ClassTLS:
			// A reissued certificate a keeper reads does not lock it out of its kept data; it is a
			// rotation, not a credential mismatch, so it is named in a note, not reported (item 3).
			certs = append(certs, c)
		default:
			related = append(related, c)
		}
	}
	if len(related) > 0 {
		changes := related
		evidence := []string{"Volumes that kept their data: " + strings.Join(kept, ", ") + "."}
		findings = append(findings, Finding{
			ID:       "HB-RI-001",
			Title:    "Data survives a reinstall, but the credentials it was written with do not",
			Severity: worstSeverity(changes, consumers, stateful, statefulKnown),
			Evidence: append(evidence, describe(changes, consumers)...),
			Impact: "After `helm uninstall` and `helm install` the volumes still hold the old data, " +
				"but the chart generated new credentials. A database initialised with the old " +
				"password refuses the new one, and data encrypted with an old key cannot be read.",
			Fix: "Keep generated credentials across a reinstall: add `helm.sh/resource-policy: keep` " +
				"to the Secret and reuse its value with `lookup`, or require an existingSecret for " +
				"anything a volume depends on.",
			Reproduce: repro,
		})
	}
	if lost := append(claimsIn(volumes, "missing"), claimsIn(volumes, "replaced")...); len(lost) > 0 {
		findings = append(findings, Finding{
			ID:       "HB-RI-002",
			Title:    "A volume that survives the reinstall comes back without its data",
			Severity: "medium",
			Evidence: []string{"The sentinel written before the uninstall is gone from: " + strings.Join(lost, ", ") + "."},
			Impact: "The claim was kept, so users expect their data back, but something emptied or " +
				"replaced it during the reinstall, such as an init container or a job that formats it.",
			Fix:       "Only initialise a volume that is empty, and never on an install that finds existing data.",
			Reproduce: repro,
		})
	}
	return findings, unrelated, certs
}

// reinstallPassSummary says what was checked, including volumes deleted with the release, which
// depend on the StorageClass and the chart and are not a finding on their own.
func reinstallPassSummary(volumes []volume, changes []oracle.Change) string {
	summary := "Secrets came back unchanged after uninstall and install"
	switch {
	case len(volumes) == 0 && len(changes) > 0:
		return "the release has no volumes, so new Secrets after a reinstall lose nothing"
	case len(changes) > 0:
		summary = "no volume kept its data, so new Secrets are expected"
	}
	if kept := claimsIn(volumes, "kept"); len(kept) > 0 {
		summary += fmt.Sprintf("; %d volume(s) kept their data", len(kept))
	}
	if deleted := append(claimsIn(volumes, "deleted"), claimsIn(volumes, "recreated")...); len(deleted) > 0 {
		summary += "; deleted with the release (depends on the StorageClass reclaim policy): " + strings.Join(deleted, ", ")
	}
	if len(volumes) == 0 {
		summary += "; the release has no volumes"
	}
	return summary
}

func claimsIn(volumes []volume, state string) []string {
	var claims []string
	for _, v := range volumes {
		if v.state == state {
			claims = append(claims, "`"+v.claim+"`")
		}
	}
	return claims
}

// transcriptRepro returns the reproduction lines of the commands run since mark.
func transcriptRepro(env *Env, mark int) []string {
	var lines []string
	for _, c := range env.Runner.Transcript()[mark:] {
		lines = append(lines, reproLine(c.Line))
	}
	return lines
}

// sentinelRepro is the reproduction step for a sentinel: a one-off pod that mounts the claim. The
// claim is named per volume in the evidence.
func sentinelRepro(why, cmd string) []string {
	return []string{
		"# " + why + " (replace CLAIM with each claim named above):",
		`kubectl -n ` + reinstallNamespace + ` run marker --rm -i --restart Never --image busybox --overrides '{"spec":{"containers":[{"name":"marker","image":"busybox","command":["sh","-c","` + cmd + `"],"volumeMounts":[{"name":"v","mountPath":"/pvc"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"CLAIM"}}]}}'`,
	}
}
