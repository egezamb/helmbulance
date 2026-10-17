# Helmbulance

**Behavioural tests for Helm charts.** Helmbulance installs a chart into a throwaway Kubernetes cluster and runs the things that go wrong in production: upgrades without the original values, uninstall and reinstall, password rotation, network isolation, broken processes, hostile traffic. It reports what actually broke, with evidence and a suggested fix.

> **Status: early.** The CLI runs three scenarios (`upgrade-without-values`, `chart-upgrade`, `zero-downtime`) end to end on kind. Nothing is released yet. See [docs/architecture.md](docs/architecture.md) for the design.

## Why

Static scanners (Trivy, Checkov, kube-linter, Kubescape, Polaris) read rendered YAML. `ct install` checks that a chart installs and that `helm test` passes. Both are useful, and neither tells you what happens on day two.

The most expensive chart bugs are behavioural. They only show up when something happens to a running release:

- An upgrade without the original `--set` flags silently drops an encryption key, and the app keeps running without it.
- A password rotation restarts the app but not the database it talks to.
- A NetworkPolicy exists, but the cluster's CNI never enforces it.
- A liveness probe calls the database, so a slow database restarts every app pod at once.

None of these produce an invalid manifest, so no linter flags them. Each one is found in minutes by doing the thing and watching what happens.

## How it works

1. **Install** the chart into an ephemeral [kind](https://kind.sigs.k8s.io/) cluster with each values file you give it.
2. **Run scenarios** against the live release. Each scenario is a small script: act (upgrade, delete, rotate, connect, flood), then assert on the cluster's state.
3. **Report** every failure with the command that reproduces it, the evidence (events, logs, rendered diff), the impact, and a suggested patch.

AI review can propose findings. Helmbulance only reports a finding once the cluster has reproduced it. In our case study, one of the AI reviewers' claims turned out to be wrong when tested, and another turned out to be worse than expected. Both were only settled by testing on a cluster.

## Scenarios

| Scenario | Question it answers | Status |
|---|---|---|
| `upgrade-without-values` | Does an upgrade from the values files, without the install-time `--set` flags, change or drop Secret values? | **works** |
| `chart-upgrade` | Does upgrading from an earlier chart version, with the same values, succeed, keep the Secrets, and leave the workloads healthy? | **works** |
| `zero-downtime` | Do requests fail while the pods are replaced, during a rolling restart or a version upgrade? How long is the outage? | **works** |
| `helmbulance scan` | Static pre-screen, no cluster, seconds: immutable fields changed since `--from-version`, checksums that change on the first upgrade, Secrets generated anew on every render. Lists **candidates** and the `test` run that confirms each. | **works** |
| `helmbulance matrix` | Does the chart install and run on every supported Kubernetes version (1.34–1.37), and does the API server warn about what it creates? | **works** |
| `reinstall` | After `helm uninstall` and `helm install`, do data and credentials come back? | v0.1 |
| `gitops-render` | Does the chart render differently without cluster access, as Argo CD does (`lookup` returns nothing there)? | v0.1 |
| `network-isolation` | Are undeclared ports and bundled datastores unreachable from other namespaces, and is the NetworkPolicy actually enforced? | v0.1 |
| `secret-rotation` | When a password changes, does every component that uses it restart and keep working? | v0.2 |
| `immutable-fields` | Does a values change hit an immutable field (StatefulSet volume templates, selectors) and break the upgrade? | v0.2 |
| `process-kill` | If a process inside the container dies, does the liveness probe notice? | v0.2 |
| `probe-abuse` | Do probes depend on things outside the process, such as the database, so that outside load or an outage makes them fail? | v0.3 |

## Case study: Open Archiver

We wrote a Helm chart for [Open Archiver](https://github.com/LogicLabs-OU/OpenArchiver), a self-hosted email archive (upstream request: [#242](https://github.com/LogicLabs-OU/OpenArchiver/issues/242)). We built it in milestones, and every milestone was tested on kind and reviewed for architecture and security. These are the problems that testing on a live cluster found, and how each was solved:

| Problem | How it showed up | Fix |
|---|---|---|
| **Storage encryption key silently lost.** An upgrade without the key in values removed it from the Secret. The app does not fail without a key: it writes new mail unencrypted and serves the old files as unreadable bytes. | Upgraded without the value, then inspected the Secret and the app's behaviour. | Keep the stored key through `lookup`, refuse a different key at render time, and add an option that stops the pod when the key is missing. |
| **Database password double-encoded in the app.** A password with `@ : / # ?` worked when written plainly and failed when correctly URL-encoded, the opposite of what users expect. | Ran the app's own helper against a real PostgreSQL with a matrix of passwords. | Upstream fix: [OpenArchiver#493](https://github.com/LogicLabs-OU/OpenArchiver/pull/493). In the chart, the password is passed as `PGPASSWORD` and never inside the URL. |
| **Password rotation broke the job queue.** Changing the Valkey password restarted the app but not Valkey, so every queue connection failed. | Rotated the password with `helm upgrade` and checked both pods and the app's logs. | A checksum annotation on every pod that reads the password. |
| **PostgreSQL password change locked the app out.** PostgreSQL reads its passwords only when it creates the database, so the chart updated the Secret and the app could no longer log in. | Upgraded with a new password. | The chart refuses the change and explains how to rotate the password safely. |
| **Internal API port reachable cluster-wide.** The app's backend listens on a port that the Service does not expose, but every pod could still reach it. | Connected from another namespace, with and without the policy. | A NetworkPolicy that admits only the web port. The bundled services accept connections only from the app and have no egress. |
| **App connected as a database superuser.** A SQL injection would have reached `COPY … TO PROGRAM`. | Queried `pg_roles` on the live database. | A separate non-superuser app role that owns the database. The superuser can log in through the local socket only. |
| **One more finding, in responsible disclosure.** | Reproduced on kind. | Details after the upstream fix. |
| **A reported problem that was not real.** A reviewer claimed PostgreSQL would be SIGKILLed on every stop. | Deleted the pod and timed it: a clean shutdown in one second, because containerd honours the image's `STOPSIGNAL`. | No change. This is why findings need a cluster to confirm them. |

The chart will be published separately; this repository will link to it.

More case studies:

- [Sweep of ~1000 Artifact Hub charts](docs/case-studies/sweep-2026-10.md): what `helmbulance scan` found and what was confirmed, including a selector bug in docker-registry-ui whose open fix was verified ([Joxit/helm-charts#22](https://github.com/Joxit/helm-charts/pull/22)).
- [The first upgrade restarts pods for nothing](docs/case-studies/first-upgrade-restart.md): a common checksum pattern, confirmed in JupyterHub, Semaphore and SeaweedFS ([#3996](https://github.com/jupyterhub/zero-to-jupyterhub-k8s/issues/3996), [#34](https://github.com/semaphoreui/charts/issues/34), [#11719](https://github.com/seaweedfs/seaweedfs/issues/11719)).
- [yugabyte 2026.1.2](docs/case-studies/yugabyte-2026.1.2.md): a patch release whose StatefulSet selector change breaks every upgrade from 2026.1.0 and 2026.1.1 ([reported](https://github.com/yugabyte/charts/issues/240)).
- [docker-registry and openfaas](docs/case-studies/oss-charts-2026-10.md): a Secret regenerated on every upgrade, and a correct negative.

## Roadmap

- **v0.1:** the CLI with `upgrade-without-values`, `reinstall`, `gitops-render` and `network-isolation`, Markdown and JSON reports, and a GitHub Action.
- **v0.2:** `secret-rotation`, `immutable-fields`, `process-kill`, SARIF for GitHub code scanning, and signed releases.
- **v0.3:** `probe-abuse`, a hardened mode for charts you did not write, and optional AI hypotheses that the cluster must confirm.
- **Later:** running against popular community charts and sending fixes upstream.

Helmbulance runs entirely on your machine or in your CI. There is no server and no account.

## Try it

Requires Go, Docker, kind and helm.

```sh
go build -o bin/helmbulance ./cmd/helmbulance
./bin/helmbulance test ./path/to/chart -f values.yaml --set some.secret=value
./bin/helmbulance test ./path/to/chart --scenario chart-upgrade --from ./old/chart --from-version 1.2.3
./bin/helmbulance matrix ./path/to/chart -f values.yaml --k8s 1.34,1.37
./bin/helmbulance scan ./path/to/chart --from ./old/chart --from-version 1.2.3     # no cluster needed
```

The fixtures in `testdata/charts/` show what it finds: each defect has a fixed twin that must pass.

**Secret drift severity.** A changed or removed key is rated from its name and its readers. It is high for a credential (password, token, key) that a workload with a PersistentVolumeClaim reads, since that data was likely written with the old value. It is low for TLS material, which charts often reissue on purpose. Everything else is medium. If the chart rotates a key on purpose, list it and it is named in the summary instead of reported. Secret names carry the release name, `hb-upgrade` in `upgrade-without-values` and `hb-chart` in `chart-upgrade`, so use a glob:

```sh
./bin/helmbulance test ./path/to/chart --expected-rotation '*-webhook-tls/*'
```

**Run only charts you trust.** `test` and `matrix` install the chart into a kind cluster on your Docker host, and the protections for someone else's chart (architecture §4) arrive in v0.3. Until then any third-party chart runs unprotected, whether it comes from a repository or a local copy. `test` and `matrix` refuse a repository, OCI or URL reference unless you pass `--allow-remote`. The refusal catches mistakes; it is not a protection: `helm pull --untar` and a local path skip it, and a local chart's vendored `charts/` subcharts are third-party code too. `scan` only renders the chart and needs no cluster.

## Contributing

Issues and ideas are welcome, especially failures you have seen in production that a scenario could catch. Open an issue describing what happened and how you found out.

## Team

- [Ege Zambelli](https://github.com/egezamb)
- [Kadirhan Emre](https://github.com/kassvl)

## License

[Apache-2.0](LICENSE)
