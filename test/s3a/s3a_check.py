# ADR 0118 verification gate 1: Hadoop S3A, as the runtime image ships it, against SeaweedFS with a
# prefix-scoped IAM key and no session token, read through SidecarCredentialsProvider. Run by
# test/s3a/run.sh under spark-submit (local mode), with the S3A settings that script passes.
#
#   s3a_check.py <bucket> <prefix> <outside key>
#
# Every check prints "PASS <name>" or raises. The refusals accept only S3A's access-denied error
# (java.nio.file.AccessDeniedException carrying S3's AccessDenied code), never any failure.
import hashlib
import sys

from pyspark.sql import SparkSession

bucket, prefix, outside = sys.argv[1], sys.argv[2].strip("/"), sys.argv[3]
root = "s3a://%s/%s" % (bucket, prefix)

spark = SparkSession.builder.appName("s3a-seaweedfs").getOrCreate()
jvm = spark._jvm
Path = jvm.org.apache.hadoop.fs.Path
fs = Path(root).getFileSystem(spark._jsc.hadoopConfiguration())


def ok(name):
    print("PASS " + name, flush=True)


def put(path, data):
    out = fs.create(Path(path), True)
    out.write(bytearray(data))
    out.close()


def get(path):
    stream = fs.open(Path(path))
    buf = jvm.java.io.ByteArrayOutputStream()
    jvm.org.apache.hadoop.io.IOUtils.copyBytes(stream, buf, 1 << 16, True)
    return bytes(buf.toByteArray())


def denied(name, action):
    try:
        action()
    except Exception as e:  # py4j's Py4JJavaError
        java = getattr(e, "java_exception", None)
        cls = java.getClass().getName() if java is not None else type(e).__name__
        msg = java.toString() if java is not None else str(e)
        if cls in ("java.nio.file.AccessDeniedException", "org.apache.hadoop.fs.s3a.AWSAccessDeniedException") \
                and "AccessDenied" in msg:
            ok("%s (refused: %s: %s)" % (name, cls, msg.splitlines()[0][:240]))
            return
        raise AssertionError("%s: failed, but not with an access-denied error: %s %s" % (name, cls, msg))
    raise AssertionError(name + ": succeeded; the key's policy should have refused it")


# The key really has no session token: SidecarCredentialsProvider read basic credentials.
provider = jvm.io.projectbooth.spark.SidecarCredentialsProvider(jvm.java.net.URI(root), fs.getConf())
creds = provider.resolveCredentials()
assert creds.getClass().getSimpleName() == "AwsBasicCredentials", creds.getClass().getName()
ok("credentials: a bare key pair, no session token (%s)" % creds.getClass().getSimpleName())

# Write and read a small object.
small = b"hello from s3a\n" * 100
put(root + "/small.txt", small)
assert get(root + "/small.txt") == small
st = fs.getFileStatus(Path(root + "/small.txt"))
assert st.getLen() == len(small) and st.isFile()
ok("write, read back and stat a small object")

# A ranged read (S3A's seek + read: a GET with Range).
stream = fs.open(Path(root + "/small.txt"))
stream.seek(15 * 99)  # forward, then back: each seek reopens the object at a range
last = bytes(stream.read() for _ in range(15))
stream.seek(15 * 50 + 6)
mid = bytes(stream.read() for _ in range(4))
stream.close()
assert last == b"hello from s3a\n" and mid == b"from", (last, mid)
ok("seek and ranged read")

# Rename, which S3A does as copy then delete.
fs.rename(Path(root + "/small.txt"), Path(root + "/renamed/small.txt"))
assert not fs.exists(Path(root + "/small.txt"))
assert get(root + "/renamed/small.txt") == small
ok("rename (copy + delete): source gone, destination holds the bytes")

# Multipart: 12 MiB with fs.s3a.multipart.size=5M, so three parts. The ETag's -N suffix is S3's
# mark of a completed multipart upload.
big = (hashlib.sha256(b"seed").digest() * (12 * 1024 * 1024 // 32))
put(root + "/big.bin", big)
st = fs.getFileStatus(Path(root + "/big.bin"))
etag = st.getEtag()
assert st.getLen() == len(big), (st.getLen(), len(big))
assert etag is not None and etag.strip('"').endswith("-3"), etag
assert hashlib.sha256(get(root + "/big.bin")).hexdigest() == hashlib.sha256(big).hexdigest()
ok("multipart upload of 12 MiB in 5 MiB parts (ETag %s), read back byte for byte" % etag)

# Listing, paged (fs.s3a.paging.maximum=2), flat and recursive.
for i in range(5):
    put("%s/list/f%d.txt" % (root, i), b"x")
names = sorted(s.getPath().getName() for s in fs.listStatus(Path(root + "/list")))
assert names == ["f%d.txt" % i for i in range(5)], names
it = fs.listFiles(Path(root), True)
count = 0
while it.hasNext():
    it.next()
    count += 1
assert count == 7, count  # renamed/small.txt, big.bin, list/f0..f4
ok("listing in pages of 2: 5 entries flat, 7 files recursive")

# A Spark job: Parquet through the default commit protocol (task and job commit are renames).
df = spark.range(0, 10000).selectExpr("id", "id % 7 AS k")
df.repartition(4).write.mode("overwrite").parquet(root + "/table")
assert fs.exists(Path(root + "/table/_SUCCESS"))
back = spark.read.parquet(root + "/table")
assert back.count() == 10000 and back.where("k = 3").count() == 1429
ok("Spark Parquet write (FileOutputCommitter renames) and read back")

# Delete, recursive.
assert fs.delete(Path(root + "/list"), True)
if fs.exists(Path(root + "/list")):
    # SeaweedFS keeps the filer's emptied folder for s3.empty_folder_cleanup_delay (filer.toml,
    # default 2m) and lists it as a zero-byte "list/" key meanwhile; S3A reads that as an empty
    # directory. Real S3 has nothing there. Accepted only if it is exactly that: no files, no
    # entries.
    st = fs.getFileStatus(Path(root + "/list"))
    entries = [s.getPath().toString() for s in fs.listStatus(Path(root + "/list"))]
    assert st.isDirectory() and entries == [], (st.toString(), entries)
    ok("recursive delete (an empty directory lingers until SeaweedFS's empty-folder cleanup)")
else:
    ok("recursive delete")

# Outside the prefix: the object exists (run.sh seeded it with the admin key), and the scoped key
# is refused reading it, writing beside it and listing the bucket root.
denied("read outside the prefix", lambda: get("s3a://%s/%s" % (bucket, outside)))
denied("write outside the prefix", lambda: put("s3a://%s/other/y.txt" % bucket, b"y"))
denied("list the bucket root", lambda: list(fs.listStatus(Path("s3a://%s/" % bucket))))

print("ALL S3A CHECKS PASSED", flush=True)
spark.stop()
