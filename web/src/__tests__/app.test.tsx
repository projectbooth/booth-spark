import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import App from "../App";
import { dataAccess } from "../pages";

type Handler = (method: string, url: string, body?: unknown) => { status?: number; json?: unknown; text?: string };

const editor = {
  subject: "u-editor",
  displayName: "editor-user",
  workspace: "acme",
  role: "editor",
  operator: false,
  canSubmit: true,
  submitMinRole: "editor",
  limits: {
    maxExecutors: 2,
    defaultExecutors: 2,
    driverMemory: "512m",
    executorMemory: "512m",
    maxMemory: "2g",
    maxDuration: "6h0m0s",
    sessionIdleTimeout: "20m0s",
    sessionMaxLifetime: "12h0m0s",
    dataAccess: { database: true, lakehouse: false, storage: true },
  },
};
const viewer = { ...editor, subject: "u-viewer", displayName: "viewer-user", role: "viewer", canSubmit: false };

const run = (over: Record<string, unknown> = {}) => ({
  id: "r1",
  kind: "application",
  workspace: "acme",
  submitter: "u-editor",
  submitterName: "editor-user",
  workload: false,
  name: "pi",
  state: "running",
  stopRequested: false,
  createdAt: "2026-10-10T10:00:00Z",
  footprintMi: 2688,
  resources: { driverMemory: "512m", executorMemory: "512m", minExecutors: 0, maxExecutors: 2, maxDuration: "6h0m0s" },
  uiPath: "/iframe/spark/runs/r1/ui/",
  main: { inlinePythonBytes: 8, inlinePythonSha256: "ab".repeat(32) },
  ...over,
});

let calls: { method: string; url: string; body?: unknown }[] = [];

/** GET ui/api/applications with any query: the list endpoint. */
const isList = (url: string) => url.split("?")[0] === "ui/api/applications";

/** The list endpoint's answer, as the backend gives it: state, then limit, and whether there are more. */
function listing(url: string, runs: ReturnType<typeof run>[]) {
  const q = new URLSearchParams(url.split("?")[1] ?? "");
  const matching = runs.filter((r) => !q.get("state") || r.state === q.get("state"));
  const limit = Number(q.get("limit") ?? 200);
  return { json: { applications: matching.slice(0, limit), more: matching.length > limit } };
}

function serve(handler: Handler) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method, url, body });
      const r = handler(method, url, body);
      const isJSON = r.json !== undefined;
      return new Response(isJSON ? JSON.stringify(r.json) : (r.text ?? ""), {
        status: r.status ?? 200,
        headers: { "Content-Type": isJSON ? "application/json" : "text/plain" },
      });
    }),
  );
}

beforeEach(() => {
  window.location.hash = "";
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the runs page", () => {
  it("lists applications and sessions, relative to the iframe path, and offers submission to an editor", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, [run({ state: "succeeded", reason: "the application finished" })]);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    expect(await screen.findByText("pi")).toBeInTheDocument();
    expect(screen.getByText("the application finished")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New application" })).toBeInTheDocument();
    expect(screen.getByText("No sessions you can see.")).toBeInTheDocument();
    // Never an absolute URL: the page lives under /iframe/spark/.
    expect(calls.every((c) => c.url.startsWith("ui/api/"))).toBe(true);
  });

  it("offers a viewer no submission, and says why", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: viewer };
      if (isList(url)) return listing(url, []);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      if (url === "ui/api/cluster") return { json: cluster };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    expect(await screen.findByText(/needs editor or above/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New application" })).toBeNull();
  });
});

const cluster = {
  master: "kubernetes",
  driver: { defaultMemory: "512m", cpuRequest: "250m", cpuLimit: "2" },
  executor: { defaultMemory: "512m", cpuRequest: "500m", cpuLimit: "1", cores: 1, maxPerRun: 2 },
  capacity: {
    maxRunning: 10,
    liveRuns: 5,
    maxRunningPerWorkspace: 4,
    liveRunsInWorkspace: 3,
    memoryBudgetMi: 24576,
    memoryInUseMi: 9600,
    memoryInUseInWorkspaceMi: 5376,
  },
};

describe("the runs page, explained", () => {
  const pipelineRun = run({
    id: "r2",
    name: "weekly",
    submitter: "pipeline:acme:p8",
    submitterName: "",
    workload: true,
    uiPath: undefined,
    dataOwner: "svc:acme:nightly",
    startedAt: "2026-10-10T10:01:00Z",
    origin: { kind: "pipeline", pipelineRun: "p8", task: "load_games", attempt: "2" },
  });
  const mine = run({ origin: { kind: "person" }, state: "pending" });
  const session = run({ id: "s1", kind: "session", name: "explore", origin: { kind: "person" }, startedAt: "2026-10-10T10:02:00Z" });

  function page() {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, [pipelineRun, mine]);
      if (url === "ui/api/sessions") return { json: { sessions: [session] } };
      if (url === "ui/api/cluster") return { json: cluster };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
  }

  it("says what applications and sessions are, on the page and on hover", async () => {
    page();
    const panel = (await screen.findByText("What are applications, sessions and clusters?")).closest("details")!;
    expect(within(panel).getByText(/runs once, start to finish/)).toBeInTheDocument();
    expect(within(panel).getByText(/like a notebook kernel with Spark behind it/)).toBeInTheDocument();
    // The install's limits, as people say them.
    expect(within(panel).getByText(/at most 6h/)).toBeInTheDocument();
    expect(within(panel).getByText(/for 20m/)).toBeInTheDocument();
    expect(within(panel).getByText(/A pipeline task that uses Spark starts an/)).toBeInTheDocument();
    // The same words as a tooltip on each heading, reachable by keyboard.
    const hint = screen.getByRole("img", { name: /An application is a Spark program that runs once/ });
    expect(hint).toHaveAttribute("title", expect.stringMatching(/runs once, start to finish/));
    expect(hint).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("img", { name: /A session is a Spark program that stays up/ })).toBeInTheDocument();
  });

  it("shows each run's owner, origin and start", async () => {
    page();
    const row = (await screen.findAllByRole("link", { name: "weekly" }))[0].closest("tr")!;
    // A pipeline's run belongs to the pipeline's owner, and names its task.
    expect(within(row).getByText("svc:acme:nightly")).toBeInTheDocument();
    expect(within(row).getByText("load_games")).toBeInTheDocument();
    expect(within(row).getByText(/Pipeline task/)).toBeInTheDocument();
    expect(within(row).getByText(/attempt 2/)).toBeInTheDocument();
    expect(within(row).getByText(new Date("2026-10-10T10:01:00Z").toLocaleString())).toBeInTheDocument();
    const mineRow = screen.getAllByRole("link", { name: "pi" })[0].closest("tr")!;
    expect(within(mineRow).getByText("editor-user")).toBeInTheDocument();
    expect(within(mineRow).getByText("A person")).toBeInTheDocument();
    expect(within(mineRow).getByText(/^waiting since /)).toBeInTheDocument();
  });

  it("names a run's module from the table, by the module core signed, not the subject", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, []);
      if (url === "ui/api/sessions")
        return {
          json: {
            sessions: [
              run({ id: "s2", kind: "session", name: "nb", workload: true, origin: { kind: "workload", module: "notebooks", workload: "notebook" } }),
              run({ id: "s3", kind: "session", name: "odd", workload: true, origin: { kind: "workload", module: "streamlit", workload: "notebook" } }),
              run({ id: "s4", kind: "session", name: "old", workload: true, origin: { kind: "workload", workload: "notebook" } }),
            ],
          },
        };
      if (url === "ui/api/cluster") return { json: cluster };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    const rowOf = async (name: string) => (await screen.findAllByRole("link", { name }))[0].closest("tr")!;
    expect(within(await rowOf("nb")).getByText("Notebook")).toBeInTheDocument();
    // A notebook-shaped subject from another module is that module's; an older run names its kind.
    expect(within(await rowOf("odd")).getByText("Another module (streamlit)")).toBeInTheDocument();
    expect(within(await rowOf("old")).getByText("Another module (notebook)")).toBeInTheDocument();
  });

  it("shows the cluster read-only: master, workers, capacity and what runs on it", async () => {
    page();
    const section = (await screen.findByRole("heading", { level: 2, name: "Clusters" })).closest("section")!;
    await within(section).findByText("Kubernetes");
    expect(within(section).getByText(/1 core each \(CPU 500m requested, 1 at most\)/)).toBeInTheDocument();
    expect(within(section).getByText(/5 running or waiting, of at most 10 on this install; 3 in this workspace/)).toBeInTheDocument();
    expect(within(section).getByText(/\(at most 4\)/)).toBeInTheDocument();
    // Three live here, and this person sees all three; two elsewhere, only counted.
    expect(within(section).queryByText(/other people's sessions/)).toBeNull();
    expect(within(section).getByText(/2 in other workspaces/)).toBeInTheDocument();
    expect(within(section).getByText(/9.4 GiB of 24 GiB held by running runs \(5.3 GiB by this workspace's\)/)).toBeInTheDocument();
    expect(within(section).getByRole("meter", { name: "Memory in use" })).toHaveAttribute("value", "9600");
    // The live runs this person can see, with what each holds.
    const table = within(section).getByRole("table");
    expect(within(table).getByRole("link", { name: "weekly" })).toHaveAttribute("href", "#/applications/r2");
    expect(within(table).getByRole("link", { name: "explore" })).toHaveAttribute("href", "#/sessions/s1");
    expect(within(table).getAllByText("0–2 × 512m")).toHaveLength(3);
    // Nothing on it changes anything.
    expect(within(section).queryByRole("button")).toBeNull();
    expect(calls.filter((c) => c.method !== "GET")).toEqual([]);
  });

  it("counts the live sessions it can't show", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, []);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      if (url === "ui/api/cluster") return { json: { ...cluster, capacity: { ...cluster.capacity, liveRuns: 2, liveRunsInWorkspace: 2 } } };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    expect(await screen.findByText(/2 in this workspace are other people's sessions/)).toBeInTheDocument();
    expect(screen.getByText("Nothing you can see is running in this workspace.")).toBeInTheDocument();
  });
});

describe("an application's page", () => {
  it("shows the log as text, never as markup, and the Spark UI link to its submitter", async () => {
    const evil = '<script>window.pwned=1</script><img src=x onerror="window.pwned=2">';
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/applications/r1") return { json: run() };
      if (url.startsWith("ui/api/applications/r1/logs")) return { text: "line one\n" + evil };
      return { status: 404, json: { error: "not found" } };
    });
    window.location.hash = "#/applications/r1";
    render(<App />);
    const log = await screen.findByLabelText("Driver log");
    await waitFor(() => expect(log.textContent).toContain(evil));
    expect(log.querySelector("script, img")).toBeNull();
    expect((window as unknown as { pwned?: number }).pwned).toBeUndefined();
    const link = screen.getByRole("link", { name: "Open the Spark UI" });
    expect(link).toHaveAttribute("href", "/iframe/spark/runs/r1/ui/");
    expect(link).not.toHaveAttribute("target");
    expect(screen.getByText(/inline Python, 8 bytes/)).toBeInTheDocument();
  });

  it("gives anyone else no Spark UI link, and no log unless they may read it", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: { ...editor, subject: "u-editor2" } };
      if (url === "ui/api/applications/r1") return { json: run() };
      return { status: 403, json: { error: "forbidden" } };
    });
    window.location.hash = "#/applications/r1";
    render(<App />);
    expect(await screen.findByText("The Spark UI opens for this run's submitter only.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open the Spark UI" })).toBeNull();
    expect(screen.getByText(/can read its log/)).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("/logs"))).toBe(false);
  });

  it("shows a pipeline's run with whom it acts as, and no Spark UI link", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: { ...editor, role: "owner" } };
      if (url === "ui/api/applications/r1")
        return {
          json: run({
            submitter: "pipeline:acme:p8",
            submitterName: "pipeline:acme:p8",
            workload: true,
            uiPath: undefined,
            dataOwner: "svc:acme:nightly",
            dataCeiling: "editor",
            dataAccess: { storage: [{ backendId: "spark", path: "smoke", access: "readwrite" }], role: "editor" },
          }),
        };
      if (url.startsWith("ui/api/applications/r1/logs")) return { text: "weekly" };
      return { status: 404, json: { error: "not found" } };
    });
    window.location.hash = "#/applications/r1";
    render(<App />);
    expect(await screen.findByText("pipeline:acme:p8 (a workload)")).toBeInTheDocument();
    expect(screen.getByText("Acting as")).toBeInTheDocument();
    expect(screen.getByText("svc:acme:nightly")).toBeInTheDocument();
    expect(screen.getByText(/has no Spark UI in Booth/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open the Spark UI" })).toBeNull();
    expect(screen.queryByText("The Spark UI opens for this run's submitter only.")).toBeNull();
  });

  it("stops a run for its submitter", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/applications/r1/stop" && m === "POST") return { status: 202, json: run({ stopRequested: true }) };
      if (url === "ui/api/applications/r1") return { json: run() };
      return { text: "" };
    });
    window.location.hash = "#/applications/r1";
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Stop" }));
    expect(calls.some((c) => c.method === "POST" && c.url === "ui/api/applications/r1/stop")).toBe(true);
  });
});

describe("submitting", () => {
  it("sends the form as the API's spec and opens the new run", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/applications" && m === "POST") return { status: 201, json: run({ id: "r9", state: "pending" }) };
      if (url.includes("/logs")) return { text: "" };
      return { json: run({ id: "r9" }) };
    });
    window.location.hash = "#/new";
    render(<App />);
    await userEvent.type(await screen.findByLabelText("Name"), "etl");
    await userEvent.click(screen.getByLabelText(/the workspace's database/));
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r9"));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.body).toMatchObject({ name: "etl", main: { inlinePython: expect.stringContaining("SparkSession") }, dataAccess: { database: true } });
  });

  it("submits a JAR in Storage with its main class and arguments", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/applications" && m === "POST") return { status: 201, json: run({ id: "r8", state: "pending" }) };
      if (url.includes("/logs")) return { text: "" };
      return { json: run({ id: "r8" }) };
    });
    window.location.hash = "#/new";
    render(<App />);
    await userEvent.type(await screen.findByLabelText("Name"), "scala smoke");
    await userEvent.click(screen.getByLabelText("JAR in Storage"));
    expect(screen.queryByRole("textbox", { name: /^PySpark code/ })).toBeNull();
    await userEvent.type(screen.getByLabelText(/^Storage backend/), "local");
    await userEvent.type(screen.getByLabelText(/^JAR path/), "/apps/scala-smoke.jar");
    await userEvent.type(screen.getByLabelText(/^Main class/), "booth.smoke.ScalaSmoke");
    await userEvent.type(screen.getByLabelText(/^Arguments/), "723445{enter}nfl");
    await userEvent.type(screen.getByLabelText(/^SHA-256/), "AB".repeat(32));
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r8"));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.body).toEqual({
      name: "scala smoke",
      main: { jar: { backendId: "local", path: "apps/scala-smoke.jar", sha256: "ab".repeat(32) }, mainClass: "booth.smoke.ScalaSmoke" },
      args: ["723445", "nfl"],
      resources: expect.anything(),
    });
  });

  it("submits Scala code where the install offers Scala, and offers it nowhere else", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: { ...editor, limits: { ...editor.limits, languages: ["python", "scala"] } } };
      if (url === "ui/api/applications" && m === "POST") return { status: 201, json: run({ id: "r7", state: "pending" }) };
      if (url.includes("/logs")) return { text: "" };
      return { json: run({ id: "r7", main: { inlineScalaBytes: 12, inlineScalaSha256: "cd".repeat(32) } }) };
    });
    window.location.hash = "#/new";
    const { unmount } = render(<App />);
    await userEvent.type(await screen.findByLabelText("Name"), "scala");
    await userEvent.click(screen.getByRole("radio", { name: "Scala code" }));
    const box = screen.getByRole("textbox", { name: /^Scala code/ });
    await userEvent.clear(box);
    await userEvent.type(box, "println(1)");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r7"));
    expect(calls.find((c) => c.method === "POST")!.body).toMatchObject({ name: "scala", main: { inlineScala: "println(1)" } });
    expect(await screen.findByText(/inline Scala, 12 bytes, sha256 cdcdcdcdcdcd…/)).toBeInTheDocument();
    unmount();

    // An install (or an older backend) that doesn't list Scala: no Scala choice.
    serve((_m, url) => (url === "ui/api/me" ? { json: editor } : { json: {} }));
    window.location.hash = "#/new";
    render(<App />);
    expect(await screen.findByRole("radio", { name: /^PySpark code/ })).toBeChecked();
    expect(screen.queryByRole("radio", { name: "Scala code" })).toBeNull();
  });

  it("offers a JAR only when the install offers runs booth-storage", async () => {
    serve((_m, url) => (url === "ui/api/me" ? { json: { ...editor, limits: { ...editor.limits, dataAccess: { database: false, lakehouse: false, storage: false } } } } : { json: {} }));
    window.location.hash = "#/new";
    render(<App />);
    expect(await screen.findByLabelText("JAR in Storage")).toBeDisabled();
    expect(screen.getByText("A JAR is read from booth-storage, which this install doesn't offer runs.")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^PySpark code/ })).toBeChecked();
    expect(screen.getByRole("textbox", { name: /^PySpark code/ })).toBeInTheDocument();
  });

  it("shows the API's refusal", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (m === "POST") return { status: 429, json: { error: "2 runs are already live on this install", code: "at_capacity" } };
      return { json: {} };
    });
    window.location.hash = "#/new";
    render(<App />);
    await userEvent.type(await screen.findByLabelText("Name"), "x");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("2 runs are already live on this install");
  });

  it("reads storage locations, one per line", () => {
    expect(dataAccess({ database: false, lakehouse: false, storage: "" })).toBeUndefined();
    expect(dataAccess({ database: false, lakehouse: true, storage: "lake/raw\nlake2/out rw\n" })).toEqual({
      database: undefined,
      lakehouse: true,
      storage: [
        { backendId: "lake", path: "raw", access: "read" },
        { backendId: "lake2", path: "out", access: "readwrite" },
      ],
    });
  });
});

describe("a session's page", () => {
  const session = { ...run({ id: "s1", kind: "session", name: "explore" }), idleTimeout: "20m0s", maxLifetime: "12h0m0s" };
  const statements = [
    { id: "t1", sessionId: "s1", seq: 1, kind: "sql", code: "select 1 as one", state: "available", createdAt: "", output: { type: "table", columns: [{ name: "one", type: "int" }], rows: [[1]], truncated: false } },
    { id: "t2", sessionId: "s1", seq: 2, kind: "python", code: "raise ValueError('<b>no</b>')", state: "error", createdAt: "", error: "ValueError: <b>no</b>" },
  ];

  it("shows statements and their results, and runs a new one for its submitter", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/sessions/s1/statements" && m === "POST") return { status: 201, json: { id: "t3", state: "waiting" } };
      if (url === "ui/api/sessions/s1/statements") return { json: { statements } };
      if (url === "ui/api/sessions/s1") return { json: session };
      return { json: {} };
    });
    window.location.hash = "#/sessions/s1";
    render(<App />);
    const table = await screen.findByRole("table");
    expect(within(table).getByText("one")).toBeInTheDocument();
    expect(within(table).getByText("1")).toBeInTheDocument();
    const err = screen.getByLabelText("Statement 2 error");
    expect(err.textContent).toBe("ValueError: <b>no</b>");
    expect(err.querySelector("b")).toBeNull();
    await userEvent.type(screen.getByLabelText("Statement"), "select 2");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && (c.body as { code: string })?.code === "select 2")).toBe(true));
  });

  it("runs Scala, not Python, in a Scala session", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (url === "ui/api/sessions/s1/statements" && m === "POST") return { status: 201, json: { id: "t3", state: "waiting" } };
      if (url === "ui/api/sessions/s1/statements") return { json: { statements: [] } };
      if (url === "ui/api/sessions/s1") return { json: { ...session, language: "scala" } };
      return { json: {} };
    });
    window.location.hash = "#/sessions/s1";
    render(<App />);
    await userEvent.click(await screen.findByRole("radio", { name: "Scala" }));
    expect(screen.queryByRole("radio", { name: "Python" })).toBeNull();
    await userEvent.type(screen.getByLabelText("Statement"), "spark.range(3).count()");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toEqual({ kind: "scala", code: "spark.range(3).count()" }));
  });

  it("starts a Scala session where the install offers Scala", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: { ...editor, limits: { ...editor.limits, languages: ["python", "scala"] } } };
      if (url === "ui/api/sessions" && m === "POST") return { status: 201, json: { ...session, id: "s9" } };
      if (url === "ui/api/sessions/s9") return { json: { ...session, id: "s9", language: "scala" } };
      return { json: { statements: [] } };
    });
    window.location.hash = "#/new-session";
    render(<App />);
    await userEvent.type(await screen.findByLabelText("Name"), "explore");
    await userEvent.click(screen.getByRole("radio", { name: "Scala" }));
    await userEvent.click(screen.getByRole("button", { name: "Start" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toMatchObject({ name: "explore", language: "scala" }));
  });

  it("gives a workspace owner who isn't its submitter no console", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: { ...editor, subject: "u-owner", role: "owner" } };
      if (url === "ui/api/sessions/s1/statements") return { json: { statements } };
      return { json: session };
    });
    window.location.hash = "#/sessions/s1";
    render(<App />);
    expect(await screen.findByText("Only its submitter runs statements in a session.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Statement")).toBeNull();
  });
});

const lakeEditor = { ...editor, limits: { ...editor.limits, dataAccess: { database: true, lakehouse: true, storage: true } } };

describe("the runs page, sessions first and applications a page at a time", () => {
  const many = Array.from({ length: 25 }, (_, i) => run({ id: "a" + i, name: "app-" + i, state: i === 24 ? "running" : "succeeded" }));

  it("puts Sessions above Applications, shows the newest 20 and the rest on Show more", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, many);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      if (url === "ui/api/cluster") return { json: cluster };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    expect(await screen.findByRole("link", { name: "app-0" })).toBeInTheDocument();
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings.indexOf("Sessions")).toBeGreaterThanOrEqual(0);
    expect(headings.indexOf("Sessions")).toBeLessThan(headings.indexOf("Applications"));
    // One page: 20, newest first, as the API gives them.
    expect(screen.getByRole("link", { name: "app-19" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "app-20" })).toBeNull();
    expect(calls.some((c) => c.url === "ui/api/applications?limit=20")).toBe(true);
    expect(screen.getByText("The newest 20.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(await screen.findByText("The newest 25.")).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "app-24" }).length).toBeGreaterThan(0);
    expect(calls.some((c) => c.url === "ui/api/applications?limit=40")).toBe(true);
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });

  it("counts every live application in Clusters, whichever page it's on", async () => {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url)) return listing(url, many);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      if (url === "ui/api/cluster") return { json: { ...cluster, capacity: { ...cluster.capacity, liveRuns: 1, liveRunsInWorkspace: 1 } } };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    const section = (await screen.findByRole("heading", { name: "Clusters", level: 2 })).closest("section")!;
    // app-24 is live but on the second page: still on the cluster.
    expect(await within(section).findByRole("link", { name: "app-24" })).toBeInTheDocument();
    expect(within(section).queryByText(/other people's sessions/)).toBeNull();
    expect(screen.getByRole("button", { name: "Show more" })).toBeInTheDocument();
  });

  it("shows why a pending run waits, in the list and on its page; nothing once it runs", async () => {
    const why = "resolving data access (will retry): the workspace's warehouse: the credential broker answered 503";
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: editor };
      if (isList(url))
        return listing(url, [
          run({ id: "w1", name: "waits", state: "pending", waitingReason: why }),
          run({ id: "w2", name: "runs", waitingReason: "stale" }),
        ]);
      if (url === "ui/api/sessions") return { json: { sessions: [] } };
      if (url === "ui/api/cluster") return { json: cluster };
      if (url === "ui/api/applications/w1") return { json: run({ id: "w1", state: "pending", waitingReason: why }) };
      if (url.includes("/logs")) return { text: "" };
      return { status: 404, json: { error: "not found" } };
    });
    render(<App />);
    const row = (await screen.findByRole("link", { name: "waits" })).closest("tr")!;
    expect(within(row).getByText("waiting: " + why)).toBeInTheDocument();
    expect(within(row).getByText("pending")).toBeInTheDocument();
    expect(screen.queryByText(/stale/)).toBeNull();
    window.location.hash = "#/applications/w1";
    expect(await screen.findByText("Waiting")).toBeInTheDocument();
    expect(screen.getByText(why)).toBeInTheDocument();
  });
});

describe("Run again", () => {
  const spec = {
    name: "weekly",
    main: { inlinePython: "print(spark.table('lakehouse.nfl.games').count())" },
    args: ["2026", "8"],
    resources: { driver: { memory: "1g" }, executors: { min: 1, max: 2, memory: "1536m" } },
    conf: { "spark.sql.shuffle.partitions": "8" },
    dataAccess: { lakehouse: true, storage: [{ backendId: "lake", path: "acme-files/in", access: "readwrite" }] },
    maxDuration: "30m",
  };
  const old = run({ name: "weekly", state: "succeeded" });
  function serveOld(over: { spec?: object; codeCleared?: boolean; run?: object } = {}) {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: lakeEditor };
      if (url === "ui/api/applications/r1/spec") return { json: { spec: over.spec ?? spec, codeCleared: !!over.codeCleared } };
      if (url === "ui/api/applications" && m === "POST") return { status: 201, json: run({ id: "r9", state: "pending" }) };
      if (url.includes("/logs")) return { text: "" };
      if (url === "ui/api/applications/r1") return { json: { ...old, ...over.run } };
      return { json: run({ id: "r9" }) };
    });
  }

  it("opens New application filled in from the run, and submits a new run", async () => {
    serveOld();
    window.location.hash = "#/applications/r1";
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Run again with these settings" }));
    expect(window.location.hash).toBe("#/new/r1");
    expect(await screen.findByText(/Submitting starts a new run; that one stays as it is/)).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toHaveValue("weekly");
    expect(screen.getByRole("textbox", { name: /^PySpark code/ })).toHaveValue(spec.main.inlinePython);
    expect(screen.getByLabelText(/^Arguments/)).toHaveValue("2026\n8");
    expect(screen.getByLabelText(/^Driver memory/)).toHaveValue("1g");
    expect(screen.getByLabelText(/^Executor memory/)).toHaveValue("1536m");
    expect(screen.getByLabelText(/the workspace's lakehouse/)).toBeChecked();
    expect(screen.getByLabelText(/^Storage locations/)).toHaveValue("lake/acme-files/in rw");
    expect(screen.getByText(/Also kept from it: spark.sql.shuffle.partitions=8; at least 1 executor; at most 30m/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r9"));
    // One new submission with the same settings; nothing sent to the old run.
    const writes = calls.filter((c) => c.method !== "GET");
    expect(writes).toHaveLength(1);
    expect(writes[0]).toMatchObject({ method: "POST", url: "ui/api/applications" });
    expect(writes[0].body).toEqual(spec);
  });

  it("says when retention cleared the code, and fills in only the settings", async () => {
    serveOld({
      spec: { ...spec, main: {} },
      codeCleared: true,
      run: { contentClearedAt: "2026-10-12T10:00:00Z", main: { inlinePythonBytes: 50, inlinePythonSha256: "ab".repeat(32) } },
    });
    window.location.hash = "#/new/r1";
    render(<App />);
    expect(await screen.findByText(/Its code was cleared .* \(sessions.resultRetention\), so only its settings are filled in/)).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: /^PySpark code/ })).toHaveValue("");
    expect(screen.getByLabelText(/^Driver memory/)).toHaveValue("1g");
    expect(screen.getByLabelText(/the workspace's lakehouse/)).toBeChecked();
  });

  it("keeps a Python file entry point as it was", async () => {
    serveOld({ spec: { name: "nightly", main: { python: { backendId: "lake", path: "jobs/etl.py", sha256: "cd".repeat(32) } } } });
    window.location.hash = "#/new/r1";
    render(<App />);
    expect(await screen.findByRole("radio", { name: /The earlier run's Python file in Storage/ })).toBeChecked();
    expect(screen.getByText("lake/jobs/etl.py")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST")?.body).toMatchObject({
        name: "nightly",
        main: { python: { backendId: "lake", path: "jobs/etl.py", sha256: "cd".repeat(32) } },
      }),
    );
  });

  it("isn't offered to someone who can't read the run's code, or can't submit", async () => {
    for (const me of [{ ...editor, subject: "u-editor2" }, viewer]) {
      serve((_m, url) => {
        if (url === "ui/api/me") return { json: me };
        if (url === "ui/api/applications/r1") return { json: old };
        if (url.includes("/logs")) return { text: "" };
        return { status: 404, json: { error: "not found" } };
      });
      window.location.hash = "#/applications/r1";
      const { unmount } = render(<App />);
      expect(await screen.findByRole("heading", { name: "weekly" })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Run again with these settings" })).toBeNull();
      unmount();
    }
  });
});

describe("lakehouse hints", () => {
  it("warns on the form when the code names lakehouse. and the box is off, without blocking the submit", async () => {
    serve((m, url) => {
      if (url === "ui/api/me") return { json: lakeEditor };
      if (url === "ui/api/applications" && m === "POST") return { status: 201, json: run({ id: "r9", state: "pending" }) };
      return { json: run({ id: "r9" }) };
    });
    window.location.hash = "#/new";
    render(<App />);
    const code = await screen.findByRole("textbox", { name: /^PySpark code/ });
    // Positive control: the default code names no lakehouse.
    expect(screen.queryByText(/the lakehouse box is off/)).toBeNull();
    await userEvent.clear(code);
    await userEvent.type(code, "spark.table('lakehouse.nfl.games').show()");
    expect(screen.getByText(/the lakehouse box is off/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/the workspace's lakehouse/));
    expect(screen.queryByText(/the lakehouse box is off/)).toBeNull();
    await userEvent.click(screen.getByLabelText(/the workspace's lakehouse/));
    // A word that only ends in lakehouse isn't the catalog.
    await userEvent.clear(code);
    await userEvent.type(code, "mylakehouse.x = 1");
    expect(screen.queryByText(/the lakehouse box is off/)).toBeNull();
    await userEvent.type(code, "\nlakehouse.x");
    expect(screen.getByText(/the lakehouse box is off/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Submit" })).toBeEnabled();
    await userEvent.type(screen.getByLabelText("Name"), "etl");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r9"));
    expect(calls.find((c) => c.method === "POST")?.body).not.toHaveProperty("dataAccess.lakehouse");
  });

  const missing =
    "pyspark.errors.exceptions.captured.AnalysisException: [REQUIRES_SINGLE_PART_NAMESPACE] spark_catalog requires a single-part namespace, but got `lakehouse`.`nfl`. SQLSTATE: 42K05";
  function failed(over: object, log: string) {
    serve((_m, url) => {
      if (url === "ui/api/me") return { json: lakeEditor };
      if (url === "ui/api/applications/r1") return { json: run({ state: "failed", reason: "the driver exited with code 1 (Error)", ...over }) };
      if (url.startsWith("ui/api/applications/r1/logs")) return { text: log };
      return { status: 404, json: { error: "not found" } };
    });
    window.location.hash = "#/applications/r1";
    render(<App />);
  }

  it("says on a failed run's page that it had no lakehouse access, when its log shows why", async () => {
    failed({}, "line\n" + missing + "\n");
    expect(await screen.findByText(/this run had no lakehouse data access/)).toBeInTheDocument();
  });

  it("says nothing when the run had lakehouse access", async () => {
    failed({ dataAccess: { lakehouse: true } }, missing);
    await waitFor(() => expect(screen.getByLabelText("Driver log").textContent).toContain("REQUIRES_SINGLE_PART_NAMESPACE"));
    expect(screen.queryByText(/this run had no lakehouse data access/)).toBeNull();
  });

  it("says nothing for the same error about another name", async () => {
    failed({}, missing.replace("`lakehouse`", "`warehouse`"));
    await waitFor(() => expect(screen.getByLabelText("Driver log").textContent).toContain("REQUIRES_SINGLE_PART_NAMESPACE"));
    expect(screen.queryByText(/this run had no lakehouse data access/)).toBeNull();
  });
});
