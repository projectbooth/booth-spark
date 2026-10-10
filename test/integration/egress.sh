#!/usr/bin/env bash
# Build step 3, egress (ADR 0110's egress ruling; step 3 requirement 4), from inside a run's driver
# as its own user code (fixtures/probe.py):
#   - runs.egress.mode=open (the default): the internet is reachable, and every in-cluster
#     destination other than the allowed ones is not: another namespace's pod, a Service ClusterIP
#     (the Service CIDR), booth-core's pod (the pod CIDR), the node's kubelet;
#   - runs.egress.mode=closed: the internet is not reachable either.
# Controls in both: DNS resolves, the Kubernetes API answers, the run's own driver port is open.
# Needs a CNI that enforces egress NetworkPolicy (the real-core job's Calico; kindnet doesn't).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lib.sh"
. "$here/api.sh"
image=$1
spark_image=$2

kc_pod=$(kubectl -n keycloak get pod -l app=keycloak -o jsonpath='{.items[0].status.podIP}')
core_svc=$(kubectl -n booth-system get svc booth-core -o jsonpath='{.spec.clusterIP}')
core_pod=$(kubectl -n booth-system get pod -l app.kubernetes.io/name=booth-core -o jsonpath='{.items[0].status.podIP}')
node_ip=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
echo "targets: keycloak pod $kc_pod, booth-core Service $core_svc, booth-core pod $core_pod, node $node_ip"
targets=$(python3 -c "import json,sys; k,s,p,n=sys.argv[1:]; print(json.dumps({'targets': {
  'the internet (1.1.1.1:443)': ['1.1.1.1', 443], 'the internet by name (example.com:443)': ['example.com', 443],
  \"another namespace's pod (keycloak)\": [k, 8080], 'a Service ClusterIP (booth-core)': [s, 8080],
  'the pod CIDR (booth-core pod)': [p, 8080], \"the node's kubelet\": [n, 10250]}}))" "$kc_pod" "$core_svc" "$core_pod" "$node_ip")
ok=1
expect() { if echo "$2" | grep -Eq "$3"; then echo "ok: $1 ($2)"; else echo "FAIL: $1: got '$2', want /$3/"; ok=0; fi; }

probe_run() { # prints the probe's JSON
  local t id extra
  t=$(tok editor-user)
  extra=$(python3 -c "import json,sys; print(json.dumps({'args': ['egress', sys.argv[1]], 'resources': {'executors': {'max': 0}}}))" "$targets")
  id=$(submit "$t" egress "$(cat "$here/fixtures/probe.py")" "$extra")
  WAIT=300 wait_state "$t" "$id" succeeded >/dev/null
  logs "$t" "$id" | sed -n 's/^PROBE //p'
}
probe_session() { # the same probe as a statement in a fresh session (step 4); prints its JSON
  local t id sid pycode res
  t=$(tok editor-user)
  id=$(start_session "$t" egress '{"resources":{"executors":{"max":0}}}')
  WAIT=300 wait_session "$t" "$id" running >/dev/null
  pycode=$(python3 -c "import json,sys; print('import sys\nsys.argv = [\'probe\', \'egress\', ' + json.dumps(sys.argv[1]) + ']\n' + open(sys.argv[2]).read())" "$targets" "$here/fixtures/probe.py")
  sid=$(stmt "$t" "$id" python "$pycode")
  res=$(WAIT=300 wait_stmt "$t" "$id" "$sid")
  v1 "$t" DELETE "/sessions/$id" >/dev/null
  echo "$res" | jq_ "d['output']['stdout']" | sed -n 's/^PROBE //p'
}
pr() { echo "$r" | python3 -c "import json,sys; print(json.load(sys.stdin)[sys.argv[1]])" "$1"; }
controls() {
  expect "control: DNS resolves" "$(pr 'control: DNS')" '^[0-9.]+$'
  expect "control: the Kubernetes API answers the driver" "$(pr 'control: the Kubernetes API')" '^200$'
  expect "control: the run's own driver port" "$(pr 'control: its own driver port')" '^open$'
  for t in "another namespace's pod (keycloak)" "a Service ClusterIP (booth-core)" "the pod CIDR (booth-core pod)" "the node's kubelet"; do
    expect "not reachable: $t (dropped)" "$(pr "$t")" '^closed:TimeoutError$'
  done
}

step "runs.egress.mode=open (the default): the internet, and only the internet"
r=$(probe_run); echo "$r"
controls
expect "reachable: the internet" "$(pr 'the internet (1.1.1.1:443)')" '^open$'
expect "reachable: the internet by name" "$(pr 'the internet by name (example.com:443)')" '^open$'
step "... and the same from a session's statement"
r=$(probe_session); echo "$r"
[ -n "$r" ] || fail "the session's probe printed nothing"
controls
expect "reachable from a session: the internet" "$(pr 'the internet (1.1.1.1:443)')" '^open$'

step "runs.egress.mode=closed: no internet either"
bash "$here/install-spark.sh" "$image" "$spark_image" --set runs.egress.mode=closed >/dev/null
backend_settled
r=$(probe_run); echo "$r"
controls
expect "not reachable: the internet (dropped)" "$(pr 'the internet (1.1.1.1:443)')" '^closed:TimeoutError$'
expect "not reachable: the internet by name (dropped)" "$(pr 'the internet by name (example.com:443)')" '^closed:TimeoutError$'
step "... and the same from a session's statement"
r=$(probe_session); echo "$r"
[ -n "$r" ] || fail "the session's probe printed nothing"
controls
expect "not reachable from a session: the internet (dropped)" "$(pr 'the internet (1.1.1.1:443)')" '^closed:TimeoutError$'

step "back to the default"
bash "$here/install-spark.sh" "$image" "$spark_image" >/dev/null
backend_settled
[ "$ok" = 1 ] || fail "egress checks failed (see above)"
echo "all egress checks passed"
