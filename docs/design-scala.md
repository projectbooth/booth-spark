# Design note: Scala beside Python on booth-spark (ADR 0121)

Status: design, no code. Written 2026-10-19 for the coordinator's review. It answers the questions in
ADR 0121's first booth-spark step (agent brief `scala-support-plan.md`, section 7, step 1).

Everything below marked **measured** was run on this machine against the published runtime image
`ghcr.io/projectbooth/booth-spark-runtime:0.3.1@sha256:637daee8…`, the image every run's driver and
executors use. Nothing was run on the homelab. The transcripts are in the appendix.

## Answers in brief

1. **The runtime image can run source Scala as it is.** It already ships the Scala 2.13.17 compiler,
   reflect and library jars, and Spark's REPL (`spark-repl_2.13-4.1.3.jar`). `spark-shell` works
   (`spark.range(5).count()` is 5). Nothing needs to be added, so there is no licence or download
   cost, and the sha256-pinned-jar rule is untouched.
2. **A small Booth runner is the right mechanism**, and a prototype of it works. It runs a script
   statement by statement through the REPL's interpreter, with executors in separate JVMs loading
   the classes it compiles. It is compiled inside the image build by the image's own `scalac`.
   `spark-shell`'s own ways to run a script are unsafe:
   - `:load` reads line by line, and silently drops multi-line expressions;
   - `:paste` compiles the file as one unit, which breaks a case class used in a typed Dataset.
3. **The JAR path works locally**, as the module launches it (`spark-submit --class`, client mode). A
   scala-smoke equivalent read 723,445 rows from an Iceberg `nfl.plays` and wrote, read and dropped a
   scratch table. A wrong row count fails the run (exit 1), and so does a wrong `mainClass` (exit 101).
   The only thing stopping a browser submission is the UI form.
4. **Memory is the real cost.** A Scala interpreter holds about 280 MiB more heap and about 240 MiB more
   non-heap than a PySpark driver. Scala-flavoured drivers therefore need a larger memory overhead
   (640 MiB instead of 384 MiB). A default Scala session then fits the 4 GiB budget, and so does the
   NFL sizing (driver 1g, executor 1536m), with about 90 MiB to spare.

## 1. Can the runtime image run source Scala?

### What is in the image (measured)

| Needed for | Jar in `/opt/spark/jars` | Size | Licence |
|---|---|---|---|
| Compiler (what the interpreter uses) | `scala-compiler-2.13.17.jar` | 12.4 MB | Apache-2.0 |
| Reflection | `scala-reflect-2.13.17.jar` | 3.8 MB | Apache-2.0 |
| Library | `scala-library-2.13.17.jar` | 5.9 MB | Apache-2.0 |
| Spark's REPL (`SparkILoop`, the executor class loader) | `spark-repl_2.13-4.1.3.jar` | 37 KB | Apache-2.0 |
| Console line editing (not needed by a runner) | `jline-3.29.0-jdk8.jar`, `jline-2.14.6.jar` | 1.6 MB | BSD-3 |
| JSON for results and pipeline output | `jackson-module-scala_2.13`, `jackson-databind` (Spark's own) | | Apache-2.0 |

Also in the image: `/opt/spark/bin/spark-shell`, and a full JDK 21 (Temurin 21.0.11, with `javac` and
`jar`). All of this comes from the `apache/spark:4.1.3-scala2.13-java21-python3-ubuntu` base, which
`images/spark-runtime` already pins by digest.

**Cost of additions: none.** The one new artifact is Booth's own runner (section 3), about 30 KB of
classes compiled from source in this repository during the image build. The build stage uses the
base image's own `scala.tools.nsc.Main` (measured: it compiles the prototype with `-usejavacp`). No
new download means no new sha256 pin: the rule that every fetched jar is pinned and verified on each
build still holds, with nothing added to it.

### `spark-shell` transcript (measured)

`docker run --rm -i --entrypoint /opt/spark/bin/spark-shell <runtime image> --master 'local[2]'
--driver-memory 1g --driver-java-options -Duser.home=/tmp`, with the statements on stdin:

```text
Welcome to
      ____              __
     / __/__  ___ _____/ /__
    _\ \/ _ \/ _ `/ __/  '_/
   /___/ .__/\_,_/_/ /_/\_\   version 4.1.3
      /_/
Using Scala version 2.13.17 (OpenJDK 64-Bit Server VM, Java 21.0.11)
Spark context available as 'sc' (master = local[2], app id = local-1792415734225).
Spark session available as 'spark'.
scala> println(s"scala ${scala.util.Properties.versionNumberString}, spark ${spark.version}, java ...")
scala 2.13.17, spark 4.1.3, java 21.0.11
scala> spark.range(5).count()
val res1: Long = 5
scala> case class Play(season: Int, yards: Int)
class Play
scala> val ds = Seq(Play(2023, 5), Play(2023, -2), Play(2024, 12)).toDS()
val ds: org.apache.spark.sql.Dataset[Play] = [season: int, yards: int]
scala> ds.groupBy("season").sum("yards").orderBy("season").show()
+------+----------+
|season|sum(yards)|
+------+----------+
|  2023|         3|
|  2024|        12|
+------+----------+
scala> val n = sc.parallelize(1 to 1000, 4).map(_ * 2).sum()
val n: Double = 1001000.0
```

`-Duser.home=/tmp` is needed. The image's `spark` user has home `/nonexistent`, and without it
jline prints a stack trace after every line ("Failed to save history"). The runner doesn't use jline,
so this only matters for a person trying `spark-shell` by hand.

**Executors in separate JVMs** (measured, `--master local-cluster[2,1,1024]`): a function built from
a REPL-defined case class ran on two executor processes (pids 291 and 292, driver pid 10). Each fetched
the REPL's compiled classes from the driver over Spark's own RPC (`spark.repl.class.outputDir`).
This is the mechanism a Kubernetes executor uses, so no new port or route is involved.

### Two pitfalls found, which decide the runner's design

| Way of running a script | Measured result |
|---|---|
| `spark-shell` `:load file` (line by line) | **Silently wrong.** `val bySeason = plays` followed by `.groupBy("season")` on the next line ran as `val bySeason = plays`, and printed the raw rows. No error. |
| `spark-shell` `:paste file` (one unit) | Multi-line expressions are right. But a typed Dataset operation (`plays.map(p => …)`) on a case class defined in the same unit fails: `NullPointerException: Cannot invoke "Object.getClass()" because "obj" is null`, inside `collect`. The encoder can't reach the case class's outer instance. |
| One statement on one line, each its own request (`spark-shell` typed by hand) | Right. |
| **Booth runner prototype**: the parser splits the script into top-level statements, each statement is one request | **Right**, in both the multi-line and the case-class cases, with separate executor JVMs. |

## 2. The existing JAR path

### (a) Run locally (measured)

**The test job.** `scala-smoke.jar` is the homelab's test JAR (Scala 2.13.17). I wrote an equivalent,
`booth.smoke.ScalaSmoke`:
- It reads `<catalog>.nfl.plays`, with the catalog name taken from `BOOTH_LAKEHOUSE_CATALOG`, and
  expects 723,445 rows. It prints the count for each season.
- It then writes, reads and drops a scratch Iceberg table.
- Every line it prints starts `SCALA-SMOKE`.

**The build.** I compiled it **inside the runtime image** with the image's own `scalac` against
Spark's jars (`-release 17`), which gave a 4.4 KB JAR with no sbt. The table was a local Iceberg
catalog with 723,445 rows seeded into `lakehouse.nfl.plays`. The catalog was configured through
Spark conf, which is how `DataConf` gives a run its catalog.

**The run.** The module runs a JAR as `spark-submit --deploy-mode client --class <mainClass>
<app.jar> <args>` (`internal/runs/objects.go`), so I ran it the same way:

```text
SCALA-SMOKE scala 2.13.17 spark 4.1.3 catalog lakehouse
SCALA-SMOKE plays 723445 (expected 723445)
SCALA-SMOKE season 2016 80383
…
SCALA-SMOKE season 2024 80382
SCALA-SMOKE scratch sum 332833500 (expected 332833500)
SCALA-SMOKE PASS
```

**What works:**
- `main.jar` plus `mainClass` plus args, run as the module runs it.
- The Iceberg catalog and its SQL extensions reach a JAR exactly as they reach Python. They are
  Spark conf, not Python code, so a JAR needs nothing extra.
- `BOOTH_LAKEHOUSE_CATALOG` is set for every data run.
- A job that exits non-zero fails the run: a wrong expected count exits 1.
- A wrong `mainClass` fails it too: exit 101, with `Failed to load class booth.smoke.NoSuchClass.`
  in the driver log.
- The entry file is read through Storage's objects API (`/modules/storage/api/backends/<id>/objects/<path>`),
  not through s3a. A `local` backend (`local:apps/scala-smoke.jar`) should therefore work, provided
  Storage serves objects from it to the run's token. **Homelab-only check.**

**What does not work, or isn't tested:**
- **The browser can't submit a JAR.** The New application form sends only
  `{main: {inlinePython}}` (`web/src/pages.tsx`), as known from the homelab.
- **The API path needs a bearer token**, which a person in the browser doesn't have to hand.
- **`main.jar` requires `dataAccess.storage` on the install**, because the entry file comes from
  Storage. Without it the API answers 422: "main.python and main.jar read files from booth-storage,
  which this install doesn't offer".
- **No Integration job has ever run a JAR**, so CI doesn't cover this path today.
- **The JAR must target Scala 2.13 and Java bytecode 21 or lower.** A Scala 2.12 or Scala 3 JAR
  fails at class loading.
- **Entry files are capped at 256 MiB** (`MaxMainFileBytes`). Dependencies go inside the JAR (a fat
  JAR without Spark); there is no `--packages`.
- **What runs is whatever the file holds when the run starts.** Ruling 3's `sha256` closes this
  (section 4).

### (b) The entry-point choice in the UI, and the smallest first PR

The New application form gets an **Entry point** choice. The radio buttons are labelled
"PySpark code", "Scala code" and "JAR in Storage".

| Choice | Fields | Sent as |
|---|---|---|
| **PySpark code** (default; today's form) | the code box | `main.inlinePython` |
| **Scala code** (after the runner PR) | the code box, with Scala's starter text | `main.inlineScala` |
| **JAR in Storage** | backend (a select of the workspace's backends), path, main class, args (one per line), optional sha256 | `main.jar`, `mainClass`, `args`, (`main.jar.sha256` after that PR) |

**"JAR in Storage"** is shown only when `/ui/api/me` says `limits.dataAccess.storage`. Otherwise the
choice is shown disabled, with the API's reason. **"Scala code"** is shown only when the limits list
`scala` (section 4).

**Smallest first PR: "UI: submit a JAR from Storage"** (UI only, plus one Integration step):
- The radio buttons with **PySpark code** and **JAR in Storage**. For a JAR, the backend id, path, main
  class and args are plain inputs; there is no storage browser yet.
- The run page already shows a JAR's entry point (`backendId/path (mainClass)`).
- **Integration (the data job):** the job compiles a tiny Scala job with the runtime image's own
  `scalac`, uploads it to the stand-in Storage, submits it as `main.jar`, and expects its marker
  line. The same job submits a wrong `mainClass` and expects `failed`, as a control.
- No API change.

After that PR is released, you can submit `local:apps/scala-smoke.jar` from the browser: Entry point
"JAR in Storage", backend `local`, path `apps/scala-smoke.jar`, its main class, and data access
lakehouse.

## 3. Runner design

One small Scala program, `booth.runner`, lives in `images/spark-runtime/scala-runner/`. It is
compiled into `/opt/booth/booth-scala-runner.jar` in the image build and has two entry points:

- **`booth.runner.Script`** runs an application's Scala: `main.inlineScala`, or a `.scala` file
  from Storage.
- **`booth.runner.Session`** is a Scala-flavoured session's driver. It serves the same
  `/statements` protocol as `internal/runs/runner/session_runner.py`.

Neither changes anything for Python.

### How code executes

1. **The interpreter.** `scala.tools.nsc.interpreter.IMain`, the class `spark-shell` uses, with
   Spark's settings:
   - `-Yrepl-class-based`, so a line's state lives in serialisable class instances, not objects;
   - `-Yrepl-outdir <dir>`, the directory executors fetch classes from;
   - `spark.repl.class.outputDir` set on the SparkConf **before** the SparkSession exists.

   This is what `org.apache.spark.repl.Main` does. The prototype (appendix B) proved it with separate
   executor JVMs.
2. **The classloader and classpath.**
   - `spark-submit` loads the runner jar, and any `--jars`, through Spark's own classloader, not
     `java.class.path`. The compiler's classpath is therefore `java.class.path` (all of
     `/opt/spark/jars`: Spark, Iceberg, hadoop-aws, the AWS SDK, the PostgreSQL JDBC driver) plus the
     runner jar plus `spark.jars`.
   - The interpreter's parent classloader is the application's (`settings.embeddedDefaults`).
   - Without both, the interpreter can't see the runner (measured: `not found: value BoothScala`).
   - Data access adds no jars. Its connectors are already in `/opt/spark/jars`, and its credentials
     are files and environment variables.
3. **Statements, not lines.**
   - The script is parsed once with the interpreter's own parser (`newUnitParser(...).parseStats()`).
   - Each top-level statement becomes one request, sliced from the source by the statements' start
     offsets.
   - A parse error fails the run before any statement runs, with the compiler's message.
   - A session statement goes through the same parser, so a statement holding several top-level
     definitions behaves like the same lines typed one by one.
4. **What a script sees**, the same names Python code gets:
   - `spark` and `sc`, defined as `@transient val spark = booth.runner.Runtime.spark` so that closures
     never capture them;
   - `import spark.implicits._` and `import org.apache.spark.sql.functions._`;
   - `args: Array[String]` holding the run's `args`;
   - a `booth` object with:
     - `booth.catalog`: `BOOTH_LAKEHOUSE_CATALOG`, or `"lakehouse"`;
     - `booth.storage`: `BOOTH_STORAGE`, parsed into a `Seq` of `(backendId, path, access, root)`;
     - `booth.json(value)` and `booth.parseJson(text)`: Jackson with its Scala module, both already
       in the image.

   The catalog itself is Spark conf, as for Python, so `spark.table("lakehouse.nfl.plays")` works with
   nothing else.
5. **How a script ends.**
   - It **succeeds** when every statement does, and the run is `succeeded`.
   - A **compile error** or a **runtime exception** stops it. The runner prints
     `BOOTH-SCALA error: the statement at line N failed` after the compiler's message or the stack
     trace, stops the SparkSession and exits 1, so the run is `failed` with the driver's exit, as for
     Python.
   - `sys.exit(n)` behaves as in Python: the JVM exits with code n.
   - Measured with the prototype: a type error at line 3 ran lines 1 and 2, then failed with
     `error: type mismatch` and exit 1. A failing `require` at line 3 failed with the exception and
     exit 1.
   - The REPL's result echo (`val res1: Long = 5`) is off for scripts, so a script prints only what it
     prints.

### Session statements and their JSON

`booth.runner.Session` is the JVM twin of `session_runner.py`:
- the same port (8998);
- the same bearer, compared in constant time (`MessageDigest.isEqual`);
- the same `/healthz`, `POST /statements` and `GET /statements/<id>`;
- one statement at a time on a worker thread, with `sc.setJobGroup` per statement;
- the same caps: 1,000 rows, 1 MiB of output, 64 KiB of error, 512 KiB of body.

It uses the JDK's `com.sun.net.httpserver`, so it needs no dependency.

**`kind: sql`** gives the same output as the Python runner, field for field: `{type: "table",
columns: [{name, type: simpleString}], rows, truncated}`, with rows halved until the JSON fits 1 MiB.
The value mapping has to match Python's `json.dumps(default=str)`:

| Values | JSON |
|---|---|
| numbers and booleans | native |
| decimals | strings, as Python's `str(Decimal)` |
| dates and timestamps | strings in Python's `str()` form (`2024-01-01 00:00:00`) |
| binary | Python's `str()` of the bytes (`bytearray(b'…')`), kept for equality, worth revisiting |
| null | `null` |
| arrays | JSON arrays |
| structs | JSON arrays of their fields (PySpark's `Row` is a tuple) |
| maps | JSON objects |

Integration proves the match: the same fixture query, covering every type, must give identical JSON
in both flavours.

**`kind: scala`** gives `{type: "text", stdout, stderr, truncated}`, the shape `python` gives:
- `stdout` is what the statement printed, captured with `Console.withOut` and a `System.out` swap for
  the worker thread's run, followed by the interpreter's result echo (`val n: Double = 1001000.0`), as
  `spark-shell` shows it.
- `stderr` is the statement's own `System.err` output. Spark's log4j output goes to the driver log,
  as for Python.

Recommended addition (question Q4): when a statement's last expression is a DataFrame or Dataset, the
output is a `table`, built the same way as for `sql`. That is what a notebook kernel wants.

**Errors** are `state: "error"`, with `error` set to:
- the compiler's messages for a compile error, with lines counted from the statement's start
  (`error: type mismatch; found: String("x") required: Int`, followed by the source line and a caret);
- the exception's stack trace for a runtime failure, with the REPL's generated wrapper frames
  (`$line12.$read$$iw…`) removed and causes kept, capped at 64 KiB, the cap the Python traceback has.

The session goes on after an error, as it does for Python.

### Logs, and the pipeline's output line

A script's `println` goes to the driver's stdout, which is the driver log. The same holds for the log
cursor, the kept tail and retention, as for Python.

The pipeline prelude belongs to booth-pipeline: it is Python text that its Spark client builds
around a task's code (`_spark_client.py`). For Scala, booth-pipeline generates a Scala prelude and
sends prelude plus task as `main.inlineScala`. booth-spark's part is to guarantee what that prelude
needs:
- `booth.parseJson` for the task's params and inputs;
- `booth.json` to encode the return value;
- `java.util.Base64`;
- `println` of one line, followed by `Console.flush()`, reaching the driver log unchanged.

The prelude ends with

```scala
println("BOOTH-PIPELINE-OUTPUT " + java.util.Base64.getEncoder.encodeToString(booth.json(Map("output" -> run(ctx))).getBytes("UTF-8")))
```

That is exactly the line the Python prelude prints, so booth-pipeline's reader (`line.startswith(OUTPUT)`
in `_spark_client.py`) needs no change.

## 4. API changes and compatibility

All additive within `/v1`. Nothing that works today changes meaning.

| Where | Addition | Notes |
|---|---|---|
| `POST /v1/applications` `main` | `inlineScala` (string, at most 256 KiB, the same as `inlinePython`) | Exactly one of `inlinePython`, `inlineScala`, `python`, `scala`, `jar`. `mainClass` stays JAR-only. The record keeps `inlineScalaBytes` and `inlineScalaSha256`, as for Python. |
| `main` | `scala: {backendId, path, sha256?}` (a `.scala` file in Storage) | Run by `booth.runner.Script`. Needs `dataAccess.storage`, as `python` and `jar` do. |
| `main.python`, `main.jar`, `main.scala` | optional `sha256` (64 lowercase hex characters) | See "sha256" below. |
| `POST /v1/sessions` | `language: "python" \| "scala"` (default `python`) | The session's flavour, fixed at creation. Shown back on `GET`. |
| `POST /v1/sessions/{id}/statements` `kind` | `scala` | In a Scala session `kind` is `sql` or `scala`; in a Python session, `sql` or `python`. The wrong one gets 400 `invalid`: "this is a Scala session; kind python needs a session started with language python". The `statements.kind` check constraint gains `scala` (a migration). |
| `GET /v1/limits` (and `/ui/api/me`'s limits) | `languages: ["python", "scala"]`, `entryPoints: ["inlinePython", "inlineScala", "python", "scala", "jar"]` | `entryPoints` lists the Storage entries only when `dataAccess.storage` is on. `languages` lists `scala` only when the chart's `runs.scala.enabled` is true (default true). The chart refuses `runs.scala.enabled` with an image that lacks the runner, as it refuses data access with plain apache/spark. |

**sha256 semantics:**
- **Checked by the run's agent** (the gate init container that already copies the entry file in,
  `internal/agent fetchMain`), while it streams the file, before the file is renamed into place.
  That is the moment and the bytes Spark will run, so nothing can change between the check and the
  read.
- **A mismatch fails the run** before Spark starts. The run ends `failed` with the reason "the entry
  point's sha256 is `<actual>`, not the `<expected>` main.jar.sha256 asked for (booth-storage
  `<backend>/<path>`)". The driver never starts.
- **At submit:**
  - a malformed value is 400 `invalid`;
  - `sha256` on `inlinePython` or `inlineScala` is 400 too, since inline code has its own recorded hash;
  - nothing is fetched at submit.
- **The log** names the run, the backend and path, and both hashes. It never logs file contents.
- **The record** keeps `sha256` in the spec after retention: a hash is metadata, not content.
- **Without `sha256`, behaviour is as today.**

## 5. Security and retention

**Same trust class as Python.** User Scala is arbitrary JVM code, and user Python already reaches the
same JVM through py4j (`spark._jvm`). The interpreter's classpath is the jars every Python run's JVM
already has. There is no new library, credential or file.

**What is different from Python, and why it is fine:**

| Difference | Why it is fine |
|---|---|
| The compiler's classpath | It is the driver's own classpath, which user Python already reaches through py4j. |
| Generated classes on disk | They go in a directory under the driver container's `/tmp`, the existing scratch emptyDir. They are gone with the pod, and the namespace is deleted when the run ends. |
| Executors fetch those classes from the driver | They use Spark's authenticated RPC (`spark.authenticate` is already on), on the existing driver port. |
| REPL state persists across statements | Python's session scope persists the same way. It lives only in the driver's memory and ends with the session. |

**No new egress, no new API rights.**
- The NetworkPolicies, the fence and driver ClusterRoles, the data-access leases and ADR 0119's
  ownership and ceilings are unchanged. The runner opens no connection the Python runner doesn't.
- A Scala session's port is the same NetworkPolicy-guarded port, and requests to it need the same
  per-session bearer.

**Retention** (ADR 0110 step 5) clears Scala content the same way:
- `ClearContent` today removes `spec.main.inlinePython`. The change adds `spec.main.inlineScala` to
  the same statement and keeps `inlineScalaBytes` and `inlineScalaSha256`, as for Python.
- Statements' `code`, `output` and `error` are cleared whatever their `kind`.
- `log_tail` is cleared as before.
- The ConfigMap carrying the code (key `main.scala` beside `main.py`) and the generated classes go
  with the namespace.
- A test asserts that an ended Scala application and a Scala session's statements are cleared, beside
  the Python ones.

## 6. Memory

Python's driver pod today is the heap plus `max(0.4 × heap, 384 MiB)` (`PodMemoryMi`). That is
896 MiB for the default 512m driver, and 1433 MiB at 1g.

Measured, one JVM with Spark in local mode:

| Driver | Heap live after GC | Metaspace | Code cache | RSS |
|---|---|---|---|---|
| PySpark (`spark-submit`, 1g heap) | 106 MiB | | | 500 MiB JVM, plus 41 MiB Python |
| Scala REPL, 512m heap, at start | 276 MiB | 102 MiB | 38 MiB | 744 MiB |
| …after the first job | 284 MiB | 141 MiB | 40 MiB | 817 MiB |
| …after 200 statements (each defines a case class and runs a Dataset) | 344 MiB | 171 MiB | 68 MiB | **898 MiB, at the 896 MiB pod limit** |
| Scala REPL, 768m heap, after 200 statements | 346 MiB | 171 MiB | 82 MiB | 1132 MiB, at the 1152 MiB limit |

**What the numbers say:**
- The interpreter holds about 170 to 240 MiB more heap than PySpark, and about 240 MiB of metaspace
  and code cache.
- Metaspace grows about 0.15 MiB per statement, as each statement's wrapper classes are loaded.
- A 512m heap still ran all 200 statements, but the container touched its limit. On Kubernetes that
  is an OOM kill waiting to happen, so today's 384 MiB overhead is not enough for a Scala driver.

**Recommendation:** for Scala-flavoured drivers only (`inlineScala`, `main.scala`, Scala sessions):
- **Overhead.** The driver's overhead becomes `max(0.4 × heap, 640 MiB)`, giving a 1152 MiB pod at
  the default 512m.
- **Fixed JVM caps** (`-XX:MaxMetaspaceSize=320m -XX:ReservedCodeCacheSize=128m`). A very long session
  then fails cleanly with `OutOfMemoryError: Metaspace` rather than being killed by the kernel. At
  0.15 MiB per statement that is roughly 1,000 statements, which a session's 12-hour maximum lifetime
  rarely reaches. The operations doc will say so.
- **Unchanged:**
  - Executors: they run no compiler and only load the generated classes.
  - `main.jar` runs: no interpreter.
  - Every Python run.

**Budget check** (4096 MiB, with sidecars at 32 MiB plus 64 MiB per lease per pod):

| Run | Driver | Executors | Sidecars | Total |
|---|---|---|---|---|
| Default Scala session, no data | 1152 | 2 × 896 | | 2944 MiB, fits |
| Scala run at the NFL sizing (driver 1g, 1 executor of 1536m, lakehouse) | 1024 + 640 = 1664 | 2150 | 2 × 96 | 4006 MiB, fits with 90 MiB to spare |
| The same as PySpark, for comparison | 1433 | 2150 | 192 | 3775 MiB |

At the NFL sizing a second lease (lakehouse plus one storage location) would add 128 MiB and no
longer fit. Admission says so with its usual `at_capacity` reason, and a 1g driver with a 1280m
executor fits.

## 7. Test plan and PR breakdown

**What CI can prove:**
- **Unit (Go):**
  - validation of `inlineScala`, `scala`, `sha256` and `language`;
  - kind and flavour rules for statements;
  - the Scala sizing in `FootprintMi`;
  - driver args for each entry point;
  - retention clearing `inlineScala`;
  - the limits fields;
  - the agent's sha256 check: a match passes, a mismatch fails with the reason, and the file is never
    renamed into place on a mismatch.
- **Runner self-test (in the image build job):** `spark-submit --class booth.runner.SelfTest` in the
  freshly built runtime image, on `local-cluster[1,1,1024]`, so executors are separate JVMs. It runs:
  - the statement splitter cases: multi-line chains, a case class followed by a typed `map`, a
    comment-only script, a parse error;
  - the error shapes for a compile error and a runtime exception;
  - the result echo, the 1,000-row and 1 MiB caps, and SQL JSON equal to a stored golden file.
- **Integration with a real core (real-core job):**
  - an `inlineScala` application and a Scala session through the gateway, with statements in order,
    state kept and an error that doesn't end the session;
  - `kind` refused in the wrong flavour;
  - retention clearing Scala content;
  - `/v1/limits` listing `scala`.
- **Integration data job:**
  - an `inlineScala` run reading and writing an Iceberg table with `dataAccess.lakehouse` (ADR 0121's
    required proof) and reading a storage location;
  - a `main.jar` run built by the image's `scalac`;
  - a `sha256` mismatch failing the run, with the matching hash as a positive control;
  - the same SQL fixture giving identical JSON from a Python and a Scala session.

**Homelab-only:**
- `local:apps/scala-smoke.jar` from the browser;
- a Scala run totalling `nfl.plays` at 723,445;
- the NFL-sizing memory fit on real nodes;
- pipeline and notebook use, which is the other modules' gate.

**PRs, in recommended order** (each its own PR, Integration green on its exact head):

| # | PR | Size | Gives |
|---|---|---|---|
| 1 | UI: entry point "JAR in Storage" plus the data job's JAR run | S | Browser JAR submission (the homelab smoke) |
| 2 | `sha256` on entry files (API, agent check, docs) | S | Ruling 3; pinned JARs for later pipeline JAR tasks |
| 3 | Runtime image: `booth-scala-runner.jar` built in the image (Script and SelfTest), and its self-test in CI. **No API yet.** | M | The mechanism, proven in CI |
| 4 | `main.inlineScala` and `main.scala`, Scala sizing, limits `languages` and `entryPoints`, retention, the UI's "Scala code" choice; real-core and data-job tests | M | Scala batch runs, and what booth-pipeline needs (unblocks ADR 0121 step 3) |
| 5 | Scala sessions (`language`, `kind: scala`, `booth.runner.Session`, the SQL JSON golden test, migration) | M–L | Interactive Scala (unblocks the notebooks kernel, step 4) |
| 6 | Optional: a DataFrame value as a `table` output (Q4) | S | Nicer notebook cells |

**The runtime image is republished in PR 3,** and the chart's `runs.image` pin moves to it. That is
the first move of that pin since 0.1.0. It is a deliberate step in the release that carries PR 3, and
the homelab picks it up with that chart version.

## 8. Risks and open questions, each with my recommended default

| # | Question or risk | Recommended default |
|---|---|---|
| Q1 | Scala 2.13 vs "the Spark 4.1 default" | **Scala 2.13.** It is not a choice: Spark 4.x is built only for Scala 2.13 (2.12 was dropped in 4.0, and Scala 3 is unsupported), and the image's REPL is 2.13.17. The docs will say: JARs must be built for Scala 2.13, against Spark 4.1.3 marked `provided`, and for Java 17 or 21 bytecode. |
| Q2 | May a Scala session's state persist across statements, as Python's does? | **Yes**, the same as Python, which is what a console or notebook user expects. The cost is metaspace growth (section 6): capped, and stated in the docs. Restarting a session is the reset. |
| Q3 | Are `sql` statements available in both flavours? | **Yes**, with JSON identical to the Python runner's, proven by the golden test. A notebook's SQL kernel then works against either flavour. |
| Q4 | A Scala statement whose value is a DataFrame: text echo or a table? | **A table** (as `sql` gives), in PR 6, so the notebook kernel shows rows. Until then, text. |
| Q5 | Scala script semantics: statement by statement (as typed in `spark-shell`), or compiled as a whole program? | **Statement by statement, split by the parser.** It is the only form measured to handle both multi-line expressions and case classes in typed Datasets. A forward reference across statements (using a `def` before its definition) doesn't work, as in `spark-shell`. The docs will say so. |
| Q6 | A Scala script that declares `object Main { def main(...) }` | **It isn't run automatically.** A script is statements; a `main` method is called only if the script calls it. A program in that shape belongs in a JAR. The UI's starter text shows the script style. |
| Q7 | `sha256` on inline code | **Refused (400).** Inline code is pinned by being inline, and its hash is recorded already. |
| Q8 | Scala sizing: a separate chart value, or fixed? | **Fixed** (640 MiB minimum overhead, metaspace and code-cache caps) in v1, with a chart value only if the homelab shows a need. Python sizing does not change. |
| R1 | `IMain`'s API is internal to Scala and Spark and may change between versions. | The runner is compiled in the image build against the image's own jars, so a mismatch fails the image build, not a run. Spark and Scala move only when the base image digest moves, and CI's self-test runs on every build. |
| R2 | Statement splitting gets an edge case wrong (an expression that only makes sense joined with the next). | The parser decides, not lines. The self-test carries the cases found here, and new ones are added as they appear. |
| R3 | No statement cancel, as for Python. A runaway Scala statement holds the session until it is deleted or hits its maximum lifetime. | Accept, as for Python (operations doc, "Sessions: known limits"). `setJobGroup` is set so a future cancel can reach the Spark jobs. |
| R4 | A retried `POST …/statements` runs twice (no `Idempotency-Key` there) | Already on the known-limits list; the same applies to Scala. |
| R5 | The memory fit at the NFL sizing is about 90 MiB. | Admission refuses with its reason rather than starting a run that can't fit. The homelab smoke runs at that sizing. |

## Appendix A: how the measurements were taken

All of these used the runtime image `0.3.1@sha256:637daee8…` locally, with Docker memory limits
standing in for pod limits:
- **The jar inventory:** `ls -la /opt/spark/jars`.
- **The `spark-shell` transcript:** statements piped on stdin.
- **Executors in separate JVMs:** `--master local-cluster[n,1,1024]`.
- **The memory table:** `MemoryPoolMXBean`, `Runtime` and `/proc/self/status`, read from inside the
  REPL. The 200-statement script alternates `case class R<i>(a: Int, b: String)` with
  `Seq(R<i>(…)).toDS().map(…).collect()`.
- **The PySpark baseline:** a `spark-submit` script reading `/proc/<jvm pid>/status`.
- **The JAR:** compiled in the image with `java -cp "/opt/spark/jars/*" scala.tools.nsc.Main
  -usejavacp -release 17`, and run with `spark-submit --deploy-mode client --class
  booth.smoke.ScalaSmoke`, against a Hadoop Iceberg catalog named `lakehouse` seeded with 723,445
  rows.

## Appendix B: the runner prototype (scratch, not in this repository)

These are the essential lines of the prototype that produced section 3's results. It was compiled in
the image into a 6.6 KB jar and run with `spark-submit --class BoothScala booth-scala.jar
script.scala` on `local-cluster[1,1,1024]`.

```scala
val outDir = Files.createTempDirectory("booth-scala-classes")
val conf = new SparkConf().set("spark.repl.class.outputDir", outDir.toString)
val settings = new Settings(msg => System.err.println("BOOTH-SCALA settings: " + msg))
settings.processArguments(List("-Yrepl-class-based", "-Yrepl-outdir", outDir.toString), processAll = true)
settings.usejavacp.value = true
val own = getClass.getProtectionDomain.getCodeSource.getLocation.getPath
settings.classpath.value = (own +: sparkJars).mkString(java.io.File.pathSeparator)
settings.embeddedDefaults(getClass.getClassLoader)
val reporter = new ReplReporterImpl(settings)
val intp = new IMain(settings, reporter)
intp.initializeCompiler()
spark = SparkSession.builder().config(conf).getOrCreate()
Seq("@transient val spark = BoothScala.spark", "@transient val sc = spark.sparkContext",
    "import spark.implicits._", "import org.apache.spark.sql.functions._").foreach(intp.interpret)
if (reporter.printResults) reporter.togglePrintResults()
// statements: parse once, slice the source at each top-level statement's start offset
import intp.global._
val run = new Run
val unit = newCompilationUnit(src, "<script>")
val trees = newUnitParser(unit).parseStats()
// …then intp.interpret(each slice); Results.Error or Incomplete stops the script with exit 1
```

Results:

| Script | Output | Exit |
|---|---|---|
| multi-line chain, case class with a typed `map` | `LOAD [2023,3] [2024,12]` and `LOAD udf 10,-4,24` | 0 |
| a type error at line 3 | line 1 and 2 ran; then `error: type mismatch;` and `BOOTH-SCALA error: the statement at line 3 failed` | 1 |
| `require` failing at line 3 | `java.lang.IllegalArgumentException: requirement failed: expected 11 rows, got 10` | 1 |
