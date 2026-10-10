// The module's own API (internal/api: /v1's handlers under ui/api, as the person core's assertion
// names). URLs are relative: this page is served at /iframe/spark/ and booth-core forwards the rest,
// attaching the viewer's identity, so the page holds no token. The backend decides every
// permission; the UI only hides what would be refused anyway.

export type Role = "owner" | "editor" | "viewer";
export type State = "pending" | "running" | "succeeded" | "failed" | "stopped";

export interface DataLimits {
  database: boolean;
  lakehouse: boolean;
  storage: boolean;
}

export interface Me {
  subject: string;
  displayName: string;
  workspace: string;
  role: Role;
  operator: boolean;
  canSubmit: boolean;
  submitMinRole: string;
  limits?: {
    maxExecutors: number;
    defaultExecutors: number;
    driverMemory: string;
    executorMemory: string;
    maxMemory: string;
    maxDuration: string;
    sessionIdleTimeout: string;
    sessionMaxLifetime: string;
    dataAccess: DataLimits;
  };
}

export interface StorageLocation {
  backendId: string;
  path: string;
  access?: "read" | "readwrite";
}

export interface DataView {
  database?: boolean;
  lakehouse?: boolean;
  storage?: StorageLocation[];
  role?: "editor" | "viewer";
  warehouseRoot?: string;
  storageRoots?: string[];
}

export interface Run {
  id: string;
  kind: "application" | "session";
  workspace: string;
  submitter: string;
  submitterName: string;
  workload: boolean;
  name: string;
  state: State;
  reason?: string;
  stopRequested: boolean;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  contentClearedAt?: string;
  lastActivityAt?: string;
  footprintMi: number;
  resources: { driverMemory: string; executorMemory: string; minExecutors: number; maxExecutors: number; maxDuration?: string };
  idleTimeout?: string;
  maxLifetime?: string;
  uiPath?: string;
  dataAccess?: DataView;
  main?: { inlinePythonBytes?: number; inlinePythonSha256?: string; python?: { backendId: string; path: string }; jar?: { backendId: string; path: string }; mainClass?: string };
}

export type StatementState = "waiting" | "running" | "available" | "error" | "cancelled";

export interface TableOutput {
  type: "table";
  columns: { name: string; type: string }[];
  rows: unknown[][];
  truncated: boolean;
}

export interface TextOutput {
  type: "text";
  stdout: string;
  stderr: string;
  truncated: boolean;
}

export interface Statement {
  id: string;
  sessionId: string;
  seq: number;
  kind: "sql" | "python";
  code: string;
  state: StatementState;
  output?: TableOutput | TextOutput;
  error?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
}

/** An error the API answered, with its own message (the body's `error`). */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch("ui/api/" + path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  const text = await res.text();
  if (!res.ok) {
    let msg = text || res.statusText;
    let code = "";
    try {
      const j = JSON.parse(text) as { error?: string; code?: string };
      msg = j.error ?? msg;
      code = j.code ?? "";
    } catch {
      // A plain-text error (a log endpoint) stays as it came.
    }
    throw new ApiError(res.status, code, msg);
  }
  const type = res.headers.get("Content-Type") ?? "";
  return (type.includes("application/json") ? JSON.parse(text) : text) as T;
}

const enc = encodeURIComponent;

export const api = {
  me: () => call<Me>("GET", "me"),
  applications: () => call<{ applications: Run[] }>("GET", "applications").then((r) => r.applications ?? []),
  application: (id: string) => call<Run>("GET", "applications/" + enc(id)),
  submit: (spec: unknown) => call<Run>("POST", "applications", spec),
  stop: (id: string) => call<Run>("POST", "applications/" + enc(id) + "/stop"),
  logs: (id: string) => call<string>("GET", "applications/" + enc(id) + "/logs?tail=2000"),
  sessions: () => call<{ sessions: Run[] }>("GET", "sessions").then((r) => r.sessions ?? []),
  session: (id: string) => call<Run>("GET", "sessions/" + enc(id)),
  startSession: (spec: unknown) => call<Run>("POST", "sessions", spec),
  deleteSession: (id: string) => call<Run>("DELETE", "sessions/" + enc(id)),
  sessionLogs: (id: string) => call<string>("GET", "sessions/" + enc(id) + "/logs"),
  statements: (id: string) =>
    call<{ statements: Statement[] }>("GET", "sessions/" + enc(id) + "/statements").then((r) => r.statements ?? []),
  addStatement: (id: string, kind: "sql" | "python", code: string) =>
    call<Statement>("POST", "sessions/" + enc(id) + "/statements", { kind, code }),
};

export const live = (s: State) => s === "pending" || s === "running";
