# Design note: managing clusters

Status: a proposal, with no code. The scope is undecided. Written 2026-10-25, after UX feedback that
"Spark should also handle managing cluster(s)". The page now shows the cluster read-only
(docs/design-v0.md, "As built: the page explained…").

## 1. What a "cluster" is here today

booth-spark has no standing Spark cluster: no master process, and no workers waiting for work.

- **Each run is its own small cluster.** Every application or session gets a namespace
  (`bspark-<id>`) and a driver pod, which runs `spark-submit` in client mode against Kubernetes and
  asks it for executor pods (dynamic allocation, 0 to `maxExecutors`).
- **It ends with the run.** Executors leave after 60 s idle, and the namespace goes when the run
  ends.
- **Spark's "master" is the Kubernetes API**, and the "workers" are whichever nodes the scheduler
  puts executors on.

What runs share is the install's admission, set in the chart's values and applied by `helm upgrade`:

| Value | What it bounds |
|---|---|
| `runs.maxRunning` | pending and running runs, all workspaces |
| `runs.maxRunningPerWorkspace` | the same, per workspace (0 for no cap) |
| `runs.memoryBudget` | the sum of every live run's largest footprint |
| `runs.maxExecutors`, `runs.maxMemory`, `runs.maxDuration` | what one run may ask for |
| `runs.driver` and `runs.executor` (`memory`, `cpu`, `nodeSelector`, `tolerations`) | pod defaults and placement |

**What these limits do and don't affect:**

- Admission is checked only at submit. Changing a limit never touches a live run.
- A refused submission is `429 at_capacity`. booth-pipeline's Spark task waits that out, retrying
  every 30 s for up to `runner.spark.admissionWaitSeconds` (900 s by default), and then fails the
  attempt.
- The backend's database is the source of truth: a backend restart or upgrade loses no run, and
  live runs carry on.

So "managing clusters" can mean five different things. Sections 2 and 3 take each in turn.

## 2. The options

### a. Resize: change the shared limits

- **What it would be:** a platform operator raises or lowers `maxRunning`, `memoryBudget`, the
  per-workspace cap, or the per-run maxima, from the page instead of from values.
- **How:** a settings table in the module's database that overrides the chart's values, applied at
  the next admission.
- **Risks to running pipelines:**
  - Lowering a limit doesn't stop anything already running. New submissions get 429, though, and
    a pipeline task waiting longer than 900 s fails.
  - Raising a limit past what the nodes can hold admits runs the nodes can't place:
    - a driver that can't be scheduled fails its run after `runs.pendingTimeout` (10 minutes), which
      is worse than a 429, because the task has already started;
    - executors that can't be scheduled leave a run slower, with fewer executors than it asked for.
  - **Drift:** the chart says one thing and the database another. A `helm upgrade` either resets
    the overrides or is silently ignored. Whichever we choose, someone will be surprised.
- **Size:** small. The risk is in the semantics, not the code.

### b. Named pools

- **What it would be:** several admission budgets instead of one, each with its own `maxRunning`
  and memory, and optionally its own node placement. For example, `pipelines` (reserved for
  unattended runs) and `interactive` (sessions and people's applications).
- **How a run chooses one:** an additive, optional `pool` field on `POST /v1/applications` and
  `/sessions`; booth-pipeline passes it from the task's Spark options. Pools are defined in values,
  with `default` keeping today's single budget, so a run that names no pool behaves exactly as now.
- **Why it's the useful one:** it answers "will the weekly pipeline get its memory when someone has
  a big session open?". Today nothing reserves capacity for the NFL pipeline.
- **Risks to running pipelines:**
  - **Fragmentation:** a task can wait on a full `pipelines` pool while `interactive` sits idle. On
    a two-node homelab, every budget split away from the rest is memory nobody else can use.
  - **Removing a pool** must refuse runs that name it (422), and a pipeline still naming it then
    fails every attempt. That needs booth-pipeline to validate the pool at save time, through
    `/v1/limits`.
  - **Placement per pool** (its own `nodeSelector`) interacts with the second-node recipe and the
    admission fence's placement rule. Every pool needs Integration coverage.
- **Size:** medium. It touches the API (additive), the store's admission query, the chart,
  booth-pipeline's runner, and the page.

### c. Restart

- **Nothing standing to restart.** Each run already starts fresh. What "restart" could mean:
  - **Restart a session:** stop it and start another with the same settings. This is useful,
    because a session's JVM can wedge, but its tables and variables are lost. It's a page-only
    feature over existing calls.
  - **Restart everything:** stop every live run. That is destructive. A pipeline task's application
    fails, and its retry starts from the beginning. Iceberg commits are atomic, but a task that
    commits more than once can leave half its writes.
  - **Drain:** stop admitting new runs and let the live ones finish, for maintenance. Pipeline tasks
    submitted meanwhile wait, then fail after 900 s.
- **Restarting the backend** is already safe (section 1) and needs no feature.

### d. Standing clusters (a Spark master with long-lived workers)

- **What it would be:** the model the feedback's words suggest, with a master and workers that stay
  up.
- **What it would give:** faster starts, with no pod start or image pull per run, and executors
  shared across runs.
- **What it would cost:**
  - **Always-on memory:** on the homelab, that's memory taken from everything else.
  - **Isolation:** each run would lose its own namespace, NetworkPolicy, quota and per-run
    credential sidecars (ADR 0110). One workspace's job could OOM another's executors, and data
    credentials would share JVMs across runs.
  - **Authentication:** Spark standalone has only a shared secret.
- **Recommendation:** don't.

### e. Per-workspace pools, later

A memory budget per workspace, beside today's per-workspace run cap, perhaps visible to its owners.
It's option b with workspaces as the pools. It can follow b, and isn't needed before more than one
team shares an install.

## 3. What a read-only next step could add

These change nothing, but each needs new Kubernetes rights (section 4):

- **Nodes:** each node's allocatable CPU and memory, and what Spark pods hold on it.
- **Live executors per run:** how many executors a run holds now, instead of only its configured
  range.

## 4. Kubernetes rights each option needs

The backend's rights are pinned by `TestChart_RBACIsExactlyTheDesign` (ADR 0110 rulings 2 and 3).
Any change here is a reviewed RBAC change.

| Option | New rights |
|---|---|
| a. resize, b. pools, e. per workspace | none (admission is the module's own database) |
| b. pools with per-pool placement | none for the backend; the admission fence's placement rule takes a pool's `nodeSelector` |
| c. restart a session, drain | none (stop already exists) |
| 3. nodes | cluster-wide `get`/`list` on `nodes` (read-only, but the first cluster-wide read beyond namespaces) |
| 3. live executors | `list` on `pods` in run namespaces (the run controller has `create` and `get` only) |
| d. standing clusters | a different architecture; not an RBAC change |

## 5. Recommendation

1. **Now:** keep what shipped, the explanation and the read-only Clusters view. Nothing else
   until the user decides the scope.
2. **First management feature, if any: named pools (b), with a `pipelines` reservation.**
   - It's the only option that protects the pipelines rather than putting them at risk.
   - Start with pools defined in the chart's values (GitOps), not edited on the page, so there's
     no drift.
   - The `default` pool keeps today's behaviour, so nothing changes for a run that names no pool.
   - Build it after the ADR 0122 Spark/lakehouse upgrade chain, which is itself sequenced after the
     first unattended `nfl-weekly-spark` run (Wed 28 Oct 09:00).
3. **Cheap and safe, any time after that:** "Restart session" on a session's page (c), and the two
   read-only views in section 3, if the RBAC additions are accepted.
4. **Resize from the page (a):** only if values plus `helm upgrade` proves too slow in practice.
   If we do build it, it's platform-operator only, shows the chart's value beside the override, and
   refuses to go below current use.
5. **Not:** standing clusters (d), or a "restart everything" button (c).

## 6. Questions for the user

1. Does "manage" mean "make sure my pipeline gets capacity" (pools), or "change sizes without a
   redeploy" (resize)? The recommendation above assumes the first.
2. Is a `pipelines` reservation worth the memory it takes from sessions on the two-node homelab?
   How much? A starting point: the NFL pipeline's largest run plus one retry.
3. Should anyone below platform operator see node-level detail (section 3)? The recommendation is
   operators only.
