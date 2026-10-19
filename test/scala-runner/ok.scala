// The runner's end-to-end script (run.sh): what a Scala application gets, on separate executor JVMs.
println(s"SCRIPT-START scala ${scala.util.Properties.versionNumberString} spark ${spark.version} args ${args.mkString(",")}")

// A case class defined in one statement, used in a typed Dataset operation in the next (fails under
// spark-shell's :paste), and a chain over several lines (silently cut under :load).
case class Play(season: Int, yards: Int)
val plays = Seq(Play(2023, 5), Play(2023, -2), Play(2024, 12)).toDS()
val bySeason = plays
  .groupBy("season")
  .agg(sum("yards").as("yards"))
  .orderBy("season")
  .as[(Int, Long)]
  .collect()
  .toList
println("SCRIPT-SEASONS " + bySeason.mkString(" "))
def doubled(x: Int): Int = x * 2
println("SCRIPT-TYPED " + plays.map(p => doubled(p.yards)).collect().mkString(","))
val executors = sc.parallelize(1 to 4, 2).map(_ => java.lang.management.ManagementFactory.getRuntimeMXBean.getPid).distinct().count()
println(s"SCRIPT-EXECUTOR-JVMS $executors")

// The lakehouse catalog by its name, as data access gives it (here a local Iceberg catalog).
spark.sql(s"CREATE NAMESPACE IF NOT EXISTS ${booth.catalog}.it")
spark.range(5).withColumn("sq", col("id") * col("id")).writeTo(s"${booth.catalog}.it.squares").createOrReplace()
println("SCRIPT-ICEBERG " + spark.table(s"${booth.catalog}.it.squares").agg(sum("sq")).first().getLong(0))
println("SCRIPT-STORAGE " + booth.storage.map(_.root).mkString(","))
println("SCRIPT-JSON " + booth.json(Map("output" -> bySeason.toMap)))
