#!/usr/bin/env bash
# Build step 3, isolation (docs/design-v0.md item 3; ADR 0110 step 3, requirement 3). Every refusal
# is paired with a control that must succeed, so a check that can't see fails instead of passing.
#   - Run A (editor-user), as its own user code in its driver, tries run B's (owner-user)
#     namespace, Secrets, pods and driver ports, and creates pods in its own namespace that the
#     run-pods admission policy must refuse (a non-allowlisted image, no nodeSelector, any account
#     but executor, a token mounted, another workspace's label).
#   - A pod outside every run can't reach a run's driver UI port (only the backend's pods may).
#   - The backend's own account can't write outside its fence: no namespace or RoleBinding that
#     isn't its own, no label on a namespace it didn't create, nothing in kube-system.
#   - The executor account has no permissions at all.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"

T_editor=$(tok editor-user)
T_owner=$(tok owner-user)
image=$(kubectl -n booth-spark get deploy -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="BOOTH_RUNS")].value}' | jq_ "d['image']")
backend=system:serviceaccount:booth-spark:booth-spark
results_ok=1
expect() { # DESCRIPTION ACTUAL PATTERN
  if echo "$2" | grep -Eq "$3"; then echo "ok: $1 ($2)"; else echo "FAIL: $1: got '$2', want /$3/"; results_ok=0; fi
}

step "run B (owner-user) runs; run A (editor-user) probes it from inside its own driver"
b=$(submit "$T_owner" target "import time; time.sleep(900)" '{"resources":{"executors":{"max":0}}}')
wait_state "$T_owner" "$b" running >/dev/null
kubectl -n "bspark-$b" wait --for=condition=Ready pod/driver --timeout=180s >/dev/null
args=$(python3 -c "import json,sys; print(json.dumps({'otherNamespace': 'bspark-' + sys.argv[1], 'image': sys.argv[2], 'otherImage': 'busybox:1.36', 'workspace': 'acme-analytics', 'nodeSelector': {'booth.projectbooth.io/pool': 'compute'}}))" "$b" "$image")
extra=$(python3 -c "import json,sys; print(json.dumps({'args': ['isolation', sys.argv[1]], 'resources': {'executors': {'max': 2}}}))" "$args")
a=$(submit "$T_editor" probe "$(cat "$here/fixtures/probe.py")" "$extra")
WAIT=300 wait_state "$T_editor" "$a" succeeded >/dev/null
r=$(logs "$T_editor" "$a" | sed -n 's/^PROBE //p')
[ -n "$r" ] || fail "run A printed no probe results: $(logs "$T_editor" "$a" | tail -20)"
pr() { echo "$r" | python3 -c "import json,sys; print(json.load(sys.stdin)[sys.argv[1]])" "$1"; }
expect "control: run A lists pods in its own namespace" "$(pr 'control: list pods in its own namespace')" '^200$'
expect "run A can't list run B's pods" "$(pr "list pods in run B's namespace")" '^403$'
expect "run A can't read run B's Secrets" "$(pr "read run B's Secrets")" '^403$'
expect "run A can't read run B's namespace" "$(pr "read run B's namespace")" '^403$'
expect "run A can't read Secrets even in its own namespace" "$(pr 'read Secrets in its own namespace')" '^403$'
expect "run A can't create a pod in run B's namespace" "$(pr "create a pod in run B's namespace")" '^403$'
expect "control: run A reaches its own driver port" "$(pr 'control: its own driver port through its Service')" '^open$'
expect "run A can't reach run B's driver RPC port" "$(pr "run B's driver RPC port")" '^closed'
expect "run A can't reach run B's driver UI port" "$(pr "run B's driver UI port")" '^closed'
expect "control: run A's driver creates a compliant executor-like pod" "$(pr 'control: a compliant executor-like pod')" '^201$'
for c in "a pod with a non-allowlisted image|image not allowed" \
         "a pod without the configured nodeSelector|nodeSelector" \
         "a pod as the driver account|executor account" \
         "a pod as the default account|executor account" \
         "an executor-account pod with a token mounted|executor account" \
         "a pod labelled with another workspace|workspace label"; do
  name=${c%%|*}; why=${c##*|}
  expect "the run-pods policy refuses $name" "$(pr "$name")" "^(403|422) booth-spark: .*$why"
done

step "a pod outside every run can't reach a run's driver UI port (only the backend's pods may)"
out=$(checked fence-ui "check \"control: keycloak from the probe namespace\" \"\$(status --max-time 5 http://keycloak.keycloak.svc:8080/realms/booth)\" 200
check \"run B's driver UI from the probe namespace\" \"\$(status --max-time 5 http://driver.bspark-$b.svc:4040/)\" 000")
echo "$out"

step "the backend's account can't write outside its fence (impersonating it)"
as() { kubectl --as="$backend" "$@" 2>&1; }
uid=$(kubectl get clusterrole booth-spark-driver -o jsonpath='{.metadata.uid}')
runns() { # NAME [LABEL-VALUE]: a run-namespace-shaped Namespace
  cat <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $1
  labels:
    booth.projectbooth.io/spark-run: ${2:-booth-spark.booth-spark}
    booth.projectbooth.io/workspace: acme-analytics
    pod-security.kubernetes.io/enforce: restricted
  ownerReferences: [{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, name: booth-spark-driver, uid: $uid}]
EOF
}
expect "control: the backend creates a run-shaped namespace" "$(runns bspark-fencectl | as create -f -)" 'created'
expect "the backend can't create a namespace outside its prefix" "$(runns kube-evil | as create -f -)" 'denied request: booth-spark may create only its own'
expect "the backend can't create a namespace for another install" "$(runns bspark-other other.install | as create -f -)" 'denied request: booth-spark may create only its own'
expect "the backend can't create an unlabelled namespace" "$(printf 'apiVersion: v1\nkind: Namespace\nmetadata: {name: bspark-bare}\n' | as create -f -)" 'denied request'
expect "the backend can't label a namespace it didn't create" "$(as label namespace kube-system evil=1)" 'forbidden'
expect "the backend can't label its own namespace either (no update)" "$(as label namespace bspark-fencectl evil=1)" 'forbidden'
kubectl create namespace booth-spark-it-notmine --dry-run=client -o yaml | kubectl apply -f - >/dev/null
expect "the backend can't delete a namespace it didn't create" "$(as delete namespace booth-spark-it-notmine --wait=false)" 'denied request: booth-spark may delete only'
rb() { # NAMESPACE ROLE SUBJECT-NS SUBJECT
  printf 'apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: probe, namespace: %s}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %s}\nsubjects: [{kind: ServiceAccount, name: %s, namespace: %s}]\n' "$1" "$2" "$4" "$3"
}
expect "control: the backend binds the driver role in its own namespace" "$(rb bspark-fencectl booth-spark-driver bspark-fencectl driver | as create -f -)" 'created'
expect "the backend can't create a RoleBinding in kube-system" "$(rb kube-system booth-spark-driver kube-system driver | as create -f -)" 'denied request'
expect "the backend can't bind cluster-admin, even in its own namespace" "$(rb bspark-fencectl cluster-admin bspark-fencectl x | as create -f -)" '(forbidden|denied request)'
expect "the backend can't bind the driver role to another account" "$(rb bspark-fencectl booth-spark-driver kube-system default | as create -f -)" 'denied request'
for v in "get secrets -A" "list pods -A" "get pods --subresource=log -n kube-system" "create pods -n kube-system" \
         "create deployments.apps -n kube-system" "update namespaces" "patch namespaces" "create clusterrolebindings"; do
  expect "the backend can't $v" "$(kubectl auth can-i $v --as="$backend" 2>&1)" '^no'
done
expect "control: the backend deletes its own namespace" "$(as delete namespace bspark-fencectl --wait=false)" 'deleted'
kubectl delete namespace booth-spark-it-notmine --wait=false >/dev/null

step "the executor account has no permissions (and run B's driver only its own namespace's pods)"
for v in "list pods" "get configmaps" "create pods" "get secrets"; do
  expect "executor can't $v" "$(kubectl auth can-i $v -n "bspark-$b" --as="system:serviceaccount:bspark-$b:executor")" '^no'
done
expect "control: run B's driver lists its own pods" "$(kubectl auth can-i list pods -n "bspark-$b" --as="system:serviceaccount:bspark-$b:driver")" '^yes'
expect "run B's driver can't list pods elsewhere" "$(kubectl auth can-i list pods -n booth-spark --as="system:serviceaccount:bspark-$b:driver")" '^no'
expect "the executor pods' template mounts no token" "$(kubectl -n "bspark-$b" get configmap app -o jsonpath='{.data.executor-template\.yaml}' | grep -c 'automountServiceAccountToken: false')" '^1$'

v1 "$T_owner" POST "/applications/$b/stop" >/dev/null
wait_state "$T_owner" "$b" stopped >/dev/null
ns_gone "bspark-$b"
[ "$results_ok" = 1 ] || fail "isolation checks failed (see above)"
echo "all isolation checks passed"
