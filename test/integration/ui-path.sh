#!/usr/bin/env bash
# Build steps 2 and 3 (docs/design-v0.md item 5): a real run's Spark UI, submitted by editor-user
# through the /v1 API, through booth-core's real iframe proxy and assertion, and booth-spark's
# proxy (default-deny allowlist, internal/uiproxy/allow.go):
#   - its submitter sees it, with links, redirects, static assets and the REST API under the run's
#     prefix, and secrets redacted on the Environment page;
#   - kill, thread-dump and heap-histogram actions are refused, whatever the method;
#   - nobody else sees it: not a workspace owner, not a platform operator, not a viewer (403), not a
#     member of another workspace (404);
#   - the driver sets no cookie on the shell's origin;
#   - then a real Chromium browses it inside an iframe on core's origin (browser/spark-ui.mjs).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
. "$here/api.sh"
port=$core_port

read -r -d '' app <<'PY' || true
# A few jobs (one with a description), a SQL query, then the driver stays up so its UI can be
# browsed.
import time
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
sc = spark.sparkContext
sc.setJobDescription("booth proof job")
print("sum", sc.parallelize(range(100000), 4).map(lambda x: x * x).sum(), flush=True)
sc.setJobDescription(None)
spark.range(1000).selectExpr("id % 7 as k").groupBy("k").count().collect()
print("PROOF-READY", flush=True)
time.sleep(1200)
PY
T_editor=$(tok editor-user)
step "editor-user submits a run whose UI stays up"
RUN=$(submit "$T_editor" ui-proof "$app" '{"conf":{"spark.sql.shuffle.partitions":"4"},"resources":{"executors":{"min":1,"max":1}}}')
echo "run $RUN"
wait_state "$T_editor" "$RUN" running >/dev/null
for _ in $(seq 1 180); do logs "$T_editor" "$RUN" | grep -q PROOF-READY && break; sleep 2; done
logs "$T_editor" "$RUN" | grep -q PROOF-READY || fail "the run never got through its jobs: $(logs "$T_editor" "$RUN" | tail -30)"

read -r -d '' common <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
field() { sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p"; }
tok() { curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$1" -d "password=$PW" | field access_token; }
iframe_url() { # USER WORKSPACE: core's iframe URL for the module (signing in through core first)
  t=$(tok "$1"); curl -s -o /dev/null -H "Authorization: Bearer $t" "$CORE/api/me"
  for i in $(seq 1 30); do
    u=$(curl -s -H "Authorization: Bearer $t" -H "X-Workspace: $2" "$CORE/api/modules/spark/iframe-url" | field url)
    [ -n "$u" ] && { echo "$u"; return; }
    sleep 2
  done
}
session() { # USER WORKSPACE: core's iframe session cookie
  u=$(iframe_url "$1" "$2")
  curl -s -o /dev/null -D - "$CORE$u" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p'
}
SH

read -r -d '' script <<'SH' || true
P=/iframe/spark/runs/$RUN/ui
ui() { curl -s -H "Cookie: booth_iframe_session=$1" "$CORE$2"; }
uistatus() { c=$1; shift; status -H "Cookie: booth_iframe_session=$c" "$@"; }
c_editor=$(session editor-user acme-analytics)
check "the submitter (editor-user) got an iframe session" "$( [ -n "$c_editor" ] && echo yes || echo no)" yes

echo "# the submitter sees the run's UI, all of it under the run's prefix"
loc=$(curl -s -o /dev/null -D - -H "Cookie: booth_iframe_session=$c_editor" "$CORE$P/" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
check "Spark's root redirect is rewritten under the prefix" "$loc" "$P/jobs/"
jobs=$(ui "$c_editor" "$P/jobs/")
contains "the jobs page renders through core and the proxy" "$jobs" "booth-spark-ui-proof"
contains "the job with a description is listed" "$jobs" "booth proof job"
contains "Spark's UI root is the run's prefix (APPLICATION_WEB_PROXY_BASE)" "$jobs" "setUIRoot('$P')"
contains "links point under the prefix" "$jobs" "href=\"$P/stages/\""
check "a static asset loads" "$(uistatus "$c_editor" "$CORE$P/static/webui.js")" 200
apps=$(ui "$c_editor" "$P/api/v1/applications")
contains "the REST API answers" "$apps" '"name" : "booth-spark-ui-proof"'
app=$(echo "$apps" | sed -n 's/.*"id" : "\([^"]*\)".*/\1/p' | head -1)
check "the executors REST endpoint the Executors tab uses" "$(uistatus "$c_editor" "$CORE$P/api/v1/applications/$app/allexecutors")" 200
env=$(ui "$c_editor" "$P/environment/")
# The driver's token path is set as spark.kubernetes.authenticate.oauthTokenFile, a name the
# redaction regex matches: the page shows the name, never the value.
contains "the Environment page shows a token-named conf" "$env" "spark.kubernetes.authenticate.oauthTokenFile"
check "... but not its value (spark.redaction.regex)" "$(echo "$env" | grep -c /var/run/secrets/kubernetes.io/serviceaccount/token)" 0
check "... nor through the REST API" "$(ui "$c_editor" "$P/api/v1/applications/$app/environment" | grep -c /var/run/secrets/kubernetes.io/serviceaccount/token)" 0
hdrs=$(curl -s -o /dev/null -D - -H "Cookie: booth_iframe_session=$c_editor" "$CORE$P/jobs/" | tr -d '\r')
check "no cookie from the driver on the shell's origin" "$(echo "$hdrs" | grep -ci '^set-cookie: ' | tr -d ' ')" 0

echo "# actions that change or expose the driver are refused"
check "kill a job (GET)" "$(uistatus "$c_editor" "$CORE$P/jobs/job/kill/?id=0")" 403
check "kill a stage (GET)" "$(uistatus "$c_editor" "$CORE$P/stages/stage/kill/?id=0")" 403
check "kill a job (POST, as the UI's form would)" "$(uistatus "$c_editor" -X POST "$CORE$P/jobs/job/kill/?id=0")" 405
check "thread dump page" "$(uistatus "$c_editor" "$CORE$P/executors/threadDump/?executorId=driver")" 403
check "heap histogram page" "$(uistatus "$c_editor" "$CORE$P/executors/heapHistogram/?executorId=driver")" 403
check "thread dump REST API (Spark serves it even with threadDumpsEnabled=false)" "$(uistatus "$c_editor" "$CORE$P/api/v1/applications/$app/executors/driver/threads")" 403
check "a dot-dot escape out of the run's prefix" "$(uistatus "$c_editor" --path-as-is "$CORE$P/../../../ui/api/me")" 400

echo "# nobody but the submitter (ADR 0110 ruling 4)"
for u in owner-user viewer-user operator-user; do
  c=$(session $u acme-analytics)
  check "$u (same workspace, not the submitter)" "$(uistatus "$c" "$CORE$P/jobs/")" 403
done
c=$(session outsider-user other-team)
check "outsider-user (another workspace): the run doesn't exist for them" "$(uistatus "$c" "$CORE$P/jobs/")" 404
check "no session at all" "$(status "$CORE$P/jobs/")" 401
check "straight to the Service, no assertion" "$(status "http://booth-spark.booth-spark.svc:8080/runs/$RUN/ui/jobs/")" 401
SH

step "the Spark UI through core (curl)"
checked ui-path "PW='$password'; RUN='$RUN'
$common
$script"

step "a pod outside the run can't reach its driver UI port; the backend's proxy can (above)"
out=$(checked ui-fence "check \"control: keycloak from the probe namespace\" \"\$(status --max-time 5 http://keycloak.keycloak.svc:8080/realms/booth)\" 200
check \"the run's driver UI, straight from the probe namespace\" \"\$(status --max-time 5 http://driver.bspark-$RUN.svc:4040/jobs/)\" 000")
echo "$out"

step "fresh iframe URLs for the browser (core's navigation token lives one minute)"
urls=$(probe ui-urls "PW='$password'
$common
echo \"editor \$(iframe_url editor-user acme-analytics)\"
echo \"operator \$(iframe_url operator-user acme-analytics)\"")
editor_url=$(echo "$urls" | awk '$1=="editor" {print $2}')
operator_url=$(echo "$urls" | awk '$1=="operator" {print $2}')
[ -n "$editor_url" ] && [ -n "$operator_url" ] || fail "could not mint iframe URLs: $(echo "$urls" | sed 's/\(booth_iframe_token=\)[^ ]*/\1<redacted>/g')"

step "Chromium through core's iframe proxy"
(cd "$here/browser" && node spark-ui.mjs "http://localhost:$port" "$editor_url" "$operator_url" "$RUN")
v1 "$T_editor" POST "/applications/$RUN/stop" >/dev/null
wait_state "$T_editor" "$RUN" stopped >/dev/null
echo "all Spark UI path checks passed"
