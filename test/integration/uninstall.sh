#!/usr/bin/env bash
# Build step 3, uninstall (ADR 0110 ruling 2; step 3 requirement 6). Every run namespace is owned
# by the chart's driver ClusterRole, so removing the chart, by any route, deletes them through
# Kubernetes' garbage collector, and Helm removes the policies, their bindings and the ClusterRoles.
#   1. `helm uninstall` of the running install, with a live run: nothing of it is left.
#   2. Reinstalled as release "spark" (the name booth-core's module lifecycle uses), with a live run
#      and a live session (step 4), then uninstalled through booth-core's module uninstall API
#      (DELETE /api/modules/spark, as a workspace owner): nothing of it is left.
# Run last: it removes booth-spark.
#
#   test/integration/uninstall.sh <booth-spark image> <Spark image>
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"
image=$1
spark_image=$2

leftovers() { # FULLNAME INSTANCE: prints whatever of that install still exists
  # A kubectl that fails prints a line too, so a failing listing never passes for an empty one.
  local f=$1 inst=$2 out
  out=$(kubectl get namespaces -l "booth.projectbooth.io/spark-run=$inst" -o name 2>/dev/null) || out="kubectl failed"
  echo "$out" | grep . || true
  out=$(kubectl get validatingadmissionpolicies,validatingadmissionpolicybindings -o name 2>/dev/null) || out="kubectl failed"
  echo "$out" | grep -E "/$f-(run-pods|fence)$|^kubectl failed" || true
  out=$(kubectl get clusterroles,clusterrolebindings -o name 2>/dev/null) || out="kubectl failed"
  echo "$out" | grep -E "/$f-(controller|run-controller|driver)$|^kubectl failed" || true
  out=$(kubectl -n default get roles,rolebindings -o name 2>/dev/null) || out="kubectl failed"
  echo "$out" | grep -E "/$f-api-endpoints$|^kubectl failed" || true
}
clean() { # FULLNAME INSTANCE
  local left=""
  for _ in $(seq 1 180); do
    left=$(leftovers "$1" "$2")
    [ -z "$left" ] && return 0
    sleep 2
  done
  fail "left behind after uninstall: $left"
}
live_run() { # prints a running run's namespace
  local t id
  t=$(tok editor-user)
  id=$(submit "$t" left-running "import time; time.sleep(900)" '{"resources":{"executors":{"max":0}}}')
  wait_state "$t" "$id" running >/dev/null
  kubectl get namespace "bspark-$id" >/dev/null || fail "no namespace for live run $id"
  echo "bspark-$id"
}

step "1. helm uninstall, with a live run"
ns1=$(live_run)
echo "live run namespace $ns1"
[ -n "$(leftovers booth-spark booth-spark.booth-spark)" ] || fail "control: the install's objects aren't visible"
helm uninstall booth-spark -n booth-spark --wait --timeout 5m
clean booth-spark booth-spark.booth-spark
ns_gone "$ns1"
echo "ok: no run namespace, policy, binding or ClusterRole left"

step "2. reinstalled as release \"spark\", uninstalled through booth-core's module uninstall API"
BOOTH_SPARK_RELEASE=spark bash "$here/install-spark.sh" "$image" "$spark_image" >/dev/null
kubectl -n booth-spark rollout status deploy/spark-booth-spark --timeout=300s
for _ in $(seq 1 60); do
  [ "$(kubectl -n booth-spark get boothmodules.booth.projectbooth.io spark -o jsonpath='{.status.phase}' 2>/dev/null)" = Healthy ] && break
  sleep 2
done
ns2=$(live_run)
echo "live run namespace $ns2"
T_editor=$(tok editor-user)
sess=$(start_session "$T_editor" left-open '{"resources":{"executors":{"max":0}}}')
WAIT=300 wait_session "$T_editor" "$sess" running >/dev/null
stmt "$T_editor" "$sess" python "import time
time.sleep(1200)" >/dev/null # busy: it can't idle out before the uninstall
ns3=bspark-$sess
kubectl get namespace "$ns3" >/dev/null || fail "no namespace for live session $sess"
echo "live session namespace $ns3"
[ -n "$(leftovers spark-booth-spark booth-spark.spark-booth-spark)" ] || fail "control: the reinstall's objects aren't visible"
helm status spark -n booth-spark >/dev/null || fail "control: helm doesn't see the release \"spark\" before the uninstall"
T_owner=$(tok owner-user)
code=$(curl -s -o /tmp/uninstall-body -w '%{http_code}' -X DELETE -H "Authorization: Bearer $T_owner" -H "X-Workspace: $WS" \
  "$core/api/modules/spark?namespace=booth-spark")
case "$code" in 200|202|204) ;; *) fail "core's uninstall API answered $code: $(cat /tmp/uninstall-body)" ;; esac
echo "core's uninstall API: $code"
clean spark-booth-spark booth-spark.spark-booth-spark
ns_gone "$ns2"
ns_gone "$ns3"
# Gone means helm's "release: not found", not any failure of helm.
rel=$(helm status spark -n booth-spark 2>&1) && fail "the release still exists after core's uninstall"
echo "$rel" | grep -q "release: not found" || fail "helm status after core's uninstall: $rel"
echo "ok: no release, run namespace, policy, binding or ClusterRole left"
echo "all uninstall checks passed"
