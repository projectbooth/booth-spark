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
#   1b. a JAR from booth-storage (main.jar, compiled here by the runtime image's scalac, pinned by
#      sha256) reads both; pinned to other bytes it never starts;
#      its own exit 1, and a main class it lacks, each fail the run;
#   1d. DROP TABLE ... PURGE from Python and from Scala (ADR 0122 part A): one request to the catalog;
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

step "1b. a JAR from booth-storage (main.jar + mainClass; docs/design-scala.md PR 1): Iceberg and s3a from Scala"
# Built here by the runtime image's own scalac (Scala 2.13, Spark's jars provided): no sbt, no download.
jar_dir=$(mktemp -d); chmod 777 "$jar_dir"; cp "$here/fixtures/JarSmoke.scala" "$jar_dir/"
docker run --rm -v "$jar_dir:/w" --entrypoint sh "${RUNTIME_IMAGE:-booth-spark-runtime:ci}" -c \
  'cd /w && mkdir classes && java -cp "/opt/spark/jars/*" scala.tools.nsc.Main -usejavacp -release 17 -d classes JarSmoke.scala && cd classes && jar cf ../smoke.jar .' \
  || fail "compiling the JAR fixture in the runtime image"
grep -qx put <<<"$(mc_ "mc pipe t/lake/acme-files/jobs/smoke.jar >/dev/null && echo put" <"$jar_dir/smoke.jar")" || fail "putting the JAR"
jar_sum=$(sha256sum "$jar_dir/smoke.jar" | cut -d' ' -f1)
# jar_run NAME MAIN-CLASS EXPECTED [SHA256]: submits the JAR as main.jar (pinned to SHA256 if given);
# sets J (the run), st (its end state) and jlog (its log).
jar_run() {
  fresh
  local pin=""
  [ -n "${4:-}" ] && pin=",\"sha256\":\"$4\""
  v1 "$T_editor" POST /applications "{\"name\":\"$1\",\"main\":{\"jar\":{\"backendId\":\"lake\",\"path\":\"acme-files/jobs/smoke.jar\"$pin},\"mainClass\":\"$2\"},
    \"args\":[\"$3\"],\"resources\":{\"executors\":{\"min\":1,\"max\":1}},
    \"dataAccess\":{\"lakehouse\":true,\"storage\":[{\"backendId\":\"lake\",\"path\":\"acme-files\",\"access\":\"read\"}]}}"
  [ "$code" = 201 ] || fail "submitting the JAR run $1: $code $body"
  J=$(echo "$body" | jq_ "d['id']")
  st=""
  for _ in $(seq 1 10); do
    T_editor=$(tok editor-user)
    st=$(WAIT=60 wait_state "$T_editor" "$J" succeeded failed 2>/dev/null) && break || true
  done
  T_editor=$(tok editor-user)
  jlog=$(logs "$T_editor" "$J")
}
# Pinned to the JAR's own hash (ADR 0121 ruling 3): the positive control for the mismatch below.
jar_run jar-smoke booth.it.JarSmoke 42 "$jar_sum"
[ "$st" = succeeded ] || { tail -60 <<<"$jlog"; fail "the JAR run (pinned to its own sha256) ended $st"; }
for want in "JAR-START scala 2.13" "JAR-ICEBERG-SUM 42" "JAR-CSV-SUM 42" "JAR-PASS"; do
  grep "^$want" <<<"$jlog" >/dev/null || { tail -40 <<<"$jlog"; fail "the JAR run's log lacks '$want'"; }
done
v1 "$T_editor" GET "/applications/$J"
grep -qx "booth.it.JarSmoke" <<<"$(echo "$body" | jq_ "d['main']['mainClass']")" || fail "the JAR run's view: $body"
grep -qx "$jar_sum" <<<"$(echo "$body" | jq_ "d['main']['jar']['sha256']")" || fail "the JAR run's view lacks its pin: $body"
ok "a JAR from booth-storage, pinned to its sha256, read the Iceberg table and the CSV as its submitter"
# Pinned to other bytes: the start gate refuses before Spark starts, naming both hashes.
other=$(printf '0%.0s' $(seq 1 64))
jar_run jar-other-bytes booth.it.JarSmoke 42 "$other"
[ "$st" = failed ] || fail "a JAR pinned to other bytes ended $st"
v1 "$T_editor" GET "/applications/$J"
reason=$(echo "$body" | jq_ "d.get('reason')")
grep -F "main.jar (booth-storage lake/acme-files/jobs/smoke.jar) has sha256 $jar_sum, not the $other the run asked for" <<<"$reason" >/dev/null   || fail "a JAR pinned to other bytes: reason '$reason'"
grep "^JAR-START" <<<"$jlog" >/dev/null && fail "Spark ran a JAR whose bytes didn't match its pin"
ok "a JAR pinned to other bytes never started: $reason"
# The job's own failure fails the run (its exit 1), and a main class the JAR lacks fails it too.
jar_run jar-wrong-sum booth.it.JarSmoke 41
[ "$st" = failed ] || fail "a JAR exiting 1 ended $st"
grep "^JAR-FAIL expected 41" <<<"$jlog" >/dev/null || { tail -40 <<<"$jlog"; fail "the failing JAR run's log lacks JAR-FAIL"; }
jar_run jar-no-class booth.it.Nope 42
[ "$st" = failed ] || fail "a JAR run with a main class the JAR lacks ended $st"
grep "Failed to load class booth.it.Nope" <<<"$jlog" >/dev/null || { tail -40 <<<"$jlog"; fail "the run's log doesn't say the class is missing"; }
ok "refused, each for its reason: the job's own exit 1, a main class the JAR lacks"

step "1c. Scala source (main.inlineScala, main.scala; ADR 0121 PR 4): typed Datasets, Iceberg and s3a, as the submitter"
fresh
v1 "$T_editor" GET /limits
grep -qx '\["python", "scala"\]' <<<"$(echo "$body" | jq_ "d['languages']")" || fail "/v1/limits doesn't offer Scala on this install: $body"
# scala_run NAME MAIN-JSON: submits a Scala application (no executors, lakehouse and one read-only
# storage location); sets S (the run), st (its end state), slog (its log) and sview (its view).
scala_run() {
  fresh
  v1 "$T_editor" POST /applications "{\"name\":\"$1\",\"main\":$2,\"args\":[\"x\",\"y\"],\"resources\":{\"executors\":{\"max\":0}},
    \"dataAccess\":{\"lakehouse\":true,\"storage\":[{\"backendId\":\"lake\",\"path\":\"acme-files\",\"access\":\"read\"}]}}"
  [ "$code" = 201 ] || fail "submitting the Scala run $1: $code $body"
  S=$(echo "$body" | jq_ "d['id']")
  st=""
  for _ in $(seq 1 10); do
    T_editor=$(tok editor-user)
    st=$(WAIT=60 wait_state "$T_editor" "$S" succeeded failed 2>/dev/null) && break || true
  done
  T_editor=$(tok editor-user)
  slog=$(logs "$T_editor" "$S")
  v1 "$T_editor" GET "/applications/$S"
  sview=$body
}
inline_json=$(python3 -c 'import json,sys; print(json.dumps({"inlineScala": open(sys.argv[1]).read()}))' "$here/fixtures/etl_inline.scala")
scala_run scala-inline "$inline_json"
[ "$st" = succeeded ] || { tail -60 <<<"$slog"; fail "the inline Scala run ended $st"; }
for want in "SCALA-CSV 42" "SCALA-ICEBERG 42" "SCALA-WROTE 84" "SCALA-ARGS x,y"; do
  grep "^$want" <<<"$slog" >/dev/null || { tail -40 <<<"$slog"; fail "the inline Scala run's log lacks '$want'"; }
done
grep -E '^(val|res[0-9]+) ' <<<"$slog" >/dev/null && fail "the inline Scala run echoed the interpreter's results"
inline_sum=$(sha256sum "$here/fixtures/etl_inline.scala" | cut -d' ' -f1)
grep -qx "$inline_sum" <<<"$(echo "$sview" | jq_ "d['main']['inlineScalaSha256']")" || fail "the inline Scala run's recorded hash: $sview"
# A Scala driver's pod: 512m heap + 640Mi overhead (ruling 8), plus the agent and two leases.
grep -qx 1312 <<<"$(echo "$sview" | jq_ "d['footprintMi']")" || fail "the Scala run's footprint (want 1152 + 160 for its sidecars): $sview"
ok "inline Scala read the CSV into a typed Dataset and the Iceberg table, wrote a table, got its args"

grep -qx put <<<"$(mc_ "mc pipe t/lake/acme-files/jobs/etl.scala >/dev/null && echo put" <"$here/fixtures/etl_file.scala")" || fail "putting the Scala file"
file_sum=$(sha256sum "$here/fixtures/etl_file.scala" | cut -d' ' -f1)
scala_run scala-file "{\"scala\":{\"backendId\":\"lake\",\"path\":\"acme-files/jobs/etl.scala\",\"sha256\":\"$file_sum\"}}"
[ "$st" = succeeded ] || { tail -60 <<<"$slog"; fail "the Scala file run ended $st"; }
grep "^SCALA-FILE 3" <<<"$slog" >/dev/null || { tail -40 <<<"$slog"; fail "the Scala file run's log lacks SCALA-FILE 3"; }
ok "a pinned .scala file from booth-storage ran"

# The control: a statement that doesn't compile stops the script there, and the run fails.
scala_run scala-broken '{"inlineScala":"println(\"SCALA-BEFORE\")\nval n: Int = \"not an int\"\nprintln(\"SCALA-NEVER\")\n"}'
[ "$st" = failed ] || fail "a Scala run that doesn't compile ended $st"
for want in "SCALA-BEFORE" "BOOTH-SCALA error: the statement at line 2 failed; the script stopped there"; do
  grep -F "$want" <<<"$slog" >/dev/null || { tail -40 <<<"$slog"; fail "the broken Scala run's log lacks '$want'"; }
done
grep "^SCALA-NEVER" <<<"$slog" >/dev/null && fail "the broken Scala run ran past its failing statement"
grep -F "exited with code 1" <<<"$(echo "$sview" | jq_ "d.get('reason')")" >/dev/null || fail "the broken Scala run's reason: $sview"
ok "a Scala statement that doesn't compile failed the run at its line"

step "1d. DROP TABLE ... PURGE from Python and from Scala (ADR 0122 part A): one request to the catalog, files gone"
# Without rest-catalog-purge, Iceberg drops the table and then walks and deletes its files itself,
# racing Lakekeeper's own purge (docs/findings-iceberg-drop-purge.md). With it, Spark deletes
# nothing: Iceberg's hint to set it, and a deleteReachableFiles job, must not appear.
for lang in py scala; do
  if [ "$lang" = py ]; then
    main=$(python3 -c 'import json,sys; print(json.dumps({"inlinePython": open(sys.argv[1]).read()}))' "$here/fixtures/purge_drop.py")
  else
    main=$(python3 -c 'import json,sys; print(json.dumps({"inlineScala": open(sys.argv[1]).read()}))' "$here/fixtures/purge_drop.scala")
  fi
  scala_run "purge-$lang" "$main"
  [ "$st" = succeeded ] || { tail -60 <<<"$slog"; fail "the $lang DROP ... PURGE run ended $st"; }
  for want in "PURGE-COUNT 10" "PURGE-DROPPED" "PURGE-GONE True"; do
    grep "^$want" <<<"$slog" >/dev/null || { tail -40 <<<"$slog"; fail "the $lang PURGE run's log lacks '$want'"; }
  done
  grep -E "Set 'rest-catalog-purge' to true|DeleteReachableFiles|deleteReachableFiles" <<<"$slog" >/dev/null \
    && fail "the $lang PURGE run deleted files client-side (rest-catalog-purge is not in effect)"
  loc=$(sed -n 's/^PURGE-LOCATION s3:\/\///p' <<<"$slog" | head -1)
  [ -n "$loc" ] || fail "the $lang PURGE run printed no location"
  # The catalog purges asynchronously (Lakekeeper's tabular_purge task): the location empties within a minute.
  left=""
  for _ in $(seq 1 30); do
    left=$(mc_ "mc ls --recursive t/$loc/ 2>/dev/null | wc -l" | tr -d ' ')
    [ "$left" = 0 ] && break
    sleep 2
  done
  [ "$left" = 0 ] || fail "$left objects left under s3://$loc after the $lang PURGE"
  ok "$lang: DROP TABLE ... PURGE succeeded, the table left the catalog, s3://$loc emptied, nothing deleted client-side"
done

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

step "2b. a Scala session with data access (ADR 0121 PR 5): Scala and sql statements, state kept, the other kind refused"
fresh
S=$(start_session "$T_editor" scala-data '{"language":"scala","resources":{"executors":{"max":0}},"dataAccess":{"lakehouse":true}}')
v1 "$T_editor" GET "/sessions/$S"
grep -qx scala <<<"$(echo "$body" | jq_ "d['language']")" || fail "the session's language: $body"
WAIT=300 wait_session "$T_editor" "$S" running >/dev/null
a=$(stmt "$T_editor" "$S" scala 'case class Sale(region: String, amount: Long)
val sales = spark.table(s"${booth.catalog}.spark_it.sales").as[Sale]')
b=$(stmt "$T_editor" "$S" scala 'val doubled = sales.map(_.amount * 2).reduce(_ + _)
println(s"SCALA-SESSION-DOUBLED $doubled")')
c=$(stmt "$T_editor" "$S" sql "SELECT count(*) AS n, sum(amount) AS s FROM lakehouse.spark_it.sales")
d=$(stmt "$T_editor" "$S" scala 'val wrong: Int = doubled.toString')
e=$(stmt "$T_editor" "$S" scala 'println(s"SCALA-SESSION-STILL ${doubled + 1}")')
ra=$(wait_stmt "$T_editor" "$S" "$a"); rb=$(wait_stmt "$T_editor" "$S" "$b"); rc=$(wait_stmt "$T_editor" "$S" "$c")
rd=$(wait_stmt "$T_editor" "$S" "$d"); re=$(wait_stmt "$T_editor" "$S" "$e")
grep -qx available <<<"$(echo "$ra" | jq_ "d['state']")" || fail "a case class and a typed Dataset over Iceberg: $ra"
out=$(echo "$rb" | jq_ "d['output']['stdout']")
grep -qx "SCALA-SESSION-DOUBLED 84" <<<"$(echo "$out")" || fail "a statement using the earlier one's Dataset: $rb"
grep -q "^val doubled: Long = 84" <<<"$(echo "$out")" || fail "the interpreter's echo: $rb"
grep -qx '\[3, 42\]' <<<"$(echo "$rc" | jq_ "d['output']['rows'][0]")" || fail "sql in a Scala session: $rc"
grep -qx error <<<"$(echo "$rd" | jq_ "d['state']")" && grep -q "type mismatch" <<<"$(echo "$rd" | jq_ "d['error']")" || fail "a statement that doesn't compile: $rd"
grep -qx "SCALA-SESSION-STILL 85" <<<"$(echo "$re" | jq_ "d['output']['stdout']")" || fail "the session after a failed statement: $re"
v1 "$T_editor" POST "/sessions/$S/statements" '{"kind":"python","code":"print(1)"}'
[ "$code" = 400 ] && grep -q "in a scala session" <<<"$body" || fail "python in a Scala session: $code $body"
v1 "$T_editor" DELETE "/sessions/$S" >/dev/null
ok "a Scala session: typed Dataset over Iceberg, state across statements, sql, an error that doesn't end it; python refused"

step "3. refused at launch, each with its reason"
fresh
for c in 'nosuch|{"storage":[{"backendId":"nosuch","path":"x"}]}|could not start: .*credential broker refused read access to nosuch/x' \
         'twice|{"storage":[{"backendId":"lake","path":"acme-files/in"},{"backendId":"lake","path":"acme-files/out"}]}|could not start: .*same bucket \(lake\)'; do
  name=${c%%|*}; rest=${c#*|}; da=${rest%%|*}; why=${rest#*|}
  v1 "$T_editor" POST /applications "$(app_json "refused-$name" "print(1)" "{\"dataAccess\":$da,\"resources\":{\"executors\":{\"max\":0}}}")"
  [ "$code" = 201 ] || fail "submit $name: $code $body"
  id=$(echo "$body" | jq_ "d['id']")
  WAIT=120 wait_state "$T_editor" "$id" failed >/dev/null
  v1 "$T_editor" GET "/applications/$id"
  grep -Eq "$why" <<<"$(echo "$body" | jq_ "d['reason']")" || fail "$name: not refused for its reason: $body"
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
