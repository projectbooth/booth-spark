# booth-spark operations

The operator's doc for booth-spark (ADR 0110): what it needs, how to install and size it, where its
runs land, what it protects and what it doesn't, and what to do when a run won't start. The design
is `docs/design-v0.md`; the `/v1` interface is `internal/api/openapi/v1.yaml`, also served at
`/modules/spark/v1/openapi.yaml`.

## What it is

One always-on backend (the controller, `/v1`, the module's UI and the Spark UI proxy) and, for each
run, a namespace of its own: `bspark-<run>`, holding one driver and 0 to `runs.maxExecutors`
executors. A run is a batch application or an interactive session. Its namespace is deleted when it
ends. No Spark Operator; nothing outside Booth reaches a run (no Spark Connect, spark-submit, JDBC
or Thrift endpoint).

The module's UI opens in the shell (Manage, Spark): the workspace's applications and their state
for everyone in it; submitting, sessions and statements for editors and owners (`submit.minRole`).
A run's driver log is shown as text, never rendered. A run's Spark UI opens for its submitter only
(below).

## Requirements

- **Kubernetes 1.30 or later, with `ValidatingAdmissionPolicy` served.** Every run namespace is
  fenced by two admission policies; the chart refuses a cluster without them.
- **A CNI that enforces NetworkPolicy, egress included.** Isolation between runs, the internet-only
  egress rule and the backend's internal port all depend on it. k3s's default does. kind's default
  (kindnet) does not enforce egress; booth-spark's Integration runs on Calico.
- **booth-core** (it registers the module, provisions its database, signs the iframe assertion and,
  with data access on, mints run tokens).
- With data access: booth-database, booth-lakehouse and/or booth-storage, each optional.

## Install

Through booth-core's module install, or with Helm into its own namespace:

```sh
helm install booth-spark charts/booth-spark -n booth-spark --create-namespace \
  --set oidc.issuerUrl=https://keycloak.example/realms/booth --set oidc.clientId=booth-design
```

Released images are on GHCR, built for linux/amd64 and linux/arm64: `ghcr.io/projectbooth/booth-spark`
(the backend) and `ghcr.io/projectbooth/booth-spark-runtime` (every run's Spark, the `runs.image`
default, pinned by digest). The chart is attached to each GitHub release.

The values an operator usually sets:

| Value | Default | What it does |
|---|---|---|
| `oidc.issuerUrl`, `oidc.clientId` | empty | `/v1` verifies tokens against them; empty, `/v1` answers 503 (the UI still works) |
| `oidc.workloadIssuerUrl` | empty | also accept booth-core's workload tokens on `/v1` (a pipeline task); a pipeline's runs get data access as its owner (ADR 0119, below), other workloads' none |
| `submit.minRole` | `editor` | the lowest role that may submit or start a session: `editor` or `owner`; viewers never |
| `runs.maxRunning`, `runs.maxRunningPerWorkspace` | 2, 1 | live runs at once, install-wide and per workspace |
| `runs.memoryBudget` | 4Gi | the memory all live runs may use together, each at its largest |
| `runs.maxExecutors`, `runs.defaultExecutors` | 2, 2 | executors per run |
| `runs.maxMemory` | 2g | the largest heap a caller may ask for, driver or executor |
| `runs.maxDuration` | 6h | an application is stopped after this long |
| `runs.egress.mode` | `open` | `open`: the internet and nothing else of the network; `closed`: not even that |
| `runs.driver.*`, `runs.executor.*` | | memory, CPU and placement of each kind of pod (below) |
| `sessions.idleTimeout`, `sessions.maxLifetime` | 20m, 12h | a session is stopped when idle that long, and after its lifetime regardless |
| `sessions.resultRetention` | 168h | an ended run's content is cleared after this long (below) |
| `dataAccess.*` | off | runs read and write data as their submitter (below) |

## Sizing and admission

The defaults fit a machine with about 4 GB free. A default pod is 896Mi (a 512m heap plus Spark's
384Mi overhead); a default run, the driver and 2 executors, is about 2.6Gi at its largest. With data
access, every pod of a run also holds its agent (32Mi) and one 64Mi credential sidecar per data path.

The backend admits a run only if its largest footprint (driver, every executor it may scale to, and
their sidecars) fits beside every live run's in `runs.memoryBudget`, and only within
`runs.maxRunning`. A refused submission is `429 at_capacity`, with the numbers. Each run namespace
also has a ResourceQuota of its footprint plus 512Mi and one pod of slack, and a LimitRange.

Executors leave after 60 seconds idle (dynamic allocation), so an idle session holds only its
driver.

**The first run on a node waits for the pull.** The runtime image is about 1.4 GB compressed. There
is no pre-pull DaemonSet; to avoid the wait, pull `runs.image` on each node beforehand.

## Placement: the second-node recipe

Drivers and executors can be kept on nodes of their own, away from the rest of the platform. The
recipe for a homelab whose second server is the Spark node:

```sh
kubectl label node <spark-node> booth.projectbooth.io/pool=compute
kubectl taint node <spark-node> booth.projectbooth.io/pool=compute:NoSchedule
```

```yaml
runs:
  driver:
    nodeSelector: {booth.projectbooth.io/pool: compute}
    tolerations: [{key: booth.projectbooth.io/pool, operator: Equal, value: compute, effect: NoSchedule}]
  executor:
    nodeSelector: {booth.projectbooth.io/pool: compute}
    tolerations: [{key: booth.projectbooth.io/pool, operator: Equal, value: compute, effect: NoSchedule}]
```

The run-pods admission policy requires every entry of the configured `nodeSelector` on every pod in
a run namespace, so a driver can't place pods elsewhere. Tolerations and affinity are not enforced:
leaving one out only keeps a pod off a tainted node, the safe direction. The backend itself is placed
by the top-level `nodeSelector`, `tolerations` and `affinity`.

Both images exist for amd64 and arm64, so the Spark node may be either. Its labels, taints and
memory are still to be supplied for the homelab (ADR 0110, "Not decided"); size `runs.memoryBudget`
to what that node has free.

## Egress

`runs.egress.mode: open` (the default, the user's choice in ADR 0110): a run's pods reach the
internet and nothing else of the network. `runs.egress.exceptCidrs` (every private, CGNAT,
link-local, loopback and reserved range) stays unreachable, which covers the cluster's pod and
Service ranges on k3s and kind, the LAN and the cloud metadata address. In-cluster, a run reaches
only its own pods, DNS, the Kubernetes API (the driver alone) and, with data access, the backend's
internal port, booth-database's Postgres and the object store.

**What "open" means, stated plainly:** anything a run's code can read, it can send to any internet
host. With data access that includes everything its submitter's leases cover: the whole workspace
schema and warehouse, and the S3 keys, which stay valid until their lease expires. Acceptable for a
trusted homelab on a LAN; not for an install where editors aren't trusted. There, set
`runs.egress.mode=closed` (DNS, the run's own pods and the API server only) or
`submit.minRole=owner`.

## What a run can and can't do

A run's code runs as its submitter, in its own namespace, with:
- a Kubernetes account on its driver (Spark needs it to create executors), allowed pods and
  config maps in the run's own namespace only, and fenced by the run-pods admission policy: only the
  allowlisted images, the run's workspace label, the configured placement, the executor account
  with no token;
- with data access, a workload token naming the run (`spark:<workspace>:<run>`), its submitter's
  role in the workspace capped at editor (for a run a pipeline submitted, its pipeline owner's,
  below), and the credential sidecars' leases.

It can't reach another run, another namespace's pods, booth-core, the node or the LAN; it can't
read Secrets, not even in its own namespace; it can't create a pod as its driver's account, or with
another image or placement.

**Known residuals, stated plainly** (ADR 0110 ruling 6, `docs/design-v0.md` item 4):

1. **Whole-workspace scope.** A read lease is the whole `public` schema or warehouse prefix; an
   editor's run can also write there.
2. **S3 keys can be copied.** They stay valid until their lease expires, even after the run is gone.
   That matters once egress is open.
3. **User code can reach the Kubernetes API server.** NetworkPolicy is per pod, and the driver runs
   the user's code with its own Kubernetes account (ADR 0110 ruling 3, a documented departure from
   ADR 0057). That account may only manage pods and config maps in the run's own namespace, and the
   admission policies limit what pods it can create, but the API server itself is within reach of
   user code, so an API-server vulnerability would be within its reach. This is specific to Spark
   on Kubernetes.
4. **Pods the driver creates.** The driver can spend its quota on pods running the allowlisted
   images; no more than its own code can already do.
5. **Revocation lag.** When a submitter loses access (or their editor role), the run is ended and
   its namespace deleted at once, which ends every connection in it; copied S3 keys outlive that.
6. **CNI enforcement.** All of the network fencing depends on the CNI enforcing NetworkPolicy.

**The Spark UI opens for its submitter only.** It is content the run's own code controls (the
driver's web server), and the shell embeds module pages same-origin, so anyone else who opened it
would run that code's script with their own session (ARCHITECTURE item 55). Owners and operators see
a run's state and log in the module's UI instead, rendered as text. Kill, thread-dump and
heap-histogram actions are refused for everyone. There is no history server: the Spark UI goes when
the driver exits.

## Who may do what

| | Viewer | Editor | Owner | Platform operator |
|---|---|---|---|---|
| See the workspace's applications | yes | yes | yes | as their role |
| Submit, start a session (`submit.minRole`) | no | yes | yes | as their role |
| Stop an application, read its log | no | their own | any | any |
| See a session, read its log | no | their own | any | no |
| Run statements in a session | no | their own | their own | no |
| Open a run's Spark UI | no | their own | their own | no |

## Sessions: known limits in v0

- **Statements run one at a time, and there is no way to cancel one.** A session runs its
  statements in order, one at a time. v0 has no cancel endpoint and no per-statement timeout. A
  runaway statement (an endless loop, a query that never finishes) keeps running until the session
  reaches `sessions.maxLifetime` (default 12h), or until its submitter or a workspace owner deletes
  the session. Until then it holds the session, its driver and its executors, and every later
  statement waits behind it. Deleting the session ends everything in it.

  A future cancel would use Spark's `cancelJobGroup`, which the runner already sets up for each
  statement. That would stop only the statement's Spark jobs. It would not stop plain Python
  running in the driver (a loop, a `time.sleep`, driver-side computation): CPython has no safe way
  to interrupt another thread. Deleting the session would remain the only way to stop that.

- **Platform operators can't see or delete sessions.** Only a session's submitter and the
  workspace's owners can. An operator who needs a node back can't end someone's session through
  booth-spark. What bounds a session is its idle timeout (`sessions.idleTimeout`, default 20m,
  with nothing waiting or running) and its maximum lifetime (`sessions.maxLifetime`, default 12h,
  busy or not). An operator with cluster access can still delete the session's namespace
  (`bspark-<id>`); the controller then records the session as failed.

## Result retention

`sessions.resultRetention` (default `168h`, 7 days) is how long an ended run's content is kept.
After that, the controller clears:
- a session's statement code, output and error;
- an application's inline Python (its length and SHA-256 stay, and its view shows them);
- the driver log tail kept when any run (session or application) ended.

The run's state, reason and timestamps stay, and `contentClearedAt` records when it was cleared. A
cleared run's log answers `410` with code `cleared`. Clearing runs with the controller's sweep,
about every 30 seconds, so content can outlive the retention by that much.

Until then, the content is in the module's own database, and it can hold workspace data: what a
query returned, or what a run printed.

## Data access

Off by default (`dataAccess.enabled`). With it on, a run reads and writes data as its submitter,
capped at editor: the workspace's booth-database schema, its booth-lakehouse warehouse (the Iceberg
catalog `lakehouse`), and up to 4 booth-storage locations it names. Turning it on needs:

- `runs.image` to be booth-spark's runtime image (`images/spark-runtime`, published to GHCR for
  linux/amd64 and linux/arm64), which is the default, pinned by digest. The chart refuses plain
  `apache/spark` with data access on: it has no JDBC driver, Iceberg or S3A;
- the data paths you have installed: `dataAccess.database.enabled`, `dataAccess.lakehouse.enabled`,
  `dataAccess.storage.enabled`. `dataAccess.database.egress` must match your booth-database
  install's namespace and Postgres labels; for an in-cluster object store (MinIO), set
  `dataAccess.objectStore.egress`, while AWS S3 is reached through `runs.egress.mode=open`;
- a CNI that enforces NetworkPolicy. The backend's internal port (8081), which hands out run tokens
  against each run's bearer, is opened to this install's run namespaces only.

Known limits, stated plainly:

- **A run ends when its submitter loses their editor role or their access.** Its namespace is
  deleted at once, which ends every connection in it. S3 keys its code copied stay valid until
  their lease expires (at least 15 minutes on MinIO).
- **A run's code can read its own token and S3 keys.** Everything its submitter's leases cover, it
  can read and, as an editor's run, write: the whole workspace schema and warehouse. With open
  egress it can also send that anywhere on the internet (ADR 0110, "Open egress"). Where editors
  aren't trusted, set `runs.egress.mode=closed` or `submit.minRole=owner`.
- **One storage location per bucket** (a v0 limit, accepted in ADR 0110's step 5 rulings). S3A
  takes credentials per bucket, so a run naming two locations in one bucket is refused at launch,
  with that reason; name their common parent instead. The warehouse (`s3://`) and one storage
  location (`s3a://`) may share a bucket.
- **On SeaweedFS, a deleted directory lingers for about 2 minutes** (ADR 0118's S3 store). SeaweedFS
  removes an emptied folder only after its filer's `s3.empty_folder_cleanup_delay` (default `2m`);
  until then S3A sees the path as an empty directory, where real S3 would have nothing. A write in
  `errorifexists` mode to a path deleted less than 2 minutes earlier can therefore fail; overwrite
  and append are unaffected, and Spark ignores the empty `_temporary` a commit leaves behind.
  Integration's `s3a` job (`test/s3a`) checks S3A against SeaweedFS and accepts exactly this state.
- **A run a pipeline submitted acts as the pipeline's owner, who may not have written its code**
  (next section).

### Runs a pipeline submits (ADR 0119)

A pipeline task submits with its workload token. Its run gets data access, or starts from a file in
booth-storage, only when booth-core's signed `booth_module` claim names the pipeline module
(`pipeline`) and the token carries core's signed `booth_owner` claim. Core binds `booth_module` to
the credential that minted the token, so it says which module asked; a subject can be chosen by any
minting module, so the subject's kind (`pipeline:<workspace>:<run>`) is only a second check. The run then reads and writes **as the pipeline's owner**:
the person who owns the pipeline, or a service account (`svc:<workspace>:<name>`, ADR 0120).
Stated plainly:

- **The owner may not have written the task's code.** Whatever a pipeline's code asks for, it gets
  with its owner's access, capped at editor. This is already true of every pipeline task, and the run
  is capped by the same token, so nothing is widened.
- **Nobody else's token gets this.** A token minted for any other module (even with a
  pipeline-shaped subject), a run's own `spark:` token (which its code can read), a pipeline token
  without the owner claim, and every workload token for a session get 422 when they ask for data. A request naming an owner is refused
  (400): the owner comes from the token only.
- **The ceiling is fixed at submit**: the lesser of editor and the pipeline token's role. Lowering
  the pipeline's role ceiling while a run lives (up to `runs.maxDuration`) doesn't change that run;
  booth-core still re-checks the owner at every re-mint, and a refused re-mint ends the run.
- **A person owner who hasn't signed in** within booth-core's owner window (7 days by default)
  can't start a run, and ends a live one at its next re-mint. A service account has no window; a
  disabled one is refused the same way.
- **No Spark UI.** It is code-controlled content, opened only by the person who submitted the run;
  a pipeline's run has no such person. Its state, reason and log are on the module's page and `/v1`.
- **Who may stop it or read its log** is as for any run: its submitter, meaning any token with the
  same subject as the one that submitted it (each task of the pipeline run), workspace owners and
  platform operators. `Idempotency-Key` is scoped to that subject too. Being named as its owner gives no extra rights; the
  pipeline's own run view is where its owner follows it.
- On the module's page and `/v1` it shows as submitted by `pipeline:<id>` (a workload), acting as
  its owner's id as it is (`dataOwner`), with its ceiling (`dataCeiling`).

## Uninstall and upgrade

`helm uninstall`, or booth-core's module uninstall, removes everything: every run namespace (each
is owned by the chart's driver ClusterRole, so Kubernetes' garbage collector deletes them), both
admission policies and their bindings, and the ClusterRoles. A run that was live is gone with its
namespace; its record stays in the module's database. Integration checks both routes leave nothing
behind.

An upgrade restarts the backend. Live runs carry on: the new backend re-adopts every one from the
database, a session's statement in flight included. A run namespace with no live run behind it, once
a minute old, is deleted by the controller's sweep.

At startup the backend waits up to a minute for its first PostgreSQL connection (a new pod's first
connections can be refused for a few seconds while the cluster's network policy catches up). It
retries only a connection that was never made (refused, a timeout, DNS, a server still starting up),
from 500 ms doubling to 8 s, and logs each retry as `connecting to postgres at <host>:<port>: not
ready yet (attempt N: <error>); retrying in <delay>`. After a minute it exits with the last error
and the number of attempts. A wrong password, a missing database or any SQL error exits at once.

## When a run won't start, or ends early

A run's `reason` (in the UI and on `/v1`) says why, in the words of whatever refused it:

- **`the driver didn't start within ...` / `not scheduled: ...`**: no node fits it. Check
  `runs.*.nodeSelector` against the nodes' labels, the taints, and free memory.
- **`ErrImagePull` / `ImagePullBackOff`**: the node can't pull `runs.image` (or, with data access, the
  backend's image or the credential sidecar). Check the registry is reachable from the node.
- **`could not start: no data access: ...`**: the run asked for something its submitter can't have:
  a backend that isn't theirs, `readwrite` as a viewer, a workspace with no lakehouse warehouse, two
  storage locations in one bucket. The broker's or booth-lakehouse's own reason follows.
- **`lost its data access: ...`**: the submitter (for a pipeline's run, its owner) lost their role, or
  their editor role, while the run lived, or hasn't signed in within booth-core's owner window (7
  days by default), or is a disabled service account. The run ended and its namespace went.
- **`422` on submission, `... only when the pipeline module submits it`** / **`... under a pipeline
  run's subject`** / **`... names its owner (booth_owner ...)`**: a workload token asked for data
  access, or a file in booth-storage, without being the pipeline module's token carrying booth-core's
  owner claim (above).
- **`could not start: a credential sidecar ... got no lease in time`**: the broker or the data module
  didn't answer within 5 minutes; see booth-core's and the data module's logs.
- **`booth-core refused to mint for booth-spark`**: the manifest's `workloadIdentity` or the
  `booth-workload-minting-credentials` Secret: an operator problem, not the submitter's.
- **`429 at_capacity`** on submission: `runs.maxRunning` or `runs.memoryBudget`; the message has the
  numbers.

The backend logs one line per run transition (`runs: <id> <state>: <reason>`) and every refused
internal-port call; `kubectl -n bspark-<run> describe pod driver` and its events show the rest while
the run lives. `/healthz` reports the database; booth-core shows the module unhealthy when it fails.
