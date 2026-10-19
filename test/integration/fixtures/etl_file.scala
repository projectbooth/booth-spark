// data.sh step 1c: a Scala file in booth-storage (main.scala, pinned by sha256), run after the inline
// Scala run wrote spark_it.scala_doubled.
println("SCALA-FILE " + spark.table(s"${booth.catalog}.spark_it.scala_doubled").count())
