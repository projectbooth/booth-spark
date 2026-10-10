#!/usr/bin/env bash
# The data backends of the data job (build step 5; docs/design-v0.md item 9), after
# deploy-realcore.sh: booth-database, a MinIO, booth-storage with its s3-kind credential provider,
# and booth-lakehouse with its bundled Lakekeeper, each from a pinned checkout, installed as
# booth-streamlit's real-core job installs them. booth-storage trusts booth-core's workload-token
# issuer (how a run's token is accepted); booth-lakehouse trusts it through the
# booth-workload-minting-credentials Secret core writes (its chart default).
#
#   test/integration/deploy-data.sh <booth-database checkout> <image> <booth-storage checkout> <image> \
#     <booth-lakehouse checkout> <image>
#
# All images must already be loaded into the cluster (pullPolicy Never).
set -euo pipefail
database_dir=$1
database_image=$2
storage_dir=$3
storage_image=$4
lakehouse_dir=$5
lakehouse_image=$6
issuer=http://keycloak.keycloak.svc:8080/realms/booth
workload_issuer=http://booth-core.booth-system.svc:8080
here=$(cd "$(dirname "$0")" && pwd)/realcore
rev() { git -C "$1" rev-parse --short HEAD 2>/dev/null || echo '?'; }

echo "--- booth-database from $database_dir ($(rev "$database_dir")), release db in booth-database (as booth-api's realstack test)"
kubectl create namespace booth-database --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install db "$database_dir/charts/booth-database" -n booth-database \
  --set image.repository="${database_image%:*}" --set image.tag="${database_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m

echo "--- MinIO (the object store; a test fixture)"
kubectl apply -f "$here/minio.yaml" >/dev/null

echo "--- booth-storage from $storage_dir ($(rev "$storage_dir")): its s3 credential provider"
kubectl create namespace booth-storage --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install booth-storage "$storage_dir/charts/booth-storage" -n booth-storage \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set oidc.workloadIssuerUrl="$workload_issuer" \
  --set credentialBroker.enabled=true \
  --set-json 'filesystem.roots=["/data/{workspace}"]' \
  --set-json 'filesystem.volumes=[{"name":"data","emptyDir":{}}]' \
  --set-json 'filesystem.volumeMounts=[{"name":"data","mountPath":"/data"}]' \
  --set image.repository="${storage_image%:*}" --set image.tag="${storage_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m

echo "--- booth-lakehouse from $lakehouse_dir ($(rev "$lakehouse_dir")), with its bundled Lakekeeper"
kubectl create namespace booth-lakehouse --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# No --wait: its pods need the database and minting Secrets core writes once it sees the BoothModule.
helm upgrade --install booth-lakehouse "$lakehouse_dir/charts/booth-lakehouse" -n booth-lakehouse \
  --set identity.oidcIssuerUrl="$issuer" --set identity.oidcAudience=booth-design \
  --set image.repository="${lakehouse_image%:*}" --set image.tag="${lakehouse_image##*:}" --set image.pullPolicy=Never
kubectl -n booth-minio rollout status deploy/minio --timeout=300s
kubectl -n booth-lakehouse rollout status deploy/booth-lakehouse-lakekeeper --timeout=600s
kubectl -n booth-lakehouse rollout status deploy/booth-lakehouse-api --timeout=600s
