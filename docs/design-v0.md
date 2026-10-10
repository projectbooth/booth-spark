# booth-spark v0 design note

Status: **draft for coordinator review**, 2026-10-09. Nothing here is built. The repo holds only a
README and this note. The service, chart, CI and the identity code come in the first commit after
the ruling. Points that need a ruling are marked **(ruling)** and gathered in section 10.

Inputs: `agent-briefs/spark.md` (with the 2026-10-09 kickoff), `ARCHITECTURE.md` (items 37, 55, 57),
ADRs 0006, 0029, 0041, 0056 to 0059, 0069, 0077, 0080, 0088, 0094, 0095, 0096, 0099 and 0100 to 0108,
the module-manifest, core-platform-api, ui-integration, testing-strategy, credential-broker and
credential-sidecar contracts, and, for house style, `booth-streamlit` (`docs/design-v0.md`,
`docs/design-data-access.md`, its chart) and `booth-api` (`internal/auth`, `internal/config`, CI).

**In one paragraph.** One small always-on Go backend. It serves the API, the module's UI, the Spark
UI proxy, and the run controller. Each run (a batch application or an interactive session) gets
**its own namespace**, holding one driver and 0 to N executors. The namespace is deleted when the
run ends. No Spark Operator. A run reads data as the person who submitted it, with a role no higher
than editor, through the fleet's credential sidecars. Only the person who submitted a run may open
its Spark UI, because that UI is content the run's own code controls (item 5).

## 1. Packaging: plain Spark-on-Kubernetes, with the module backend as the only controller

**Recommendation: no Spark Operator (ruling: the brief expected one).** For each run the backend
creates the objects that `spark-submit --deploy-mode cluster` creates: a driver Pod, a headless
Service and a ConfigMap holding `spark.properties` and the executor pod template. The driver
container runs the stock image entrypoint (`driver --properties-file …`), so the backend needs no
JVM. The driver then asks the Kubernetes API for executors through Spark's own scheduler backend.
It uses dynamic allocation with shuffle tracking, since there is no external shuffle service on
Kubernetes.

Why not the operator? I checked kubeflow `spark-operator` chart 2.5.2 (2026-07-31, the current
release):
- It installs three cluster-scoped CRDs (`SparkApplication`, `ScheduledSparkApplication`,
  `SparkConnect`) from `crds/`. Helm never upgrades those, and uninstall leaves them behind.
- It runs a controller and a mutating webhook (`webhook.enable: true` by default): two always-on
  pods plus webhook certificates.
- It watches `spark.jobNamespaces`, which must already exist (default `[default]`). That conflicts
  with per-run namespaces (item 3).
- booth-spark still needs its own controller for authorization, idle shutdown, quotas, the UI proxy
  and the API, so we would have two controllers acting on the same pods.

The fleet precedent is the module backend as the only controller: booth-pipeline's Job per task
(ADR 0096) and booth-streamlit's Deployment per app (its `design-v0.md` (a)). If the ruling is for
the operator anyway, the fallback is chart 2.5.2 pinned by digest, with the webhook off and
`jobNamespaces` selected by label.

**Versions, pinned by digest** (resolved against the registries on 2026-10-09):
- **Spark 4.1.3**: `apache/spark:4.1.3-scala2.13-java21-python3-ubuntu@sha256:bf9d035a7c32a8ca46aa58d6348182ffd7d2dff6409206ecfbb3915ff1fef211`
  (amd64 and arm64, about 810 MB compressed).
  - Not 4.2.0, the newest release: Maven Central has no `iceberg-spark-runtime-4.2` yet. Iceberg
    1.12.0 ships runtimes for Spark 4.0 and 4.1.
  - Spark 4.1.3 builds against Hadoop 3.4.2 and AWS SDK v2 2.29.52 (`spark-parent` pom). The added
    jars match those exactly: `hadoop-aws` 3.4.2, the SDK bundle 2.29.52, `iceberg-spark-runtime-4.1_2.13`
    and `iceberg-aws-bundle` 1.12.0, and a PostgreSQL JDBC driver.
  - Each jar is downloaded at build time and refused unless its sha256 matches. This is
    booth-streamlit's DuckDB-extension precedent.
- **Backend:** builder `golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c`,
  runtime `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab`.
- **Credential sidecar:** `ghcr.io/projectbooth/credential-sidecar@sha256:6a0a795efd27f165e0714beb163d91f5c2feff55cfdc6f287aee979ae02cce14`,
  the digest booth-api, booth-streamlit, booth-notebooks and booth-pipeline already pin.

**What runs when:**
- **Always:** the backend Deployment, one replica, about 64Mi.
- **On demand:** a run's namespace with its driver and its executors.
- **Never in v0:** an operator, a history server, a shuffle service.

The runtime image (`images/spark-runtime`) adds two small module-owned pieces: the session runner
(item 6) and the Java credential helpers (item 4).

## 2. Identity: the fleet's token settings, with `oidc.jwksUrl` from the first commit

Three issuers, and each is accepted on exactly one route group:

| Chart value (env) | Default | Route group | Precedent |
|---|---|---|---|
| `oidc.issuerUrl` (`BOOTH_OIDC_ISSUER_URL`) | `""` (empty: `/v1` answers 503) | `/v1/*` through core's gateway, bearer token | booth-api, booth-storage |
| `oidc.clientId`, `oidc.requireAudience` | `""`, `true` | same | same; `clientId` required when the issuer is set |
| `oidc.groupsClaim` | `groups` | every route group | ADR 0025, must match core |
| `oidc.jwksUrl` (`BOOTH_OIDC_JWKS_URL`) | `""` (empty: discovery) | key fetch for `oidc.issuerUrl` | ADR 0108; booth-api `5f05949` |
| `oidc.workloadIssuerUrl` (`BOOTH_WORKLOAD_ISSUER_URL`) | `""` | `/v1/*`, core's workload tokens (pipeline tasks) | booth-storage, ADR 0056/0059 |
| `identity.issuerUrl` | `http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity` | the iframe routes: the module UI and the Spark UI proxy, `X-Booth-Identity` only | booth-streamlit, ADR 0069 |

The rules come straight from the precedents:
- `internal/auth` is booth-api's (`NewVerifier` with the JWKS override). It also gets booth-storage's
  `WorkloadIssuerURL` addition. Tests are copied with both:
  - booth-api: `TestVerifierJWKSOverride`, `TestVerifierWithoutOverrideUsesDiscovery`,
    `TestNewVerifierRejectsJWKSURLWithoutIssuer`, `TestNewVerifierLogsIssuerAndKeySourceOnce`,
    `TestLoad_JWKSURL`;
  - booth-storage: `TestVerifier_WorkloadIssuer`, `…DownAtStartup`, `…TrailingSlash`,
    `TestNewVerifier_WorkloadIssuerMustDifferFromIdP`.
- `internal/identity` is booth-streamlit's. `aud` must be `spark`, only asymmetric algorithms are
  accepted, and subjects shaped like workload tokens are refused.
- Startup errors: `jwksUrl` set without `issuerUrl`, or `clientId` empty with `issuerUrl` set. The
  effective issuer and key URL are logged once. `iss` is always compared exactly.
- Roles come from the verified token's `groups` (ADR 0041). `X-Booth-Role` can only narrow them,
  and a header that claims more is refused. `/platform/operator` is read off the same verified
  claims (ADR 0094); core carries it in the assertion since `264856f`.
- A workload token never opens the UI, and an iframe assertion is never accepted as a bearer on
  `/v1`. The issuers, audiences and route groups all differ.
- The iframe issuer is core's in-cluster URL, so it needs no key override (the ADR 0108 inventory
  says the same of booth-streamlit). `identity.issuerUrl` copies booth-streamlit's name, and its
  values comment carries the `svc` vs `svc.cluster.local` warning from ADR 0069.

## 3. Who may do what, and how workspaces are kept apart

Arbitrary user code runs on the cluster, so the proposal starts from owner and editor:

| Action | viewer | editor | owner | `/platform/operator` |
|---|---|---|---|---|
| See runs and their status in the workspace | yes | yes | yes | every workspace |
| Submit an application, start a session | no | yes | yes | only as their role in that workspace |
| Send statements to a session | the session's owner only | | | no |
| Stop a run | — | their own | any in the workspace | any (to free a node) |
| Read a run's logs | no | their own | any in the workspace | any |
| Open a run's Spark UI | the run's submitter only (item 5) | | | no |

Limits and quotas are chart values; there is no UI for them. Logs are restricted because user code
can print its own token (item 4). If a viewer could read an editor's log, that would be a route to
editor access. Owners already hold more than any run's token carries, and booth-logging's own
policy (ADR 0067/0077) gives owners and operators the same pod logs anyway.

**Isolation: one namespace per run (ruling, because ADR 0029 deliberately left namespace strategy
open).** The deciding fact is Spark-specific. **The driver must hold Kubernetes API rights to
create executor pods, and the driver runs the user's code.** That is a departure from ADR 0057's
rule that a user-code pod has `automountServiceAccountToken: false` and no RBAC.

Why one namespace per run:
- In a shared namespace, that code could read every other run's pod specs, mount every other run's
  Secret into a pod it creates, and delete other runs' pods. Kubernetes RBAC can't restrict by
  label.
- A namespace per workspace still leaves those attacks open between runs in the same workspace,
  from an editor's run against an owner's.

Namespace `bspark-<runId>` holds only that run's objects:
- **Labels:** `booth.projectbooth.io/workspace: <ws>` (ADR 0077, set by the backend from verified
  identity) and Pod Security Admission `enforce: restricted`.
- **Accounts:** a `driver` ServiceAccount bound to the chart's fixed ClusterRole
  `booth-spark-driver`, which covers pods, configmaps and persistentvolumeclaims in that namespace
  only. Step 1 pins the exact minimum with a test. An `executor` ServiceAccount with no
  permissions and `automountServiceAccountToken: false`.
- **Limits:** a ResourceQuota and a LimitRange sized from the run's request (item 7).
- **NetworkPolicy:**
  - Default deny.
  - Driver and executors may reach each other.
  - DNS.
  - The driver may reach the API server, by its endpoint IPs. The backend reads the `kubernetes`
    Endpoints in `default`, so this is not hand-configured.
  - Ingress to the driver's UI and session ports from the backend's pods only.
  - Egress to the backend's internal port and to the data backends in item 4.
  - Internet egress is closed by default (`runs.egress.mode: closed | open`; booth-pipeline also
    defaults to closed).
- **The per-run bearer** Secret (item 4).
- **Deleting the namespace** ends the run, its pods, every sidecar's lease renewal and its
  objects, in one step.

**Two ValidatingAdmissionPolicies** (cluster-scoped, from the chart; Kubernetes 1.30 or later):
1. **Pods in a namespace labelled `booth.projectbooth.io/spark-run`** must:
   - use only images on the pinned digest allowlist (Spark runtime, credential sidecar, the
     booth-spark agent);
   - carry the namespace's workspace label;
   - carry the configured `nodeSelector` entries (item 8).

   A pod the driver creates must also run as the `executor` ServiceAccount, with no token mounted.
   So the driver can create executor-like pods within its quota, and nothing else.
2. **The backend's own fence.** The backend's cluster-wide power is limited to:
   - creating and deleting namespaces that carry the label and the name prefix;
   - creating RoleBindings inside such namespaces, to exactly two ClusterRoles
     (`bind` with `resourceNames`).

   All its namespaced work happens through a RoleBinding it creates in each run's namespace. It
   gets no `update` on Namespaces, so it cannot label `kube-system`. It never holds cluster-wide
   `pods`, `secrets` or `pods/log`.

booth-spark would be the first module chart that ships cluster-scoped RBAC and admission policies.
I'd like that confirmed (section 10).

## 4. Data access: as the submitter, capped at editor, through the fleet's sidecars

**Identity.** The manifest declares `workloadIdentity: {mint: true}` (ADR 0056/0058). For each run,
the backend mints a token with:
- `subject: spark:<workspace>:<runId>` (matches core's `<kind>:<id>` rule, as booth-streamlit's
  `streamlit:<ws>:<appId>` does);
- `owner: <submitter sub>`;
- `roleCeiling: editor`. Data access never needs owner. A viewer cannot submit at all.

The minted role is the lesser of the ceiling and the submitter's current role, so a demoted editor's
next mint is a viewer token. A refused mint (the owner lost access, or the 7-day rule) **fails the
run and deletes its namespace**. The backend re-mints at two thirds of a token's life (booth-api's
rule) and serves tokens only on its internal port. The minting credential stays in the backend pod
(ADR 0057).

**How the token reaches the pods.** The driver and every executor run a small `agent` container
(the backend image, a native sidecar). It authenticates with the per-run bearer and fetches the
current token. It writes the token to a memory-backed volume shared with the credential sidecars
and the Spark container. No Kubernetes object ever holds a token (booth-pipeline's rule).

**Paths:**
- **booth-database:** a `postgres` sidecar in the driver **and in every executor**, because JDBC
  partition reads run on executors. App code gets `DATABASE_URL`. The sidecar uses
  `--access=readwrite` for an editor's or owner's run, and `read` if the minted role came back as
  viewer. Off unless `boothDatabase.url` is set (ADR 0092 gate).
- **booth-lakehouse:** for the warehouse location (`GET /api/warehouse` with the run's token; a 404
  means no sidecar), an `s3` sidecar in every pod. Two module-owned Java helpers are in the image:
  - an s3a `AwsCredentialsProvider` that re-reads the sidecar's file when it changes. Hadoop's
    static providers don't pick up renewed keys; this is the credential-sidecar contract's "Known
    limitation" for engines that aren't boto3-based.
  - an Iceberg `AuthManager` that reads the token file on every REST call (the REST catalog is
    booth-lakehouse's API through core's gateway, with `X-Workspace`).
- **booth-storage files:** a submission may name up to 4 `{backendId, path}` prefixes (ADR 0045).
  Each gets its own `s3` sidecar process and s3a per-bucket settings. Only S3-type backends work:
  filesystem, Azure and GCS backends have no Spark path in v0.
- **No route to booth-core.** The sidecars' `--core-url` and the Iceberg REST base point at the
  backend's internal port. It forwards `POST /api/credentials` and `/modules/lakehouse/*` to core
  unchanged, with the caller's own bearer (booth-streamlit's ADR 0107 item 3 shape).

**Every credential a run's code can read in its own pods:**

| Credential | Issued and owned by | Grants | Lifetime |
|---|---|---|---|
| Workload token (`spark:<ws>:<runId>`) | booth-core, minted by booth-spark | the submitter's role in the workspace, at most editor, through the gateway | 10 min, renewed while the run lives |
| Per-run bearer | booth-spark | fetch *this run's* token; statements into *this* session | the run |
| Driver ServiceAccount token | Kubernetes | pods/configmaps/PVCs in this run's namespace, fenced by policy 1 | bound token, the pod's life |
| S3 keys (warehouse, declared prefixes) | booth-storage, via the broker | read or readwrite on those prefixes | lease, at least 15 min on MinIO |
| Postgres lease | booth-database, via the broker | `SELECT` (plus writes if readwrite) on the workspace's `public` schema | up to 1 h; the code can't read it directly, but can start its own sidecar with its own token |
| Spark RPC secret (`spark.authenticate=true`) | Spark itself | talking to this driver | the run |

There is no module database credential, no minting credential, no event-bus credential, and no
token belonging to another run.

**Residuals, stated plainly (as ADR 0107 does):**
1. **Whole-workspace scope.** A read lease is the whole `public` schema or warehouse prefix
   (ADR 0095). An editor's run can also write there.
2. **S3 keys can be copied.** They stay valid until their lease expires, even after the run is
   gone. That matters once egress is open.
3. **Network reach.** NetworkPolicy is per pod. User code can reach everything the sidecars reach,
   **including the Kubernetes API server**, which it calls with its own narrow ServiceAccount.
   API-server vulnerabilities are therefore within reach of user code. This exposure is specific to
   Spark on Kubernetes.
4. **Pods the driver creates.** The driver can spend its quota on pods running the allowlisted
   images. That is no more than it can already do with its own code.
5. **Revocation lag.** On a refused mint the namespace is deleted at once, which ends every
   connection. Copied S3 keys outlive that.
6. **CNI enforcement.** All of this depends on the CNI enforcing NetworkPolicy (ARCHITECTURE item
   37b). kind's default CNI does, and booth-pipeline's Integration relies on that. The homelab's
   k3s is checked by a negative test.
7. **Runs submitted with a workload token** (e.g. a pipeline task) have no human `owner` to mint
   for: ADR 0058 keeps the owner out of the token. v0 accepts them with `dataAccess` empty and
   refuses data access with a 422 (ruling).

## 5. The Spark UI behind iframe-proxy

The path:

```
browser ─ /iframe/spark/runs/<id>/ui/… ─▶ core (+ X-Booth-Identity, ADR 0069) ─▶ backend ─▶ driver:4040
```

**The backend is the only route.** Driver ingress allows the backend's pods only.

On every request, including websockets, the backend:
1. verifies the assertion (item 2);
2. requires that the run belong to the assertion's workspace and that the caller is its submitter;
3. strips **every** inbound `X-Booth-*` header, plus `Authorization` and `Cookie` (core's
   `booth_iframe_session` cookie included), before forwarding. The assertion is a 2-minute bearer
   for this module (booth-streamlit's design-v0 (b) step 3).

The driver gets no identity at all.

**Kill and admin actions are switched off in Spark itself, then blocked again at the proxy:**
- `spark.ui.killEnabled=false`.
- `spark.ui.threadDumpsEnabled=false` and `spark.ui.heapHistogramEnabled=false`: they show stack
  and heap contents.
- `spark.redaction.regex` is extended to cover `token|bearer|credential`. Secrets are passed as
  file paths, never as conf values.
- The proxy forwards only GET and HEAD and refuses `*/kill*` paths with 403.
- The driver's UI proxy base is set so links resolve under the prefix. This is the
  `APPLICATION_WEB_PROXY_BASE` environment variable, or the `spark.ui.proxyBase` system property,
  which Spark 4.1.3's `UIUtils` reads; neither is documented. Step 1 proves this against a real
  core, as booth-streamlit's step 1 did.

Stopping a run is the module's own API call, gated by item 3's table.

**What we do about the UI's lack of authentication.** We don't rely on Spark for any of it:
- Nobody reaches 4040 except the backend.
- The backend decides who.
- Spark's own `spark.ui.filters` and ACLs are not a boundary, because they run inside the user's
  JVM, which can switch them off.

That same fact is why **only the submitter may open the UI**. The user's code owns the driver's
Jetty and can serve any HTML or JavaScript on 4040. The shell embeds iframes same-origin with
`allow-scripts allow-same-origin` (ADR 0069). So a workspace owner or operator who opened an
editor's Spark UI would run that editor's script with access to their own bearer token. This is
ARCHITECTURE item 55, the hole ADR 0105 closed for Streamlit. Restricting the UI to the submitter
means the author is the only person who runs the script. Everyone else sees run status, state and
logs in the module's own UI, which the backend renders as escaped text.

The Spark UI disappears when the driver exits. There is no history server in v0.

## 6. The published job-submission interface (`/v1`)

The interface is served on the ordinary gateway route `/modules/spark/v1/…` (bearer +
`X-Workspace`). It accepts people's OIDC tokens and core's workload tokens (ADR 0059).
- **Documented:** OpenAPI 3.1 at `docs/api/v1.yaml`, also served at `/v1/openapi.json`, and linted
  in CI with `@redocly/cli` as booth-api does.
- **Versioning:** within `v1`, changes are additive only (new optional fields, new endpoints).
  Anything breaking is `/v2`, served alongside `/v1` for at least one minor release.
- **Errors:** `{error, code}` in JSON. `POST` requests take an `Idempotency-Key` header, so a
  retrying caller doesn't start two runs.

| Endpoint | What it does |
|---|---|
| `POST /v1/applications` | `{name, main: {python: {backendId, path}} \| {jar: {backendId, path}, mainClass} \| {inlinePython: "<≤256 KiB>"}, args[], resources: {driver: {cores, memory}, executors: {min, max, cores, memory}}, conf: {…allowlisted keys only}, dataAccess: {database, lakehouse, storage: [{backendId, path, access}]}, maxDuration}` returns 201 with `{id, state, …}` |
| `GET /v1/applications[?state=&mine=true]`, `GET /v1/applications/{id}` | `state`: `pending → running → succeeded \| failed \| stopped`, with reason, times and resources |
| `POST /v1/applications/{id}/stop` | stop (item 3's rule) |
| `GET /v1/applications/{id}/logs?tail=` | driver log: live while running, the last 1 MiB after it ends (kept in the module's own database) |
| `POST /v1/sessions`, `GET`, `POST …/stop` | the same resource and data-access body; `idleTimeout` |
| `POST /v1/sessions/{id}/statements` `{kind: sql \| python, code}`, `GET …/statements/{sid}` | runs in the session's SparkSession; results capped at 1,000 rows or 1 MiB, JSON |
| `GET /v1/limits` | what this caller may request, so a client doesn't guess |

**Sessions in v0.** A session is a long-lived driver running a module-owned PySpark session
runner. It listens on the pod IP and accepts statements only from the backend, with the per-run
bearer. That keeps every client on the gateway's HTTP path.

**Spark Connect is not enabled.** It would be an unauthenticated gRPC port, which the HTTP gateway
doesn't carry (ADR 0007). Whether notebooks should later get a Spark Connect endpoint is a question
for later (section 10).

There is no consumer-side wiring anywhere (ADR 0006).

## 7. Homelab defaults and idle shutdown

All chart values, sized so that a default run fits a machine with about 4 GB free:

| | Request | Limit | Spark-side |
|---|---|---|---|
| Backend | 50m / 64Mi | 500m / 256Mi | — |
| Driver | 250m / 896Mi | 1 CPU / 896Mi | `spark.driver.cores=1`, `memory=512m` (+384Mi overhead) |
| Executor (each) | 250m / 896Mi | 1 CPU / 896Mi | `cores=1`, `memory=512m`; dynamic allocation `min 0`, `max 2`, `executorIdleTimeout=60s` |
| Agent + each sidecar | 10m / 16Mi | 100m / 64Mi | — |

Install-wide defaults:
- `runs.maxRunning: 2` and `runs.maxRunningPerWorkspace: 1`.
- `runs.memoryBudget: 4Gi`. The backend admits a run only if the largest footprint of every running
  run fits. A default run's largest footprint is about 2.9Gi, so one full-size run runs at a time,
  and two small ones fit.
- `runs.maxExecutors: 2`.
- `applications.maxDuration: 6h`.

**Idle shutdown:**
- Executors leave a session after 60 s idle, so an idle session holds only its driver.
- A session with no statement for `sessions.idleTimeout: 20m` is stopped and its namespace deleted,
  bringing it to zero pods. `sessions.maxLifetime: 12h`.
- A finished application's namespace is deleted as soon as its final state and log tail are
  recorded.

**First run.** The Spark image is about 810 MB compressed, so the first run on a node waits for the
pull. The operations doc says so. There is no pre-pull DaemonSet in v0.

## 8. Placement (allowed for Spark alone; ARCHITECTURE item 57)

Plain Kubernetes names, one block per kind of pod, as booth-database (`postgres.nodeSelector`,
`affinity`, `tolerations`) and booth-logging do:

```yaml
nodeSelector: {}        # the backend (controller)
tolerations: []
affinity: {}
runs:
  driver:   {nodeSelector: {}, tolerations: [], affinity: {}}
  executor: {nodeSelector: {}, tolerations: [], affinity: {}}
```

Where they are applied:
- **Driver:** on the Pod the backend creates.
- **Executors:** through the executor pod template.
- **Enforcement:** admission policy 1 (item 3) requires the configured `nodeSelector` on every pod
  in a run namespace, so a driver can't place pods elsewhere. Tolerations and affinity aren't
  enforced; leaving one out only keeps a pod off a tainted node, which is the safe direction.

For the two-server homelab, the operations doc gives one recipe: label and taint the second node
`booth.projectbooth.io/pool=compute`, and set both pod kinds' `nodeSelector` and toleration to
match. The fleet-wide convention stays a coordinator decision. Nothing here assumes it.

## 9. CI

Every job is pinned to `runs-on: ubuntu-24.04` (ADR 0099).

**`ci.yml`, on every push and PR, required by branch protection on `main`:**
- `go`:
  - gofmt, go mod tidy and vet;
  - `go test -race` against a real PostgreSQL (booth-api's `hack/docker-compose.yml` pattern),
    covering the auth/identity/config tests in item 2, the run state machine, admission, idle and
    quota logic, and the API handlers;
  - contract tests that render the chart with `helm template` and check:
    - the BoothModule against `contracts/module-manifest.md` and booth-core's real CRD schema;
    - the backend's ClusterRole and Role, and the driver ClusterRole, are *exactly* the design's
      (booth-streamlit's `TestChart_BackendRoleIsExactlyTheDesignNotes`);
    - both admission policies;
    - every image is digest-pinned.
- `python`: pytest for the session runner.
- `java`: unit tests for the two helpers.
- `web`, `helm-lint`.
- `image`: builds the backend and runtime images, with jar sha256 checks.
- The OpenAPI lint.

**`integration.yml`, on merge to `main`, nightly, and on dispatch.** The first step after checkout is
`docker/login-action@v3` with `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`, using booth-core's guard
for forks and Dependabot. That came from booth-core PR #11, merged 2026-10-09. The job then creates
a kind cluster from a digest-pinned `kindest/node` image (the default kindnet CNI enforces
NetworkPolicy), and deploys Keycloak and a **real booth-core** built from a pinned `CORE_REF`
(booth-api's `realstack` pattern), then booth-spark. It checks:
- **Registration and access:** the BoothModule reconciles. SparkPi (Python) succeeds as an editor
  and is refused for a viewer. A forged `X-Booth-Role` gets 403. A workload token cannot open the
  UI.
- **The Spark UI:** it loads through the real iframe path for the submitter and gets 403 for
  another owner. Kill paths are refused.
- **Isolation:** run A's driver tries run B's namespace, Secrets, pods and driver port; a pod with a
  non-allowlisted image; a pod without the `nodeSelector`; and a write in `kube-system` as the
  backend. All are refused (booth-pipeline's isolation test, with its built-in control).
- **Idle and cleanup:** a 30-second idle timeout returns a session to zero pods, and its namespace
  is gone. A finished run leaves nothing behind.
- **Data access:** a second job adds booth-storage, MinIO (`ghcr.io/projectbooth/minio-test`),
  booth-lakehouse and booth-database at pinned refs. A run reads and writes an Iceberg table and a
  Postgres table. Then the submitter loses access, and the run fails and is cleaned up.

## 10. Not decided here: for the coordinator and the user

**For the coordinator:**
1. **No Spark Operator** (item 1). The brief expected one.
2. **One namespace per run, created by the module** (item 3, ADR 0029). This makes booth-spark the
   first module chart that ships a ClusterRole, a ClusterRoleBinding and ValidatingAdmissionPolicies,
   and that needs Kubernetes 1.30 or later. Does core's installer allow cluster-scoped objects in a
   module chart?
3. **The driver holds Kubernetes RBAC in its own namespace**, a departure from ADR 0057. If this is
   refused, the alternatives are:
   - Spark standalone mode inside the run's namespace (master and workers created by the backend,
     no API rights for user code, about 1 GB more per run);
   - a filtering Kubernetes API proxy in the backend (strong, but a large build).
4. **The Spark UI is visible only to its submitter** (item 5). The real fix is item 55 (a separate
   origin), not this module's work.
5. **Workload-submitted runs get no data access** until a person owner can be named (item 4,
   residual 7).
6. **Residuals 1 to 6 in item 4** accepted for v0?
7. **Spark Connect deferred**, with sessions reached through `/v1` statements only.

**For the user:**
1. Is **editor** the right floor for submitting work, or **owner only** for now, as ADR 0105 chose
   for Streamlit? Editor is my recommendation, given items 3 to 5.
2. The homelab: the second node's labels and taints, its free memory, and whether it is amd64 or
   arm64 (the image ships both).
3. Should a run be able to **write** to the lakehouse and database (editor runs get readwrite), or
   only read in v0?
4. Is closed internet egress the right default for runs? (booth-notebooks is open, booth-pipeline
   is closed.)

**Build order once ruled.** Each step is its own PR, merged green before the next:
1. Scaffold: the backend with health and the manifest, the chart, CI, and the identity code from
   item 2 (`oidc.jwksUrl` included).
2. Prove the iframe path to a real driver UI behind a real core.
3. Per-run namespaces, the admission policies, and applications, with the isolation tests.
4. Sessions, statements and idle shutdown.
5. Data access: the token agent and the database sidecar, then the lakehouse and storage.
6. The UI and the operations doc.

## As built: step 1 (scaffold, CI, identity), 2026-10-09

Ruled as ADR 0110. Built as items 2 and 9 describe, with these differences. Each one is deliberate,
and each is covered by a test.

- **Two "who am I" endpoints that item 6 doesn't list:** `GET /v1/me` and, on the iframe side,
  `GET /ui/api/me`. Each returns what the module derived from the caller's verified credential
  (subject, workspace, role, operator, workload, and whether `submit.minRole` lets them submit).
  Integration uses them to prove both identity paths against a real core. They are additive within
  `v1`.
- **`/v1`'s verifier is built on first use** (`auth.Lazy`). The module starts, and stays healthy,
  while the identity provider is still coming up, as booth-core does. Until discovery succeeds, `/v1`
  answers 503, not 401, because the caller's token may well be fine. Configuration errors that
  discovery can't fix still stop startup: a key URL without an issuer, or a workload issuer equal to
  the OIDC issuer.
- **A workload token's subject must be `<kind>:<id>`.** A token from core's workload issuer with a
  person-shaped subject is refused rather than treated as a person, because core never mints one
  (ADR 0058).
- **The access log never writes a query string.** Core's iframe entry URL carries its navigation
  token there. Integration checks that nothing token-shaped is logged.
- **The ValidatingAdmissionPolicy check works against live discovery.** It looks for the
  `admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy` kind, which a live cluster lists.
  Helm's offline `template` lists no kind-level APIs, so offline renders (the contract tests) pass
  `--api-versions` exactly as a 1.30+ cluster would. One contract test renders without it to prove
  the refusal. The standins job proves the check passes on a real cluster.
- **Integration also runs on `pull_request`.** ADR 0110 merges each step only with a green
  Integration run on its PR's exact head SHA. A `workflow_dispatch` can't target a PR until the
  workflow exists on `main`, and a `pull_request` run gives every PR that run automatically.
- **No Kubernetes API access yet.** The backend's ServiceAccount has no rules, and neither it nor the
  pod mounts a token. Step 3 adds exactly item 3's fenced rights, with the exact-rules test.
- **Pinned for Integration:**
  - booth-core at `f631bd2` (master, 2026-10-09). The CRD is vendored from the same commit, and the
    job checks they are byte-identical.
  - kind v0.33.0 with `kindest/node:v1.35.8@sha256:07b2536e…`.
  - Keycloak `26.0@sha256:09a381c7…`, `postgres:16-alpine@sha256:721873c3…`, and
    `curlimages/curl:8.17.0@sha256:935d9100…`.

## As built: step 2 (the Spark UI path), 2026-10-10

Item 5's path, proven against a real Spark 4.1.3 driver (the pinned image, started by hand), a real
booth-core and Keycloak, and a real Chromium. These are the facts the build found and what was done
about each; each is covered by a test.

- **The proxy base works through `APPLICATION_WEB_PROXY_BASE`.** Spark renders every link,
  `setUIRoot(...)` and its JavaScript's REST calls under it. The Executors tab, which is built
  client-side from `/api/v1/...`, loads in Chromium, and every request stays under the run's prefix.
- **Spark's redirects are absolute and ignore the proxy base.** `/` and `/jobs` answer
  `Location: http://<host>/jobs/`, so the proxy rewrites every `Location` under the run's prefix,
  host dropped.
- **`spark.ui.threadDumpsEnabled=false` does not cover the REST API.**
  `/api/v1/applications/<app>/executors/<id>/threads` still returns full stack traces. The proxy
  refuses that path itself, along with the `kill`, `threadDump` and `heapHistogram` paths, whatever
  the method. With `killEnabled=false`, Spark answers the kill URLs with a redirect rather than a
  404, so the proxy's refusal is the real boundary.
- **Run ids can never be `proxy` or `history`.** Spark's `utils.js` treats a path segment with
  either name as the start of an application id.
- **What reaches the driver is an allowlist of request headers:** Accept, Accept-Language,
  Cache-Control, the conditional headers, Range and User-Agent. Nothing identifying the viewer gets
  through. `booth_iframe_token` is dropped from the query, although core never forwards it.
- **The driver may not set cookies.** `Set-Cookie` is removed from its responses, because they are
  served on the shell's origin.
- **Authorization order:** another workspace or an unknown id is 404; same workspace but not the
  submitter is 403, operators and owners included; a non-GET/HEAD method is 405; a blocked action is
  403; a `..` or encoded separator is 400.
- **Spark runs hardened as the image's own uid 185:** read-only root filesystem, emptyDirs for
  `/tmp` and the work dir, all capabilities dropped. Step 3's driver pods start from this.
- **Spark UI settings live in one place** (`internal/sparkconf`). The proof driver uses exactly them,
  and a contract test keeps the two equal.
- **Not built in this step:**
  - The proof run table (`uiProof.runs`) is test-only, empty by default, and removed in step 3 in
    favour of real run records.
  - The proof driver's port is not fenced by a NetworkPolicy yet; per-run namespaces and their
    policies are step 3.
- **Integration loads the pinned image's linux/amd64 manifest** (`sha256:7cdb42ed…`), after
  checking that it is that entry of the pinned index (`sha256:bf9d035a…`). `kind load` can't import
  a multi-arch index whose other platforms aren't present locally.
