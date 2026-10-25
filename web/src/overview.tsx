// The runs page's plain-language layer: what applications and sessions are, where each run came
// from, and what Spark runs on in this install (GET /v1/cluster). Read-only.
import { api, live, type Cluster, type Me, type Run } from "./api";
import { Badge, ErrorBox, Hint, duration, when } from "./components";
import { useLoad } from "./hooks";

/** The words the explainer and the hints share, so the two never disagree. */
export const explain = {
  application:
    "An application is a Spark program that runs once, start to finish, and then stops by itself. You can't send it more work while it runs.",
  session:
    "A session is a Spark program that stays up and waits for you to send it code, one statement at a time, keeping its tables and variables between statements: like a notebook kernel with Spark behind it.",
  owner: "Who the run belongs to: the person who started it, or, for a pipeline's run, the pipeline's owner, whose data access it uses.",
  origin: "What started it: a person on this page (or with their own token through the API), or a pipeline task.",
  started: "When its driver started running. A pending run is still waiting for its pods.",
  cluster:
    "Spark here has no standing cluster. Each application or session starts its own driver, which asks Kubernetes for executors; all of them go away when the run ends. What runs share is this install's limits.",
  master: "Spark's cluster manager. Here it is Kubernetes: each run's driver asks it for executors.",
  workers:
    "Executors: the pods that do a run's work. Each run gets its own, up to its maximum, gives back idle ones after a minute, and gives back all of them when it ends.",
};

/** What applications and sessions are, who starts them, how long they live, and how they relate. */
export function Explainer({ me }: { me: Me }) {
  const l = me.limits;
  return (
    <details className="explainer">
      <summary>What are applications, sessions and clusters?</summary>
      <div className="explainer-grid">
        <div>
          <h3>Applications</h3>
          <p>{explain.application}</p>
          <dl>
            <dt>Who starts one</dt>
            <dd>A person, with New application below, or a pipeline task that uses Spark: each attempt of the task starts one.</dd>
            <dt>How long it lives</dt>
            <dd>
              Until its code finishes or fails, someone stops it, or it reaches its time limit{l && <> (at most {duration(l.maxDuration)})</>}.
            </dd>
            <dt>Who sees it</dt>
            <dd>
              Everyone in the workspace sees that it ran and how it ended. Its log is for whoever submitted it, the workspace owners and
              platform operators.
            </dd>
          </dl>
        </div>
        <div>
          <h3>Sessions</h3>
          <p>{explain.session}</p>
          <dl>
            <dt>Who starts one</dt>
            <dd>A person, with New session below. Pipeline tasks start applications, not sessions.</dd>
            <dt>How long it lives</dt>
            <dd>
              Until it's deleted, or it sits idle
              {l && <> for {duration(l.sessionIdleTimeout)}</>}, or reaches its lifetime limit
              {l && <> ({duration(l.sessionMaxLifetime)})</>}.
            </dd>
            <dt>Who sees it</dt>
            <dd>Only the person who started it and the workspace owners.</dd>
          </dl>
        </div>
      </div>
      <h3>Pipelines and notebooks</h3>
      <p>
        A pipeline task that uses Spark starts an <strong>application</strong>; it shows below with the origin "Pipeline task" and the
        pipeline's owner, and a retried task starts a new one. Booth's notebooks don't start Spark sessions today; a session is
        started here.
      </p>
      <h3>Clusters</h3>
      <p>{explain.cluster}</p>
    </details>
  );
}

/**
 * What the page calls a run another module started, by that module's id as core signed it into the
 * token (origin.module). One line per module that starts Spark runs; any other is "Another module".
 */
export const moduleOrigins: Record<string, string> = {
  notebooks: "Notebook",
};

/** Where a run came from, in words. */
export function OriginLabel({ run }: { run: Run }) {
  const o = run.origin;
  if (!o) return <>{run.workload ? "A workload" : "A person"}</>;
  switch (o.kind) {
    case "person":
      return <>A person</>;
    case "pipeline":
      return (
        <>
          Pipeline task{o.task && <> <span className="mono">{o.task}</span></>}
          {(o.pipelineRun || o.attempt) && (
            <div className="muted">
              {o.pipelineRun && <>run <span className="mono">{o.pipelineRun}</span></>}
              {o.pipelineRun && o.attempt && ", "}
              {o.attempt && <>attempt {o.attempt}</>}
            </div>
          )}
        </>
      );
    default: {
      const named = o.module ? moduleOrigins[o.module] : undefined;
      if (named) return <>{named}</>;
      return <>Another module{(o.module || o.workload) && <> ({o.module || o.workload})</>}</>;
    }
  }
}

/** Whom a run belongs to: its submitter, or for a pipeline's run, the pipeline's owner. */
export function ownerOf(run: Run) {
  if (run.dataOwner) return run.dataOwner;
  return run.submitterName || run.submitter;
}

/** When a run started, or why it hasn't. */
export function startOf(run: Run) {
  if (run.startedAt) return when(run.startedAt);
  if (live(run.state)) return "waiting since " + when(run.createdAt);
  return "never started";
}

const mi = (n: number) => (n >= 1024 ? (n / 1024).toFixed(1).replace(/\.0$/, "") + " GiB" : n + " MiB");

/** What Spark runs on in this install, how much of it is in use, and this workspace's live runs. */
export function ClusterSection({ apps, sessions }: { apps?: Run[]; sessions?: Run[] }) {
  const c = useLoad(api.cluster, [], 10000, () => true);
  const running = [...(apps ?? []), ...(sessions ?? [])].filter((r) => live(r.state));
  return (
    <section>
      <div className="title">
        <h2>Clusters</h2>
        <Hint text={explain.cluster} />
      </div>
      <ErrorBox error={c.error} />
      {c.value ? <ClusterFacts c={c.value} visible={running.length} /> : !c.error && <p className="muted">Loading…</p>}
      <h3>Running on it now</h3>
      {running.length === 0 ? (
        <p className="muted">Nothing you can see is running in this workspace.</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>State</th>
              <th>Owner</th>
              <th>Driver</th>
              <th>Executors</th>
              <th>Memory, at most</th>
            </tr>
          </thead>
          <tbody>
            {running.map((r) => (
              <tr key={r.id}>
                <td>
                  <a href={"#/" + (r.kind === "session" ? "sessions" : "applications") + "/" + encodeURIComponent(r.id)}>{r.name}</a>
                </td>
                <td>{r.kind}</td>
                <td>
                  <Badge state={r.state} />
                </td>
                <td>{ownerOf(r)}</td>
                <td>{r.resources.driverMemory}</td>
                <td>
                  {r.resources.minExecutors}–{r.resources.maxExecutors} × {r.resources.executorMemory}
                </td>
                <td>{mi(r.footprintMi)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function ClusterFacts({ c, visible }: { c: Cluster; visible: number }) {
  const k = c.capacity;
  const hiddenHere = Math.max(0, k.liveRunsInWorkspace - visible);
  const elsewhere = k.liveRuns - k.liveRunsInWorkspace;
  return (
    <>
      <p>
        One: this install's Spark on Kubernetes. <span className="muted">{explain.cluster}</span>
      </p>
      <dl className="facts">
        <dt>
          Master
          <Hint text={explain.master} />
        </dt>
        <dd>{c.master === "kubernetes" ? "Kubernetes" : c.master}</dd>
        <dt>
          Workers
          <Hint text={explain.workers} />
        </dt>
        <dd>
          executors, started for each run: {c.executor.cores ?? 1} core each (CPU {c.executor.cpuRequest} requested, {c.executor.cpuLimit} at
          most), {c.executor.defaultMemory} of memory unless the run asks for more, up to {c.executor.maxPerRun} per run
        </dd>
        <dt>Drivers</dt>
        <dd>
          one per run: CPU {c.driver.cpuRequest} requested, {c.driver.cpuLimit} at most, {c.driver.defaultMemory} of memory unless the run asks
          for more
        </dd>
        <dt>Runs</dt>
        <dd>
          {k.liveRuns} running or waiting, of at most {k.maxRunning} on this install; {k.liveRunsInWorkspace} in this workspace
          {k.maxRunningPerWorkspace > 0 && <> (at most {k.maxRunningPerWorkspace})</>}
          {(hiddenHere > 0 || elsewhere > 0) && (
            <div className="muted">
              {hiddenHere > 0 && <>{hiddenHere} in this workspace are other people's sessions. </>}
              {elsewhere > 0 && <>{elsewhere} in other workspaces.</>}
            </div>
          )}
        </dd>
        <dt>Memory</dt>
        <dd>
          <meter min={0} max={k.memoryBudgetMi} value={k.memoryInUseMi} aria-label="Memory in use" />
          {mi(k.memoryInUseMi)} of {mi(k.memoryBudgetMi)} held by running runs ({mi(k.memoryInUseInWorkspaceMi)} by this workspace's). A
          new run that would go over is refused until some ends; a pipeline task waits and retries.
        </dd>
      </dl>
    </>
  );
}
