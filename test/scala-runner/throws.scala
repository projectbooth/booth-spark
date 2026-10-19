println("SCRIPT-BEFORE")
val rows = spark.range(10).count()
require(rows == 11, s"expected 11 rows, got $rows")
println("SCRIPT-NEVER")
