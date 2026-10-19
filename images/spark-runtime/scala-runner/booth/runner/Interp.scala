// booth-spark's Scala runner (docs/design-scala.md section 3; ADR 0121): Scala source runs in a
// Spark driver through the Scala REPL's interpreter, the classes spark-shell uses, as the image's
// own Scala 2.13 and Spark ship them. Compiled in the runtime image's build by the image's scalac.
package booth.runner

import java.io.{File, PrintWriter}
import java.nio.file.{Files, Path}

import scala.tools.nsc.Settings
import scala.tools.nsc.interpreter.{IMain, Results}
import scala.tools.nsc.interpreter.shell.ReplReporterImpl

import org.apache.spark.SparkConf

/** One top-level statement of a script: its first line (1-based) and its source. */
final case class Statement(line: Int, code: String)

/** How running some source ended. */
sealed trait Outcome
object Outcome {
  case object Done extends Outcome
  /** The statement at line failed: it didn't compile, or it threw (the interpreter has said which). */
  final case class Failed(line: Int) extends Outcome
  /** The source doesn't parse; nothing ran. */
  final case class Unparsable(message: String) extends Outcome
}

object Interp {

  /** Where the interpreter writes the classes it compiles. Executors fetch them from the driver
    * over Spark's own RPC, which spark.repl.class.outputDir turns on: it must be in the SparkConf
    * before the SparkSession exists, as Spark's repl Main sets it.
    */
  def classDir(): Path = Files.createTempDirectory("booth-scala-classes")

  def sparkConf(classes: Path): SparkConf = new SparkConf().set("spark.repl.class.outputDir", classes.toString)

  /** The names a script sees, as Python code gets them: spark, sc, args and booth, and the imports
    * spark-shell makes. Everything is spelled from _root_, since `booth` is also a value here.
    * spark and sc are @transient so a closure never captures them.
    */
  val prelude: Seq[String] = Seq(
    "@transient val spark = _root_.booth.runner.Runtime.spark",
    "@transient val sc = spark.sparkContext",
    "val args: Array[String] = _root_.booth.runner.Runtime.args",
    "val booth = _root_.booth.runner.Booth",
    "import spark.implicits._",
    "import org.apache.spark.sql.functions._"
  )
}

/** An interpreter for one script or one session. Output (results, compiler messages, a failed
  * statement's exception) goes to out. Not thread-safe: one statement at a time.
  */
final class Interp(classes: Path, out: PrintWriter, echo: Boolean) {
  private val settings = {
    val s = new Settings(msg => out.println("booth-scala: " + msg))
    s.processArguments(List("-Yrepl-class-based", "-Yrepl-outdir", classes.toString), processAll = true)
    s.usejavacp.value = true
    // spark-submit loads this jar (and --jars) through its own classloader, not java.class.path:
    // the compiler needs them on its classpath, and the interpreter's classes need that loader as
    // their parent, or a script can't see booth.runner (measured with the prototype).
    val own = new File(getClass.getProtectionDomain.getCodeSource.getLocation.toURI).getPath
    val jars = sys.props.get("spark.jars").toSeq.flatMap(_.split(',')).map(_.trim).filter(_.nonEmpty).map(_.stripPrefix("file:"))
    s.classpath.value = (own +: jars).distinct.mkString(File.pathSeparator)
    s.embeddedDefaults(getClass.getClassLoader)
    s
  }
  private val rep = new ReplReporterImpl(settings, out)
  private val intp = new IMain(settings, rep)
  require(intp.initializeCompiler(), "the Scala compiler didn't start")

  /** Defines the prelude's names, without echo. */
  def start(): Unit = rep.withoutPrintingResults {
    Interp.prelude.foreach { p =>
      if (intp.interpret(p) != Results.Success) throw new IllegalStateException("booth-scala: the prelude failed at: " + p)
    }
  }
  if (!echo && rep.printResults) rep.togglePrintResults()

  /** Splits source into its top-level statements with the interpreter's own parser, so a
    * multi-line expression stays whole (spark-shell's :load splits by lines, and silently runs
    * `val x = a` without the `.f()` on the next line). Each statement runs as its own request, so a
    * case class defined in one works in a typed Dataset operation in the next (spark-shell's
    * :paste runs a file as one unit, where it doesn't).
    */
  def split(src: String): Either[String, Seq[Statement]] = {
    val g = intp.global
    val errorsBefore = g.reporter.errorCount
    new g.Run // the parser needs a current compiler run
    val unit = g.newCompilationUnit(src, "<script>")
    val trees = g.newUnitParser(unit).parseStats()
    if (g.reporter.errorCount > errorsBefore) return Left("the source doesn't parse")
    val starts = trees.filterNot(_.isEmpty).map(t => if (t.pos.isRange) t.pos.start else t.pos.point).distinct.sorted
    val ends = starts.drop(1) :+ src.length
    Right(starts.zip(ends).map { case (s, e) => Statement(unit.source.offsetToLine(s) + 1, src.substring(s, e).trim) }.filter(_.code.nonEmpty))
  }

  /** Runs source statement by statement; stops at the first that fails. */
  def run(src: String): Outcome = split(src) match {
    case Left(why)    => Outcome.Unparsable(why)
    case Right(stmts) => run(stmts)
  }

  /** Runs statements in order; stops at the first that fails. */
  def run(stmts: Seq[Statement]): Outcome =
    stmts.iterator.map(st => (st, intp.interpret(st.code))).collectFirst {
      case (st, Results.Error) => Outcome.Failed(st.line)
      case (st, Results.Incomplete) =>
        out.println(s"booth-scala: the statement at line ${st.line} is incomplete")
        Outcome.Failed(st.line)
    }.getOrElse(Outcome.Done)

  def close(): Unit = intp.close()
}
