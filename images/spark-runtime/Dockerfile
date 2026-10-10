# syntax=docker/dockerfile:1.7
# The Spark runtime image every run's driver and executors run (docs/design-v0.md items 1 and 4):
# Apache Spark 4.1.3 as published, plus what data access needs, nothing else:
#   - the PostgreSQL JDBC driver (booth-database, through the postgres sidecar on localhost);
#   - Iceberg's Spark runtime (booth-lakehouse's REST catalog);
#   - hadoop-aws and the AWS SDK v2 bundle it is built against (s3a, for the warehouse and the
#     declared booth-storage locations), versions matching Spark's own Hadoop 3.4.2;
#   - booth-spark's two helpers (src/): S3A credentials re-read from the credential sidecar's file,
#     and Iceberg REST authentication re-read from the run's token file.
# Every base image is pinned by digest and every jar by its SHA-256 (each also matches Maven
# Central's published SHA-1; resolved 2026-10-10).
#
#   docker build -f images/spark-runtime/Dockerfile -t booth-spark-runtime .

ARG SPARK_IMAGE=apache/spark:4.1.3-scala2.13-java21-python3-ubuntu@sha256:bf9d035a7c32a8ca46aa58d6348182ffd7d2dff6409206ecfbb3915ff1fef211

FROM ${SPARK_IMAGE} AS spark

FROM eclipse-temurin:21-jdk-noble@sha256:b468c3fc688b14450571494f588bd939378e7fd542ed5a73f8efc13f17872a87 AS helpers
ADD --checksum=sha256:73914527305a40cce504b0d3d90b23caf565136912d607ae8ae7c5895512332c \
    https://repo1.maven.org/maven2/org/postgresql/postgresql/42.7.14/postgresql-42.7.14.jar /jars/
ADD --checksum=sha256:3123c3798af642dd537230923548e013ef41cc28b6626e5225808cf5670a0f82 \
    https://repo1.maven.org/maven2/org/apache/iceberg/iceberg-spark-runtime-4.1_2.13/1.12.0/iceberg-spark-runtime-4.1_2.13-1.12.0.jar /jars/
ADD --checksum=sha256:d2d24e6b715b5656b9d238bf13b6682a07e2004dd36aad74a73aeb854319f4d9 \
    https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-aws/3.4.2/hadoop-aws-3.4.2.jar /jars/
ADD --checksum=sha256:62638f33045690c6bc5e496b54b0b2e89a9ad38cbedacdf6e017fea6bc85f55c \
    https://repo1.maven.org/maven2/software/amazon/awssdk/bundle/2.29.52/bundle-2.29.52.jar /jars/
COPY --from=spark /opt/spark/jars/hadoop-client-api-3.4.2.jar /classpath/
COPY images/spark-runtime/src/ /src/
RUN javac --release 21 -Xlint:all,-classfile -Werror -d /out -cp "/jars/*:/classpath/*" $(find /src -name '*.java') \
 && jar --create --file /jars/booth-spark-helpers.jar -C /out . \
 && chmod 0644 /jars/*.jar

FROM spark
COPY --from=helpers /jars/ /opt/spark/jars/
