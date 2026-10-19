package booth.runner

import java.io.PrintWriter
import java.nio.charset.StandardCharsets
import java.nio.file.{Files, Paths}

import org.apache.spark.sql.SparkSession

/** An application's Scala (main.inlineScala, or a .scala file from booth-storage):
  *
  *   spark-submit … --class booth.runner.Script /opt/booth/booth-scala-runner.jar <script> [args…]
  *
  * The script runs statement by statement. It succeeds when every statement does. The first that
  * doesn't compile or throws stops it: the compiler's message or the exception is in the log,
  * followed by "BOOTH-SCALA error: …", and the driver exits 1, so the run ends failed. A script that
  * doesn't parse fails before Spark starts. sys.exit(n) in the script exits with n, as in Python.
  * Only what the script prints is printed (no REPL echo).
  */
object Script {
  def main(argv: Array[String]): Unit = {
    if (argv.isEmpty) {
      System.err.println("usage: booth.runner.Script <script.scala> [args...]")
      sys.exit(2)
    }
    val src = new String(Files.readAllBytes(Paths.get(argv(0))), StandardCharsets.UTF_8)
    val classes = Interp.classDir()
    val out = new PrintWriter(System.out, true)
    val interp = new Interp(classes, out, echo = false)
    val stmts = interp.split(src) match {
      case Right(s) => s
      case Left(why) =>
        out.flush()
        System.err.println(s"BOOTH-SCALA error: $why; nothing ran")
        sys.exit(1)
    }
    Runtime.args = argv.drop(1)
    Runtime.spark = SparkSession.builder().config(Interp.sparkConf(classes)).getOrCreate()
    interp.start()
    val outcome = interp.run(stmts)
    out.flush()
    System.out.flush()
    Runtime.spark.stop()
    outcome match {
      case Outcome.Done => ()
      case Outcome.Failed(line) =>
        System.err.println(s"BOOTH-SCALA error: the statement at line $line failed; the script stopped there")
        sys.exit(1)
      case Outcome.Unparsable(why) =>
        System.err.println(s"BOOTH-SCALA error: $why")
        sys.exit(1)
    }
  }
}
