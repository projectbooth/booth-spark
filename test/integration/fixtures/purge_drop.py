# data.sh step 1d (ADR 0122 part A): DROP TABLE ... PURGE from Python succeeds, as one request to the
# catalog (rest-catalog-purge=true), after a table has had several commits and been read.
from pyspark.sql import SparkSession

spark = SparkSession.builder.getOrCreate()
t = "lakehouse.spark_it.purge_py"
spark.sql("DROP TABLE IF EXISTS " + t)
spark.sql(f"CREATE TABLE {t} (id BIGINT, v STRING) USING iceberg")
for i in range(5):
    spark.sql(f"INSERT INTO {t} VALUES ({2 * i}, 'a'), ({2 * i + 1}, 'b')")
print("PURGE-COUNT", spark.table(t).count(), flush=True)
meta = spark.sql(f"SELECT file FROM {t}.metadata_log_entries ORDER BY timestamp DESC LIMIT 1").first()[0]
print("PURGE-LOCATION", meta.rsplit("/metadata/", 1)[0], flush=True)
spark.sql(f"DROP TABLE {t} PURGE")
print("PURGE-DROPPED", flush=True)
gone = "purge_py" not in [r.tableName for r in spark.sql("SHOW TABLES IN lakehouse.spark_it").collect()]
print("PURGE-GONE", gone, flush=True)
