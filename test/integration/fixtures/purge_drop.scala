// data.sh step 1d (ADR 0122 part A): the same DROP TABLE ... PURGE from Scala.
val t = "lakehouse.spark_it.purge_scala"
spark.sql(s"DROP TABLE IF EXISTS $t")
spark.sql(s"CREATE TABLE $t (id BIGINT, v STRING) USING iceberg")
(0 until 5).foreach(i => spark.sql(s"INSERT INTO $t VALUES (${2 * i}, 'a'), (${2 * i + 1}, 'b')"))
println("PURGE-COUNT " + spark.table(t).count())
val meta = spark.sql(s"SELECT file FROM $t.metadata_log_entries ORDER BY timestamp DESC LIMIT 1").first().getString(0)
println("PURGE-LOCATION " + meta.split("/metadata/")(0))
spark.sql(s"DROP TABLE $t PURGE")
println("PURGE-DROPPED")
println("PURGE-GONE " + (if (spark.sql("SHOW TABLES IN lakehouse.spark_it").collect().exists(_.getString(1) == "purge_scala")) "False" else "True"))
