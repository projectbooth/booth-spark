package booth.runner

import java.io.{PrintWriter, StringWriter}

/** The runner's own checks that need no SparkSession (test/scala-runner/run.sh runs them in the
  * built image, before its end-to-end scripts): how source splits into statements, and `booth`'s
  * helpers. Prints one line per check; exits 1 if any fails.
  */
object SelfTest {
  private var failed = 0

  private def check(name: String, ok: Boolean, detail: => String): Unit =
    if (ok) println(s"SELFTEST ok: $name")
    else { failed += 1; println(s"SELFTEST FAIL: $name: $detail") }

  def main(argv: Array[String]): Unit = {
    val log = new StringWriter
    val interp = new Interp(Interp.classDir(), new PrintWriter(log, true), echo = false)
    def lines(src: String): Either[String, Seq[(Int, String)]] = interp.split(src).map(_.map(s => (s.line, s.code)))

    check("one statement per top-level statement, with its first line",
      lines("val a = 1\nval b = a +\n  2\n\nprintln(b)\n") == Right(Seq((1, "val a = 1"), (2, "val b = a +\n  2"), (5, "println(b)"))),
      lines("val a = 1\nval b = a +\n  2\n\nprintln(b)\n").toString)
    check("a chain over several lines stays one statement (spark-shell's :load splits it)",
      lines("val total = Seq(1, 2, 3)\n  .map(_ * 2)\n  .sum\nprintln(total)") == Right(Seq((1, "val total = Seq(1, 2, 3)\n  .map(_ * 2)\n  .sum"), (4, "println(total)"))),
      lines("val total = Seq(1, 2, 3)\n  .map(_ * 2)\n  .sum\nprintln(total)").toString)
    check("a case class, a def and an object are each their own statement",
      lines("case class P(a: Int)\ndef twice(x: Int) = x * 2\nobject O { val v = 1 }\n").map(_.map(_._1)) == Right(Seq(1, 2, 3)),
      lines("case class P(a: Int)\ndef twice(x: Int) = x * 2\nobject O { val v = 1 }\n").toString)
    check("statements on one line split at ';'",
      lines("val a = 1; val b = 2").map(_.size) == Right(2), lines("val a = 1; val b = 2").toString)
    check("comments only: no statements",
      lines("// nothing\n/* still nothing */\n") == Right(Nil), lines("// nothing\n/* still nothing */\n").toString)
    check("a statement keeps its comments and annotations",
      lines("@deprecated(\"x\", \"1\") def old = 1\nval y = 2 // trailing\n").map(_.map(_._1)) == Right(Seq(1, 2)),
      lines("@deprecated(\"x\", \"1\") def old = 1\nval y = 2 // trailing\n").toString)
    check("source that doesn't parse is refused, with nothing run",
      lines("val = 1\nprintln(\"never\")").isLeft, lines("val = 1\nprintln(\"never\")").toString)

    check("booth.json writes Scala values",
      Booth.json(Map("a" -> 1, "b" -> List(1, 2), "c" -> None, "d" -> "x")) == """{"a":1,"b":[1,2],"c":null,"d":"x"}""",
      Booth.json(Map("a" -> 1, "b" -> List(1, 2), "c" -> None, "d" -> "x")))
    check("booth.parseJson reads plain values",
      Booth.parseJson("""{"season":2024,"teams":["a","b"],"ok":true,"none":null}""") ==
        Map("season" -> 2024, "teams" -> List("a", "b"), "ok" -> true, "none" -> null),
      String.valueOf(Booth.parseJson("""{"season":2024,"teams":["a","b"],"ok":true,"none":null}""")))
    check("booth.storage reads BOOTH_STORAGE",
      Booth.storageFrom("""[{"backendId":"lake","path":"raw","access":"read","root":"s3a://lake/raw"}]""") ==
        Seq(StorageLocation("lake", "raw", "read", "s3a://lake/raw")) && Booth.storageFrom("") == Nil,
      Booth.storageFrom("""[{"backendId":"lake","path":"raw","access":"read","root":"s3a://lake/raw"}]""").toString)

    interp.close()
    if (failed > 0) {
      println(s"SELFTEST: $failed failed; the interpreter said:\n$log")
      sys.exit(1)
    }
    println("SELFTEST PASS")
  }
}
