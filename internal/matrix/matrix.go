// Package matrix installs a chart on several Kubernetes versions, one disposable kind cluster per
// version, and reports where it does not install, does not become healthy, or uses APIs the
// version warns about.
package matrix

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/helm"
	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/sandbox"
	"github.com/egezamb/helmbulance/internal/scenario"
)

// NodeImages are the kind node images for kind v0.33, pinned by digest.
var NodeImages = map[string]string{
	"1.34": "kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d",
	"1.35": "kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0",
	"1.36": "kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed",
	"1.37": "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5",
}

// Versions returns the supported versions, oldest first.
func Versions() []string {
	var v []string
	for k := range NodeImages {
		v = append(v, k)
	}
	sort.Slice(v, func(i, j int) bool { return minor(v[i]) < minor(v[j]) })
	return v
}

func minor(v string) int {
	var maj, min int
	fmt.Sscanf(v, "%d.%d", &maj, &min)
	return min
}

// Options is what one matrix run needs.
type Options struct {
	Runner  *execx.Runner
	Chart   string
	Values  helm.Values
	Timeout time.Duration
	Log     io.Writer
}

// warningLine matches the API server warnings helm prints, e.g.
// "W1011 12:00:00.000000 12345 warnings.go:70] policy/v1beta1 PodDisruptionBudget is deprecated in v1.21+, unavailable in v1.25+".
var warningLine = regexp.MustCompile(`warnings\.go:\d+\] (.*)`)

// Run checks the chart on one Kubernetes version and removes the cluster afterwards.
func Run(ctx context.Context, version string, o Options) (res scenario.Result) {
	res.Scenario = "k8s-" + version
	image, ok := NodeImages[version]
	if !ok {
		res.Verdict = scenario.Inconclusive
		res.Summary = "no kind node image for Kubernetes " + version + "; supported: " + strings.Join(Versions(), ", ")
		return res
	}
	fmt.Fprintf(o.Log, "Creating a Kubernetes %s cluster…\n", version)
	c, err := sandbox.Create(ctx, o.Runner, image)
	if err != nil {
		res.Verdict = scenario.Inconclusive
		res.Summary = "the cluster did not start: " + err.Error()
		return res
	}
	defer func() {
		if err := c.Delete(context.Background(), o.Runner); err != nil {
			fmt.Fprintf(o.Log, "cleanup: %v\n", err)
		}
	}()

	h := &helm.Client{Runner: o.Runner, Kubeconfig: c.Kubeconfig, Timeout: o.Timeout}
	k := &kube.Client{Runner: o.Runner, Kubeconfig: c.Kubeconfig}
	const release, ns = "hb-matrix", "hb-matrix"

	mark := len(o.Runner.Transcript())
	stderr, err := h.InstallCapture(ctx, release, ns, o.Chart, o.Values)
	repro := []string{"kind create cluster --image " + image, reproLine(o.Runner.Transcript()[mark].Line)}
	warnings := apiWarnings(stderr)

	if err != nil {
		return installFailed(ctx, res, version, err, warnings, repro)
	}

	if bad := unhealthy(ctx, k, ns); len(bad) > 0 {
		res.Verdict = scenario.Fail
		res.Summary = "installs on Kubernetes " + version + " but does not become healthy"
		res.Findings = append(res.Findings, scenario.Finding{
			ID:        "HB-K8S-002",
			Title:     "Workloads are not healthy on Kubernetes " + version,
			Severity:  "high",
			Evidence:  append(bad, warnings...),
			Impact:    "The install reports success, but the application does not run on this version.",
			Fix:       "Compare the pod events and logs with an older version; typical causes are changed defaults (cgroup v2, seccomp, sidecar handling) or a removed feature gate.",
			Reproduce: repro,
		})
		return res
	}

	if len(warnings) > 0 {
		res.Verdict = scenario.Fail
		res.Summary = fmt.Sprintf("runs on Kubernetes %s, with %d API warning(s)", version, len(warnings))
		res.Findings = append(res.Findings, scenario.Finding{
			ID:        "HB-K8S-003",
			Title:     "The API server warns about resources this chart creates on Kubernetes " + version,
			Severity:  "medium",
			Evidence:  warnings,
			Impact:    "A deprecated API keeps working until the release that removes it; then installs and upgrades fail.",
			Fix:       "Move to the API version the warning names, gated on `.Capabilities.APIVersions.Has` if older clusters still need the old one.",
			Reproduce: repro,
		})
		return res
	}
	res.Verdict = scenario.Pass
	res.Summary = "installs and runs on Kubernetes " + version + " with no API warnings"
	return res
}

// Attribute keeps a failed install (HB-K8S-001) as a finding only if the chart installed on
// another version: otherwise the failure is not about a Kubernetes version, and the result is
// INCONCLUSIVE. A version installed when its result is not an install failure or INCONCLUSIVE.
// A failure that names a missing API stays a finding either way: the API was removed in a
// Kubernetes version, perhaps one older than every version tested.
func Attribute(results []scenario.Result) []scenario.Result {
	for _, r := range results {
		if r.Verdict != scenario.Inconclusive && !installFailure(r) {
			return results
		}
	}
	out := make([]scenario.Result, len(results))
	for i, r := range results {
		if installFailure(r) && !missingAPI(r) {
			r.Verdict = scenario.Inconclusive
			r.Summary += "; it installed on no version tested, so the cause is not a Kubernetes version (run helmbulance test)"
			r.Findings = nil
		}
		out[i] = r
	}
	return out
}

// missingAPI reports whether an install failure says the cluster does not serve a built-in kind the
// chart uses, which is how a removed API version fails. A kind from another group needs a CRD,
// and kind clusters have none, so that failure says nothing about the Kubernetes version.
func missingAPI(r scenario.Result) bool {
	for _, e := range r.Findings[0].Evidence {
		if (strings.Contains(e, "no matches for kind") || strings.Contains(e, "resource mapping not found")) && builtInGroup(e) {
			return true
		}
	}
	return false
}

// builtInGroups are the API groups Kubernetes serves itself. A fixed list, since some CRD groups
// (gateway.networking.k8s.io, snapshot.storage.k8s.io) also end in .k8s.io.
var builtInGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "policy": true, "extensions": true, "autoscaling": true,
	"networking.k8s.io": true, "rbac.authorization.k8s.io": true, "admissionregistration.k8s.io": true,
	"apiextensions.k8s.io": true, "apiregistration.k8s.io": true, "storage.k8s.io": true,
	"scheduling.k8s.io": true, "coordination.k8s.io": true, "certificates.k8s.io": true,
	"discovery.k8s.io": true, "events.k8s.io": true, "node.k8s.io": true,
	"flowcontrol.apiserver.k8s.io": true, "resource.k8s.io": true,
}

var inVersion = regexp.MustCompile(`in version "([^"]*)"`)

// builtInGroup reports whether a "no matches for kind … in version "<group>/<version>"" message
// names a built-in group. The core group has no slash ("v1"). No "in version" means unknown.
func builtInGroup(msg string) bool {
	m := inVersion.FindStringSubmatch(msg)
	if m == nil {
		return false
	}
	group, _, ok := strings.Cut(m[1], "/")
	if !ok {
		group = ""
	}
	return builtInGroups[group]
}

func installFailure(r scenario.Result) bool {
	return r.Verdict == scenario.Fail && len(r.Findings) > 0 && r.Findings[0].ID == "HB-K8S-001"
}

// installFailed is the result when helm install failed on Kubernetes version. A timeout, a pull
// error or a cluster that did not answer says nothing about the version, so it is INCONCLUSIVE.
func installFailed(ctx context.Context, res scenario.Result, version string, err error, warnings, repro []string) scenario.Result {
	if scenario.IsEnvironmentError(ctx, err) {
		res.Verdict = scenario.Inconclusive
		res.Summary = "the install did not finish on Kubernetes " + version + ": " + helmMessage(err)
		return res
	}
	res.Verdict = scenario.Fail
	res.Summary = "does not install on Kubernetes " + version
	res.Findings = append(res.Findings, scenario.Finding{
		ID:        "HB-K8S-001",
		Title:     "The chart does not install on Kubernetes " + version,
		Severity:  "high",
		Evidence:  append([]string{"`helm install` failed: " + helmMessage(err)}, warnings...),
		Impact:    "Clusters on Kubernetes " + version + " cannot run this chart. Upgrading the cluster breaks existing installs too.",
		Fix:       "Look for an API version this release removed (`kubectl api-versions` on the cluster) or a field it no longer accepts, and gate it on `.Capabilities.KubeVersion` / `.Capabilities.APIVersions.Has`.",
		Reproduce: repro,
	})
	return res
}

func apiWarnings(stderr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		m := warningLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		w := strings.Trim(strings.TrimSpace(m[1]), `"`)
		w = strings.TrimPrefix(w, "Warning: ")
		if !seen[w] {
			seen[w] = true
			out = append(out, "API warning: "+w)
		}
	}
	return out
}

// unhealthy waits for pods to settle and lists those that are not ready. Restarts alone are not
// counted: many apps restart once at first start while their database comes up.
func unhealthy(ctx context.Context, k *kube.Client, ns string) []string {
	// Restarts at the first read: a pod that crashes and comes back while Settle watches is a
	// problem even if it is ready again.
	var first map[string]int
	return scenario.Settle(ctx, func() []string {
		pods, err := k.Pods(ctx, ns)
		if err != nil {
			return []string{"could not read pods: " + err.Error()}
		}
		jobs, err := k.FailedJobs(ctx, ns)
		if err != nil {
			return []string{"could not read Jobs: " + err.Error()}
		}
		if first == nil {
			first = map[string]int{}
			for _, p := range pods {
				first[p.UID] = p.Restarts
			}
		}
		return append(append(notReady(pods), restartedSince(pods, first)...), jobs...)
	})
}

// restartedSince lists pods that restarted more than they had in first, by pod UID.
func restartedSince(pods []kube.Pod, first map[string]int) []string {
	var out []string
	for _, p := range pods {
		if n := p.Restarts - first[p.UID]; n > 0 && !p.FinishedJob() {
			out = append(out, fmt.Sprintf("pod `%s` restarted %d time(s) while Helmbulance watched it", p.Name, n))
		}
	}
	return out
}

// notReady lists the pods that are not ready.
func notReady(pods []kube.Pod) []string {
	var bad []string
	for _, p := range pods {
		if !p.Ready && !p.FinishedJob() {
			bad = append(bad, fmt.Sprintf("pod `%s` is not ready (%d restart(s))", p.Name, p.Restarts))
		}
	}
	return bad
}

func reproLine(line string) string {
	fields := strings.Fields(line)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "--kubeconfig" {
			fields[i+1] = "$KUBECONFIG"
		}
	}
	return strings.Join(fields, " ")
}

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
