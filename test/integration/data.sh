#!/usr/bin/env bash
# Build step 5: data access (docs/design-v0.md item 4; ADR 0110), through core's gateway with real
# tokens, against real booth-database, booth-storage (its s3 credential provider), booth-lakehouse
# with Lakekeeper and a MinIO (deploy-data.sh), on Calico:
#   0. acme-analytics gets an s3 backend on MinIO and a lakehouse warehouse (as its owner); the run's
#      input CSV and its entry point are put in booth-storage;
#   1. an editor's application starts from that Python file, reads the CSV from its storage location
#      (on an executor), writes and reads an Iceberg table and a Postgres table, and writes its
#      result back to storage; its view shows role editor and its roots;
#   2. a session with data access queries both tables; the run's bearer isn't in its Spark container;
#   3. refused at launch, each with its reason: a backend that doesn't exist, two storage locations
#      in one bucket;
#   4. network: a data run reaches the backend's internal port and booth-database's Postgres; a run
#      without data access, and a pod outside every run, are dropped there;
#   5. the submitter is demoted to viewer: their editor's run ends, its namespace goes; then loses
#      access altogether: the same.
# Every refusal is matched by its reason and has its control.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"

ok() { echo "ok: $*"; }
fresh() {
  T_owner=$(tok owner-user)
  T_editor=$(tok editor-user)
  T_editor2=$(tok editor2-user)
}
# No `PRODUCER | grep -q`: grep -q exits at its first match, and under pipefail a producer still
# writing then fails the check (found by Integration). Checks read a here-string instead.
# Keycloak's access tokens live 5 minutes: every step mints fresh ones, and so does every long wait.
# gw TOKEN METHOD PATH [JSON]: another module's API through core's gateway, in $WS.
gw() {
  local args=(-s -o /tmp/gw-body -w '%{http_code}' -X "$2" -H "Authorization: Bearer $1" -H "X-Workspace: $WS")
  [ -n "${4:-}" ] && args+=(-H 'Content-Type: application/json' --data-binary "$4")
  code=$(curl "${args[@]}" "$core$3")
  body=$(cat /tmp/gw-body)
}
mc_() { kubectl -n booth-minio exec -i deploy/minio -- sh -c "mc alias set t http://127.0.0.1:9000 booth-test booth-test-secret >/dev/null && $1"; }
kc_groups() { # USER add|remove GROUP-PATH, through Keycloak's admin API
  local t uid gid
  t=$(curl -s -X POST "http://localhost:$kc_port/realms/master/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=admin-cli -d username=admin -d "password=$kc_admin" | jq_ "d.get('access_token')")
  uid=$(curl -s -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/users?username=$1&exact=true" | jq_ "d[0]['id']")
  gid=$(curl -s -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/group-by-path$3" | jq_ "d['id']")
  [ -n "$uid" ] && [ -n "$gid" ] || fail "keycloak: no user $1 or group $3"
  local m=PUT; [ "$2" = remove ] && m=DELETE
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X $m -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/users/$uid/groups/$gid")" = 204 ] \
    || fail "keycloak: $2 $1 $3"
}
# tcp_py HOST PORT...: Python printing NET <host:port>=open|closed:<why> for each.
tcp_py() {
  python3 -c "import json,sys; t=sys.argv[1:]; print('import socket\ndef tcp(h, p):\n    try:\n        socket.create_connection((socket.gethostbyname(h), p), timeout=4).close()\n        return \"open\"\n    except Exception as e:\n        return \"closed:\" + type(e).__name__\nfor h, p in ' + json.dumps([(t[i], int(t[i+1])) for i in range(0, len(t), 2)]) + ':\n    print(\"NET %s:%d=%s\" % (h, p, tcp(h, p)))')" "$@"
}

step "0. acme-analytics: an s3 backend on MinIO, its lakehouse warehouse, the run's input and entry point"
fresh
gw "$T_owner" POST /modules/storage/api/admin/backends \
  '{"id":"lake","displayName":"lake","kind":"s3","config":{"endpoint":"http://minio.booth-minio.svc:9000","bucket":"lake","pathStyle":true},"credentials":{"accessKeyId":"booth-test","secretAccessKey":"booth-test-secret"}}'
case "$code" in 200|201|409) ;; *) fail "registering the s3 backend: $code $body" ;; esac
for _ in $(seq 1 30); do
  gw "$T_owner" PUT /modules/lakehouse/api/warehouse '{"backendId":"lake","path":"acme-lake"}'
  case "$code" in 201|409) break ;; esac
  sleep 5
done
gw "$T_owner" GET /modules/lakehouse/api/warehouse
[ "$code" = 200 ] && grep -q '"backendId":"lake"' <<<"$body" || fail "acme-analytics has no warehouse: $code $body"
echo "warehouse: $body"
grep -qx put <<<"$(printf 'region,amount\nnorth,10\nsouth,20\neast,12\n' | mc_ "mc pipe t/lake/acme-files/in/sales.csv >/dev/null && echo put")" || fail "putting the input CSV"
grep -qx put <<<"$(mc_ "mc pipe t/lake/acme-files/jobs/etl.py >/dev/null && echo put" <"$here/fixtures/etl.py")" || fail "putting the entry point"
ok "backend lake (bucket lake), warehouse s3://lake/acme-lake, input and entry point under acme-files/"

step "1. an editor's application from booth-storage: CSV, Iceberg, Postgres and back to storage"
fresh
body_json='{"name":"etl","main":{"python":{"backendId":"lake","path":"acme-files/jobs/etl.py"}},
  "resources":{"executors":{"min":1,"max":1}},
  "dataAccess":{"database":true,"lakehouse":true,"storage":[{"backendId":"lake","path":"acme-files","access":"readwrite"}]}}'
v1 "$T_editor" POST /applications "$body_json"
[ "$code" = 201 ] || fail "submit: $code $body"
A=$(echo "$body" | jq_ "d['id']")
echo "run $A"
for _ in $(seq 1 120); do
  ns_labels=$(kubectl get namespace "bspark-$A" -o jsonpath='{.metadata.labels}' 2>/dev/null || true)
  [ -n "$ns_labels" ] && break; sleep 1
done
grep -q '"booth.projectbooth.io/database-client":"true"' <<<"$(echo "$ns_labels")" || fail "the run's namespace lacks booth-database's client label: $ns_labels"
st=""
for _ in $(seq 1 10); do
  T_editor=$(tok editor-user)
  st=$(WAIT=60 wait_state "$T_editor" "$A" succeeded failed 2>/dev/null) && break || true
done
T_editor=$(tok editor-user)
log=$(logs "$T_editor" "$A")
[ "$st" = succeeded ] || { echo "$log" | tail -60; v1 "$T_editor" GET "/applications/$A"; fail "the data run ended $st: $body"; }
for want in "STORAGE-ROOT s3a://lake/acme-files" "READ-CSV 3" "ICEBERG-ROWS 3 SUM 42" "JDBC-ROWS 3" "WROTE-STORAGE"; do
  # Not grep -q on a pipe: it stops reading at the first match, and pipefail then fails the echo.
  grep "^$want" <<<"$log" >/dev/null || { tail -40 <<<"$log"; fail "the run's log lacks '$want'"; }
done
grep '\.csv$' <<<"$(mc_ "mc ls --recursive t/lake/acme-files/out/result/")" >/dev/null || fail "the run's result isn't in storage"
grep 'metadata.json$' <<<"$(mc_ "mc ls --recursive t/lake/acme-lake/")" >/dev/null || fail "no Iceberg metadata under the warehouse"
gw "$T_owner" GET /modules/lakehouse/api/tables
grep -q 'sales' <<<"$(echo "$body")" || fail "booth-lakehouse doesn't list spark_it.sales: $code $body"
v1 "$T_editor" GET "/applications/$A"
grep -qx editor <<<"$(echo "$body" | jq_ "d['dataAccess']['role']")" || fail "the run's role: $body"
grep -qx "s3://lake/acme-lake" <<<"$(echo "$body" | jq_ "d['dataAccess']['warehouseRoot']")" || fail "the run's warehouse root: $body"
grep -qx "s3a://lake/acme-files" <<<"$(echo "$body" | jq_ "d['dataAccess']['storageRoots'][0]")" || fail "the run's storage root: $body"
ns_gone "bspark-$A"
ok "read on an executor, Iceberg and Postgres written and read back, result in storage; role editor"

step "2. a session with data access: both tables, and what its Spark container holds"
fresh
db_pod=$(kubectl -n booth-database get pod "$(ready_pod booth-database app.kubernetes.io/component=postgres)" -o jsonpath='{.status.podIP}')
S=$(start_session "$T_editor" data '{"resources":{"executors":{"max":0}},"dataAccess":{"database":true,"lakehouse":true}}')
WAIT=300 wait_session "$T_editor" "$S" running >/dev/null
# The session's real data bearer, from its Secret (the test's cluster-admin view), as a SHA-256 the
# statement compares against: the bearer itself never goes into the statement's code.
bearer_sha=$(kubectl -n "bspark-$S" get secret data -o jsonpath='{.data.bearer}' | base64 -d | sha256sum | cut -c1-64)
[ ${#bearer_sha} = 64 ] || fail "control: the session's data bearer Secret isn't there"
a=$(stmt "$T_editor" "$S" sql "SELECT count(*) AS n, sum(amount) AS s FROM lakehouse.spark_it.sales")
b=$(stmt "$T_editor" "$S" python "import hashlib, os
props = {'driver': 'org.postgresql.Driver'}
print('JDBC', spark.read.jdbc(os.environ['JDBC_DATABASE_URL'], 'spark_it_sales', properties=props).count())
print('BEARER-MOUNTED', os.path.exists('/opt/booth/data'))
hit = [k for k, v in os.environ.items() if hashlib.sha256(v.encode()).hexdigest() == '$bearer_sha']
print('BEARER-IN-ENV', bool(hit), ','.join(hit))
print('TOKEN-READABLE', os.access('/var/run/booth/token/token', os.R_OK))")
c=$(stmt "$T_editor" "$S" python "$(tcp_py booth-spark.booth-spark.svc 8081 "$db_pod" 5432)")
ra=$(wait_stmt "$T_editor" "$S" "$a"); rb=$(wait_stmt "$T_editor" "$S" "$b"); rc=$(wait_stmt "$T_editor" "$S" "$c")
grep -qx '\[3, 42\]' <<<"$(echo "$ra" | jq_ "d['output']['rows'][0]")" || fail "the session's Iceberg query: $ra"
out=$(echo "$rb" | jq_ "d['output']['stdout']")
echo "$out"
grep -qx "JDBC 3" <<<"$(echo "$out")" || fail "the session's JDBC read: $rb"
grep -qx "BEARER-MOUNTED False" <<<"$(echo "$out")" || fail "the run's bearer is mounted in its Spark container"
grep -qx "BEARER-IN-ENV False " <<<"$(echo "$out")" || fail "the run's bearer is in its Spark container's environment"
# Readable by the run's code, as item 4's credential table says: it is how Iceberg authenticates.
grep -qx "TOKEN-READABLE True" <<<"$(echo "$out")" || fail "the token file isn't where Spark reads it"
net=$(echo "$rc" | jq_ "d['output']['stdout']")
echo "$net"
grep -qx "NET booth-spark.booth-spark.svc:8081=open" <<<"$(echo "$net")" || fail "control: a data run can't reach the backend's internal port"
grep -qx "NET $db_pod:5432=open" <<<"$(echo "$net")" || fail "control: a data run can't reach booth-database's Postgres"
v1 "$T_editor" DELETE "/sessions/$S" >/dev/null
ok "Iceberg 3 rows (42), JDBC 3 rows; no bearer in Spark's container; the internal port and Postgres reachable"

step "3. refused at launch, each with its reason"
fresh
for c in 'nosuch|{"storage":[{"backendId":"nosuch","path":"x"}]}|could not start: .*credential broker refused read access to nosuch/x' \
         'twice|{"storage":[{"backendId":"lake","path":"acme-files/in"},{"backendId":"lake","path":"acme-files/out"}]}|could not start: .*same bucket \(lake\)'; do
  name=${c%%|*}; rest=${c#*|}; da=${rest%%|*}; want=${rest#*|}
  v1 "$T_editor" POST /applications "$(app_json "refused-$name" "print(1)" "{\"dataAccess\":$da,\"resources\":{\"executors\":{\"max\":0}}}")"
  [ "$code" = 201 ] || fail "submit $name: $code $body"
  id=$(echo "$body" | jq_ "d['id']")
  WAIT=120 wait_state "$T_editor" "$id" failed >/dev/null
  v1 "$T_editor" GET "/applications/$id"
  grep -Eq "$want" <<<"$(echo "$body" | jq_ "d['reason']")" || fail "$name: not refused for its reason: $body"
  kubectl get namespace "bspark-$id" >/dev/null 2>&1 && fail "$name: a namespace was created for a refused run"
  ok "$name: $(echo "$body" | jq_ "d['reason']")"
done

step "4. network: without data access a run is dropped at the internal port and Postgres; so is a pod outside every run"
fresh
P=$(submit "$T_editor" nodata "$(tcp_py booth-spark.booth-spark.svc 8081 "$db_pod" 5432)" '{"resources":{"executors":{"max":0}}}')
WAIT=300 wait_state "$T_editor" "$P" succeeded >/dev/null
net=$(grep '^NET ' <<<"$(logs "$T_editor" "$P")" || true)
echo "$net"
grep -qx "NET booth-spark.booth-spark.svc:8081=closed:TimeoutError" <<<"$(echo "$net")" || fail "a run without data access reaches the backend's internal port"
grep -qx "NET $db_pod:5432=closed:TimeoutError" <<<"$(echo "$net")" || fail "a run without data access reaches booth-database's Postgres"
out=$(probe internal-outside "curl -s -o /dev/null -w '%{http_code}' -m 5 -X POST http://booth-spark.booth-spark.svc:8081/internal/token; echo \" exit=\$?\"")
echo "a pod outside every run -> the backend's internal port: $out"
[ "$out" = "000 exit=28" ] || fail "a pod outside every run wasn't dropped at the internal port: '$out' (want a timeout)"
ok "dropped (timeouts), with step 2's data session as the control"

step "5. the submitter loses their editor role, then all access: their running sessions end"
fresh
end_by() { # GROUP-CHANGE... ; waits for editor2's busy session to end for its reason
  local want=$1; shift
  local s
  T_owner=$(tok owner-user)
  T_editor2=$(tok editor2-user)
  s=$(start_session "$T_editor2" doomed '{"resources":{"executors":{"max":0}},"dataAccess":{"lakehouse":true}}')
  WAIT=300 wait_session "$T_editor2" "$s" running >/dev/null
  stmt "$T_editor2" "$s" python "import time
time.sleep(900)" >/dev/null
  # Control: it is running with its data access before the change.
  v1 "$T_editor2" GET "/sessions/$s"; [ "$(echo "$body" | jq_ "d['state']")" = running ] || fail "control: $body"
  "$@"
  tok editor2-user >/dev/null # signs in through core, so core records the change
  for _ in $(seq 1 90); do
    v1 "$T_owner" GET "/sessions/$s"; [ "$(echo "$body" | jq_ "d['state']")" = failed ] && break; sleep 2
  done
  grep -q "lost its data access: .*$want" <<<"$(echo "$body" | jq_ "d['reason']")" || fail "not ended for '$want': $body"
  ns_gone "bspark-$s"
  ok "ended: $(echo "$body" | jq_ "d['reason']")"
}
demote() {
  kc_groups editor2-user remove /workspaces/acme-analytics/editor
  kc_groups editor2-user add /workspaces/acme-analytics/viewer
}
cut_off() { kc_groups editor2-user remove /workspaces/acme-analytics/editor; }
restore() {
  (kc_groups editor2-user remove /workspaces/acme-analytics/viewer) 2>/dev/null || true
  kc_groups editor2-user add /workspaces/acme-analytics/editor
  T_editor2=$(tok editor2-user)
}
end_by "now a viewer" demote
restore
end_by "no longer has access" cut_off
restore

echo "all data checks passed"
