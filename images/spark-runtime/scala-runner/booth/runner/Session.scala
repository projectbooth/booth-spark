package booth.runner

import java.io.{ByteArrayOutputStream, OutputStreamWriter, PrintStream, PrintWriter, Writer}
import java.math.{BigDecimal => JBigDecimal}
import java.net.InetSocketAddress
import java.nio.charset.StandardCharsets
import java.nio.file.{Files, Paths}
import java.security.MessageDigest
import java.util.concurrent.{Executors, LinkedBlockingQueue}

import scala.collection.mutable

import com.fasterxml.jackson.databind.ObjectMapper
import com.fasterxml.jackson.module.scala.{ClassTagExtensions, DefaultScalaModule}
import com.sun.net.httpserver.{HttpExchange, HttpServer}

import org.apache.spark.sql.{Row, SparkSession}

/** A Scala-flavoured session's driver (docs/design-scala.md section 3; ADR 0121 PR 5): the JVM
  * twin of runner/session_runner.py, with the same protocol, the same bearer, the same caps.
  *
  *   POST /statements      {"id", "kind": "sql"|"scala", "code"}   -> 202 (queued)
  *   GET  /statements/<id>                                         -> the statement's state and output
  *   GET  /healthz                                                 -> 200 once the SparkSession exists
  *
  * Statements run one at a time, in order, in one interpreter: what one defines, the next can use
  * (ADR 0121 ruling 2). A statement that fails is "error" with the compiler's message or the
  * exception; the session goes on. sql output is the Python runner's, value for value (ruling 3).
  */
object Session {
  val Port = 8998
  val MaxRows = 1000
  val MaxOutput: Int = 1 << 20
  val MaxError: Int = 64 << 10
  val MaxBody: Int = 512 << 10

  private val mapper = {
    val m = new ObjectMapper() with ClassTagExtensions
    m.registerModule(DefaultScalaModule)
    m
  }

  private final class Statement(val id: String, val kind: String, val code: String) {
    @volatile var state: String = "waiting"
    @volatile var output: Any = null
    @volatile var error: String = ""
  }

  /** The interpreter's output goes wherever the running statement's capture is. */
  private final class Switch extends Writer {
    @volatile var target: Writer = new OutputStreamWriter(System.out, StandardCharsets.UTF_8)
    def write(cbuf: Array[Char], off: Int, len: Int): Unit = target.write(cbuf, off, len)
    def flush(): Unit = target.flush()
    def close(): Unit = ()
  }

  def main(argv: Array[String]): Unit = {
    val tokenFile = sys.env.getOrElse("BOOTH_SESSION_TOKEN_FILE", sys.error("BOOTH_SESSION_TOKEN_FILE is not set"))
    val token = new String(Files.readAllBytes(Paths.get(tokenFile)), StandardCharsets.UTF_8).trim
    if (token.isEmpty) sys.error("booth-spark Scala session runner: empty session token")

    val classes = Interp.classDir()
    val switch = new Switch
    val interp = new Interp(classes, new PrintWriter(switch, true), echo = true)
    val spark = SparkSession.builder().config(Interp.sparkConf(classes)).getOrCreate()
    Runtime.spark = spark
    interp.start()
    val sc = spark.sparkContext

    val statements = mutable.LinkedHashMap.empty[String, Statement]
    val work = new LinkedBlockingQueue[Statement]()

    val worker = new Thread(() => {
      while (true) {
        val st = work.take()
        st.state = "running"
        sc.setJobGroup(st.id, "booth statement " + st.id, interruptOnCancel = true)
        try {
          if (st.kind == "sql") {
            st.output = sql(spark, st.code)
            st.state = "available"
          } else {
            val (ok, text, truncated) = scalaStatement(interp, switch, st.code)
            if (ok) {
              st.output = Map("type" -> "text", "stdout" -> text, "stderr" -> "", "truncated" -> truncated)
              st.state = "available"
            } else {
              st.error = cap(text, MaxError)._1
              st.state = "error"
            }
          }
        } catch {
          case t: Throwable => // a failed statement never ends the session
            st.error = cap(stackTrace(t), MaxError)._1
            st.state = "error"
        } finally sc.clearJobGroup()
      }
    }, "booth-statements")
    worker.setDaemon(true)
    worker.start()

    val server = HttpServer.create(new InetSocketAddress("0.0.0.0", Port), 16)
    server.setExecutor(Executors.newFixedThreadPool(4))
    server.createContext("/", (ex: HttpExchange) => {
      try {
        System.err.println(s"session-runner: ${ex.getRequestMethod} ${ex.getRequestURI.getPath}") // never a body
        val got = Option(ex.getRequestHeaders.getFirst("Authorization")).getOrElse("")
        val authorized = got.startsWith("Bearer ") &&
          MessageDigest.isEqual(got.drop(7).trim.getBytes(StandardCharsets.UTF_8), token.getBytes(StandardCharsets.UTF_8))
        val path = ex.getRequestURI.getPath
        if (!authorized) reply(ex, 401, Map("error" -> "unauthorized"))
        else (ex.getRequestMethod, path) match {
          case ("GET", "/healthz") => reply(ex, 200, Map("status" -> "ok", "applicationId" -> sc.applicationId))
          case ("GET", p) if p.startsWith("/statements/") =>
            val sid = p.stripPrefix("/statements/")
            statements.synchronized(statements.get(sid)) match {
              case None     => reply(ex, 404, Map("error" -> "unknown statement"))
              case Some(st) => reply(ex, 200, Map("id" -> st.id, "state" -> st.state, "output" -> st.output, "error" -> st.error))
            }
          case ("POST", "/statements") =>
            val n = Option(ex.getRequestHeaders.getFirst("Content-Length")).flatMap(_.toIntOption).getOrElse(0)
            if (n <= 0 || n > MaxBody) reply(ex, 400, Map("error" -> "body size"))
            else {
              val req = scala.util.Try(mapper.readValue[Map[String, Any]](ex.getRequestBody.readNBytes(n))).toOption
              val parsed = req.flatMap { m =>
                (m.get("id"), m.get("kind"), m.get("code")) match {
                  case (Some(id: String), Some(kind: String), Some(code: String)) if kind == "sql" || kind == "scala" => Some((id, kind, code))
                  case _ => None
                }
              }
              parsed match {
                case None => reply(ex, 400, Map("error" -> "need id, kind (sql or scala) and code"))
                case Some((id, kind, code)) =>
                  val (st, fresh) = statements.synchronized {
                    statements.get(id) match {
                      case Some(existing) => (existing, false) // a backend retrying after a restart: already queued
                      case None =>
                        val s = new Statement(id, kind, code)
                        statements(id) = s
                        (s, true)
                    }
                  }
                  if (fresh) work.put(st)
                  reply(ex, 202, Map("id" -> st.id, "state" -> st.state))
              }
            }
          case _ => reply(ex, 404, Map("error" -> "not found"))
        }
      } catch {
        case t: Throwable =>
          System.err.println("session-runner: " + t)
          scala.util.Try(reply(ex, 500, Map("error" -> "internal")))
      } finally ex.close()
    })
    server.start()
    println(s"booth-spark Scala session runner: ready on port $Port (application ${sc.applicationId})")
    new java.util.concurrent.CountDownLatch(1).await() // until the driver is stopped (the namespace is deleted)
  }

  private def reply(ex: HttpExchange, code: Int, body: Any): Unit = {
    val data = mapper.writeValueAsBytes(body)
    ex.getResponseHeaders.set("Content-Type", "application/json")
    ex.sendResponseHeaders(code, data.length.toLong)
    ex.getResponseBody.write(data)
  }

  /** One Scala statement: everything it prints, the interpreter's echo of its results, and on
    * failure the compiler's message or the exception, all captured in order.
    */
  private[runner] def scalaStatement(interp: Interp, switch: Switch, code: String): (Boolean, String, Boolean) = {
    val buf = new ByteArrayOutputStream()
    val ps = new PrintStream(buf, true, StandardCharsets.UTF_8)
    val w = new OutputStreamWriter(ps, StandardCharsets.UTF_8)
    val prevOut = System.out
    switch.target = w
    System.setOut(ps)
    val outcome =
      try Console.withOut(ps)(Console.withErr(ps)(interp.run(code)))
      finally {
        w.flush()
        System.setOut(prevOut)
        switch.target = new OutputStreamWriter(prevOut, StandardCharsets.UTF_8)
      }
    val (text, truncated) = cap(buf.toString(StandardCharsets.UTF_8), MaxOutput)
    outcome match {
      case Outcome.Done          => (true, text, truncated)
      case Outcome.Failed(line)  => (false, text + s"\n(the statement at line $line failed)", false)
      case Outcome.Unparsable(m) => (false, text + "\n" + m, false)
    }
  }

  /** A sql statement's table, as the Python runner gives it: at most MaxRows rows, halved until the
    * JSON fits MaxOutput. Values follow Python's json.dumps(default=str) of what PySpark collects
    * (sqlValue), so both flavours give the same JSON.
    */
  private[runner] def sql(spark: SparkSession, code: String): Map[String, Any] = {
    val df = spark.sql(code)
    val rows = df.limit(MaxRows + 1).collect()
    val fields = df.schema.fields
    var out = Map[String, Any](
      "type" -> "table",
      "columns" -> fields.toList.map(f => Map("name" -> f.name, "type" -> f.dataType.simpleString)),
      "rows" -> rows.take(MaxRows).toList.map(r => fields.indices.toList.map(i => sqlValue(r.get(i)))),
      "truncated" -> (rows.length > MaxRows)
    )
    if (mapper.writeValueAsBytes(out).length > MaxOutput) {
      var kept = out("rows").asInstanceOf[List[Any]]
      while (kept.nonEmpty && mapper.writeValueAsBytes(out.updated("rows", kept)).length > MaxOutput) kept = kept.take(kept.size / 2)
      out = out.updated("rows", kept).updated("truncated", true)
    }
    out
  }

  /** One collected value as the Python runner's JSON has it. */
  private[runner] def sqlValue(v: Any): Any = v match {
    case null                                          => null
    case b: java.lang.Boolean                          => b
    case n @ (_: java.lang.Byte | _: java.lang.Short | _: java.lang.Integer | _: java.lang.Long) => n
    case f: java.lang.Float                            => f.toDouble
    case d: java.lang.Double                           => d
    case d: JBigDecimal                                => pyDecimal(d) // Python: str(Decimal)
    case s: String                                     => s
    case d: java.sql.Date                              => d.toLocalDate.toString // str(date): 2024-01-31
    case d: java.time.LocalDate                        => d.toString
    case t: java.sql.Timestamp                         => pyDateTime(t.toLocalDateTime) // str(datetime), naive local
    case t: java.time.LocalDateTime                    => pyDateTime(t)
    case b: Array[Byte]                                => "b'" + pyBytes(b) + "'" // str(bytes): PySpark 4 collects bytes
    case r: Row                                        => r.toSeq.toList.map(sqlValue) // a Row is a tuple: a list
    case m: scala.collection.Map[_, _]                 => m.map { case (k, x) => String.valueOf(sqlValue(k)) -> sqlValue(x) }.toMap
    case s: scala.collection.Seq[_]                    => s.toList.map(sqlValue)
    case other                                         => other.toString
  }

  // Python keeps an exponent only for a negative scale: str(Decimal('1E+3')).
  private def pyDecimal(d: JBigDecimal): String = if (d.scale < 0) d.toString else d.toPlainString

  private def pyDateTime(t: java.time.LocalDateTime): String = {
    val base = f"${t.getYear}%04d-${t.getMonthValue}%02d-${t.getDayOfMonth}%02d ${t.getHour}%02d:${t.getMinute}%02d:${t.getSecond}%02d"
    val micros = t.getNano / 1000
    if (micros == 0) base else f"$base.$micros%06d"
  }

  private def pyBytes(b: Array[Byte]): String = b.map { x =>
    val c = x & 0xff
    c match {
      case '\\'                       => "\\\\"
      case '\''                       => "\\'"
      case '\t'                       => "\\t"
      case '\n'                       => "\\n"
      case '\r'                       => "\\r"
      case _ if c >= 0x20 && c < 0x7f => c.toChar.toString
      case _                          => f"\\x$c%02x"
    }
  }.mkString

  private[runner] def cap(s: String, n: Int): (String, Boolean) = {
    val b = s.getBytes(StandardCharsets.UTF_8)
    if (b.length <= n) (s, false)
    else (new String(java.util.Arrays.copyOf(b, n), StandardCharsets.UTF_8).replace("�", ""), true)
  }

  private def stackTrace(t: Throwable): String = {
    val sw = new java.io.StringWriter
    t.printStackTrace(new PrintWriter(sw))
    sw.toString
  }
}
