# Verdict quality

A verdict is only worth something if it is right. A `FAIL` on a healthy chart costs the maintainer's trust; a `PASS` on a broken one costs an outage. This page says how the repository checks its own verdicts, and what those checks do not show.

## The table

`testdata/verdicts.yaml` lists the cases whose right answer is known. A case is one `helmbulance` run on a fixture chart, in one of three roles:

| Role | The chart | The right answer |
|---|---|---|
| `defect` | has one defect, put there on purpose | the findings listed for it, and no other |
| `fixed` | is the same chart without that defect | none of those findings |
| `incomplete` | cannot finish: its image cannot be pulled | `INCONCLUSIVE`, exit code 3, no finding |

A defect case and its fixed twin form a pair. Only a pair shows that a finding means something: an oracle that reports on every chart passes every defect case, and an oracle that never reports passes every fixed one.

For each case the table gives:
- the exit code;
- the verdict of every scenario in the report, so `SKIPPED` or `INCONCLUSIVE` cannot stand in for `PASS`;
- the exact set of finding IDs;
- for each finding, text that its evidence must contain, so the finding is about the right object and not only of the right kind.

`tools/verdicts` runs the cases and compares. A run that writes no `report.json` never counts as "no findings".

## What CI checks

| Check | What it catches | When |
|---|---|---|
| Every case against the table | A missed finding, also when another finding keeps the exit code at 1. A finding on the fixed twin. The right ID about the wrong object. A scenario that was skipped or added without the table knowing. | Every pull request, under Helm 3 and Helm 4 |
| Coverage lock | A finding ID in `internal/` that no pair tells apart, unless the table lists it under `uncovered` with the reason. An entry that is still under `uncovered` once a pair covers it, so the list can only shrink. | Every pull request, in `go test` and in the fixtures jobs |
| Repeat | A verdict that changes between runs of the same chart: every case runs three times and the runs must agree. | Every night on `main` |
| Mutation report | Code that could be wrong with every unit test green (below). | Every pull request; report only |

The coverage lock is part of `go test ./tools/verdicts` and needs no cluster, so a new finding ID without a pair fails in seconds.

## The scorecard

Each fixtures job writes a scorecard to its job summary. One row per finding ID:

| Column | Meaning |
|---|---|
| Found where expected | Runs of defect cases that gave the finding, of those that should. Less than all is a missed finding (false negative). |
| Absent in the fixed twin | Runs of the fixed twins that stayed free of it, of all their runs. Less than all is a false alarm (false positive). |
| Seen elsewhere | Runs of any other case that gave the finding. Anything but 0 is a false alarm. |

A second table lists every run with its exit code, its findings and how it differs from the table.

## Adding a finding

1. Write a fixture chart with the defect and a twin without it, as small as they can be (`testdata/charts/`).
2. Add both to `testdata/verdicts.yaml` under one `pair`, with the exit code, the verdicts and, for the defect case, the finding and a text from its evidence that names the object.
3. If the ID was listed under `uncovered`, remove it there.
4. Run the pair on a cluster and look at both reports before writing down what is "expected". The table holds what is right, not what the tool happens to print.

When an oracle changes and a case fails, decide first which of the two is wrong. If it is the table, change it in the same pull request and say why.

## Running it

```sh
go build -o bin/helmbulance ./cmd/helmbulance
scripts/fixtures.sh                                   # every case; a new cluster per run
scripts/fixtures.sh -only scan-regenerates-secret,scan-keeps-secret   # the scan cases need no cluster
scripts/fixtures.sh -cluster helmbulance-xxxx -repeat 3
go test ./tools/verdicts                              # the table and the coverage lock, no cluster
scripts/mutation.sh                                   # the mutation report, under a minute
```

`-cluster` reuses a sandbox that an earlier `helmbulance test --keep-cluster` left behind; the matrix cases always make their own, on Kubernetes `$K8S_VERSION` (default 1.37). The scorecard, the logs and the reports land in the directory given with `-out`, or in a new temporary one.

## Mutation report

`scripts/mutation.sh` changes the code in small ways, one at a time (a `<` into a `<=`, a `==` into a `!=`), and runs the unit tests of that package. A change that the tests notice is "killed", one they do not is "lived". The report lists, per package, how many were killed, lived, timed out, or sit in code no unit test runs.

It is a report and not a gate: the job has no threshold and is not a required check. Read it when you change an oracle: a mutant that lived in the lines you touched is a case your tests do not tell apart.

## What this does not show

- **Five finding IDs have no pair yet**: `HB-CU-002`, `HB-CU-003`, `HB-K8S-001`, `HB-K8S-002` and `HB-SCAN-002`. Nothing checks them. They are listed under `uncovered` in the table.
- **The fixture charts are small and written by us.** They show that an oracle tells one defect from its fix. They do not show how often it is right on real charts.
- **The table is only as right as the people who wrote it.** A wrong expectation makes a wrong verdict look checked.
- **Evidence is checked for what it must contain, not for what it must not.** A defect case whose finding also lists an object that is fine still passes.
- **One case covers incomplete runs.** Other ways a run can fail to finish (an admission webhook that rejects, a cluster that does not start) have none.
- **The matrix cases run on one Kubernetes version.**
- **The mutation report only counts unit tests.** A package without any is listed as such. Today that includes `internal/scan` and `internal/matrix`: for the findings they produce, the table is the only check.
