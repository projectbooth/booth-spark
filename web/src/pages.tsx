import { useState, type FormEvent } from "react";
import { api, live, type Me, type Run, type StorageLocation } from "./api";
import { Badge, ErrorBox, Field, Pre, when } from "./components";
import { go, useLoad } from "./hooks";

const anyLive = (rs: Run[]) => rs.some((r) => live(r.state));

/** The workspace's applications, and the sessions this person may see. */
export function RunsPage({ me }: { me: Me }) {
  const apps = useLoad(api.applications, [], 5000, anyLive);
  const sessions = useLoad(api.sessions, [], 5000, anyLive);
  return (
    <>
      <section>
        <div className="section-head">
          <h2>Applications</h2>
          {me.canSubmit && (
            <button type="button" onClick={() => go("new")}>
              New application
            </button>
          )}
        </div>
        <ErrorBox error={apps.error} />
        <RunTable runs={apps.value} kind="applications" empty="No applications in this workspace yet." />
      </section>
      <section>
        <div className="section-head">
          <h2>Sessions</h2>
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
      {!me.canSubmit && (
        <p className="muted">
          Your role can see runs but not submit them: that needs {me.submitMinRole} or above.
        </p>
      )}
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
          <th>Submitted by</th>
          <th>Created</th>
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
            <td>{r.submitterName || r.submitter}</td>
            <td>{when(r.createdAt)}</td>
            <td className="reason">{r.reason}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

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
      <dt>Submitted by</dt>
      <dd>
        {run.submitterName || run.submitter}
        {run.workload && " (a workload)"}
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
      {(run.main?.python || run.main?.jar) && (
        <>
          <dt>Code</dt>
          <dd className="mono">
            {run.main.python
              ? `${run.main.python.backendId}/${run.main.python.path}`
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
      <ErrorBox error={busy} />
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

/** The application's entry point as the form holds it (docs/design-scala.md, section 2 (b)). */
export interface EntryChoice {
  kind: "python" | "jar";
  code: string;
  backendId: string;
  path: string;
  mainClass: string;
  args: string;
  sha256: string;
}

/** The form's entry point as the API's main and args. */
export function entryPoint(c: EntryChoice): { main: Record<string, unknown>; args?: string[] } {
  if (c.kind === "python") return { main: { inlinePython: c.code } };
  const args = c.args
    .split("\n")
    .map((a) => a.trim())
    .filter(Boolean);
  return {
    main: {
      jar: { backendId: c.backendId.trim(), path: c.path.trim().replace(/^\/+/, ""), sha256: c.sha256.trim().toLowerCase() || undefined },
      mainClass: c.mainClass.trim(),
    },
    args: args.length ? args : undefined,
  };
}

function EntryFields({ me, value, onChange }: { me: Me; value: EntryChoice; onChange: (v: EntryChoice) => void }) {
  const storage = !!me.limits?.dataAccess?.storage;
  const set = (over: Partial<EntryChoice>) => onChange({ ...value, ...over });
  return (
    <>
      <fieldset>
        <legend>Entry point</legend>
        <label className="check">
          <input type="radio" name="entry" checked={value.kind === "python"} onChange={() => set({ kind: "python" })} /> PySpark code
        </label>
        <label className="check">
          <input type="radio" name="entry" checked={value.kind === "jar"} disabled={!storage} onChange={() => set({ kind: "jar" })} /> JAR in
          Storage
        </label>
        {!storage && <p className="muted">A JAR is read from booth-storage, which this install doesn't offer runs.</p>}
      </fieldset>
      {value.kind === "python" ? (
        <Field label="PySpark code" hint="At most 256 KiB. It runs as you, in its own namespace.">
          <textarea className="code" rows={12} value={value.code} onChange={(e) => set({ code: e.target.value })} />
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
          <Field label="Arguments" hint="One per line (optional)">
            <textarea rows={3} value={value.args} onChange={(e) => set({ args: e.target.value })} />
          </Field>
          <Field label="SHA-256" hint="Optional: the run refuses to start if the JAR's bytes don't hash to this">
            <input value={value.sha256} pattern="[0-9a-fA-F]{64}" onChange={(e) => set({ sha256: e.target.value })} />
          </Field>
        </>
      )}
    </>
  );
}

export function NewApplicationPage({ me }: { me: Me }) {
  const [name, setName] = useState("");
  const [entry, setEntry] = useState<EntryChoice>({
    kind: "python",
    code: "from pyspark.sql import SparkSession\n\nspark = SparkSession.builder.getOrCreate()\nprint(spark.range(10).count())\n",
    backendId: "",
    path: "",
    mainClass: "",
    args: "",
    sha256: "",
  });
  const [res, setRes] = useState<ResourceChoice>({ driver: "", max: String(me.limits?.defaultExecutors ?? ""), executor: "" });
  const [data, setData] = useState<DataChoice>({ database: false, lakehouse: false, storage: "" });
  const [error, setError] = useState<Error | undefined>();
  const [busy, setBusy] = useState(false);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    api
      .submit({ name, ...entryPoint(entry), resources: resources(res), dataAccess: dataAccess(data) })
      .then((r) => go("applications/" + encodeURIComponent(r.id)), setError)
      .finally(() => setBusy(false));
  };
  return (
    <form onSubmit={submit}>
      <p>
        <a href="#/">← Runs</a>
      </p>
      <h2>New application</h2>
      <Field label="Name">
        <input required value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <EntryFields me={me} value={entry} onChange={setEntry} />
      <Resources me={me} value={res} onChange={setRes} />
      <DataFields me={me} value={data} onChange={setData} />
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
  const [error, setError] = useState<Error | undefined>();
  const submit = (e: FormEvent) => {
    e.preventDefault();
    api
      .startSession({ name, resources: resources(res), dataAccess: dataAccess(data) })
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
  const [kind, setKind] = useState<"sql" | "python">("sql");
  const [code, setCode] = useState("");
  const [error, setError] = useState<Error | undefined>();
  if (session.error) return <ErrorBox error={session.error} />;
  if (!session.value) return <p className="muted">Loading…</p>;
  const s = session.value;
  const mine = s.submitter === me.subject;
  const run = (e: FormEvent) => {
    e.preventDefault();
    api.addStatement(id, kind, code).then(() => {
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
              <input type="radio" checked={kind === "python"} onChange={() => setKind("python")} /> Python
            </label>
          </div>
          <textarea
            className="code"
            rows={5}
            aria-label="Statement"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder={kind === "sql" ? "SELECT 1" : "print(spark.range(10).count())"}
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
