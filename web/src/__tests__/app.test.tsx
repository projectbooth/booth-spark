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
      if (url === "ui/api/applications") return { json: { applications: [run({ state: "succeeded", reason: "the application finished" })] } };
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
      if (url === "ui/api/applications") return { json: { applications: [] } };
      return { json: { sessions: [] } };
    });
    render(<App />);
    expect(await screen.findByText(/needs editor or above/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New application" })).toBeNull();
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
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await waitFor(() => expect(window.location.hash).toBe("#/applications/r8"));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.body).toEqual({
      name: "scala smoke",
      main: { jar: { backendId: "local", path: "apps/scala-smoke.jar" }, mainClass: "booth.smoke.ScalaSmoke" },
      args: ["723445", "nfl"],
      resources: expect.anything(),
    });
  });

  it("offers a JAR only when the install offers runs booth-storage", async () => {
    serve((_m, url) => (url === "ui/api/me" ? { json: { ...editor, limits: { ...editor.limits, dataAccess: { database: false, lakehouse: false, storage: false } } } } : { json: {} }));
    window.location.hash = "#/new";
    render(<App />);
    expect(await screen.findByLabelText("JAR in Storage")).toBeDisabled();
    expect(screen.getByText("A JAR is read from booth-storage, which this install doesn't offer runs.")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "PySpark code" })).toBeChecked();
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
