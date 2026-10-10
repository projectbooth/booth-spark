#!/usr/bin/env bash
# Shared helpers for the integration scripts, sourced after `set -euo pipefail`.
#
# In-cluster HTTP runs in a one-off curl pod whose logs are read after it finishes, never
# `kubectl run --rm -i`, whose attach race loses short-lived output (booth-catalog's flaky runs,
# ADR 0099's applied notes; booth-streamlit's lib.sh does the same).
ns=booth-spark
probe_ns=booth-spark-it-probe
# curlimages/curl:8.17.0, pinned by digest (resolved 2026-10-09).
probe_image=curlimages/curl@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "--- $*"; }

kubectl create namespace "$probe_ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# ready_pod NS SELECTOR [NOT-NAME]: waits (up to 180s) for a pod matching SELECTOR whose Ready
# condition is True, that is not being deleted and is not named NOT-NAME, and prints its name. A
# pod's phase stays Running while it terminates, and `rollout status` right after deleting a pod
# can answer before the deployment has noticed, so neither tells an old pod from its replacement.
ready_pod() {
  local name=""
  for _ in $(seq 1 180); do
    name=$(kubectl -n "$1" get pods -l "$2" -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    m = p["metadata"]
    if m.get("deletionTimestamp") or m["name"] == sys.argv[1]:
        continue
    if any(c["type"] == "Ready" and c["status"] == "True" for c in p.get("status", {}).get("conditions", [])):
        print(m["name"])
        break' "${3:-}")
    [ -n "$name" ] && { echo "$name"; return 0; }
    sleep 1
  done
  fail "no ready pod for $2 in $1${3:+ other than $3} after 180s"
}

# probe NAME SCRIPT: runs SCRIPT (sh) in a curl pod in $probe_ns and prints its output.
probe() {
  local name=$1 script=$2 phase=""
  kubectl -n "$probe_ns" delete pod "$name" --ignore-not-found --wait >/dev/null 2>&1
  kubectl -n "$probe_ns" run "$name" --restart=Never --image="$probe_image" --command -- sh -c "$script" >/dev/null
  for _ in $(seq 1 240); do
    phase=$(kubectl -n "$probe_ns" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  kubectl -n "$probe_ns" logs "pod/$name" 2>&1 || echo "(no logs; pod phase: ${phase:-unknown})"
  kubectl -n "$probe_ns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
}

# checked NAME SCRIPT: runs a probe whose script reports with the `check` helper below, prints its
# output, and fails unless every check passed and the script ran to the end.
#   check DESCRIPTION ACTUAL EXPECTED     (string equality)
#   contains DESCRIPTION HAYSTACK NEEDLE
read -r -d '' probe_prelude <<'SH' || true
failed=0
check() { if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1: got '$2', want '$3'"; failed=1; fi; }
contains() { case "$2" in *"$3"*) echo "ok: $1" ;; *) echo "FAIL: $1: '$3' not in '$(echo "$2" | head -c 300)'"; failed=1 ;; esac; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
SH
checked() {
  local out
  out=$(probe "$1" "$probe_prelude
$2
[ \"\$failed\" = 0 ] && echo PROBE-DONE")
  echo "$out" | sed 's/\(booth_iframe_token=\)[^ "&]*/\1<redacted>/g'
  echo "$out" | grep -q '^PROBE-DONE$' || fail "probe $1 did not pass (see above)"
}
