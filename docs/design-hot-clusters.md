# Design note: hot clusters that clients attach to

Status: a proposal, with no code. Written 2026-10-25 for the coordinator; it may become ADR 0125.

The user asked: "can we allow for hot clusters to be attached?" The coordinator takes that as the
answer to the scope question in docs/design-clusters.md. Clusters become named objects of two modes:

- **ephemeral:** today's behaviour, and the default;
- **hot:** kept warm, stopping itself when idle, and attachable by notebooks and, opt-in, by
  pipeline tasks.

Named pools and resizing from the page stay deferred.

Constraints: ADR 0110, ADR 0119, ADR 0120, the ADR 0122 upgrade chain, and the homelab's memory.
The homelab already refuses a driver plus two 1 GiB executors. On it, a cold start measures 30 to
45 s from submit to the first log line, and an executor registers about 45 s after submit.

## 0. Summary

- **A hot cluster is a long-lived run, a session generalised.** It keeps its own namespace,
  NetworkPolicy, quota and credential sidecars, so ADR 0110's isolation is unchanged *between*
  clusters. What's new is that several clients share one driver *inside* a cluster.
- **That sharing is safe only within one identity.** The starting position holds: one owner, one
  data lease, renewed exactly as a run's is today. It needs three additions: a ceiling check on
  attach, pipelines attaching only where the cluster's owner opted in, and no person attaching to a
  service account's cluster (section 2).
- **Clients attach through sessions and statements.** An attachment is a session whose `cluster`
  is the hot cluster, with its own SparkSession and Python namespace. Spark Connect stays deferred
  (section 3).
- **A pipeline task picks a cluster with an optional `cluster` on its application.** If the
  cluster can't take the task, it falls back to ephemeral. Each task gets its own log and its own
  cancel (section 4).
- **The homelab's memory is the binding constraint.** One hot cluster can use up the room the NFL
  pipeline needs.
  - Hot clusters get their own cap.
  - An *idle* hot cluster yields to a submission that doesn't fit (section 6).
  - Nothing hot is enabled before the 28 Oct run and the ADR 0122 chain.
- **Smallest useful first slice:** a person's own hot cluster, attached from the Spark page and
  `/v1`, Python only.
  - It needs no new Kubernetes rights and no core change.
  - It's off by default (section 8).

## 1. Lifecycle

### Objects

`clusters` is a new table in the module's database, unique by `(workspace, name)`. It holds:

- the mode (`ephemeral` or `hot`) and the owner;
- resources, data access and language;
- `idleTimeout` and `maxLifetime`;
- `allowPipelines`.

What each mode means:

- **A named ephemeral cluster** is saved settings: every run that names it starts fresh, exactly as
  today. A run that names no cluster runs as it does now. Nothing changes for existing callers.
- **A hot cluster** has at most one *instance* at a time. The instance is an ordinary run row
  (kind `cluster`) with its own `bspark-<id>` namespace, driver and sidecars. Stopping it ends the
  instance; the named cluster stays, `stopped`, and can be started again.

### States

`stopped → starting → running (idle | busy) → stopping → stopped`.

| Step | What happens |
|---|---|
| **Create** | records the cluster; starts nothing. Who may create: anyone at `submit.minRole` for their own cluster; a workspace owner for a service account's (section 2). |
| **Start** | admits the instance against capacity (section 6), then launches it like a session. Starting is explicit (the page, `/v1`), or automatic when a pipeline task names a stopped cluster with `allowPipelines` (section 4). |
| **Idle auto-stop** | idle means no attachment has a statement or job waiting or running, and nothing was submitted for `idleTimeout`. Default 20 min, as sessions (ADR 0110); a chart cap, `clusters.hot.maxIdleTimeout`, default 2 h. |
| **Maximum lifetime** | a hard stop even when busy, as sessions. Default and cap 12 h (ADR 0110). Raising the cap past 12 h needs a ruling. The page warns from 15 minutes before. A pipeline job is placed only on a cluster with at least the job's `maxDuration` left (section 4). |
| **Stop** | by its owner, a workspace owner, or the module (idle, lifetime, lost access, or yielding, section 6). |
| **Delete** | only when stopped. |

Executors still come and go with dynamic allocation: they're released after 60 s idle, as today.
"Warm" means the driver, its JVM and its sidecars' leases. A cluster may set
`minExecutors: 1` to keep one executor warm too, at that executor's full memory cost.

### What happens to attached work

- **When the cluster stops:**
  - Every waiting or running statement is cancelled, with the reason "cluster `<name>` stopped:
    `<why>`".
  - Every attachment ends; each attached session shows `stopped`, with that reason.
  - A pipeline job is marked `stopped` with that reason. booth-pipeline already treats a stop it
    didn't ask for as a failure and retries (section 4).
- **When the backend restarts** (a chart upgrade, a node move): nothing is lost.
  - The database is the source of truth, and the controller picks live runs and in-flight
    statements up again (`internal/runs/controller.go`). Hot clusters inherit that.
  - The module restart in each ADR 0122 step is therefore safe for running hot clusters.
- **When a runtime-image upgrade lands** (`runs.image` moves):
  - Running instances keep their old image, since pods are never mutated. New instances use the
    new one.
  - The instance is marked *stale*. A stale cluster is stopped and restarted the next time it's
    idle, and no new pipeline job is placed on it.
  - The 12 h lifetime bounds the skew.
- **On uninstall:** namespaces are deleted (today's tested behaviour), which ends all attached
  work.

## 2. Identity and credentials

The starting position is workable: **a hot cluster belongs to one identity, only that identity
attaches, and it holds one data lease.**

### How the lease is renewed: the run mechanism, unchanged

A hot cluster instance is a run, so its credentials are a run's:

1. The backend mints one workload token per instance from core. Its subject is
   `spark:<workspace>:<instanceId>`, its owner is the cluster's owner, and its ceiling is fixed at
   start (`internal/dataaccess/tokens.go`).
2. The backend re-mints at two thirds of the token's life. **Every mint re-checks the owner live in
   core**:
   - a person's role, and their last sign-in within `ownerMaxAge` (7 days, irrelevant inside 12 h);
   - a service account's stored role and `disabled` flag (ADR 0120).
3. Each pod's agent fetches the current token from `/internal/token` at two thirds of its
   remaining life (`internal/agent/agent.go`). The credential sidecars renew their broker leases
   with it: S3 keys at least 15 minutes, a Postgres lease up to 1 h.
4. **On a refused mint, the cluster ends.** That covers a removed person, a disabled service
   account, or an editor demoted to viewer (the ADR 0110 step 5 ruling). The same fail-closed path
   as runs.

So "refresh before expiry" is already continuous, and needs nothing new. The only change is
duration: a run's lease lives for minutes to hours; a hot cluster's lives up to 12 h, renewed the
same way.

### The additions the starting position needs

1. **An attachment never raises the ceiling.** The lease's role is fixed when the instance starts.
   An attach whose own ceiling is lower is refused (403). For example, a pipeline token from a
   viewer-role pipeline (ADR 0119 ceiling `viewer`) can't attach to an editor-lease cluster.
   Otherwise it would write with the owner's editor lease.
2. **Data access is fixed at start.** A job asking for a storage location the cluster's plan
   doesn't hold can't attach. For a pipeline, it falls back to ephemeral (section 4).
3. **Who counts as "that identity":**

   | Cluster owner | May attach | May see and stop it |
   |---|---|---|
   | a person | that person's own token (the page, `/v1`); a notebook workload token whose core-signed `booth_owner` is that person (phase 3); a pipeline task whose `booth_owner` is that person, only if `allowPipelines` | the owner, the workspace owners |
   | `svc:<ws>:<name>` | a pipeline task with core-signed `booth_module = pipeline` and `booth_owner = svc:<ws>:<name>`, only if `allowPipelines` | workspace owners |

4. **No person attaches to a service account's cluster, workspace owners included.** Otherwise a
   person's code would run under the account's lease, and core's audit would say the account did
   it, which is what ADR 0120 exists to stop. Workspace owners can see and stop it, and that's all.
5. **Creating a service account's cluster** is for workspace owners only. That mirrors ADR 0120's
   "only owners may assign a service account to a pipeline", enforced here because core doesn't
   know who asked. booth-spark already names the owner in its mint request, and core checks that
   owner's role and workspace. *To confirm with core:* that a `svc:` owner mint from booth-spark,
   not only from booth-pipeline, is allowed under ADR 0120's mint rules.

### Residuals

- **Audit granularity drops.** Core's mint log names the instance (`spark:<ws>:<instanceId>`), not
  the job or person that attached. booth-spark logs each attachment and job against the instance,
  so the trail exists, but in two places.
- **A larger blast radius.** A refused mint now ends every attached client, not one run. For a
  service account's cluster, disabling the account stops all its pipelines' attached tasks at
  once. That is the point of disabling, but it's worth knowing.
- **Copied S3 keys stay valid until their lease expires,** as today (ADR 0110 residual 2). A hot
  cluster doesn't lengthen any single lease, only the time over which leases are issued.

## 3. How clients attach

### Recommendation: sessions and statements

**An attachment is a session.** Clients attach with `POST /v1/sessions {cluster: "<name>"}`, and
then use the existing statements API, console, idle rules and visibility. A session without
`cluster` is today's single-client session, unchanged.

The runner changes, because today it is one worker thread over one queue and one shared namespace
(`internal/runs/runner/session_runner.py`):

- **A SparkSession per attachment:** `spark.newSession()`, so temp views, SQL conf and the current
  database don't leak between attachments. They share one SparkContext, its executors and its
  cache.
- **A Python namespace per attachment:** a separate globals dict. It is not a security boundary,
  because they share one process, which is why attachment is limited to one identity.
- **One worker per attachment,** with Spark's FAIR scheduler and one pool per attachment, so one
  client's long job doesn't stall another's. A per-cluster cap on concurrent attachments: default
  4, chart cap.
- **A job group per statement or job** (`setJobGroup`), so cancel can call `cancelJobGroup`
  (section 4).
- **Scala:** the `IMain` interpreter is one per JVM and expensive. Phase 1 hot clusters are Python
  only. A Scala hot cluster later takes one attachment at a time.

### Isolation, stated plainly

**Between clusters, nothing changes:**

- each instance has its own namespace, NetworkPolicy, quota, executor pods and leases;
- one cluster can't reach another;
- a person's cluster can't reach a service account's.

**Within a cluster,** attachments share a JVM, a process, executors and a lease:

- **Memory:** one attachment's OOM or `collect()` of a big table kills the driver, ending every
  attachment.
- **Visibility:** one attachment can see another's cached data, Python objects and files on the
  driver, given the will to.
- **Why that's acceptable:** all attachments are the same identity, with the same data access, so
  sharing gives none of them access it didn't already have. The cost is reliability, not
  confidentiality.
- **Spark UI:** one cluster's UI shows every attachment's jobs. It opens for a person owner only
  (ADR 0110 ruling 4). A service account's cluster has no UI in Booth, as a workload's run has none
  today.

### Spark Connect: keep it deferred

Spark Connect would give notebooks the real PySpark DataFrame API, not a statement proxy. That is
the biggest user-facing gain available, and the reason to revisit it later. Against it, now:

- It reopens ADR 0110 ruling 7.
- It's gRPC, which core's HTTP gateway doesn't carry (ADR 0007).
- Its own authentication, as far as I know, is a shared token at most (to verify against Spark
  4.1), so per-client identity would need a module-owned proxy in front of it.
- Every one of those is a core or security change bigger than this whole note.

My recommendation is a separate ADR, after phase 3 shows whether the statement proxy is enough for
notebooks.

## 4. Pipeline tasks: "Runs on"

### Choosing

- booth-pipeline's Spark task gets a **"Runs on"** option: *Ephemeral (default)* or a named
  cluster. Its builder lists `GET /v1/clusters` filtered to the ones its owner may attach to
  (section 2): the owner's own, with `allowPipelines`.
- booth-pipeline's client then sends what it sends today, `POST /v1/applications`, with an
  optional `cluster: "<name>"`. Everything it already uses keeps working unchanged:
  - `GET /v1/applications/{id}`, `/stop` and `/logs`;
  - `Idempotency-Key <run>/<taskKey>/<attempt>`;
  - the 429 wait.

  The client's change is one field.

### Placement, at submit

A **job** is a task attempt placed on a hot cluster. It is still an `applications` row, with a
`clusterInstance`.

1. The named cluster is **ephemeral** → today's path.
2. It's **hot and running** → the job goes on the instance only if all of these hold:
   - the caller may attach;
   - the ceiling and data access fit;
   - the instance isn't stale;
   - it has at least the job's `maxDuration` of lifetime left;
   - a concurrency slot is free (default: 1 job at a time per cluster, chart cap).
3. It's **hot and stopped**, with `allowPipelines` → start it if it fits capacity (section 6), and
   place the job when it's running.
4. Otherwise → **fall back to ephemeral**. The run's status says so ("ran ephemeral: cluster
   `<name>` was stopping"). If the fallback doesn't fit either, it's today's `429 at_capacity`, and
   the task waits it out as now.

**Fallback is on by default for pipelines,** `fallback: none` to refuse instead. The task's
correctness never depends on warmth: a hot cluster only makes it faster.

### On a shared driver

- **The job's code:** it runs as a script in its own SparkSession and globals (section 3), in its
  own job group. Exit and exceptions map to `succeeded` and `failed` as today.
- **Failure:**
  - The job's own failure fails only the job.
  - A driver crash or OOM fails every job on it, with "cluster `<name>` stopped: `<why>`", and the
    instance ends.
- **Retry:** booth-pipeline's retry is a new attempt, so a new key and a new placement, ephemeral
  if the cluster is gone. A retry after a crash doesn't wait for the cluster to restart.
- **Cancel:** `POST /v1/applications/{id}/stop` cancels the job group, which ends its Spark jobs at
  once, and interrupts its worker thread.
  - Python code that is not in a Spark call can ignore the interrupt.
  - After a 30 s grace, the job is marked `stopped`. If its thread is still running, the instance
    is marked *tainted*: it takes no new jobs and stops when idle.
  - `maxDuration` is enforced the same way.
  - Stopping the cluster always ends it.
- **Logs:** booth-pipeline reads a task's output from its log, so per-job logs are a hard
  requirement, not a nicety.
  - The runner captures each job's stdout and stderr in its own buffer, by thread, and
    `/applications/{id}/logs` serves that buffer with today's cursor semantics.
  - Spark's own log4j lines aren't per job. They stay in the cluster's driver log, which is visible
    to whoever may see the cluster.
- **State between tasks:**
  - Fresh sessions and globals mean no temp views or Python state carry over.
  - Iceberg's catalog cache must not serve one job another's stale metadata. Either verify that
    each new SparkSession gets its own catalog instance, or set `cache-enabled=false` on hot
    clusters. This is a phase 2 test, not an assumption.
  - ADR 0122's `rest-catalog-purge=true` applies unchanged.

## 5. Notebooks

booth-notebooks starts no Spark sessions today. Attaching needs three pieces:

1. **A workload subject:** `notebook:<workspace>:<serverId>`, one per person's notebook server.
   - Minted by booth-notebooks, with core's signed `booth_module = notebooks` and
     `booth_owner = <the person's sub>`.
   - booth-notebooks needs `workloadIdentity.mint` in its manifest: a core entitlement, not core
     code, as far as I can tell; to confirm.
   - The subject's kind, `notebook`, is what the page's origin shows as "Notebook" (the PR #28
     ruling's one-line mapping).
2. **A booth-spark rule, a sibling of ADR 0119:**
   - A session with `cluster` from a workload token is accepted only when `booth_module` is
     `notebooks`, `booth_owner` equals the cluster's owner, and the owner is a person.
   - Notebooks never attach to a service account's cluster.
   - Ephemeral sessions from notebooks stay refused data access, as all workload sessions are now
     (ADR 0119).
3. **A client in the kernel:**
   - A small Python package with `%%spark` and `%%sql` magics, plus `booth_spark.sql(...)`
     returning a pandas frame from the statement's table output, through core's gateway.
   - It is not a PySpark `SparkSession`. Users expecting `df.groupBy(...)` on the notebook side
     won't get it. That's the gap Spark Connect would close (section 3).

For the notebook user: open a notebook, attach to "my-warm", get the first result in seconds
instead of 45 s. Closing the notebook ends the attachment, and the cluster idles out on its own.

## 6. Capacity

A hot instance is admitted like any run, at its **full footprint** (driver, `maxExecutors`, the
sidecars), against `runs.memoryBudget`, `runs.maxRunning` and the per-workspace cap. On top:

- **`clusters.hot.maxMemory`**, a share of the budget hot clusters may hold in total. The default is
  40% of `runs.memoryBudget`, and on the homelab it should be sized to one small hot cluster.
- **`clusters.hot.maxPerWorkspace`,** default 1.

### Refuse or queue

- **A start by a person that doesn't fit is refused** with `409 at_capacity`, with the numbers, as
  a submission is today. People retry; a queued hot start would hold a promise of memory nobody can
  see.
- **A pipeline's implicit start that doesn't fit** isn't an error: the job falls back to ephemeral,
  and that is admitted or 429'd as today.

### Idle hot clusters yield

When any submission doesn't fit, but stopping one or more **idle** hot instances would make room,
the backend stops them, oldest-idle first, and admits the submission. Nothing running is lost; only
warmth is. A **busy** instance never yields; preemption of running work is out of scope.

### Effect on scheduled pipelines

Today, the NFL weekly run needs a driver plus executors on nodes that already refuse a driver plus
two 1 GiB executors. With hot clusters:

- An **idle** hot cluster yields, so the pipeline still starts. It pays one stop, a namespace
  delete of seconds, on top of today's cold start.
- A **busy** hot cluster, someone mid-query at 09:00 Wednesday, doesn't yield. The pipeline waits
  in 429 for up to 900 s, then fails the attempt.
  - Mitigation: the hot cap is sized so that cap plus the NFL run's footprint fits the budget.
  - If the user wants a guarantee rather than a sizing rule, that's named pools, which are
    deferred.
- **A pipeline on its own hot cluster:** the gain is per task after the first. The first task
  still pays a cold start, unless booth-pipeline starts the cluster ahead of the schedule; that's
  phase 4 at the earliest. The warm driver saves its start (about 30 s). Executors still take
  their own start unless `minExecutors: 1` keeps one warm at 1 GiB or more. Phase 1 should measure
  attach-to-first-result with and without a warm executor before anyone sizes for it.

## 7. The Spark page

A **Clusters** list replaces today's read-only Clusters section. That section's install capacity
stays above the list.

| Column | Shows |
|---|---|
| Name | the cluster's name, linking to its page |
| Mode | Ephemeral, or Hot |
| State | stopped, starting, running (idle / busy), stopping; *stale* and *tainted* as notes |
| Owner | a person, or `svc:<ws>:<name>` |
| Idle | "idle 6 min, stops in 14 min", or "busy" |
| Lifetime | "stops at 21:40 (in 3 h 10 min)" |
| Attached | a count, plus who: people's sessions, notebooks, pipeline jobs (pipeline, task, attempt) |
| Memory | what the instance holds, against the hot cap |

- **Visibility:** every member sees the list, name, mode, state and owner, as applications are
  visible today. Attachments and the cluster's driver log are for its owner and workspace owners,
  as sessions are.
- **Actions:**
  - Start, Stop and Delete for those allowed (section 1);
  - **Attach**, which opens a session console on the cluster, for the identity that may.
- **Hints and the explainer** gain "ephemeral" and "hot" in the same plain words as PR #28.

## 8. Phased build plan

Each phase is its own PR, or a few, behind `clusters.hot.enabled: false` until its gate passes.
None starts before the 28 Oct `nfl-weekly-spark` run is verified and the ADR 0122 chain is done.

| Phase | What | New Kubernetes rights | Core change |
|---|---|---|---|
| **1. Smallest useful slice** | `clusters` table and `/v1/clusters` (create, list, get, start, stop, delete); hot instances; person-owned only; Python only; `POST /v1/sessions {cluster}` with a SparkSession, globals and worker per attachment; idle auto-stop, lifetime, stale; hot caps and the idle-yield rule; the page's list and Attach | none (an instance is a run namespace) | none |
| **2. Pipelines** | `cluster` and `fallback` on applications; jobs with per-job logs, job-group cancel and taint; the remaining-lifetime check; `allowPipelines`; service-account clusters created by workspace owners; named ephemeral clusters as saved settings; booth-pipeline's "Runs on" | none | confirm `svc:` owner mints from booth-spark (section 2) |
| **3. Notebooks** | the ADR 0119-sibling rule for `booth_module = notebooks`; booth-notebooks' kernel client and magics; origin "Notebook" | none | booth-notebooks' `workloadIdentity.mint` entitlement in its manifest (configuration; confirm no code) |
| **4. Optional, each its own decision** | warm-ahead-of-schedule starts from booth-pipeline; Scala hot clusters (one attachment); live executor counts on the page; Spark Connect | live executors: `list` on pods in run namespaces; Spark Connect: none here | Spark Connect: gRPC through core's gateway, a new ADR |

**Phase 1's tests:**
- **Two attachments:**
  - temp views and globals are separate;
  - a long query in one doesn't block the other.
- **Who may attach:** an attach by another person is refused (403), with the owner's attach as the
  positive control. A viewer-ceiling attach to an editor cluster is refused, also with a positive
  control.
- **Stops:**
  - idle stop at the timeout, not before;
  - the lifetime stop cancels a running statement, with the reason;
  - a mint refused mid-life ends the cluster and its attachments.
- **Lease renewal** over more than one token life, using the existing test knobs.
- **Capacity:** an idle hot cluster yields to an application that doesn't fit; a busy one doesn't,
  and the application gets 429.
- **Upgrade:** a module restart mid-statement loses nothing; a `runs.image` change marks the
  instance stale.

## 9. Risks to running pipelines

1. **Memory taken from scheduled runs** (section 6). A busy hot cluster at schedule time can push
   the NFL run into 429 and a failed attempt. Mitigations: the hot cap sized against the NFL
   footprint, idle-yield, and off until after 28 Oct. A guarantee needs pools.
2. **Shared failure.** One job's OOM ends the others on the same cluster. Mitigations: one job at a
   time per cluster by default; retries fall back to ephemeral.
3. **Uncancellable Python.** A job ignoring the interrupt holds its slot until the cluster stops.
   It's tainted, so it takes no new jobs, but it holds memory until it idles out or hits its
   lifetime.
4. **Output parsing.** booth-pipeline reads task output from the log, and per-job capture is new
   code on that path. A regression fails tasks that succeeded. Phase 2's Integration must run the
   NFL task shape on a hot cluster and compare its output to ephemeral.
5. **Warm state making results depend on placement.** Iceberg metadata caching and anything left on
   the driver's disk. Mitigation: fresh sessions per job, and the catalog-cache check in section 4.
6. **Image skew during upgrades.** A job on a stale instance runs the old Spark. Mitigation: no new
   jobs on a stale instance, and the 12 h bound.
7. **Disabling a service account** ends every task attached to its cluster at once. That is
   intended, but its effect is wider than today's one run.

## 10. What I would not do

- **Spark Connect now** (section 3), or any port beside the `/v1` HTTP path.
- **Clusters shared across identities:** "team clusters" where several people attach under one
  lease. That breaks the ceiling and audit rules (ADR 0110 item 4, ADR 0120).
- **People attaching to service accounts' clusters,** workspace owners included.
- **A Spark standalone master** or any process outside per-instance namespaces
  (docs/design-clusters.md, option d).
- **Hot clusters with no maximum lifetime,** or past 12 h without a ruling.
- **Preempting busy clusters,** or silently moving running work.
- **Resizing a running instance in place.** Stop it, edit it, start it.
- **Pools, or resizing from the page** (deferred by the coordinator).
- **Letting an attachment add data access** beyond what the cluster's plan holds.

## 11. Questions for the coordinator

1. **The idle default for hot clusters:** keep ADR 0110's 20 minutes, or something longer? Longer
   means more memory held, and fewer yields to pipelines.
2. **Pipeline fallback to ephemeral on by default** (section 4): agree?
3. **Core:** may booth-spark mint for a `svc:` owner on a workspace owner's request? And is
   booth-notebooks' minting entitlement configuration only?
4. **Is a statement-proxy client enough for notebooks** in phase 3, knowing it isn't a PySpark
   `SparkSession`? If not, Spark Connect becomes the question, and it's a bigger one.
