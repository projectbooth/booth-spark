import { useState, type FormEvent } from "react";
import { api, live, type FileRef, type Me, type Run, type Spec, type SpecView, type StorageLocation } from "./api";
import { Badge, ErrorBox, Field, Hint, Pre, when } from "./components";
import { go, useLoad } from "./hooks";
import { ClusterSection, Explainer, OriginLabel, explain, ownerOf, startOf } from "./overview";

const anyLive = (rs: Run[]) => rs.some((r) => live(r.state));

/** How many applications the runs page shows at a time. */
export const pageSize = 20;

/** The sessions this person may see, then the workspace's applications, newest first, a page at a time. */
export function RunsPage({ me }: { me: Me }) {
  const [pages, setPages] = useState(1);
  const apps = useLoad(() => api.applicationsPage(pages * pageSize), [pages], 5000, (p) => anyLive(p.runs));
  // The Clusters section counts every live application, on whichever page.
  const liveApps = useLoad(api.liveApplications, [], 5000, anyLive);
  const sessions = useLoad(api.sessions, [], 5000, anyLive);
  return (
    <>
      <Explainer me={me} />
      <section>
        <div className="section-head">
          <div className="title">
            <h2>Sessions</h2>
            <Hint text={explain.session} />
          </div>
          {me.canSubmit && (
            <button type="button" onClick={() => go("new-session")}>
              New session
            </button>
          )}
        </div>
        <p className="muted">A session is its submitter's and the workspace owners' only.</p>
        <ErrorBox error={sessions.error} />
        <RunTable runs={sessions.value} kind="sessions" empty="No sessions you can see." />
      </section>
      <section>
        <div className="section-head">
          <div className="title">
            <h2>Applications</h2>
            <Hint text={explain.application} />
          </div>
          {me.canSubmit && (
            <button type="button" onClick={() => go("new")}>
              New application
            </button>
          )}
        </div>
        <ErrorBox error={apps.error} />
        <RunTable runs={apps.value?.runs} kind="applications" empty="No applications in this workspace yet." />
        {apps.value && (apps.value.more || pages > 1) && (
          <div className="pager">
            <span className="muted">The newest {apps.value.runs.length}.</span>
            {apps.value.more && (
              <button type="button" className="secondary" onClick={() => setPages(pages + 1)}>
                Show more
              </button>
            )}
          </div>
        )}
      </section>
      {!me.canSubmit && (
        <p className="muted">
          Your role can see runs but not submit them: that needs {me.submitMinRole} or above.
        </p>
      )}
      <ClusterSection apps={liveApps.value} sessions={sessions.value} />
    </>
  );
}

function RunTable({ runs, kind, empty }: { runs?: Run[]; kind: "applications" | "sessions"; empty: string }) {
  if (!runs) return <p className="muted">Loading…</p>;
  if (runs.length === 0) return <p className="muted">{empty}</p>;
  return (
    <table className="table">
      <thead>
        <tr>
          <th>Name</th>
          <th>State</th>
          <th>
            Owner
            <Hint text={explain.owner} />
          </th>
          <th>
            Origin
            <Hint text={explain.origin} />
          </th>
          <th>
            Started
            <Hint text={explain.started} />
          </th>
          <th>Reason</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.id}>
            <td>
              <a href={"#/" + kind + "/" + encodeURIComponent(r.id)}>{r.name}</a>
              <div className="muted mono">{r.id}</div>
            </td>
            <td>
              <Badge state={r.state} />
            </td>
            <td className="reason">{ownerOf(r)}</td>
            <td>
              <OriginLabel run={r} />
            </td>
            <td>{startOf(r)}</td>
            <td className="reason">
              {r.reason}
              {waiting(r) && <span className="muted">waiting: {r.waitingReason}</span>}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** Whether a run is pending with a reason it still waits (state stays pending). */
const waiting = (r: Run) => r.state === "pending" && !!r.waitingReason;

function Facts({ run }: { run: Run }) {
  const d = run.dataAccess;
  return (
    <dl className="facts">
      <dt>State</dt>
      <dd>
        <Badge state={run.state} /> {run.stopRequested && live(run.state) && <span className="muted">stopping…</span>}
      </dd>
      {run.reason && (
        <>
          <dt>Reason</dt>
          <dd className="reason">{run.reason}</dd>
        </>
      )}
      {waiting(run) && (
        <>
          <dt>Waiting</dt>
          <dd className="reason">{run.waitingReason}</dd>
        </>
      )}
      <dt>Submitted by</dt>
      <dd>
        {run.submitterName || run.submitter}
        {run.workload && " (a workload)"}
      </dd>
      <dt>
        Origin
        <Hint text={explain.origin} />
      </dt>
      <dd>
        <OriginLabel run={run} />
      </dd>
      {run.dataOwner && (
        <>
          <dt>Acting as</dt>
          <dd>
            <span className="mono">{run.dataOwner}</span>
            <span className="muted"> — the owner of the pipeline that submitted it; its data access is theirs, up to {run.dataCeiling ?? "editor"}</span>
          </dd>
        </>
      )}
      <dt>Created</dt>
      <dd>{when(run.createdAt)}</dd>
      {run.startedAt && (
        <>
          <dt>Started</dt>
          <dd>{when(run.startedAt)}</dd>
        </>
      )}
      {run.finishedAt && (
        <>
          <dt>Ended</dt>
          <dd>{when(run.finishedAt)}</dd>
        </>
      )}
      <dt>Resources</dt>
      <dd>
        driver {run.resources.driverMemory}; {run.resources.minExecutors}–{run.resources.maxExecutors} executors of{" "}
        {run.resources.executorMemory}; up to {run.footprintMi} MiB in all
      </dd>
      {run.main?.inlinePythonSha256 && (
        <>
          <dt>Code</dt>
          <dd className="mono">
            inline Python, {run.main.inlinePythonBytes} bytes, sha256 {run.main.inlinePythonSha256.slice(0, 12)}…
          </dd>
        </>
      )}
      {run.main?.inlineScalaSha256 && (
        <>
          <dt>Code</dt>
          <dd className="mono">
            inline Scala, {run.main.inlineScalaBytes} bytes, sha256 {run.main.inlineScalaSha256.slice(0, 12)}…
          </dd>
        </>
      )}
      {(run.main?.python || run.main?.scala || run.main?.jar) && (
        <>
          <dt>Code</dt>
          <dd className="mono">
            {run.main.python
              ? `${run.main.python.backendId}/${run.main.python.path}`
              : run.main.scala
                ? `${run.main.scala.backendId}/${run.main.scala.path} (Scala)`
                : `${run.main.jar!.backendId}/${run.main.jar!.path} (${run.main.mainClass})`}
          </dd>
        </>
      )}
      {d && (
        <>
          <dt>Data access</dt>
          <dd>
            {[d.database && "database", d.lakehouse && "lakehouse", ...(d.storage ?? []).map((s) => `${s.backendId}/${s.path} (${s.access ?? "read"})`)]
              .filter(Boolean)
              .join(", ")}
            {d.role && <span className="muted"> — as {d.role === "editor" ? "an editor: read and write" : "a viewer: read only"}</span>}
          </dd>
        </>
      )}
      {run.contentClearedAt && (
        <>
          <dt>Content cleared</dt>
          <dd>{when(run.contentClearedAt)} (sessions.resultRetention)</dd>
        </>
      )}
    </dl>
  );
}

function SparkUILink({ run, me }: { run: Run; me: Me }) {
  // A run a pipeline submitted has no person to be its author, so no Spark UI (ADR 0119).
  if (run.workload && run.state === "running") {
    return <p className="muted">A run submitted by a workload has no Spark UI in Booth; its state and log are here.</p>;
  }
  if (!run.uiPath) return null;
  // Only the submitter may open it (docs/design-v0.md item 5): its page is the run's own code.
  if (run.submitter !== me.subject) {
    return <p className="muted">The Spark UI opens for this run's submitter only.</p>;
  }
  return (
    <p>
      {/* In this frame: the Spark UI is served through the same iframe proxy. */}
      <a className="button" href={run.uiPath}>
        Open the Spark UI
      </a>
    </p>
  );
}

export function ApplicationPage({ id, me }: { id: string; me: Me }) {
  const run = useLoad(() => api.application(id), [id], 3000, (r) => live(r.state));
  const mayLog = !!run.value && (run.value.submitter === me.subject || me.role === "owner" || me.operator);
  const logs = useLoad(() => (mayLog ? api.logs(id) : Promise.resolve("")), [id, mayLog, run.value?.state], 5000, () =>
    run.value ? live(run.value.state) : false,
  );
  const [busy, setBusy] = useState<Error | undefined>();
  if (run.error) return <ErrorBox error={run.error} />;
  if (!run.value) return <p className="muted">Loading…</p>;
  const r = run.value;
  return (
    <section>
      <p>
        <a href="#/">← Runs</a>
      </p>
      <h2>{r.name}</h2>
      <Facts run={r} />
      <SparkUILink run={r} me={me} />
      {live(r.state) && mayLog && !r.stopRequested && (
        <button
          type="button"
          className="danger"
          onClick={() => api.stop(id).then(run.reload, setBusy)}
        >
          Stop
        </button>
      )}
      {me.canSubmit && mayLog && (
        <p>
          <button type="button" className="secondary" onClick={() => go("new/" + encodeURIComponent(id))}>
            Run again with these settings
          </button>
          <Hint text="Opens New application filled in with this run's code and settings. Submitting starts a new run; this one stays as it is." />
        </p>
      )}
      <ErrorBox error={busy} />
      {r.state === "failed" && !r.dataAccess?.lakehouse && noLakehouseInLog(String(logs.value ?? "")) && (
        <p className="warning" role="note">
          Hint: this run had no lakehouse data access, so Spark read <code>lakehouse</code> as a namespace, not a catalog.
        </p>
      )}
      <h3>Driver log</h3>
      {!mayLog ? (
        <p className="muted">Its submitter, the workspace's owners and platform operators can read its log.</p>
      ) : logs.error ? (
        <ErrorBox error={logs.error} />
      ) : (
        <Pre text={String(logs.value ?? "")} label="Driver log" />
      )}
    </section>
  );
}

/** Spark's error for `lakehouse.x.y` without the lakehouse catalog: a namespace in spark_catalog. */
export const noLakehouseInLog = (log: string) => /REQUIRES_SINGLE_PART_NAMESPACE[^\n]*`lakehouse`/.test(log);

/** Whether code names the lakehouse catalog (`lakehouse.` as a word). */
export const mentionsLakehouse = (code: string) => /(^|[^\w.])lakehouse\./.test(code);

function DataFields({ me, value, onChange }: { me: Me; value: DataChoice; onChange: (v: DataChoice) => void }) {
  const offered = me.limits?.dataAccess;
  if (!offered || (!offered.database && !offered.lakehouse && !offered.storage)) {
    return <p className="muted">This install offers runs no data access.</p>;
  }
  return (
    <fieldset>
      <legend>Data access, as you (capped at editor)</legend>
      {offered.database && (
        <label className="check">
          <input type="checkbox" checked={value.database} onChange={(e) => onChange({ ...value, database: e.target.checked })} /> the
          workspace's database (DATABASE_URL, JDBC_DATABASE_URL)
        </label>
      )}
      {offered.lakehouse && (
        <label className="check">
          <input type="checkbox" checked={value.lakehouse} onChange={(e) => onChange({ ...value, lakehouse: e.target.checked })} /> the
          workspace's lakehouse (the Iceberg catalog <code>lakehouse</code>)
        </label>
      )}
      {offered.storage && (
        <Field label="Storage locations" hint="One per line: backendId/path, then ' rw' for read and write. At most 4, one per bucket.">
          <textarea rows={3} value={value.storage} onChange={(e) => onChange({ ...value, storage: e.target.value })} />
        </Field>
      )}
    </fieldset>
  );
}

interface DataChoice {
  database: boolean;
  lakehouse: boolean;
  storage: string;
}

/** The form's data choice as the API's dataAccess (undefined when nothing is asked for). */
export function dataAccess(c: DataChoice) {
  const storage: StorageLocation[] = c.storage
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [loc, flag] = l.split(/\s+/);
      const i = loc.indexOf("/");
      return { backendId: loc.slice(0, i), path: loc.slice(i + 1), access: flag === "rw" ? "readwrite" : "read" };
    });
  if (!c.database && !c.lakehouse && storage.length === 0) return undefined;
  return { database: c.database || undefined, lakehouse: c.lakehouse || undefined, storage: storage.length ? storage : undefined };
}

function Resources({ me, value, onChange }: { me: Me; value: ResourceChoice; onChange: (v: ResourceChoice) => void }) {
  const l = me.limits;
  return (
    <div className="row">
      <Field label="Driver memory" hint={`default ${l?.driverMemory}, at most ${l?.maxMemory}`}>
        <input value={value.driver} placeholder={l?.driverMemory} onChange={(e) => onChange({ ...value, driver: e.target.value })} />
      </Field>
      <Field label="Executors (most)" hint={`0 to ${l?.maxExecutors}`}>
        <input
          type="number"
          min={0}
          max={l?.maxExecutors}
          value={value.max}
          onChange={(e) => onChange({ ...value, max: e.target.value })}
        />
      </Field>
      <Field label="Executor memory" hint={`default ${l?.executorMemory}`}>
        <input value={value.executor} placeholder={l?.executorMemory} onChange={(e) => onChange({ ...value, executor: e.target.value })} />
      </Field>
    </div>
  );
}

interface ResourceChoice {
  driver: string;
  max: string;
  executor: string;
}

function resources(c: ResourceChoice) {
  return {
    driver: c.driver ? { memory: c.driver } : undefined,
    executors: { max: c.max === "" ? undefined : Number(c.max), memory: c.executor || undefined },
  };
}

/** The application's entry point as the form holds it (docs/design-scala.md, section 2 (b)). "file"
 * is a Python or Scala file in booth-storage, which only Run again fills in (`file`). */
export interface EntryChoice {
  kind: "python" | "scala" | "jar" | "file";
  code: string;
  scalaCode: string;
  backendId: string;
  path: string;
  mainClass: string;
  args: string;
  sha256: string;
  file?: { language: "python" | "scala"; ref: FileRef };
}

/** The form's entry point as the API's main and args. */
export function entryPoint(c: EntryChoice): { main: Record<string, unknown>; args?: string[] } {
  const args = c.args
    .split("\n")
    .map((a) => a.trim())
    .filter(Boolean);
  const withArgs = (main: Record<string, unknown>) => ({ main, args: args.length ? args : undefined });
  if (c.kind === "python") return withArgs({ inlinePython: c.code });
  if (c.kind === "scala") return withArgs({ inlineScala: c.scalaCode });
  if (c.kind === "file" && c.file) return withArgs({ [c.file.language]: c.file.ref });
  return withArgs({
    jar: { backendId: c.backendId.trim(), path: c.path.trim().replace(/^\/+/, ""), sha256: c.sha256.trim().toLowerCase() || undefined },
    mainClass: c.mainClass.trim(),
  });
}

function EntryFields({ me, value, onChange }: { me: Me; value: EntryChoice; onChange: (v: EntryChoice) => void }) {
  const storage = !!me.limits?.dataAccess?.storage;
  const scala = !!me.limits?.languages?.includes("scala");
  const set = (over: Partial<EntryChoice>) => onChange({ ...value, ...over });
  const file = value.file;
  return (
    <>
      <fieldset>
        <legend>Entry point</legend>
        {file && (
          <label className="check">
            <input type="radio" name="entry" checked={value.kind === "file"} onChange={() => set({ kind: "file" })} /> The earlier run's{" "}
            {file.language === "python" ? "Python" : "Scala"} file in Storage
          </label>
        )}
        <label className="check">
          <input type="radio" name="entry" checked={value.kind === "python"} onChange={() => set({ kind: "python" })} /> PySpark code
        </label>
        {scala && (
          <label className="check">
            <input type="radio" name="entry" checked={value.kind === "scala"} onChange={() => set({ kind: "scala" })} /> Scala code
          </label>
        )}
        <label className="check">
          <input type="radio" name="entry" checked={value.kind === "jar"} disabled={!storage} onChange={() => set({ kind: "jar" })} /> JAR in
          Storage
        </label>
        {!storage && <p className="muted">A JAR is read from booth-storage, which this install doesn't offer runs.</p>}
      </fieldset>
      {value.kind === "file" && file ? (
        <p>
          <span className="mono">
            {file.ref.backendId}/{file.ref.path}
          </span>
          {file.ref.sha256 && <span className="muted mono"> sha256 {file.ref.sha256.slice(0, 12)}…</span>}
          <span className="muted"> — read as you when the run starts{file.ref.sha256 && ", and refused if its bytes changed"}.</span>
        </p>
      ) : value.kind === "python" ? (
        <Field label="PySpark code" hint="At most 256 KiB. It runs as you, in its own namespace.">
          <textarea className="code" rows={12} value={value.code} onChange={(e) => set({ code: e.target.value })} />
        </Field>
      ) : value.kind === "scala" ? (
        <Field
          label="Scala code"
          hint="At most 256 KiB, Scala 2.13. Statements run in order, as in spark-shell: spark, sc, args and booth are defined, and the first that fails ends the run."
        >
          <textarea className="code" rows={12} value={value.scalaCode} onChange={(e) => set({ scalaCode: e.target.value })} />
        </Field>
      ) : (
        <>
          <div className="row">
            <Field label="Storage backend" hint="The backend's id in booth-storage, e.g. local">
              <input required value={value.backendId} onChange={(e) => set({ backendId: e.target.value })} />
            </Field>
            <Field label="JAR path" hint="e.g. apps/job.jar, read as you when the run starts">
              <input required value={value.path} onChange={(e) => set({ path: e.target.value })} />
            </Field>
          </div>
          <Field label="Main class" hint="Fully qualified, e.g. com.example.Main. Built for Scala 2.13 against Spark 4.1.3 (provided).">
            <input required value={value.mainClass} onChange={(e) => set({ mainClass: e.target.value })} />
          </Field>
          <Field label="SHA-256" hint="Optional: the run refuses to start if the JAR's bytes don't hash to this">
            <input value={value.sha256} pattern="[0-9a-fA-F]{64}" onChange={(e) => set({ sha256: e.target.value })} />
          </Field>
        </>
      )}
      <Field label="Arguments" hint="One per line (optional): sys.argv[1:] in Python, args in Scala, the main class's arguments in a JAR">
        <textarea rows={3} value={value.args} onChange={(e) => set({ args: e.target.value })} />
      </Field>
    </>
  );
}

const blankEntry: EntryChoice = {
  kind: "python",
  code: "from pyspark.sql import SparkSession\n\nspark = SparkSession.builder.getOrCreate()\nprint(spark.range(10).count())\n",
  scalaCode: "// spark, sc, args and booth are already defined.\nprintln(spark.range(10).count())\n",
  backendId: "",
  path: "",
  mainClass: "",
  args: "",
  sha256: "",
};

/** An earlier run, for Run again: the run as the page shows it, and its spec. */
export interface Prior {
  run: Run;
  view: SpecView;
}

/** What a spec sets that the form has no field for; Run again keeps it as it was. */
interface Kept {
  conf?: Record<string, string>;
  maxDuration?: string;
  minExecutors?: number;
}

/** The form filled in from an earlier run's spec: never its code once retention cleared it. */
export function fromSpec(me: Me, { run, view }: Prior) {
  const s: Spec = view.spec;
  const m = s.main;
  const entry: EntryChoice = { ...blankEntry, code: "", scalaCode: "", args: (s.args ?? []).join("\n") };
  if (m.jar) {
    Object.assign(entry, { kind: "jar", backendId: m.jar.backendId, path: m.jar.path, sha256: m.jar.sha256 ?? "", mainClass: m.mainClass ?? "" });
  } else if (m.python || m.scala) {
    entry.kind = "file";
    entry.file = m.python ? { language: "python", ref: m.python } : { language: "scala", ref: m.scala! };
  } else if (m.inlineScala !== undefined || run.main?.inlineScalaSha256) {
    entry.kind = "scala";
    entry.scalaCode = m.inlineScala ?? "";
  } else {
    entry.code = m.inlinePython ?? "";
  }
  const ex = s.resources?.executors;
  const d = s.dataAccess;
  const kept: Kept = { conf: s.conf, maxDuration: s.maxDuration, minExecutors: ex?.min };
  return {
    name: s.name,
    entry,
    res: {
      driver: s.resources?.driver?.memory ?? "",
      max: String(ex?.max ?? run.resources.maxExecutors ?? me.limits?.defaultExecutors ?? ""),
      executor: ex?.memory ?? "",
    },
    data: {
      database: !!d?.database,
      lakehouse: !!d?.lakehouse,
      storage: (d?.storage ?? []).map((l) => `${l.backendId}/${l.path}${l.access === "readwrite" ? " rw" : ""}`).join("\n"),
    },
    kept,
  };
}

function keptText(k: Kept) {
  return [
    ...Object.entries(k.conf ?? {}).map(([key, v]) => `${key}=${v}`),
    k.minExecutors ? `at least ${k.minExecutors} executor${k.minExecutors === 1 ? "" : "s"}` : "",
    k.maxDuration ? `at most ${k.maxDuration}` : "",
  ]
    .filter(Boolean)
    .join("; ");
}

/** New application, blank or filled in from an earlier run (#/new/<id>, Run again). */
export function NewApplicationPage({ me, from }: { me: Me; from?: string }) {
  const prior = useLoad(
    () =>
      from
        ? Promise.all([api.application(from), api.applicationSpec(from)]).then(([run, view]): Prior => ({ run, view }))
        : Promise.resolve(undefined),
    [from],
  );
  if (!from) return <ApplicationForm me={me} />;
  if (prior.error)
    return (
      <section>
        <p>
          <a href="#/">← Runs</a>
        </p>
        <ErrorBox error={prior.error} />
      </section>
    );
  if (!prior.value) return <p className="muted">Loading…</p>;
  return <ApplicationForm me={me} prior={prior.value} />;
}

function ApplicationForm({ me, prior }: { me: Me; prior?: Prior }) {
  const [init] = useState(() => (prior ? fromSpec(me, prior) : undefined));
  const [name, setName] = useState(init?.name ?? "");
  const [entry, setEntry] = useState<EntryChoice>(init?.entry ?? blankEntry);
  const [res, setRes] = useState<ResourceChoice>(init?.res ?? { driver: "", max: String(me.limits?.defaultExecutors ?? ""), executor: "" });
  const [data, setData] = useState<DataChoice>(init?.data ?? { database: false, lakehouse: false, storage: "" });
  const [error, setError] = useState<Error | undefined>();
  const [busy, setBusy] = useState(false);
  const kept: Kept = init?.kept ?? {};
  const code = entry.kind === "python" ? entry.code : entry.kind === "scala" ? entry.scalaCode : "";
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    const r = resources(res);
    api
      .submit({
        name,
        ...entryPoint(entry),
        resources: { ...r, executors: { ...r.executors, min: kept.minExecutors } },
        conf: kept.conf,
        maxDuration: kept.maxDuration,
        dataAccess: dataAccess(data),
      })
      .then((run) => go("applications/" + encodeURIComponent(run.id)), setError)
      .finally(() => setBusy(false));
  };
  return (
    <form onSubmit={submit}>
      <p>
        <a href="#/">← Runs</a>
      </p>
      <h2>New application</h2>
      {prior && (
        <div className="notice" role="status">
          Filled in from <a href={"#/applications/" + encodeURIComponent(prior.run.id)}>{prior.run.name}</a>{" "}
          <span className="mono muted">{prior.run.id}</span>. Submitting starts a new run; that one stays as it is.
          {prior.view.codeCleared && (
            <p>
              Its code was cleared{prior.run.contentClearedAt && <> {when(prior.run.contentClearedAt)}</>} (sessions.resultRetention), so
              only its settings are filled in: add the code again.
            </p>
          )}
          {keptText(kept) && <p className="muted">Also kept from it: {keptText(kept)}.</p>}
        </div>
      )}
      <Field label="Name">
        <input required value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <EntryFields me={me} value={entry} onChange={setEntry} />
      <Resources me={me} value={res} onChange={setRes} />
      <DataFields me={me} value={data} onChange={setData} />
      {mentionsLakehouse(code) && !data.lakehouse && (
        <p className="warning" role="status">
          {me.limits?.dataAccess?.lakehouse ? (
            <>
              The code mentions <code>lakehouse.</code> but the lakehouse box is off: without it the run has no <code>lakehouse</code> catalog
              and Spark fails with REQUIRES_SINGLE_PART_NAMESPACE. You can still submit.
            </>
          ) : (
            <>
              The code mentions <code>lakehouse.</code>, but this install offers runs no lakehouse access, so Spark will fail with
              REQUIRES_SINGLE_PART_NAMESPACE.
            </>
          )}
        </p>
      )}
      <ErrorBox error={error} />
      <button type="submit" disabled={busy}>
        Submit
      </button>
    </form>
  );
}

export function NewSessionPage({ me }: { me: Me }) {
  const [name, setName] = useState("");
  const [res, setRes] = useState<ResourceChoice>({ driver: "", max: "0", executor: "" });
  const [data, setData] = useState<DataChoice>({ database: false, lakehouse: false, storage: "" });
  const [language, setLanguage] = useState<"python" | "scala">("python");
  const [error, setError] = useState<Error | undefined>();
  const scala = !!me.limits?.languages?.includes("scala");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    api
      .startSession({ name, language: scala ? language : undefined, resources: resources(res), dataAccess: dataAccess(data) })
      .then((s) => go("sessions/" + encodeURIComponent(s.id)), setError);
  };
  return (
    <form onSubmit={submit}>
      <p>
        <a href="#/">← Runs</a>
      </p>
      <h2>New session</h2>
      <p className="muted">
        It stops after {me.limits?.sessionIdleTimeout} with nothing running, and after {me.limits?.sessionMaxLifetime} in any case.
        Statements run one at a time; a runaway one runs until then, or until you delete the session.
      </p>
      <Field label="Name">
        <input required value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      {scala && (
        <fieldset>
          <legend>Language (fixed for the session; SQL works in both)</legend>
          <label className="check">
            <input type="radio" name="language" checked={language === "python"} onChange={() => setLanguage("python")} /> Python
          </label>
          <label className="check">
            <input type="radio" name="language" checked={language === "scala"} onChange={() => setLanguage("scala")} /> Scala
          </label>
        </fieldset>
      )}
      <Resources me={me} value={res} onChange={setRes} />
      <DataFields me={me} value={data} onChange={setData} />
      <ErrorBox error={error} />
      <button type="submit">Start</button>
    </form>
  );
}

export function SessionPage({ id, me }: { id: string; me: Me }) {
  const session = useLoad(() => api.session(id), [id], 3000, (s) => live(s.state));
  const statements = useLoad(() => api.statements(id), [id], 2000, (ss) => ss.some((s) => s.state === "waiting" || s.state === "running"));
  const [kind, setKind] = useState<"sql" | "lang">("sql");
  const [code, setCode] = useState("");
  const [error, setError] = useState<Error | undefined>();
  if (session.error) return <ErrorBox error={session.error} />;
  if (!session.value) return <p className="muted">Loading…</p>;
  const s = session.value;
  const mine = s.submitter === me.subject;
  const scalaSession = s.language === "scala";
  const run = (e: FormEvent) => {
    e.preventDefault();
    api.addStatement(id, kind === "sql" ? "sql" : scalaSession ? "scala" : "python", code).then(() => {
      setCode("");
      setError(undefined);
      statements.reload();
      session.reload();
    }, setError);
  };
  return (
    <section>
      <p>
        <a href="#/">← Runs</a>
      </p>
      <h2>{s.name}</h2>
      <Facts run={s} />
      <SparkUILink run={s} me={me} />
      {live(s.state) && !s.stopRequested && (
        <button type="button" className="danger" onClick={() => api.deleteSession(id).then(session.reload, setError)}>
          Delete session
        </button>
      )}
      <h3>Statements</h3>
      {(statements.value ?? []).map((st) => (
        <div key={st.id} className="statement">
          <div className="statement-head">
            <span className="mono">
              #{st.seq} {st.kind}
            </span>{" "}
            <Badge state={st.state} />
          </div>
          {st.code ? <Pre text={st.code} label={`Statement ${st.seq} code`} /> : <p className="muted">(code cleared)</p>}
          {st.output?.type === "table" && (
            <table className="table">
              <thead>
                <tr>
                  {st.output.columns.map((c) => (
                    <th key={c.name}>{c.name}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {st.output.rows.map((row, i) => (
                  <tr key={i}>
                    {row.map((v, j) => (
                      <td key={j}>{v === null ? "null" : String(v)}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {st.output?.type === "text" && (
            <>
              {st.output.stdout && <Pre text={st.output.stdout} label={`Statement ${st.seq} output`} />}
              {st.output.stderr && <Pre text={st.output.stderr} label={`Statement ${st.seq} stderr`} />}
            </>
          )}
          {st.output?.truncated && <p className="muted">(output truncated)</p>}
          {st.error && <Pre text={st.error} label={`Statement ${st.seq} error`} />}
        </div>
      ))}
      {mine && live(s.state) && (
        <form onSubmit={run} className="console">
          <div className="row">
            <label className="check">
              <input type="radio" checked={kind === "sql"} onChange={() => setKind("sql")} /> SQL
            </label>
            <label className="check">
              <input type="radio" checked={kind === "lang"} onChange={() => setKind("lang")} /> {scalaSession ? "Scala" : "Python"}
            </label>
          </div>
          <textarea
            className="code"
            rows={5}
            aria-label="Statement"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder={kind === "sql" ? "SELECT 1" : scalaSession ? "spark.range(10).count()" : "print(spark.range(10).count())"}
          />
          <button type="submit" disabled={!code.trim()}>
            Run
          </button>
        </form>
      )}
      {!mine && <p className="muted">Only its submitter runs statements in a session.</p>}
      <ErrorBox error={error} />
    </section>
  );
}
