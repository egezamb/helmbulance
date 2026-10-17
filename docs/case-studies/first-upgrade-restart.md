# The first upgrade restarts pods for nothing

**Reported upstream:** [JupyterHub #3996](https://github.com/jupyterhub/zero-to-jupyterhub-k8s/issues/3996), [Semaphore #34](https://github.com/semaphoreui/charts/issues/34), [SeaweedFS #11719](https://github.com/seaweedfs/seaweedfs/issues/11719).

## The pattern

Many charts restart their pods when a Secret changes by putting a hash of the rendered Secret template in the pod template:

```yaml
annotations:
  checksum/secret: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}
```

When that Secret template also keeps generated values with `lookup` (reuse the stored value, otherwise `randAlphaNum`), the hash goes wrong once:

1. **On install,** `lookup` finds nothing. The real Secret gets one random value. The `include` for the hash renders the template a second time, which draws another random value, so the hash does not describe the real Secret.
2. **On the first upgrade,** `lookup` returns the stored value. The hash now matches the real Secret, and that is a change, so every pod that carries it is replaced, although nothing changed.
3. **From then on,** the hash is stable.

The Secret itself never changes, so a Secret-only check misses this. Helmbulance's `upgrade-without-values` scenario compares pod template revisions across an upgrade with no new values (finding `HB-UV-003`). That control step is exactly the first upgrade after install; a second no-op upgrade then tells "the first upgrade" from "every upgrade".

## How the charts were found

1. **Static pre-screen of about 480 popular Artifact Hub charts.** It looked for pod-template annotations that hash an included template whose body, or a helper it includes, uses both `lookup` and a random generator. 20 charts matched, among them Harbor, JupyterHub, Authelia, Netbox, Zitadel, NeuVector, SeaweedFS, Semaphore and several Bitnami charts.
2. **Confirmed on kind for three of them.**

| Chart | Version | What restarts | Impact |
|---|---|---|---|
| JupyterHub | 4.4.2 | hub and proxy | short hub outage; single-user servers keep running |
| Semaphore | 16.2.2 | the app | a real outage: one replica, `Recreate` |
| SeaweedFS | 4.48.0 (S3 with auth) | the filer StatefulSet and the S3 gateway | a short outage of the filesystem's metadata service |

For JupyterHub, by hand:

| | hub pod | `checksum/secret` | `cookie_secret` |
|---|---|---|---|
| after install | `90d2a615…` | `ce4d9d3a…` | `29b3f6df…` |
| 1st upgrade, no changes | **replaced** | **changed** | unchanged |
| 2nd upgrade, no changes | unchanged | unchanged | unchanged |

SeaweedFS shows the pattern in its plainest form. Two workloads each include the S3 Secret template for their checksum, so on install the same Secret gets **two different** hashes:

| | filer-0 pod | filer checksum | S3 checksum | Secret |
|---|---|---|---|---|
| after install | `06faf8ab…` | `78c07bb7…` | `df816172…` | `c572432d…` |
| 1st upgrade, no changes | **replaced** | `24ccc99e…` | `24ccc99e…` | unchanged |
| 2nd upgrade, no changes | unchanged | unchanged | unchanged | unchanged |

Not verified:
- **Harbor 1.19.2:** its images are published for amd64 only, and the sandbox here is arm64. It is a candidate for a run on an amd64 CI runner.
- **Authelia 0.11.22:** does not start with default values; it needs a users file and session configuration.
- **The other 15 candidates:** the pre-screen only ranks them.

## The fix

Hash the inputs that come from values, not the rendered Secret. Generated values never change after install, so they do not need to trigger a restart:

```yaml
annotations:
  checksum/values: {{ toJson .Values.someSection | sha256sum }}
```

The `keeps-secret` fixture in this repository follows this pattern.
