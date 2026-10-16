#!/usr/bin/env bash
# ADR 0118 verification gate 1: Hadoop S3A from the booth-spark runtime image against SeaweedFS,
# with a prefix-scoped IAM user's bare access key (no session token), read through the image's
# SidecarCredentialsProvider. Needs only Docker.
#
#   test/s3a/run.sh <runtime image>        e.g. booth-spark-runtime:ci
#   S3A_CHECKSUM=CRC32 test/s3a/run.sh ... the same, with S3A sending CRC32 checksums on uploads
#                                          (fs.s3a.create.checksum.algorithm)
#
# SeaweedFS is set up as booth-storage's spike did (its docs/design-seaweedfs-iam-spike.md,
# commit b3c7863, section 1): embedded S3 + IAM on one port, an admin identity seeded through
# weed shell, then IAM CreateUser, PutUserPolicy (Get/Put/DeleteObject on the prefix, ListBucket
# conditioned on it) and CreateAccessKey with the admin key, the way booth-storage's seaweedfs
# provider will mint (ADR 0118 decision 2).
set -euo pipefail

RUNTIME_IMAGE=${1:?usage: run.sh <runtime image>}
SEAWEEDFS_IMAGE=chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
AWSCLI_IMAGE=amazon/aws-cli:2.31.0@sha256:3b018ce74732c98acf6f1de59b3a89587cb7f9eb6ea0d1447d1779091b2bf057

NET=booth-s3a-gate
SW=booth-s3a-seaweedfs
BUCKET=booth-s3a
PREFIX=ws/run-1
OUTSIDE=root.csv
ADMIN_KEY=admin_key
ADMIN_SECRET=admin_secret_for_the_gate_only
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
# Docker sees the host path; on Git Bash (Windows) that is the Windows form of mktemp's.
MOUNT=$WORK
command -v cygpath >/dev/null 2>&1 && MOUNT=$(cygpath -m "$WORK")

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "--- SeaweedFS log (last 200 lines) ---"
    docker logs --tail 200 "$SW" 2>&1 || true
  fi
  docker rm -f "$SW" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

# Git Bash on Windows would rewrite the container-side paths below; CI runs on Linux.
export MSYS_NO_PATHCONV=1

aws_admin() {
  docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID="$ADMIN_KEY" -e AWS_SECRET_ACCESS_KEY="$ADMIN_SECRET" \
    -e AWS_DEFAULT_REGION=us-east-1 -v "$MOUNT:/work" "$AWSCLI_IMAGE" --endpoint-url "http://$SW:8333" "$@"
}

docker network create "$NET" >/dev/null

# iam.json carries only an STS signing key, which SeaweedFS needs even for the plain IAM API.
mkdir -p "$WORK/etc"
printf '{"sts":{"signingKey":"%s"}}\n' "$(head -c 32 /dev/urandom | base64)" >"$WORK/etc/iam.json"
docker run -d --name "$SW" --network "$NET" -v "$MOUNT/etc:/etc/seaweedfs:ro" "$SEAWEEDFS_IMAGE" \
  -v=2 server -dir=/data -ip.bind=0.0.0.0 -s3 -s3.ip.bind=0.0.0.0 -s3.port=8333 \
  -s3.iam -s3.iam.readOnly=false -s3.iam.config=/etc/seaweedfs/iam.json >/dev/null

echo "Seeding the admin identity (SeaweedFS boots with none)"
for i in $(seq 1 60); do
  if docker exec "$SW" sh -c "echo 's3.configure -access_key $ADMIN_KEY -secret_key $ADMIN_SECRET -user admin -actions Admin -apply' | weed shell" >"$WORK/shell.log" 2>&1 \
    && aws_admin s3api list-buckets >/dev/null 2>&1; then
    break
  fi
  [ "$i" -eq 60 ] && { cat "$WORK/shell.log"; echo "SeaweedFS never accepted the admin identity"; exit 1; }
  sleep 2
done

echo "Bucket, an object outside the prefix, and the scoped IAM user"
aws_admin s3api create-bucket --bucket "$BUCKET" >/dev/null
printf 'a,b\n1,2\n' >"$WORK/outside.csv"
aws_admin s3api put-object --bucket "$BUCKET" --key "$OUTSIDE" --body /work/outside.csv >/dev/null
aws_admin s3api head-object --bucket "$BUCKET" --key "$OUTSIDE" >/dev/null # it exists: a refusal is not a 404

cat >"$WORK/policy.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::$BUCKET/$PREFIX/*"]},
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::$BUCKET"],"Condition":{"StringLike":{"s3:prefix":["$PREFIX/*"]}}}
]}
EOF
aws_admin iam create-user --user-name spark-run-1 >/dev/null
aws_admin iam put-user-policy --user-name spark-run-1 --policy-name prefix --policy-document file:///work/policy.json
read -r KEY_ID KEY_SECRET < <(aws_admin iam create-access-key --user-name spark-run-1 \
  --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text | tr -d '\r')
[ -n "$KEY_ID" ] && [ -n "$KEY_SECRET" ] || { echo "CreateAccessKey returned no key"; exit 1; }

# The credential sidecar's file shape (contracts/credential-sidecar.md): [default], no session token.
mkdir -p "$WORK/creds"
printf '[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n' "$KEY_ID" "$KEY_SECRET" >"$WORK/creds/credentials"
chmod 0644 "$WORK/creds/credentials"
cp "$HERE/s3a_check.py" "$WORK/s3a_check.py"

# The per-bucket settings booth-spark's DataConf sets (internal/runs/objects_data.go), plus the
# gate's own: 5 MiB parts (S3A's minimum) so 12 MiB is a three-part upload, and two-key pages.
B="spark.hadoop.fs.s3a.bucket.$BUCKET"
echo "Running S3A from $RUNTIME_IMAGE"
docker run --rm --network "$NET" -v "$MOUNT:/work" --entrypoint /opt/spark/bin/spark-submit "$RUNTIME_IMAGE" \
  --master 'local[2]' \
  --conf spark.ui.enabled=false \
  --conf spark.hadoop.fs.s3a.aws.credentials.provider=io.projectbooth.spark.SidecarCredentialsProvider \
  --conf "$B.aws.credentials.provider=io.projectbooth.spark.SidecarCredentialsProvider" \
  --conf "$B.booth.credentials.s3a=/work/creds/credentials" \
  --conf "$B.endpoint=http://$SW:8333" \
  --conf "$B.endpoint.region=us-east-1" \
  --conf "$B.path.style.access=true" \
  --conf "$B.multipart.size=5M" \
  --conf "$B.paging.maximum=2" \
  ${S3A_CHECKSUM:+--conf "$B.create.checksum.algorithm=$S3A_CHECKSUM"} \
  /work/s3a_check.py "$BUCKET" "$PREFIX" "$OUTSIDE" 2>&1 | tee "$WORK/spark.log" | grep -E 'PASS|ALL S3A|Error|Exception|Traceback' || true
grep -q '^ALL S3A CHECKS PASSED' "$WORK/spark.log" || {
  echo "--- spark-submit output (last 120 lines) ---"; tail -120 "$WORK/spark.log"; exit 1; }

# Independently of S3A: the admin view of what it wrote.
# With S3A_CHECKSUM=CRC32, S3A sent a CRC32 per part and SeaweedFS kept the composite; without it,
# S3A sends none (hadoop-aws 3.4.2 and SDK 2.29.52 add no checksums by default).
read -r SIZE ETAG CRC TYPE < <(aws_admin s3api head-object --bucket "$BUCKET" --key "$PREFIX/big.bin" \
  --checksum-mode ENABLED --query '[ContentLength,ETag,ChecksumCRC32,ChecksumType]' --output text | tr -d '\r')
echo "big.bin as SeaweedFS stores it: size $SIZE, ETag $ETAG, CRC32 $CRC ($TYPE)"
if [ -n "${S3A_CHECKSUM:-}" ]; then
  [ "$CRC" != None ] && [ "$TYPE" = COMPOSITE ] || { echo "S3A_CHECKSUM=$S3A_CHECKSUM, but SeaweedFS stored no checksum"; exit 1; }
else
  [ "$CRC" = None ] || { echo "a checksum was stored though S3A was not asked for one"; exit 1; }
fi
# SeaweedFS's side: the rename's server-side copy, and the scoped key's refusals.
echo "--- SeaweedFS: copies and refusals"
docker logs "$SW" 2>&1 | grep -E 'Unified copy strategy|" 403 ' || { echo "SeaweedFS logged no copy or no 403"; exit 1; }
echo "S3A gate passed"
