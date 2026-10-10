#!/usr/bin/env bash
# Deploys booth-spark beside a stand-in PostgreSQL, with booth-core's BoothModule CRD (vendored from
# booth-core's chart at the real-core job's CORE_REF) but no booth-core. Covers: the chart installs on
# a real 1.30+ cluster (its ValidatingAdmissionPolicy check passes against live discovery), the
# BoothModule is accepted by core's real schema with no fields pruned, and the backend reaches its
# database. It cannot cover anything core does (provisioning, assertions, workload tokens).
#
#   test/integration/deploy-standins.sh <image>     # image already loaded into the cluster
set -euo pipefail

image=$1
ns=booth-spark
repo=$(cd "$(dirname "$0")/../.." && pwd)

kubectl apply -f "$repo/test/integration/fixtures/boothmodule-crd.yaml"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" apply -f "$repo/test/integration/fixtures/standins.yaml"
kubectl -n "$ns" rollout status deployment/postgres --timeout=180s

# The database is ours, not core's; there is no core here to provision one. No identity provider
# either: /v1 answers 503 and the iframe routes refuse everything, which verify.sh checks.
helm upgrade --install booth-spark "$repo/charts/booth-spark" \
  --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set postgres.provisionedByCore=false --set postgres.dsnSecret.name=booth-spark-db \
  --wait --timeout 5m
