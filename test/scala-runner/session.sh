#!/usr/bin/env bash
# The Scala session runner (booth.runner.Session; docs/design-scala.md section 3, ADR 0121 PR 5)
# beside the Python one (internal/runs/runner/session_runner.py), both in the runtime image just
# built, each with its bearer, as a session's driver runs them:
#   - sql gives the same JSON in both flavours, for a row of every common type (ruling 3, the
#     golden check) and for the 1,000-row cap;
#   - Scala statements run in order in one interpreter: what one defines, the next uses (ruling 2),
#     including a case class in a typed Dataset; the interpreter's echo and println are the output;
#   - a statement that doesn't compile, and one that throws, are "error" with the reason, and the
#     session goes on;
#   - a kind the flavour doesn't run, and a wrong bearer, are refused.
#
#   test/scala-runner/session.sh booth-spark-runtime:ci
set -euo pipefail
image=${1:?usage: session.sh RUNTIME-IMAGE}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
mount=$repo
command -v cygpath >/dev/null && mount=$(cygpath -m "$repo") # Git Bash on Windows (local runs)
token=booth-session-test-token
PY=${PYTHON:-python3}
failed=0
fail() { echo "FAIL: $*"; failed=1; }
cleanup() { docker rm -f booth-sess-py booth-sess-scala >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
tokdir=$(mktemp -d)
chmod 755 "$tokdir" # mktemp -d is 0700; the runners run as the image's spark user (uid 185)
printf '%s' "$token" >"$tokdir/token"
chmod 644 "$tokdir/token"
tokmount=$tokdir
command -v cygpath >/dev/null && tokmount=$(cygpath -m "$tokdir")

common=(-d -v "$tokmount:/run/booth-session:ro" -e BOOTH_SESSION_TOKEN_FILE=/run/booth-session/token -e TZ=UTC --memory 2g --entrypoint /opt/spark/bin/spark-submit)
docker run "${common[@]}" --name booth-sess-py -p 18998:8998 -v "$mount/internal/runs/runner:/w:ro" "$image" \
  --master 'local[1]' --conf spark.ui.enabled=false /w/session_runner.py >/dev/null
docker run "${common[@]}" --name booth-sess-scala -p 18999:8998 "$image" \
  --master 'local[1]' --conf spark.ui.enabled=false \
  --conf 'spark.driver.extraJavaOptions=-XX:MaxMetaspaceSize=320m -XX:ReservedCodeCacheSize=128m' \
  --class booth.runner.Session /opt/booth/booth-scala-runner.jar >/dev/null

# call PORT METHOD PATH [JSON]: sets code and body.
call() {
  local args=(-s -w '
%{http_code}' -X "$2" -H "Authorization: Bearer ${BEARER:-$token}") resp
  [ -n "${4:-}" ] && args+=(-H 'Content-Type: application/json' --data-binary "$4")
  resp=$(curl "${args[@]}" "http://localhost:$1$3" || true)
  code=${resp##*$'
'}
  body=${resp%$'
'*}
}
for port in 18998 18999; do
  for _ in $(seq 1 120); do call "$port" GET /healthz || true; [ "$code" = 200 ] && break; sleep 1; done
  [ "$code" = 200 ] || { docker logs booth-sess-py 2>&1 | tail -20; docker logs booth-sess-scala 2>&1 | tail -20; echo "FAIL: the runner on $port never became healthy"; exit 1; }
done
echo "ok: both runners are up"

n=0
# run PORT KIND CODE: runs one statement to its end; sets st (its JSON).
run() {
  n=$((n + 1))
  local id="t$n" payload
  payload=$("$PY" -c 'import json,sys; print(json.dumps({"id": sys.argv[1], "kind": sys.argv[2], "code": sys.argv[3]}))' "$id" "$2" "$3")
  call "$1" POST /statements "$payload"
  [ "$code" = 202 ] || fail "statement $id ($2) on $1: $code $body"
  for _ in $(seq 1 120); do
    call "$1" GET "/statements/$id"
    case "$("$PY" -c 'import json,sys; print(json.load(sys.stdin)["state"])' <<<"$body")" in available | error) break ;; esac
    sleep 0.5
  done
  st=$body
}
field() { "$PY" -c 'import json,sys; d=json.load(sys.stdin); v=eval(sys.argv[1]); print(json.dumps(v, sort_keys=True) if isinstance(v,(dict,list)) else v)' "$1" <<<"$st"; }

echo "--- sql: the same JSON in both flavours"
golden="SELECT 1 AS i, CAST(2 AS BIGINT) AS l, CAST(3 AS SMALLINT) AS sm, 1.5D AS d, CAST(1.25 AS FLOAT) AS f,
  CAST('1.230' AS DECIMAL(10,3)) AS dec, 'x\"y' AS s, true AS b, NULL AS n, DATE'2024-01-31' AS dt,
  TIMESTAMP'2024-01-31 12:34:56.123456' AS ts, TIMESTAMP'2024-01-31 12:00:00' AS ts0,
  ARRAY(1, 2) AS arr, NAMED_STRUCT('a', 1, 'b', 'y') AS st, MAP('k', 1) AS m, ARRAY(NAMED_STRUCT('z', CAST(NULL AS INT))) AS nested,
  X'0041FF0A' AS bin"
run 18998 sql "$golden"; py=$(field "d['output']")
run 18999 sql "$golden"; sc=$(field "d['output']")
[ "$py" = "$sc" ] || { echo "python: $py"; echo "scala:  $sc"; fail "sql output differs between the flavours"; }
grep -q '"truncated": false' <<<"$sc" || fail "sql output: $sc"
run 18998 sql "SELECT id FROM range(1001)"; py=$(field "[len(d['output']['rows']), d['output']['truncated'], d['output']['rows'][-1]]")
run 18999 sql "SELECT id FROM range(1001)"; sc=$(field "[len(d['output']['rows']), d['output']['truncated'], d['output']['rows'][-1]]")
[ "$py" = '[1000, true, [999]]' ] && [ "$sc" = "$py" ] || fail "the 1,000-row cap: python $py, scala $sc"
[ "$failed" = 0 ] && echo "ok: identical sql JSON, every type and the row cap"

echo "--- scala statements"
run 18999 scala 'case class Play(season: Int, yards: Int)
val plays = Seq(Play(2023, 5), Play(2023, -2), Play(2024, 12)).toDS()'
[ "$(field "d['state']")" = available ] || fail "defining a case class and a Dataset: $st"
run 18999 scala 'val total = plays
  .map(p => p.yards * 2)
  .reduce(_ + _)
println(s"total $total")'
out=$(field "d['output']['stdout']")
grep -q '^total 30$' <<<"$out" || fail "a later statement using the earlier ones: $st"
grep -q '^val total: Int = 30$' <<<"$out" || fail "the interpreter's echo: $st"
[ "$(field "d['output']['type']")" = text ] || fail "scala output type: $st"
run 18999 scala 'val broken: Int = "x"'
[ "$(field "d['state']")" = error ] && grep -q 'type mismatch' <<<"$(field "d['error']")" || fail "a statement that doesn't compile: $st"
run 18999 scala 'require(total == 31, s"total is $total")'
[ "$(field "d['state']")" = error ] && grep -q 'requirement failed: total is 30' <<<"$(field "d['error']")" || fail "a statement that throws: $st"
run 18999 scala 'println(total + 1)'
grep -q '^31$' <<<"$(field "d['output']['stdout']")" || fail "the session after two failures: $st"
run 18999 sql "SELECT count(*) AS n FROM range(7)"
[ "$(field "d['output']['rows']")" = "[[7]]" ] || fail "sql in a Scala session: $st"
[ "$failed" = 0 ] && echo "ok: state kept, echo, errors with their reason, the session goes on"

echo "--- refusals"
call 18999 POST /statements '{"id":"bad","kind":"python","code":"print(1)"}'
[ "$code" = 400 ] || fail "python in the Scala runner: $code"
call 18998 POST /statements '{"id":"bad","kind":"scala","code":"println(1)"}'
[ "$code" = 400 ] || fail "scala in the Python runner: $code"
BEARER=wrong call 18999 GET /healthz
[ "$code" = 401 ] || fail "a wrong bearer: $code"
BEARER=wrong call 18999 GET /statements/t1
[ "$code" = 401 ] || fail "a wrong bearer reading a statement: $code"
[ "$failed" = 0 ] && echo "ok: the other flavour's kind and a wrong bearer refused"

if [ "$failed" != 0 ]; then
  docker logs booth-sess-scala 2>&1 | tail -30
  exit 1
fi
echo "PASS: the Scala session runner"
