#!/usr/bin/env bash
# Build step 4 (docs/design-v0.md items 6 and 7, ADR 0110), through core's gateway with real tokens,
# on a real cluster (install-spark.sh sets sessions.idleTimeout=60s, sessions.maxLifetime=30m):
#   - an editor starts a session in its own namespace; SQL and Python statements run in order in
#     one SparkSession (a variable survives to the next statement); a failing statement is "error"
#     with its traceback and the session goes on;
#   - only its submitter and the workspace's owners see it (another editor, a viewer and an
#     operator get 404); only its submitter runs statements; submit.minRole applies;
#   - idle shutdown: a statement running longer than the idle timeout keeps the session alive;
#     once nothing runs, it stops on its own and its namespace goes;
#   - its maximum lifetime stops it even while busy, and cancels what was running;
#   - delete on request (by a workspace owner) ends it and its namespace;
#   - a backend restart mid-statement: the session is re-adopted, the statement's result arrives,
#     and the next statement runs;
#   - an orphaned run namespace (no live run in the database) is reaped.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"

# Keycloak's access tokens live 5 minutes and this script runs longer: every step mints fresh ones
# (a step that outlived its token once read every session's state as empty).
fresh() {
  T_editor=$(tok editor-user)
  T_editor2=$(tok editor2-user)
  T_owner=$(tok owner-user)
  T_viewer=$(tok viewer-user)
  T_operator=$(tok operator-user)
}
ok() { echo "ok: $*"; }

step "an editor's session: statements in order, state kept, a failure doesn't end it"
fresh
s=$(start_session "$T_editor" explore '{"resources":{"executors":{"max":1}}}')
echo "session $s"
WAIT=300 wait_session "$T_editor" "$s" running >/dev/null
kubectl get namespace "bspark-$s" -o jsonpath='{.metadata.labels.booth\.projectbooth\.io/workspace}' | grep -qx acme-analytics || fail "the session's namespace isn't its workspace's"
a=$(stmt "$T_editor" "$s" sql "select 1 + 1 as two, 'x' as letter")
b=$(stmt "$T_editor" "$s" python "n = spark.range(1000).count()
print('rows', n)")
c=$(stmt "$T_editor" "$s" python "raise ValueError('boom from a statement')")
d=$(stmt "$T_editor" "$s" python "print('still here', n + 1)")
ra=$(wait_stmt "$T_editor" "$s" "$a"); rb=$(wait_stmt "$T_editor" "$s" "$b")
rc=$(wait_stmt "$T_editor" "$s" "$c"); rd=$(wait_stmt "$T_editor" "$s" "$d")
echo "$ra" | jq_ "d['state']" | grep -qx available || fail "sql: $ra"
echo "$ra" | jq_ "d['output']['rows'][0][0]" | grep -qx 2 || fail "sql result: $ra"
echo "$ra" | jq_ "d['output']['columns'][0]['name']" | grep -qx two || fail "sql columns: $ra"
echo "$rb" | jq_ "d['output']['stdout']" | grep -q "rows 1000" || fail "python: $rb"
echo "$rc" | jq_ "d['state']" | grep -qx error || fail "a failing statement: $rc"
echo "$rc" | jq_ "d['error']" | grep -q "ValueError: boom from a statement" || fail "no traceback: $rc"
echo "$rd" | jq_ "d['output']['stdout']" | grep -q "still here 1001" || fail "state lost after a failure: $rd"
wait_session "$T_editor" "$s" running >/dev/null
ok "sql, python, a failure (with its traceback) and the session goes on, state kept"

step "who sees a session: its submitter and the workspace's owners only"
fresh
# Each refusal is matched by its message, and each has its control (the same request by someone
# allowed), so a wrong route or an unrelated error can't pass for it.
nosuch='"no such session"'
for who in T_editor T_owner; do
  v1 "${!who}" GET "/sessions/$s"; [ "$code" = 200 ] || fail "control: $who can't see the session: $code"
  v1 "${!who}" GET /sessions; [ "$code" = 200 ] && echo "$body" | grep -q "$s" || fail "control: $who doesn't list the session: $code"
done
for who in T_editor2 T_viewer T_operator; do
  v1 "${!who}" GET "/sessions/$s"; [ "$code" = 404 ] && echo "$body" | grep -q "$nosuch" || fail "$who sees the session: $code $body"
  v1 "${!who}" GET /sessions; [ "$code" = 200 ] || fail "$who can't list sessions at all: $code"
  echo "$body" | grep -q "$s" && fail "$who lists the session"
  v1 "${!who}" DELETE "/sessions/$s"; [ "$code" = 404 ] && echo "$body" | grep -q "$nosuch" || fail "$who deleted the session: $code $body"
done
v1 "$T_owner" POST "/sessions/$s/statements" '{"kind":"sql","code":"select 1"}'
[ "$code" = 403 ] && echo "$body" | grep -q "only the session's submitter can run statements" || fail "an owner ran a statement in someone else's session: $code $body"
v1 "$T_viewer" POST /sessions '{"name":"x"}'
[ "$code" = 403 ] && echo "$body" | grep -q "starting sessions needs the editor role" || fail "a viewer started a session: $code $body"
v1 "$T_operator" POST /sessions '{"name":"x"}'
[ "$code" = 403 ] && echo "$body" | grep -q "starting sessions needs the editor role" || fail "an operator who is a viewer started a session: $code $body"
ok "editor2, viewer and operator get 404; an owner sees it but can't type into it; viewers can't start one"

step "a backend restart mid-statement: re-adopted, the result arrives, the next statement runs"
fresh
long=$(stmt "$T_editor" "$s" python "import time
time.sleep(40)
print('survived the restart')")
for _ in $(seq 1 30); do
  v1 "$T_editor" GET "/sessions/$s/statements/$long"; [ "$(echo "$body" | jq_ "d['state']")" = running ] && break; sleep 1
done
[ "$(echo "$body" | jq_ "d['state']")" = running ] || fail "the long statement never started: $body"
old=$(kubectl -n booth-spark get pod -l app.kubernetes.io/name=booth-spark -o jsonpath='{.items[0].metadata.name}')
kubectl -n booth-spark delete pod "$old" --wait=false >/dev/null
kubectl -n booth-spark rollout status deploy/booth-spark --timeout=180s >/dev/null
new=$(kubectl -n booth-spark get pod -l app.kubernetes.io/name=booth-spark --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
[ "$new" != "$old" ] || fail "the backend pod wasn't replaced"
echo "backend $old -> $new"
fresh
rl=$(WAIT=180 wait_stmt "$T_editor" "$s" "$long")
echo "$rl" | jq_ "d['output']['stdout']" | grep -q "survived the restart" || fail "after the restart: $rl"
after=$(stmt "$T_editor" "$s" python "print('after', n)")
echo "$(wait_stmt "$T_editor" "$s" "$after")" | grep -q "after 1000" || fail "the session lost its state across the backend restart"
wait_session "$T_editor" "$s" running >/dev/null
ok "re-adopted by the new backend, the statement's result recorded, state kept"

step "idle shutdown: a statement running past the idle timeout (60s) keeps the session alive"
fresh
busy=$(stmt "$T_editor" "$s" python "import time
time.sleep(80)
print('done after 80s')")
sleep 75
v1 "$T_editor" GET "/sessions/$s"
[ "$(echo "$body" | jq_ "d['state']")" = running ] || fail "the session idled while a statement ran: $body"
wait_stmt "$T_editor" "$s" "$busy" | grep -q "done after 80s" || fail "the busy statement"
ok "still running at 75s, with a statement running"
step "... then, with nothing waiting or running, it stops on its own and its namespace goes"
fresh
t0=$(date +%s)
WAIT=180 wait_session "$T_editor" "$s" stopped >/dev/null || true
v1 "$T_editor" GET "/sessions/$s"
echo "$body" | jq_ "d['reason']" | grep -q "(its idle timeout)" || fail "not stopped as idle: $body"
ns_gone "bspark-$s"
ok "stopped as idle after $(( $(date +%s) - t0 ))s with nothing running; namespace gone"

step "its maximum lifetime stops a busy session and cancels what was running"
fresh
m=$(start_session "$T_editor" short '{"maxLifetime":"90s","resources":{"executors":{"max":0}}}')
WAIT=300 wait_session "$T_editor" "$m" running >/dev/null
forever=$(stmt "$T_editor" "$m" python "import time
time.sleep(600)")
fresh
WAIT=240 wait_session "$T_editor" "$m" stopped >/dev/null || true
v1 "$T_editor" GET "/sessions/$m"
echo "$body" | jq_ "d['reason']" | grep -q "maximum lifetime" || fail "not stopped at its lifetime: $body"
wait_stmt "$T_editor" "$m" "$forever" | jq_ "d['state']" | grep -qx cancelled || fail "the running statement wasn't cancelled"
ns_gone "bspark-$m"
ok "stopped at its 90s lifetime while busy; the statement cancelled; namespace gone"

step "delete on request: an owner deletes an editor's session"
fresh
del=$(start_session "$T_editor" to-delete '{"resources":{"executors":{"max":0}}}')
WAIT=300 wait_session "$T_editor" "$del" running >/dev/null
v1 "$T_owner" DELETE "/sessions/$del"; [ "$code" = 202 ] || fail "owner delete: $code $body"
wait_session "$T_editor" "$del" stopped >/dev/null
ns_gone "bspark-$del"
v1 "$T_editor" POST "/sessions/$del/statements" '{"kind":"sql","code":"select 1"}'
[ "$code" = 409 ] && echo "$body" | grep -q '"session_over"' || fail "a statement into a deleted session: $code $body"
ok "deleted, namespace gone, no more statements (409)"

step "an orphaned run namespace (no live run behind it) is spared while young, then reaped"
fresh
orphan=bspark-rorphan$(date +%s | tail -c 6)
uid=$(kubectl get clusterrole booth-spark-driver -o jsonpath='{.metadata.uid}')
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $orphan
  labels:
    booth.projectbooth.io/spark-run: booth-spark.booth-spark
    booth.projectbooth.io/run: rorphan
    booth.projectbooth.io/workspace: acme-analytics
    pod-security.kubernetes.io/enforce: restricted
  ownerReferences: [{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, name: booth-spark-driver, uid: $uid}]
EOF
kubectl get namespace "$orphan" >/dev/null || fail "control: the orphan namespace wasn't created"
# The race main's Integration hit (run 38068125668): a sweep sees a run-shaped namespace a moment
# after it is created. Wait for a sweep to actually see this one (it logs sparing it) and check it
# is still there, then that a later sweep reaps it once it is older than the grace (1m).
for _ in $(seq 1 90); do
  kubectl -n booth-spark logs deploy/booth-spark --since=5m 2>/dev/null | grep -q "sweep: sparing $orphan " && break
  sleep 1
done
kubectl -n booth-spark logs deploy/booth-spark --since=5m | grep -q "sweep: sparing $orphan " || fail "no sweep saw $orphan within 90s"
age=$(( $(date +%s) - $(date -d "$(kubectl get namespace "$orphan" -o jsonpath='{.metadata.creationTimestamp}')" +%s) ))
kubectl get namespace "$orphan" -o jsonpath='{.status.phase}' | grep -qx Active || fail "$orphan didn't survive a sweep at ${age}s old"
echo "a sweep spared $orphan at under a minute old; still active at ${age}s"
ns_gone "$orphan"
ok "the orphan $orphan was spared by a sweep while young, then reaped"

echo "all session checks passed"
