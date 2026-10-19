// A JAR entry point (data.sh step 1b; docs/design-scala.md, PR 1), compiled in CI by the runtime
// image's own scalac (Scala 2.13, Spark provided), uploaded to booth-storage and started as
// main.jar with this main class. As its submitter (an editor), after step 1's run, it:
//   - reads the Iceberg table step 1 wrote, through the lakehouse catalog Spark's conf names;
//   - reads the CSV from its declared storage location (s3a, BOOTH_STORAGE);
//   - checks both against its argument (the expected sum) and exits 1 if either differs.
// Each step prints a marker line data.sh checks.
package booth.it

import org.apache.spark.sql.SparkSession
import org.apache.spark.sql.functions.sum

object JarSmoke {
  def main(args: Array[String]): Unit = {
    val expected = args(0).toLong
    val spark = SparkSession.builder().getOrCreate()
    import spark.implicits._
    spark.sparkContext.setLogLevel("WARN")
    val cat = sys.env.getOrElse("BOOTH_LAKEHOUSE_CATALOG", "lakehouse")
    println(s"JAR-START scala ${scala.util.Properties.versionNumberString} spark ${spark.version} catalog $cat")
    val iceberg = spark.table(s"$cat.spark_it.sales").agg(sum("amount")).first().getLong(0)
    println(s"JAR-ICEBERG-SUM $iceberg")
    val root = spark.read.json(Seq(sys.env("BOOTH_STORAGE")).toDS()).select("root").as[String].first()
    val csv = spark.read.option("header", true).option("inferSchema", true).csv(root + "/in/sales.csv").agg(sum("amount")).first().get(0).toString.toLong
    println(s"JAR-CSV-SUM $csv")
    spark.stop()
    if (iceberg != expected || csv != expected) {
      println(s"JAR-FAIL expected $expected")
      sys.exit(1)
    }
    println("JAR-PASS")
  }
}
