# A data run's entry point (data.sh), uploaded to booth-storage and started as main.python, so
# the run reads its own code from storage first. As its submitter (an editor), it:
#   - reads a CSV from its declared storage location (s3a://, read on an executor);
#   - writes and reads an Iceberg table in the workspace's lakehouse (catalog "lakehouse");
#   - writes and reads a Postgres table in the workspace's database (JDBC, read on an executor);
#   - writes its result back to the storage location (readwrite).
# Each step prints a marker line data.sh checks.
import json
import os

from pyspark.sql import SparkSession

spark = SparkSession.builder.getOrCreate()
root = json.loads(os.environ["BOOTH_STORAGE"])[0]["root"]
print("STORAGE-ROOT", root, flush=True)

df = spark.read.option("header", True).option("inferSchema", True).csv(root + "/in/sales.csv")
print("READ-CSV", df.count(), flush=True)

spark.sql("CREATE NAMESPACE IF NOT EXISTS lakehouse.spark_it")
df.writeTo("lakehouse.spark_it.sales").createOrReplace()
total = spark.sql("SELECT sum(amount) AS s FROM lakehouse.spark_it.sales").collect()[0]["s"]
print("ICEBERG-ROWS", spark.table("lakehouse.spark_it.sales").count(), "SUM", total, flush=True)

url = os.environ["JDBC_DATABASE_URL"]
props = {"driver": "org.postgresql.Driver"}
df.write.jdbc(url, "spark_it_sales", mode="overwrite", properties=props)
back = spark.read.jdbc(url, "spark_it_sales", column="amount", lowerBound=0, upperBound=100, numPartitions=2, properties=props)
print("JDBC-ROWS", back.count(), flush=True)

df.coalesce(1).write.mode("overwrite").option("header", True).csv(root + "/out/result")
print("WROTE-STORAGE", flush=True)
spark.stop()
