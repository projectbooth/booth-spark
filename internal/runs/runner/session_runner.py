# booth-spark's session runner (build step 4, docs/design-v0.md item 6): the program a session's
# driver runs. It holds one SparkSession for the session's life and executes statements, one at a
# time, that the backend sends it. Nothing else talks to it: its port is open to the backend's pods
# only (NetworkPolicy), and every request must carry the session's bearer, delivered as a Secret in
# the session's own namespace. It is not Spark Connect, and nothing outside Booth reaches it
# (ADR 0110 ruling 7).
#
#   POST /statements      {"id", "kind": "sql"|"python", "code"}   -> 202 (queued)
#   GET  /statements/<id>                                          -> the statement's state and output
#   GET  /healthz                                                  -> 200 once the SparkSession exists
#
# A statement that fails (an exception, a bad query) is recorded as "error" with its traceback; the
# session goes on. Results are kept in memory, so a backend that restarts mid-statement asks again
# and gets them. The runner exits only when its driver is stopped (the namespace is deleted).
import contextlib
import hmac
import io
import json
import os
import queue
import sys
import threading
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from pyspark.sql import SparkSession

PORT = 8998
MAX_ROWS = 1000
MAX_OUTPUT = 1 << 20  # bytes of output kept per statement
MAX_ERROR = 64 << 10
MAX_BODY = 512 << 10

with open(os.environ["BOOTH_SESSION_TOKEN_FILE"]) as f:
    TOKEN = f.read().strip()
if not TOKEN:
    sys.exit("booth-spark session runner: empty session token")

spark = SparkSession.builder.getOrCreate()
sc = spark.sparkContext
# The statements' shared namespace: what one statement defines, the next can use.
scope = {"spark": spark, "sc": sc, "__name__": "__booth_session__"}
statements = {}
lock = threading.Lock()
work = queue.Queue()


def cap(s, n):
    b = s.encode("utf-8", "replace")
    return (b[:n].decode("utf-8", "ignore"), len(b) > n)


def run_sql(code):
    df = spark.sql(code)
    rows = df.limit(MAX_ROWS + 1).collect()
    truncated = len(rows) > MAX_ROWS
    out = {
        "type": "table",
        "columns": [{"name": f.name, "type": f.dataType.simpleString()} for f in df.schema.fields],
        "rows": [[v for v in r] for r in rows[:MAX_ROWS]],
        "truncated": truncated,
    }
    text = json.dumps(out, default=str)
    if len(text) > MAX_OUTPUT:
        # Too big as a whole: keep as many rows as fit.
        while out["rows"] and len(json.dumps(out, default=str)) > MAX_OUTPUT:
            out["rows"] = out["rows"][: len(out["rows"]) // 2]
        out["truncated"] = True
    return json.loads(json.dumps(out, default=str))


def run_python(code):
    stdout, stderr = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        exec(compile(code, "<statement>", "exec"), scope)  # noqa: S102 - the session owner's own code
    out, t1 = cap(stdout.getvalue(), MAX_OUTPUT)
    err, t2 = cap(stderr.getvalue(), MAX_OUTPUT)
    return {"type": "text", "stdout": out, "stderr": err, "truncated": t1 or t2}


def worker():
    while True:
        sid = work.get()
        with lock:
            st = statements[sid]
            st["state"] = "running"
        sc.setJobGroup(sid, "booth statement " + sid, interruptOnCancel=True)
        try:
            output = run_sql(st["code"]) if st["kind"] == "sql" else run_python(st["code"])
            result = {"state": "available", "output": output, "error": ""}
        except BaseException:  # noqa: BLE001 - a failed statement never ends the session
            err, _ = cap(traceback.format_exc(), MAX_ERROR)
            result = {"state": "error", "output": None, "error": err}
        finally:
            sc.setJobGroup("", "")
        with lock:
            st.update(result)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # one line per request, never a body
        sys.stderr.write("session-runner: %s %s\n" % (self.command, self.path))

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def authorized(self):
        got = self.headers.get("Authorization", "")
        if got.startswith("Bearer ") and hmac.compare_digest(got[7:].strip().encode(), TOKEN.encode()):
            return True
        self.reply(401, {"error": "unauthorized"})
        return False

    def do_GET(self):  # noqa: N802
        if not self.authorized():
            return
        if self.path == "/healthz":
            return self.reply(200, {"status": "ok", "applicationId": sc.applicationId})
        if self.path.startswith("/statements/"):
            sid = self.path[len("/statements/"):]
            with lock:
                st = statements.get(sid)
                body = None if st is None else {k: st[k] for k in ("id", "state", "output", "error")}
            return self.reply(404, {"error": "unknown statement"}) if body is None else self.reply(200, body)
        self.reply(404, {"error": "not found"})

    def do_POST(self):  # noqa: N802
        if not self.authorized():
            return
        if self.path != "/statements":
            return self.reply(404, {"error": "not found"})
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0 or n > MAX_BODY:
            return self.reply(400, {"error": "body size"})
        try:
            req = json.loads(self.rfile.read(n))
            sid, kind, code = req["id"], req["kind"], req["code"]
            assert isinstance(sid, str) and kind in ("sql", "python") and isinstance(code, str)
        except Exception:  # noqa: BLE001
            return self.reply(400, {"error": "need id, kind (sql or python) and code"})
        with lock:
            if sid in statements:  # a backend retrying after a restart: already queued
                return self.reply(202, {"id": sid, "state": statements[sid]["state"]})
            statements[sid] = {"id": sid, "kind": kind, "code": code, "state": "waiting", "output": None, "error": ""}
        work.put(sid)
        self.reply(202, {"id": sid, "state": "waiting"})


threading.Thread(target=worker, daemon=True).start()
print("booth-spark session runner: ready on port %d (application %s)" % (PORT, sc.applicationId), flush=True)
ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
