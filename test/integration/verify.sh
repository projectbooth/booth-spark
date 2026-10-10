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
# Minting is declared exactly when data access is on (DATA_ACCESS=1, the data job).
if [ "${DATA_ACCESS:-}" = "1" ]; then
  test "$(bm '{.spec.workloadIdentity.mint}')" = "true" || fail "workloadIdentity.mint not declared with data access on"
else
  test -z "$(bm '{.spec.workloadIdentity}')" || fail "workloadIdentity declared with data access off"
fi
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

step "the cluster serves ValidatingAdmissionPolicy, and both of booth-spark's policies are installed and bound"
kubectl api-resources --api-group=admissionregistration.k8s.io -o name | grep -qx validatingadmissionpolicies.admissionregistration.k8s.io   || fail "this cluster doesn't serve ValidatingAdmissionPolicy (the chart should have refused to install)"
for p in booth-spark-run-pods booth-spark-fence; do
  kubectl get validatingadmissionpolicy "$p" >/dev/null || fail "policy $p missing"
  kubectl get validatingadmissionpolicybinding "$p" >/dev/null || fail "binding $p missing"
done
echo "ok: both policies"

step "the backend's Kubernetes rights are exactly the fenced ones (docs/design-v0.md item 3), live"
sa="system:serviceaccount:$ns:booth-spark"
for args in "create namespaces" "delete namespaces" "list namespaces" "create rolebindings -n some-ns"             "get endpointslices.discovery.k8s.io/kubernetes -n default" "get clusterroles/booth-spark-driver"             "bind clusterroles/booth-spark-driver" "bind clusterroles/booth-spark-run-controller"; do
  test "$(kubectl auth can-i $args --as="$sa")" = "yes" || fail "the backend needs, and lacks: $args"
done
for args in "get secrets -A" "list pods -A" "get pods --subresource=log -n kube-system" "create pods -n kube-system"             "update namespaces" "patch namespaces" "bind clusterroles/cluster-admin" "get clusterroles/cluster-admin"             "create clusterrolebindings" "get endpointslices.discovery.k8s.io -n kube-system" "create deployments.apps -n $ns"; do
  test "$(kubectl auth can-i $args --as="$sa")" = "no" || fail "unexpectedly ALLOWED: $args"
done
test "$(kubectl -n "$ns" get pod "$(ready_pod "$ns" app.kubernetes.io/name=booth-spark)" -o jsonpath='{.spec.automountServiceAccountToken}')" = "true"   || fail "the backend pod doesn't mount its token"
echo "ok: exactly the fenced rights"

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
