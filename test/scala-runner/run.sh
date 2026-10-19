#!/usr/bin/env bash
# The Scala runner's checks (docs/design-scala.md section 7; ADR 0121, PR 3), in the runtime image
# just built: booth.runner.SelfTest (how source splits into statements, `booth`'s helpers), then
# booth.runner.Script end to end on local-cluster, so executors are separate JVMs that fetch the
# interpreter's classes from the driver, as on Kubernetes:
#   - ok.scala succeeds: a multi-line chain, a case class in a typed Dataset, an Iceberg table by
#     the catalog's name, args, booth.storage and booth.json;
#   - a statement that doesn't compile, and one that throws, stop the script there (exit 1);
#   - source that doesn't parse runs nothing (exit 1); sys.exit(3) exits 3.
#
#   test/scala-runner/run.sh booth-spark-runtime:ci
set -euo pipefail
image=${1:?usage: run.sh RUNTIME-IMAGE}
here=$(cd "$(dirname "$0")" && pwd)
mount=$here
command -v cygpath >/dev/null && mount=$(cygpath -m "$here") # Git Bash on Windows (local runs)
runner=/opt/booth/booth-scala-runner.jar
failed=0
fail() { echo "FAIL: $*"; failed=1; }

echo "--- booth.runner.SelfTest"
docker run --rm --entrypoint /opt/spark/bin/spark-submit "$image" --master 'local[1]' --conf spark.ui.enabled=false \
  --class booth.runner.SelfTest "$runner" >/tmp/selftest.log 2>&1 || true
grep '^SELFTEST' /tmp/selftest.log || true
grep -qx 'SELFTEST PASS' /tmp/selftest.log || { tail -40 /tmp/selftest.log; fail "the self-test"; }

# script NAME [ARGS...]: runs NAME.scala with booth.runner.Script; sets rc and out (stdout and stderr).
script() {
  local name=$1
  shift
  rc=0
  docker run --rm -v "$mount:/w:ro" --memory 3g \
    -e BOOTH_LAKEHOUSE_CATALOG=lakehouse -e 'BOOTH_STORAGE=[{"backendId":"lake","path":"raw","access":"read","root":"s3a://lake/raw"}]' \
    --entrypoint /opt/spark/bin/spark-submit "$image" --master 'local-cluster[2,1,1024]' --driver-memory 1g \
    --conf spark.ui.enabled=false \
    --conf spark.sql.extensions=org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions \
    --conf spark.sql.catalog.lakehouse=org.apache.iceberg.spark.SparkCatalog \
    --conf spark.sql.catalog.lakehouse.type=hadoop --conf spark.sql.catalog.lakehouse.warehouse=file:/tmp/wh \
    --class booth.runner.Script "$runner" "/w/$name.scala" "$@" >/tmp/script.log 2>&1 || rc=$?
  out=$(cat /tmp/script.log)
}
# expect NAME TEXT...: each TEXT is in the script's output.
expect() {
  local name=$1 want
  shift
  for want in "$@"; do
    grep -F -- "$want" <<<"$out" >/dev/null || { grep -E 'SCRIPT-|BOOTH-SCALA|error' <<<"$out" | head -20; fail "$name: no '$want' in its output"; }
  done
}

echo "--- ok.scala"
script ok one two
[ "$rc" = 0 ] || { tail -40 <<<"$out"; fail "ok.scala exited $rc"; }
expect ok "SCRIPT-START scala 2.13" "args one,two" "SCRIPT-SEASONS (2023,3) (2024,12)" "SCRIPT-TYPED 10,-4,24" \
  "SCRIPT-EXECUTOR-JVMS 2" "SCRIPT-ICEBERG 30" "SCRIPT-STORAGE s3a://lake/raw" 'SCRIPT-JSON {"output":{"2023":3,"2024":12}}'
grep -E '^(val|res[0-9]+|import) ' <<<"$out" >/dev/null && fail "ok.scala: the REPL echoed a result (a script prints only what it prints)"
[ "$failed" = 0 ] && echo "ok: ok.scala"

echo "--- compile-error.scala"
script compile-error
[ "$rc" = 1 ] || fail "compile-error.scala exited $rc, want 1"
expect compile-error "SCRIPT-BEFORE" "error: type mismatch" "BOOTH-SCALA error: the statement at line 3 failed; the script stopped there"
grep -q SCRIPT-NEVER <<<"$out" && fail "compile-error.scala ran past its failing statement"

echo "--- throws.scala"
script throws
[ "$rc" = 1 ] || fail "throws.scala exited $rc, want 1"
expect throws "SCRIPT-BEFORE" "java.lang.IllegalArgumentException: requirement failed: expected 11 rows, got 10" \
  "BOOTH-SCALA error: the statement at line 3 failed; the script stopped there"
grep -q SCRIPT-NEVER <<<"$out" && fail "throws.scala ran past its failing statement"

echo "--- unparsable.scala"
script unparsable
[ "$rc" = 1 ] || fail "unparsable.scala exited $rc, want 1"
expect unparsable "BOOTH-SCALA error: the source doesn't parse; nothing ran"
grep -q SCRIPT-NEVER <<<"$out" && fail "unparsable.scala ran a statement"
grep -q 'SparkContext: Running Spark' <<<"$out" && fail "unparsable.scala started Spark"

echo "--- exit-code.scala"
script exit-code
[ "$rc" = 3 ] || fail "exit-code.scala exited $rc, want 3 (sys.exit's)"
expect exit-code "SCRIPT-BEFORE"

[ "$failed" = 0 ] || exit 1
echo "PASS: the Scala runner"
