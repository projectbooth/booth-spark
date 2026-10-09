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

## Not covered yet (later build steps)

- Run namespaces, the two admission policies, the exact-rules RBAC tests, isolation, and uninstall cleaning up run namespaces, policies and ClusterRoles against a real core (step 3).
- The Spark UI path (step 2), sessions and idle shutdown (step 4), data access and egress (step 5).

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
