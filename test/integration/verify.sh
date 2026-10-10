#!/usr/bin/env bash
# Checks a deployed booth-spark. Run after deploy-standins.sh, or after deploy-realcore.sh with
# REAL_CORE=1 for the checks only a real booth-core can satisfy.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${REAL_CORE:-}" = "1" ]; then
  step "booth-core provisions the module's database (ADR 0053)"
  for _ in $(seq 1 90); do
    kubectl -n "$ns" get secret booth-database-credentials >/dev/null 2>&1 && break
    sleep 2
  done
  kubectl -n "$ns" get secret booth-database-credentials >/dev/null 2>&1 || fail "booth-core never wrote booth-database-credentials into $ns"
  for key in dsn host port database username password; do
    test -n "$(kubectl -n "$ns" get secret booth-database-credentials -o jsonpath="{.data.$key}")" \
      || fail "booth-database-credentials has no $key"
  done
  echo "ok: booth-database-credentials"
fi

step "the backend rolls out"
kubectl -n "$ns" rollout status deployment/booth-spark --timeout=300s

step "registration (ADR 0019): the live resource, after core's CRD schema has pruned anything unknown"
bm() { kubectl -n "$ns" get boothmodules.booth.projectbooth.io spark -o jsonpath="$1"; }
test "$(bm '{.spec.id}')" = "spark" || fail "id"
test "$(bm '{.spec.uiIntegrationMode}')" = "iframe-proxy" || fail "uiIntegrationMode"
test "$(bm '{.spec.navGroup}')" = "manage" || fail "navGroup"
test "$(bm '{.spec.navPath}')" = "/spark" || fail "navPath"
test "$(bm '{.spec.healthCheckPath}')" = "/healthz" || fail "healthCheckPath"
test -z "$(bm '{.spec.workloadIdentity}')" || fail "workloadIdentity declared before data access exists"
if [ "${REAL_CORE:-}" = "1" ]; then
  test "$(bm '{.spec.database.enabled}')" = "true" || fail "database.enabled was pruned or wrong"
fi
echo "ok: BoothModule"

step "health, and every non-health route refuses a caller without the right credential"
svc=http://booth-spark.booth-spark.svc:8080
read -r -d '' script <<SH || true
body=\$(curl -s $svc/healthz)
contains "/healthz database ok" "\$body" '"database":"ok"'
contains "/healthz status ok" "\$body" '"status":"ok"'
check "/livez" "\$(status $svc/livez)" 200
check "module UI without an assertion" "\$(status $svc/)" 401
check "ui api without an assertion" "\$(status $svc/ui/api/me)" 401
check "an unknown path without an assertion (no map of what exists)" "\$(status $svc/nothing-here)" 401
check "a forged assertion" "\$(status -H 'X-Booth-Identity: e30.e30.e30' $svc/ui/api/me)" 401
SH
if [ "${REAL_CORE:-}" = "1" ]; then
  script+=$'\n'"check \"/v1 without a token\" \"\$(status $svc/v1/me)\" 401"
else
  # No identity provider configured in the stand-in job: /v1 says so instead of guessing.
  script+=$'\n'"check \"/v1 with no issuer configured\" \"\$(status -H 'Authorization: Bearer x' $svc/v1/me)\" 503"
fi
checked verify-http "$script"

step "no Kubernetes API access for the module's service account (step 3 adds exactly the fenced rights)"
sa="system:serviceaccount:$ns:booth-spark"
for args in "get secrets -n $ns" "list pods -n $ns" "create pods -n $ns" "create namespaces" "get secrets -n kube-system" "create rolebindings -n $ns"; do
  test "$(kubectl auth can-i $args --as="$sa")" = "no" || fail "unexpectedly ALLOWED: $args"
done
test "$(kubectl -n "$ns" get pod -l app.kubernetes.io/name=booth-spark -o jsonpath='{.items[0].spec.automountServiceAccountToken}')" = "false" \
  || fail "the backend pod mounts a service-account token"
echo "ok: no API access, no token mounted"

if [ "${REAL_CORE:-}" = "1" ]; then
  step "booth-core's health reconciler sees the module as Healthy"
  phase=""
  for _ in $(seq 1 60); do
    phase="$(bm '{.status.phase}')"
    [ "$phase" = "Healthy" ] && break
    sleep 2
  done
  test "$phase" = "Healthy" || fail "BoothModule status.phase = '$phase', want Healthy"
  echo "ok: Healthy"
fi

echo "all checks passed"
