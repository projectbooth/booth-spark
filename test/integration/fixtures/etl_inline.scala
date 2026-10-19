// data.sh step 1c (ADR 0121, PR 4): an application's inline Scala, run by the runtime image's Scala
// runner as its submitter (an editor) with lakehouse and storage access, after step 1. It reads the
// CSV from its storage location into a typed Dataset, reads step 1's Iceberg table through the
// lakehouse catalog, writes a new Iceberg table and reads it back. Each step prints a marker line.
case class Sale(region: String, amount: Long)
val root = booth.storage.head.root
val sales = spark.read.option("header", true).option("inferSchema", true).csv(root + "/in/sales.csv").as[Sale]
println(s"SCALA-CSV ${sales.map(_.amount).reduce(_ + _)}")
val iceberg = spark.table(s"${booth.catalog}.spark_it.sales").agg(sum("amount")).first().getLong(0)
println(s"SCALA-ICEBERG $iceberg")
sales
  .map(s => s.copy(amount = s.amount * 2))
  .writeTo(s"${booth.catalog}.spark_it.scala_doubled")
  .createOrReplace()
println("SCALA-WROTE " + spark.table(s"${booth.catalog}.spark_it.scala_doubled").agg(sum("amount")).first().getLong(0))
println("SCALA-ARGS " + args.mkString(","))
