# Helmbulance architecture

Status: v0.1 in progress. This document sets the boundaries the code is built inside; it changes when the code teaches us something. It went through an architecture review and a security review, and the decisions below include their findings.

## 1. What the system does

Given a Helm chart and its values:

1. Bring up a disposable Kubernetes cluster that behaves like a production one where it matters for the test.
2. Install the chart and work out what it deployed.
3. Run day-two scenarios against the live release: lifecycle operations (upgrade, uninstall and reinstall, rotation), faults, and network checks.
4. Decide, with evidence, whether each scenario exposed a defect, and report it in a form people and CI can use.
5. Never harm anything outside the sandbox.

### What it finds, and what it does not

Helmbulance finds problems in how a chart deploys and operates an application:

- secrets that change or vanish on upgrade;
- data that does not survive a reinstall;
- rotations that leave components out of step;
- ports and datastores reachable from where they should not be;
- probes that depend on things outside the process.

It is not an application security scanner. A bug in the application's own code may show up as a symptom, for example a probe that fails under load. Finding the root cause is still a person's job. The report says "something is wrong here", not "the bug is on this line".

## 2. Principles

- **Reproduced or not reported.** A finding exists only if the cluster reproduced it. Rules and optional AI analysis produce hypotheses, which become scenario parameters; they are never findings on their own.
- **Control before treatment.** Every assertion is checked twice: on an untouched release or a neutral operation (the control), and after the action (the treatment). A difference that already shows in the control is reported as such, not blamed on the action.
- **Three verdicts.** `PASS`, `FAIL`, or `INCONCLUSIVE`.
  - A timeout, a failed install or a cluster that never settles is `INCONCLUSIVE`, never a finding. Errors that come from the run rather than the chart (helm's `--wait` timing out, a killed process, an image pull or registry rate limit, a cluster that does not answer) are always `INCONCLUSIVE`. A `FAIL` comes from the chart: an API or render rejection, a failing call to the chart's own admission webhook, or a check that failed.
  - `matrix` blames a Kubernetes version for a failed install only if the chart installed on another version, or if the error names a missing built-in API (`no matches for kind` in a group Kubernetes serves itself, such as `policy/v1beta1`), which a version removed. A missing kind from any other group needs a CRD, and kind clusters have none, so that failure is INCONCLUSIVE.
  - A scenario with nothing to test (no HTTP Service, no `--from-version`) is `SKIPPED`. It is not a verdict on the chart and counts for nothing: a run in which everything was skipped is `INCONCLUSIVE`.
  - Exit codes: 0 `PASS`, 1 `FAIL`, 2 usage or setup error, 3 `INCONCLUSIVE`.
  - From v0.2, a `FAIL` is re-run on a fresh release, and only a failure that reproduces twice is reported.
- **App-agnostic first.** Scenarios rely on what Kubernetes can observe. Knowledge of a specific application is an optional plug-in.
- **The chart is untrusted code.** A chart can create privileged pods, mount host paths and run any image. Section 4 says how far each mode protects against that.
- **The measurement must not change what it measures.** No sandbox-wide policy, admission rule or traffic may alter the behaviour under test. Every oracle asks: did I produce this signal myself?

## 3. Components

```
                ┌────────────────────────── helmbulance CLI (Go) ───────────────────────────┐
chart + values ─▶ Chart model ─▶ Sandbox ─▶ Scenario engine ─▶ Oracles ─▶ Reporter            │──▶ Markdown / JSON / SARIF
                │   (render,      (kind)     (control/act/      (Secrets, PVC sentinels,      │
                │    lookup scan)            observe)            health, reachability)        │
                └────────────────────────────────────────────────────────────────────────────┘
```

### 3.1 CLI and tool drivers

- **Language:** Go, distributed as one binary.
- **Helm and kind** run as commands, not as embedded libraries. Two reasons:
  - Helmbulance tests the Helm version the user actually runs. Helm 4 applies resources server-side and Helm 3 client-side, so results can differ between them.
  - The reproduction command in a report is the command that really ran.
- **kubectl** is used for reads in v0.1. client-go replaces it where watching or ephemeral containers need it.
- **The tool's own clusters only.**
  - Every sandbox gets its own kubeconfig (mode 0600, in `~/.helmbulance/clusters/<name>/`), and the SHA-256 of the cluster CA is recorded when the cluster is created.
  - Reusing a cluster (`--cluster`) checks that fingerprint.
  - There is no flag to run scenarios against any other cluster. Lifecycle and network scenarios uninstall releases and open connections; they must never meet a real cluster.
- **`--set` values** are masked in every transcript and report.

### 3.2 Chart model

The chart is read on the host before any sandbox exists, so reading it is hardened:

- symlinks and paths outside the chart are refused;
- the size and file count of chart archives are capped;
- dependencies are never fetched or updated (only vendored `charts/` or a lock-verified set);
- the Helm version in use is reported, and the project's CI runs `govulncheck`.

The model records what the chart creates:

| Area | What is recorded |
|---|---|
| Workloads | Containers, ports, probes, security contexts, update strategy, volumes, and env vars read from Secrets |
| Secrets | Which workload reads which key |
| Network | Services and NetworkPolicies |
| Storage | PVCs and StatefulSet volume templates |
| Cluster-scoped resources | CRDs, cluster roles, webhooks. Their presence turns off parallel runs. |
| `lookup` | Found statically in every template, subcharts included, then confirmed after install by comparing a client-side render with a server-side dry run |

**Where `lookup` matters.** Argo CD renders charts with `helm template`, where `lookup` returns nothing. Flux's helm-controller runs real Helm installs, where `lookup` works. A `lookup` finding names Argo CD and does not claim to apply to "GitOps" in general.

### 3.3 Sandbox

**v0.1:**
- kind v0.24 or newer, with a pinned node image. Its default CNI, kindnet, enforces NetworkPolicy through kube-network-policies.
- Enforcement is proven when the sandbox starts, with a canary: a deny-all policy and a test pod. If the canary fails, network scenarios report `INCONCLUSIVE`.
- kind's default `standard` StorageClass (local-path, reclaim policy Delete). Reports say so, because data-survival results depend on the reclaim policy.
- Pod Security admission in `warn` and `audit` mode. The reached level (restricted, baseline or privileged) is reported as information. Enforcing it would stop many charts from installing and would change what is measured.
- No ingress controller and no metrics-server in v0.1, because no v0.1 scenario needs them. When an HTTP entry point is needed, it will be a Gateway API implementation (ingress-nginx was retired in March 2026).
- `listenAddress` and `apiServerAddress` bound to `127.0.0.1`.
- Memory and CPU limits on the node containers.

**Speed and image pulls:**
- Scenarios run one after another in one cluster (`--parallel=1` by default). A full application with a database and a search engine, installed three or four times at once, does not fit in a laptop's Docker memory, and running out of memory is the largest source of flaky results.
- Images pulled for the first install stay on the node, so later installs start faster.
- `--keep-cluster` and `--cluster` reuse a sandbox across runs.
- A registry mirror can come later. One pull-through mirror serves only one upstream registry, so it is not worth it in v0.1.
- Images that do not exist for the host's architecture are reported as "not installable on arm64", not as a chart finding.

### 3.4 Scenario engine

Each scenario goes through these phases:

| Phase | What it does |
|---|---|
| `Applies(model)` | Decides whether the chart has what the scenario tests |
| `Setup` | Installs a fresh release in its own namespace, under its own release name |
| `Control` | Records the baseline, or runs the neutral form of the operation |
| `Act` | Performs the operation |
| `Observe` + `Assert` | Waits until the cluster settles (no change for N seconds, with a timeout), then compares against the control |
| `Cleanup` | Uninstalls, deletes the namespace, and checks for leftovers: orphan PVCs, Secrets, Jobs, hooks |

A run's deadline, Ctrl-C or a SIGTERM to helmbulance (`kill`, `timeout`, `docker stop`) stops every tool call with the children that stay in its process group: the group gets SIGTERM, so helm can mark the release failed, and is killed 30 s later. `Cleanup` still runs after that, with its own 10 minute limit. A second Ctrl-C or SIGTERM quits at once and leaves the cluster as it is.

#### Scenario definitions

**`upgrade-without-values`** (implemented). Helm reuses the previous values only when an upgrade gets no values flags at all, so a plain "upgrade without values" changes nothing and cannot find the bug. The scenario has two steps:
- **Control:** `helm upgrade` with no values flags. Helm reuses the install values, so nothing should change. Secrets that change here are regenerated on every render.
- **Treatment:** `helm upgrade --reset-values -f <files>`. This leaves out every install-time `--set`, which is what happens when someone upgrades months later from the values file in git. Keys that change or disappear here, and did not already change in the control, are values the chart does not keep once set.

**`chart-upgrade`** (implemented).
- Install the earlier version (`--from-version`), check that the release runs it, upgrade it to itself as the control, then upgrade to the chart under test with the same values. The report names both versions helm installed.
- helm ignores `--version` for a chart on disk, so a local chart needs `--from` with the earlier chart, and a local `--from` must be the version `--from-version` names. Both are checked before anything runs (exit 2).
- Report a failed upgrade with Helm's error, Secrets the new version changes or drops, and workloads that are not healthy afterwards.

**`zero-downtime`** (implemented).
- A pod in the release namespace sends one request every 200 ms to the most HTTP-like Service port (`--traffic-target`, `--traffic-path` to choose). ExternalName, headless and `externalIPs` Services are never chosen, so the traffic stays on the release's pods.
- Control: 20 s with no change, which must be clean; a control with failures runs once more before the run is inconclusive. Treatment: the same traffic during the version upgrade (with `--from-version`), or during a rolling restart of every Deployment and StatefulSet. Only failures from the start of the operation on count, measured on the traffic pod's own clock; a window with no requests is inconclusive.
- Reports failed requests, the longest outage, and the settings that explain it: Recreate, a single replica, no readiness probe, no preStop.
- This is a measurement, not a load test: about five requests a second, inside the sandbox, to the release's own Service.
- The traffic pod runs `busybox` pinned by digest, as a non-root user with no ServiceAccount token, no capabilities and a read-only root filesystem (Pod Security `restricted`). It still lives in the release namespace, so the chart's own mutating webhooks can change it.

**`helmbulance scan`** (implemented). A static pre-screen: no cluster, seconds. It reports candidates, never findings; each names the `helmbulance test` run that confirms or rejects it. It exits 0 unless `--strict`.
- **Immutable fields:** render `--from-version` as an install and the chart as an upgrade (`--is-upgrade`), compare selectors, StatefulSet `serviceName`/`volumeClaimTemplates`/`podManagementPolicy`, Job templates and PVC access modes. Hooks and Jobs named after `.Release.Revision` are skipped (a real upgrade gives them a new name).
- **First-upgrade restart:** a pod-template checksum of an included template that keeps generated values with `lookup` (helpers it includes are followed one level).
- **Regenerated Secrets:** render twice; keys that differ in a Secret that is not a hook and whose template (or helper) does not use `lookup`.
- Checked against the cases confirmed on a cluster: yugabyte, JupyterHub, SeaweedFS and docker-registry are flagged; zammad's revision-named Job and this project's OpenArchiver chart are not.

**`helmbulance matrix`** (implemented). A command rather than a scenario, because each Kubernetes version needs its own cluster.
- For each version (kind node images pinned by digest: 1.34, 1.35, 1.36, 1.37): create a cluster, install, wait for the pods, collect the API server's warnings from helm's stderr, delete the cluster.
- Reports a failed install, pods that never become ready (restarts alone are not counted, apps often restart once at first start), and warnings about deprecated APIs.
- Known deprecations are also caught statically (`helm lint`, pluto); what the matrix adds is whether the chart actually runs on each version.

**`reinstall`** (v0.1).
- Write a sentinel file into each PVC.
- Uninstall, install again, and check the sentinels, the Secrets and the health of the release.
- Report which PVCs were deleted with the release, and say that the result depends on the StorageClass reclaim policy.

**`gitops-render`** (v0.1).
- Find `lookup` statically.
- Install the chart, then compare a client-side render with a server-side dry run of the same values.
- Secrets that differ would be regenerated on every Argo CD sync.

**`network-isolation`** (v0.1, or v0.2 if time runs short).
- **Treatment:** the release with its NetworkPolicies. **Control:** the same release with the chart's NetworkPolicy objects removed for a moment. No values key is needed.
- **Port discovery:** read `/proc/net/tcp{,6}` from an ephemeral container in each pod's network namespace. `containerPort` is informational only, and the ports that matter are often not declared.
- **Output:** an access matrix (source namespace × destination port). A finding is raised only for listeners that no Service exposes and for ports of bundled datastores reachable from foreign namespaces. A reachable Service port on its own is not a defect.

**`secret-rotation`** (v0.2).
- Change one value that flows into a Secret, then upgrade.
- Every pod that reads that Secret must get a new UID, and no credential errors may follow.
- Restarting is not sufficient. Some databases keep the password they were created with, so after a restart the app is locked out. The health and credential oracles must also pass.

**`immutable-fields`** (v0.2).
- Render the old and new values.
- Find changes to immutable fields, such as StatefulSet volume templates, `serviceName` and selectors.
- Confirm each one with a single upgrade.

**`process-kill`** (v0.2).
- An ephemeral container joins the target container's process namespace, runs as the target's user, and kills a process that is not PID 1.
- PID 1 ignores signals it has no handler for, even from root inside its own namespace. Killing PID 1 would also only test the container restart, not the liveness probe.
- The pod is replaced afterwards, because ephemeral containers cannot be removed.

**`probe-abuse`** (v0.3).
- Applies load or simulates an outage of a dependency.
- Checks whether probes fail for reasons outside the process.
- The load generator has a CPU limit, and the same load is applied to an endpoint the probes do not use (the control). Only a probe that fails where the control does not is a finding. Otherwise a laptop's CPU starvation would look like a chart bug.

### 3.5 Oracles and their limits

| Oracle | How | False positives | False negatives | Guard |
|---|---|---|---|---|
| Secret drift (implemented) | HMAC-SHA256 of every key of the Secrets the release has rendered in any revision of the run (`helm get manifest` and `helm get hooks`), so a "generate once" Secret kept with `resource-policy: keep` stays compared, with a random per-run key that is never stored, read once two reads in a row agree. Changed and removed keys are findings; added keys, values moved to another key and Secrets the release never rendered are named in the summary. A move whose old key a workload still reads is a removed key. When no key was compared, the summary says Secret drift was not tested. | Rotation scenarios, where change is expected; a Secret the chart renders and the app then rewrites once, later than the settle window | Generated values kept in ConfigMaps; keys removed from `stringData`, which stay in the Secret; Secrets the release does not render, such as passwords a hook Job writes with `kubectl create secret`; a new key that a workload switches to | Only the release's own Secrets, after a settle window (done); expected-change set per scenario; ConfigMaps scanned too (v0.2) |
| Data survival | Sentinel file written into each PVC by an ephemeral container running as the target's user and group | Reclaim policy Delete in the sandbox | App re-initialised its data even though the file survived; data on `emptyDir` | Combine with the health and credential oracles; report `emptyDir` data paths; `INCONCLUSIVE` if the sentinel cannot be written |
| Health | Ready conditions, restart counts and termination reasons, compared with the control | Start-up crash loops that also happen in the control | A dead process stays "Ready" when there is no liveness probe | Compare deltas, not absolute counts; settle window |
| Credential errors | Log patterns per known component, within a time window | Errors caused by Helmbulance's own probes | Apps that do not log auth failures; logs lost on restart | Evidence only, never a verdict on its own; probe pod IPs excluded; `--previous` logs read |
| Rollout coverage | Pods that read a changed Secret got new UIDs | Apps that hot-reload mounted Secrets | A restart that does not fix the mismatch (database keeps the old password) | Always combined with health and credentials |
| Reachability | Probe pod connects from the release namespace and from a foreign namespace | Policy enforcement is asynchronous right after install | Undeclared ports | CNI canary, bounded retries, port discovery |

Every oracle has a fixture: a small chart with the defect and a fixed twin (`testdata/charts/`). An oracle change is only accepted when the bad fixture still fails and the good one still passes. The fixtures already caught one bug in the tool itself: a fixture that wrote its key through `stringData` passed when it should have failed, because Kubernetes keeps a key that is removed from `stringData`.

### 3.6 Reporter

- **Formats:**
  - Markdown for people.
  - JSON with a `schemaVersion`.
  - SARIF from v0.2.
- **SARIF details:** each finding points at the template line or values key it comes from, because code scanning needs a location. Fingerprints are stable (scenario, resource and key). Private repositories need GitHub Code Security to show SARIF, so the Markdown report is also offered as a job summary and pull request comment.
- **Exit codes:** `0` clean, `1` findings, `2` tool error, `3` incomplete (for example, the chart did not install).
- **Redaction:**
  - Secret `data` and `stringData`, and Helm release Secrets in full.
  - URL user info.
  - env and ConfigMap values whose names look secret.
  - Any known secret value found verbatim in logs or events.
- **Escaping:** text that comes from the cluster (names, events, logs) is escaped and length-limited before it reaches Markdown, a terminal or a pull request comment.
- **Failure bundle:** when a scenario fails or is inconclusive, `kind export logs`, events, `describe` output and the Helm history are saved next to the report.

### 3.7 Optional AI hypotheses (v0.3)

A model may read the chart and propose hypotheses, for example "the readiness probe calls the database, so a slow query makes it fail". Each hypothesis maps to a scenario with parameters, and is reported only if that scenario reproduces it.

The AI step is off by default and never runs commands. Values are redacted before anything is sent to a provider, and the user is asked first.

## 4. Security model

| Mode | Who wrote the chart | What protects the machine |
|---|---|---|
| Local, own chart (default for a local directory) | The user | The same as `ct install`: the user trusts their own chart. kind nodes are privileged containers, so the container runtime is **not** a security boundary. |
| Local, third-party chart (v0.3: automatic for OCI, URL and repository charts, `--untrusted` otherwise; until then `--allow-remote` runs them unprotected) | Someone else | Pod Security `baseline` enforced through an `AdmissionConfiguration` file on the node, which the chart cannot change through the API. The chart is installed through an impersonated identity that cannot edit admission webhooks or policies, namespace labels, RBAC bindings or Helmbulance's own namespace. Host access (privileged, `hostPath`, `hostNetwork`, `hostPID`) is refused. Running inside a VM with no host mounts is recommended, and required for charts that need host access. |
| CI (GitHub Actions) | The pull request author | The runner VM is disposable, but its tokens and the Actions cache are not. The workflow runs on `pull_request`, never `pull_request_target`. The test job gets `contents: read` only, no secrets, no `id-token`, `persist-credentials: false`, and saves no caches. SARIF upload runs in a separate job that only reads the artefact. The Action warns on self-hosted runners that are not ephemeral. |

**Network scenarios that send load or unusual traffic (v0.3)** run only when egress is cut **outside** the cluster:
- the Docker network of the sandbox, or the host firewall;
- CoreDNS resolves only `cluster.local`;
- the Docker gateway, `host.docker.internal`, link-local and LAN ranges are dropped.

NetworkPolicy cannot provide this, because the chart can add its own policies and `hostNetwork` pods ignore them. Where the cut cannot be set up, these scenarios refuse to run.

Targets are checked when the connection is made, not when a name is resolved. A target must be the IP of a running pod in the release namespace. ExternalName and selector-less Services, `externalIPs` and load balancer addresses are refused, so a chart cannot point Helmbulance's traffic at a third party. Rate and duration caps are compiled into the probe binary; flags can only lower them.

**Supply chain of the tool:**
- Release binaries and the probe image are signed with cosign (keyless) and published with an SBOM and SLSA provenance.
- Images are published under `ghcr.io/egezamb/`. The probe image digest is embedded in the binary.
- Node images and add-on manifests are pinned by digest.
- The GitHub Action verifies the binary signature before running it, and users are told to pin the Action by commit SHA.

## 5. No hosted infrastructure

Helmbulance runs where the user runs it: on their machine or in their CI. There is no Helmbulance server, no account and no collected data.

The project itself needs only free GitHub services:
- the repository;
- Releases for binaries;
- GitHub Container Registry for the probe image;
- the Action on Marketplace;
- optionally, Pages for documentation.

A hosted service (submit a chart, get a report) is a possible later product, not part of this plan. If it ever exists, every job runs in its own microVM, charts are fetched outside the VM, and results coming out of it are treated as untrusted.

## 6. Relation to existing tools

| Tool | What it does | How Helmbulance relates |
|---|---|---|
| Trivy, Checkov, kube-linter, Kubescape, Polaris | Static checks of rendered manifests | Complementary: they read YAML, Helmbulance observes behaviour. Their results may be included in the report later. |
| chart-testing (`ct`) | Lint, install, `helm test` | Helmbulance starts where `ct install` stops: after the first install. |
| helm-unittest | Assertions on rendered templates | No cluster. Helmbulance's fixtures could use it for render-level checks. |
| Terratest, kuttl, Chainsaw, Testkube | Frameworks to write your own end-to-end tests | They need a test written for each chart. Helmbulance ships ready scenarios with control and treatment built in. Exporting scenarios in kuttl or Chainsaw format is a possible bridge. |
| Chaos Mesh, Litmus | Fault injection in long-running clusters | Platforms installed in a cluster. Helmbulance is a short-lived test run focused on chart lifecycle. |
| helm-diff | Shows what an upgrade changes | Shows the diff. Helmbulance shows whether the change breaks something. |

## 7. Repository layout

```
cmd/helmbulance/        CLI entry point
internal/execx/         command runner with masked transcripts
internal/sandbox/       kind lifecycle and ownership checks
internal/helm/          helm CLI driver
internal/kube/          cluster reads
internal/oracle/        oracles
internal/scenario/      scenarios
internal/report/        Markdown and JSON (SARIF later)
testdata/charts/        fixture charts: one defect and one fixed twin each
docs/                   architecture, scenario reference, case studies
```

## 8. Milestones

| Version | Scope |
|---|---|
| v0.1 | CLI, kind sandbox, `upgrade-without-values` (done), `reinstall`, `gitops-render`, `network-isolation` (TCP, CNI canary, port discovery), Markdown and JSON, fixtures, a composite GitHub Action that runs the CLI |
| v0.2 | `secret-rotation`, `immutable-fields`, `process-kill`, SARIF, double-run confirmation of failures, signed releases with SBOM |
| v0.3 | `probe-abuse` with egress isolation outside the cluster, third-party chart mode, application plug-ins, optional AI hypotheses |
| Later | Running against popular community charts and sending fixes upstream; a hosted service only if there is demand |
