#!/usr/bin/env bash
# Build step 3, the run lifecycle through core's gateway with real tokens (docs/design-v0.md items 3,
# 6 and 7):
#   - an editor submits a PySpark application; it runs with real executors in its own namespace
#     (fenced, restricted, owned by the driver ClusterRole), succeeds, keeps its log, and its
#     namespace is gone;
#   - a viewer can't submit; nobody but the submitter, an owner or an operator reads its logs;
#   - an application that fails is "failed" with the driver's exit; a stopped one is "stopped";
#   - admission refuses what doesn't fit.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"

T_editor=$(tok editor-user)
T_viewer=$(tok viewer-user)
T_owner=$(tok owner-user)
T_editor2=$(tok operator-user) # an operator who is a viewer in acme-analytics

read -r -d '' pi <<'PY' || true
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
sc = spark.sparkContext
print("RESULT", sc.parallelize(range(1000000), 8).map(lambda x: x % 7).sum(), flush=True)
print("EXECUTORS", sc._jsc.sc().getExecutorMemoryStatus().size() - 1, flush=True)
spark.stop()
PY

step "an editor's PySpark application runs with real executors and succeeds"
id=$(submit "$T_editor" pi "$pi" '{"resources":{"executors":{"min":2,"max":2}}}')
echo "run $id"
ns=bspark-$id
wait_state "$T_editor" "$id" running succeeded >/dev/null
for _ in $(seq 1 60); do kubectl get namespace "$ns" >/dev/null 2>&1 && break; sleep 1; done
step "its namespace: labelled, restricted, owned by the driver ClusterRole"
nsj=$(kubectl get namespace "$ns" -o json 2>/dev/null || true)
if [ -n "$nsj" ]; then
  echo "$nsj" | jq_ "d['metadata']['labels']['booth.projectbooth.io/workspace']" | grep -qx acme-analytics || fail "workspace label"
  echo "$nsj" | jq_ "d['metadata']['labels']['pod-security.kubernetes.io/enforce']" | grep -qx restricted || fail "PSA label"
  echo "$nsj" | jq_ "d['metadata']['ownerReferences'][0]['name']" | grep -qx booth-spark-driver || fail "owner"
  echo "ok: namespace $ns"
else
  echo "info: the run finished before its namespace could be inspected"
fi
wait_state "$T_editor" "$id" succeeded >/dev/null
out=$(logs "$T_editor" "$id")
echo "$out" | grep -q "RESULT 2999997" || fail "no result in the driver log: $(echo "$out" | tail -20)"
echo "$out" | grep -q "EXECUTORS 2" || fail "the application did not get its 2 executors: $(echo "$out" | grep EXECUTORS)"
echo "ok: succeeded with 2 executors, log kept"
ns_gone "$ns"
echo "ok: its namespace is gone"

step "who may do what (docs/design-v0.md item 3)"
v1 "$T_viewer" POST /applications "$(app_json pi "print(1)")"
[ "$code" = 403 ] || fail "a viewer submitted: $code"
v1 "$T_editor2" POST /applications "$(app_json pi "print(1)")"
[ "$code" = 403 ] || fail "an operator who is a viewer submitted: $code"
v1 "$T_viewer" GET "/applications/$id"
[ "$code" = 200 ] || fail "a viewer can't see a run: $code"
v1 "$T_viewer" GET "/applications/$id/logs"
[ "$code" = 403 ] || fail "a viewer read a run's logs: $code"
v1 "$T_owner" GET "/applications/$id/logs"
[ "$code" = 200 ] || fail "an owner can't read a run's logs: $code"
v1 "$T_editor2" GET "/applications/$id/logs"
[ "$code" = 200 ] || fail "an operator can't read a run's logs: $code"
echo "ok: viewers and operators don't submit; logs for submitter, owners and operators"

step "a failing application is failed, with the driver's exit"
bad=$(submit "$T_editor" boom "raise SystemExit(3)" '{"resources":{"executors":{"max":0}}}')
WAIT=240 wait_state "$T_editor" "$bad" failed >/dev/null
v1 "$T_editor" GET "/applications/$bad"
echo "$body" | jq_ "d['reason']" | grep -q "exited with code" || fail "reason: $body"
ns_gone "bspark-$bad"
echo "ok: failed ($(echo "$body" | jq_ "d['reason']"))"

step "an owner stops an editor's long run; its namespace goes"
long=$(submit "$T_editor" sleeper "import time; time.sleep(600)" '{"resources":{"executors":{"max":0}}}')
wait_state "$T_editor" "$long" running >/dev/null
v1 "$T_owner" POST "/applications/$long/stop"
[ "$code" = 202 ] || fail "stop: $code $body"
wait_state "$T_editor" "$long" stopped >/dev/null
ns_gone "bspark-$long"
echo "ok: stopped and cleaned up"

step "admission refuses a run that doesn't fit the memory budget"
# Each needs up to 3 x 2867Mi = 8601Mi (sized by max executors, none started): one fits the test's
# 10Gi budget, a second doesn't.
big_spec='{"resources":{"driver":{"memory":"2g"},"executors":{"min":0,"max":2,"memory":"2g"}}}'
v1 "$T_editor" POST /applications "$(app_json big "import time; time.sleep(120)" "$big_spec")"
[ "$code" = 201 ] || fail "the first big run didn't fit: $code $body"
big=$(echo "$body" | jq_ "d['id']")
v1 "$T_editor" POST /applications "$(app_json big2 "print(1)" "$big_spec")"
refused_code=$code refused_body=$body
v1 "$T_editor" POST "/applications/$big/stop" >/dev/null
[ "$refused_code" = 429 ] || fail "a second big run fit a 10Gi budget: $refused_code $refused_body"
echo "$refused_body" | grep -q '"code":"at_capacity"' || fail "no at_capacity: $refused_body"
echo "$refused_body" | grep -q 'runs.memoryBudget' || fail "the refusal doesn't name the budget: $refused_body"
echo "ok: refused (429 at_capacity, runs.memoryBudget)"
wait_state "$T_editor" "$big" stopped >/dev/null

echo "all run lifecycle checks passed"
