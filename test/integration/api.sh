#!/usr/bin/env bash
# Host-side helpers for the step 3 scripts (runs.sh, isolation.sh, egress.sh, ui-path.sh,
# uninstall.sh), sourced after lib.sh. booth-core and Keycloak are reached through port-forwards.
# Tokens are requested with Keycloak's in-cluster Host header: in dev mode Keycloak derives `iss`
# from it, so a token minted from the runner carries exactly the issuer core and booth-spark trust.
core_port=${CORE_PORT:-18083}
kc_port=${KEYCLOAK_PORT:-18091}
core="http://localhost:$core_port"
WS=acme-analytics
password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)
kc_admin=$(kubectl -n keycloak get secret keycloak-admin -o jsonpath='{.data.password}' | base64 -d)

pf_pids=()
trap 'kill "${pf_pids[@]}" 2>/dev/null || true' EXIT
kubectl -n booth-system port-forward svc/booth-core "$core_port:8080" >"/tmp/pf-core-$core_port.log" 2>&1 &
pf_pids+=($!)
kubectl -n keycloak port-forward svc/keycloak "$kc_port:8080" >"/tmp/pf-kc-$kc_port.log" 2>&1 &
pf_pids+=($!)
for _ in $(seq 1 30); do
  curl -s -o /dev/null "$core/healthz" && curl -s -o /dev/null "http://localhost:$kc_port/realms/booth" && break
  sleep 1
done

jq_() { python3 -c "import json,sys; d=json.load(sys.stdin); v=eval(sys.argv[1]); print('' if v is None else (json.dumps(v) if isinstance(v,(dict,list)) else v))" "$1"; }

# tok USER: a fresh access token for USER (editor-user, owner-user, ...), signed in through core so
# core's directory sees them (ADR 0047).
tok() {
  local t
  t=$(curl -s -X POST -H "Host: keycloak.keycloak.svc:8080" "http://localhost:$kc_port/realms/booth/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$1" -d "password=$password" | jq_ "d.get('access_token')")
  [ -n "$t" ] || fail "no token for $1"
  curl -s -o /dev/null -H "Authorization: Bearer $t" "$core/api/me"
  echo "$t"
}

# v1 TOKEN METHOD PATH [JSON] [extra curl args...]: calls /v1 through core's gateway in $WS; sets
# $code and $body.
v1() {
  local t=$1 m=$2 p=$3 data=${4:-}
  shift 3; [ $# -gt 0 ] && shift
  local args=(-s -o /tmp/v1-body -w '%{http_code}' -X "$m" -H "Authorization: Bearer $t" -H "X-Workspace: ${V1_WS:-$WS}")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' --data-binary "$data")
  code=$(curl "${args[@]}" "$@" "$core/modules/spark/v1$p")
  body=$(cat /tmp/v1-body)
}

# app_json NAME PYTHON [EXTRA_JSON_FIELDS]: an application body with inline Python.
app_json() {
  python3 -c "import json,sys; d={'name':sys.argv[1],'main':{'inlinePython':sys.argv[2]}}; d.update(json.loads(sys.argv[3] or '{}')); print(json.dumps(d))" "$1" "$2" "${3:-}"
}

# submit TOKEN NAME PYTHON [EXTRA]: submits and prints the new run's id. A 502/503/504 from the
# gateway (the backend's pod being replaced, e.g. right after a helm upgrade) is retried.
submit() {
  local data
  data=$(app_json "$2" "$3" "${4:-}")
  for _ in $(seq 1 10); do
    v1 "$1" POST /applications "$data"
    case "$code" in 502|503|504) sleep 3 ;; *) break ;; esac
  done
  [ "$code" = 201 ] || fail "submit $2: $code $body"
  echo "$body" | jq_ "d['id']"
}

# backend_settled: waits until exactly one backend pod exists and it is ready (after an upgrade,
# the old pod can still be terminating, and in the Service's endpoints, for a few seconds).
backend_settled() {
  kubectl -n booth-spark rollout status deploy/booth-spark --timeout=300s >/dev/null
  for _ in $(seq 1 60); do
    [ "$(kubectl -n booth-spark get pods -l app.kubernetes.io/name=booth-spark -o name | wc -l | tr -d ' ')" = 1 ] && return 0
    sleep 2
  done
  fail "the old backend pod never went away"
}

# wait_state TOKEN ID STATE... [timeout seconds via WAIT=]: waits until the run is in one of the
# states; prints it. Fails on a terminal state not listed.
wait_state() {
  local t=$1 id=$2 st=""
  shift 2
  for _ in $(seq 1 "${WAIT:-180}"); do
    v1 "$t" GET "/applications/$id"
    st=$(echo "$body" | jq_ "d.get('state')")
    for want in "$@"; do [ "$st" = "$want" ] && { echo "$st"; return 0; }; done
    case "$st" in succeeded|failed|stopped) fail "run $id is $st ($(echo "$body" | jq_ "d.get('reason')")), wanted $*" ;; esac
    sleep 1
  done
  fail "run $id still '$st' after ${WAIT:-180}s, wanted $*"
}

# logs TOKEN ID: the run's driver log (live, or the tail kept when it ended).
logs() { v1 "$1" GET "/applications/$2/logs?tail=5000"; echo "$body"; }

# session TOKEN [WORKSPACE]: core's iframe session cookie for the module.
session() {
  local url
  url=$(curl -s -H "Authorization: Bearer $1" -H "X-Workspace: ${2:-$WS}" "$core/api/modules/spark/iframe-url" | jq_ "d.get('url')")
  [ -n "$url" ] || fail "no iframe url"
  curl -s -o /dev/null -D - "$core$url" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p'
}

# ns_gone NS: waits until the namespace no longer exists.
ns_gone() {
  for _ in $(seq 1 180); do
    kubectl get namespace "$1" >/dev/null 2>&1 || return 0
    sleep 1
  done
  fail "namespace $1 still exists"
}

# --- sessions (build step 4) ---

# start_session TOKEN NAME [EXTRA_JSON_FIELDS]: starts a session; prints its id.
start_session() {
  local body
  body=$(python3 -c "import json,sys; d={'name':sys.argv[1]}; d.update(json.loads(sys.argv[2] or '{}')); print(json.dumps(d))" "$2" "${3:-}")
  local data=$body
  for _ in $(seq 1 10); do
    v1 "$1" POST /sessions "$data"
    case "$code" in 502|503|504) sleep 3 ;; *) break ;; esac
  done
  [ "$code" = 201 ] || fail "start session $2: $code $body"
  echo "$body" | jq_ "d['id']"
}

# wait_session TOKEN ID STATE...: waits until the session is in one of the states; prints it.
wait_session() {
  local t=$1 id=$2 st=""
  shift 2
  for _ in $(seq 1 "${WAIT:-240}"); do
    v1 "$t" GET "/sessions/$id"
    st=$(echo "$body" | jq_ "d.get('state')")
    for want in "$@"; do [ "$st" = "$want" ] && { echo "$st"; return 0; }; done
    case "$st" in succeeded|failed|stopped) fail "session $id is $st ($(echo "$body" | jq_ "d.get('reason')")), wanted $*" ;; esac
    sleep 1
  done
  fail "session $id still '$st' after ${WAIT:-240}s, wanted $*"
}

# stmt TOKEN SESSION KIND CODE: sends a statement; prints its id.
stmt() {
  local data
  data=$(python3 -c "import json,sys; print(json.dumps({'kind':sys.argv[1],'code':sys.argv[2]}))" "$3" "$4")
  v1 "$1" POST "/sessions/$2/statements" "$data"
  [ "$code" = 201 ] || fail "statement in $2: $code $body"
  echo "$body" | jq_ "d['id']"
}

# wait_stmt TOKEN SESSION STATEMENT: waits until the statement has ended; prints its JSON.
wait_stmt() {
  local st=""
  for _ in $(seq 1 "${WAIT:-240}"); do
    v1 "$1" GET "/sessions/$2/statements/$3"
    st=$(echo "$body" | jq_ "d.get('state')")
    case "$st" in available|error|cancelled) echo "$body"; return 0 ;; esac
    sleep 1
  done
  fail "statement $3 still '$st' after ${WAIT:-240}s"
}
