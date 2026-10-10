# Integration tests (layer 3)

`contracts/testing-strategy.md` layer 3: a real kind cluster (kind v0.33.0, Kubernetes 1.35.8, node
image pinned by digest), run by `.github/workflows/integration.yml` on pull requests, on merge to
`main`, nightly and by hand. ADR 0110 merges each build step only with a green run on its PR's exact
head SHA.

| Job | Deploys | Covers |
|---|---|---|
| `standins` | booth-core's BoothModule CRD (vendored from `CORE_REF`, `fixtures/boothmodule-crd.yaml`), a throwaway PostgreSQL, then this chart with its own database Secret | The chart installs on a real 1.30+ cluster (its ValidatingAdmissionPolicy check passes against live discovery); core's real CRD schema keeps every manifest field; the backend reaches Postgres; `/healthz`; every iframe route refuses a caller without an assertion (including unknown paths); `/v1` answers 503 with no issuer configured; the service account has no Kubernetes API access and no token mounted. |
| `real-core` | A real Keycloak (pinned digest) and a real booth-core built at `CORE_REF`, then this chart with its defaults plus every identity setting | Everything above, plus: core provisions `booth-database-credentials` (ADR 0053) and marks the module `Healthy`; `identity.sh` (below). The job first checks the vendored CRD is byte-identical to core's at `CORE_REF`. |

`identity.sh` (docs/design-v0.md item 2):

- `/v1` through core's gateway with real Keycloak tokens: role derived from the token, `submit.minRole` applied (owner and editor may submit, viewer and an operator-viewer may not), `/platform/operator` read off the token, display name from `preferred_username`; an outsider is refused.
- Straight to the Service, bypassing the gateway: a forged `X-Booth-Role: owner` on an editor's token is 403 (ADR 0041); no token or an untrusted one is 401.
- A real workload token, minted by core for a test-only BoothModule declaring `workloadIdentity.mint`, is accepted on `/v1` and marked as a workload caller (ADR 0056/0059), and refused as an iframe assertion.
- The iframe path through core's real proxy: core's real assertion gives the right subject, role, operator claim and display name; no session is 401.
- Each credential only on its own route group: a bearer token on an iframe route, or an OIDC token presented as an assertion, is 401.
- Keys come from `oidc.jwksUrl`: it is deliberately spelled differently from the issuer (`…svc.cluster.local` against `…svc`), and the backend's single startup log line names it, the issuer and the workload issuer. Nothing token-shaped is logged.

The real-core job runs kind **with Calico** (`cni/`): kindnet enforces NetworkPolicy ingress but not
egress, and the run namespaces' egress rules are a boundary (ADR 0110). Then, in order:

- `runs.sh`: an editor's PySpark application runs with 2 real executors in its own labelled,
  restricted namespace owned by the driver ClusterRole, succeeds, keeps its log, and its namespace
  goes. A viewer, and an operator who is a viewer, can't submit. Logs are for the submitter, owners
  and operators only. A failing application is `failed` with the driver's exit. An owner stops an
  editor's run. Admission refuses what doesn't fit the memory budget (429).
- `sessions.sh` (step 4): an editor's session, in its own namespace, runs SQL and Python statements
  in order in one SparkSession. State is kept from statement to statement, and a failing statement
  is `error` with its traceback while the session goes on. Only its submitter and workspace owners
  see it (another editor, a viewer and an operator get 404), and only its submitter runs
  statements. Idle shutdown: a statement running longer than the 60s test timeout keeps the session
  alive, and once nothing runs it stops on its own and its namespace goes. Its maximum lifetime
  stops it while busy and cancels the running statement. An owner deletes it. A backend restart
  mid-statement re-adopts it: the result arrives and the next statement runs. An orphaned run
  namespace is reaped.
- `isolation.sh`: run A (an editor's application), then session E (an editor's, as a statement),
  probe session B (owner), whose driver really listens on its RPC, UI and runner ports, as their own
  user code (`fixtures/probe.py`):
  B's pods, Secrets, namespace and driver ports are refused. In its own namespace the run-pods policy
  refuses a non-allowlisted image, a missing nodeSelector, any account but `executor`, a mounted
  token, and another workspace's label. A pod outside every run can't reach a driver's UI port.
  Impersonating the backend's account: no namespace or RoleBinding outside its fence, no label on any
  namespace, nothing in kube-system. The executor account has no permissions. Every refusal has a
  control that must succeed.
- `ui-path.sh`: a real run's Spark UI through core's real iframe proxy (curl, then Chromium: Jobs,
  Executors, a stage's detail page, Environment, SQL). It is for the submitter only; owners, viewers
  and operators get 403, another workspace 404. The allowlist refuses kill, thread-dump and
  heap-histogram pages and the REST threads endpoint. A token-named conf is redacted, the driver
  sets no cookie, every request stays under the prefix, and the driver's UI port is closed to
  anything but the backend's pods.
- `egress.sh`: from inside a run, and from a session's statement, with mode `open`, the internet is reachable while another
  namespace's pod, a Service ClusterIP, booth-core's pod and the node's kubelet are not. With mode
  `closed`, the internet is not reachable either. DNS, the API and the run's own port stay reachable
  as controls.
- `uninstall.sh`: `helm uninstall` with a live run, then a reinstall as release `spark` uninstalled
  through booth-core's module uninstall API with a live run. Each time, no run namespace, policy,
  binding, ClusterRole or `default` Role is left.

## Not covered yet (later build steps)

- Data access (step 5).

## Running locally

```sh
kind create cluster --name booth-spark-ci --image "$KIND_NODE_IMAGE"   # value in integration.yml
docker build -t booth-spark:ci . && kind load docker-image booth-spark:ci --name booth-spark-ci
test/integration/deploy-standins.sh booth-spark:ci
test/integration/verify.sh
```

For `real-core`, check out booth-core at `CORE_REF`, build and load its image the same way, and run
`deploy-realcore.sh <core checkout> booth-core:ci booth-spark:ci`, then `REAL_CORE=1 verify.sh` and
`identity.sh`, on a fresh cluster.
