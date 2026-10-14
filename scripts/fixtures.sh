#!/usr/bin/env bash
# Runs every case of testdata/verdicts.yaml and checks each against its expected exit code, verdicts
# and findings: a chart with a defect must give exactly the findings listed for it, its fixed twin
# must not. Exits non-zero if any result differs. tools/verdicts does the work; its flags pass
# through.
#
#   scripts/fixtures.sh [-cluster helmbulance-xxxx] [-only name,name] [-repeat 3]
#
# -cluster reuses a sandbox for the test cases; otherwise each run makes one. The matrix cases
# always make their own cluster, on Kubernetes $K8S_VERSION.
#
# Needs a built bin/helmbulance, Go, Docker, kind and helm.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./tools/verdicts -k8s "${K8S_VERSION:-1.37}" "$@"
