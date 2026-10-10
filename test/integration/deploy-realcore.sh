#!/usr/bin/env bash
# Deploys a real Keycloak and a real booth-core (built from a pinned checkout), then booth-spark with
# its chart defaults plus every token-verifying setting (docs/design-v0.md item 2), so that:
#   - core provisions the module's database from `database: {enabled: true}` (ADR 0053);
#   - /v1 verifies real Keycloak tokens, with keys fetched through oidc.jwksUrl (ADR 0108) from a
#     URL deliberately spelled differently from the issuer (the cluster.local form), so a pass proves
#     keys came from the override while `iss` was compared against the issuer;
#   - /v1 also trusts core's workload issuer (oidc.workloadIssuerUrl, ADR 0056/0059);
#   - the iframe routes verify core's real X-Booth-Identity assertion (ADR 0069).
# Core runs with its bundled Keycloak and Ingress off, as core's own kind-deploy job runs it.
#
#   test/integration/deploy-realcore.sh <booth-core checkout> <core image> <spark image>
#
# Both images must already be loaded into the cluster (pullPolicy Never). The test password is
# generated per run and kept in the Secret keycloak/realcore-test-password for identity.sh.
set -euo pipefail

core_dir=$1
core_image=$2
image=$3
ns=booth-spark
issuer=http://keycloak.keycloak.svc:8080/realms/booth
jwks=http://keycloak.keycloak.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs
workload_issuer=http://booth-core.booth-system.svc:8080
repo=$(cd "$(dirname "$0")/../.." && pwd)
here="$repo/test/integration/realcore"

echo "--- Keycloak (realm: owner, editor, viewer and an operator-viewer of acme-analytics; an owner of other-team)"
kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f - >/dev/null
password=$(openssl rand -hex 12)
kubectl -n keycloak create secret generic realcore-test-password --from-literal=password="$password" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n keycloak create secret generic keycloak-admin --from-literal=password="$(openssl rand -hex 12)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
realm=$(mktemp)
sed "s/__TEST_PASSWORD__/$password/g" "$here/realm-booth.json.tpl" >"$realm"
kubectl -n keycloak create configmap keycloak-realm --from-file=realm-booth.json="$realm" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$realm"
kubectl apply -f "$here/keycloak.yaml" >/dev/null
# booth-core starts without Keycloak (it retries OIDC discovery), so don't wait here; wait at the end.

echo "--- booth-core from $core_dir ($(git -C "$core_dir" rev-parse --short HEAD 2>/dev/null || echo '?'))"
kubectl create namespace booth-system --dry-run=client -o yaml | kubectl apply -f -
# Core's own CRD, applied explicitly: helm never updates a CRD that already exists.
kubectl apply -f "$core_dir/charts/booth-core/crds/"
helm upgrade --install booth-core "$core_dir/charts/booth-core" --namespace booth-system \
  --set keycloak.enabled=false --set ingress.enabled=false \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set-string iframeSigningKey="$(openssl rand -hex 32)" \
  --set workloadIdentity.issuerUrl="$workload_issuer" \
  --set image.repository="${core_image%:*}" --set image.tag="${core_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m
kubectl -n booth-system rollout status deploy/booth-core --timeout=300s

echo "--- booth-spark with chart defaults (database from core) and every identity setting"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
# No --wait on purpose: the pod cannot start until core has written booth-database-credentials,
# which it does only after it sees the BoothModule this install creates. verify.sh waits for that.
helm upgrade --install booth-spark "$repo/charts/booth-spark" --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set oidc.jwksUrl="$jwks" --set oidc.workloadIssuerUrl="$workload_issuer"

kubectl -n keycloak rollout status deploy/keycloak --timeout=600s
