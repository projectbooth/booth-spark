#!/usr/bin/env bash
# Installs (or upgrades) booth-spark the way the real-core job tests it: chart defaults (database
# from core), every identity setting (docs/design-v0.md item 2), and runs sized for a CI runner,
# placed on the node labelled booth.projectbooth.io/pool=compute. Extra helm arguments pass through
# (egress.sh switches runs.egress.mode with them).
#
#   test/integration/install-spark.sh <booth-spark image> <Spark image> [helm args...]
#
# BOOTH_SPARK_RELEASE names the release (default booth-spark; uninstall.sh uses "spark", the name
# booth-core's module uninstall API removes). Namespace booth-spark either way.
set -euo pipefail
image=$1
spark_image=$2
shift 2
release=${BOOTH_SPARK_RELEASE:-booth-spark}
repo=$(cd "$(dirname "$0")/../.." && pwd)
issuer=http://keycloak.keycloak.svc:8080/realms/booth
jwks=http://keycloak.keycloak.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs
workload_issuer=http://booth-core.booth-system.svc:8080

kubectl create namespace booth-spark --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install "$release" "$repo/charts/booth-spark" --namespace booth-spark \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set oidc.jwksUrl="$jwks" --set oidc.workloadIssuerUrl="$workload_issuer" \
  --set runs.image="$spark_image" --set runs.imagePullPolicy=Never \
  --set runs.maxRunning=4 --set runs.maxRunningPerWorkspace=0 --set runs.memoryBudget=10Gi \
  --set runs.pendingTimeout=5m \
  --set sessions.idleTimeout=60s --set sessions.maxLifetime=30m \
  --set 'runs.driver.nodeSelector.booth\.projectbooth\.io/pool=compute' \
  --set 'runs.executor.nodeSelector.booth\.projectbooth\.io/pool=compute' \
  "$@"
