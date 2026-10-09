#!/usr/bin/env bash
# docs/design-v0.md item 2, against a real Keycloak and a real booth-core (deploy-realcore.sh):
#   - /v1 through core's gateway verifies real people's tokens, with keys fetched through
#     oidc.jwksUrl, derives the role itself, and applies submit.minRole (ADR 0110);
#   - straight to the Service (bypassing the gateway), a forged X-Booth-Role is refused (ADR 0041);
#   - /v1 accepts core's real workload tokens, marked as workload callers (ADR 0056/0059);
#   - the iframe routes, through core's real iframe proxy, verify core's real X-Booth-Identity
#     assertion, including the /platform/operator claim (ADR 0094);
#   - each credential works only on its own route group.
#
# Tokens are requested inside the cluster, because a token's `iss` must be Keycloak's in-cluster URL,
# which is what core and booth-spark trust.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)

step "the backend reports where its keys come from (ADR 0108) and which issuers it trusts"
# Lazy verification: the log line appears on the first /v1 request, which the probe below makes;
# checked after it.

step "a minting credential from core, for a workload token (a test-only BoothModule declaring workloadIdentity.mint)"
kubectl create namespace booth-spark-it-mint --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: booth.projectbooth.io/v1alpha1
kind: BoothModule
metadata:
  name: sparkit
  namespace: booth-spark-it-mint
spec:
  id: sparkit
  displayName: booth-spark integration minter
  version: 0.1.0
  contractVersion: 0.1.0
  hasOwnUi: false
  healthCheckPath: /healthz
  serviceRef: {name: none, namespace: booth-spark-it-mint, port: 8080}
  workloadIdentity: {mint: true}
EOF
for _ in $(seq 1 60); do
  kubectl -n booth-spark-it-mint get secret booth-workload-minting-credentials >/dev/null 2>&1 && break
  sleep 2
done
mint_cred=$(kubectl -n booth-spark-it-mint get secret booth-workload-minting-credentials -o jsonpath='{.data.credential}' | base64 -d)
mint_url=$(kubectl -n booth-spark-it-mint get secret booth-workload-minting-credentials -o jsonpath='{.data.url}' | base64 -d)
[ -n "$mint_cred" ] && [ -n "$mint_url" ] || fail "core never wrote booth-workload-minting-credentials"
echo "ok: minting credential for $mint_url"

read -r -d '' script <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
SPARK=http://booth-spark.booth-spark.svc:8080
WS=acme-analytics
field() { sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p"; }
tok() { curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$1" -d "password=$PW" | field access_token; }

for u in owner editor viewer operator outsider; do
  t=$(tok "$u-user"); [ -n "$t" ] || { echo "FAIL: no token for $u-user"; failed=1; }
  eval "T_$u=\$t"
  # Signing in through core records the person (and their roles) in core's directory (ADR 0047),
  # which is what minting checks the owner against (ADR 0058).
  curl -s -o /dev/null -H "Authorization: Bearer $t" "$CORE/api/me"
done
editor_sub=$(curl -s -H "Authorization: Bearer $T_editor" "$KC/userinfo" | field sub)

echo "# /v1 through core's gateway, real Keycloak tokens"
v1() { curl -s -H "Authorization: Bearer $1" -H "X-Workspace: $WS" "$CORE/modules/spark/v1/me"; }
for i in $(seq 1 30); do [ "$(status -H "Authorization: Bearer $T_editor" -H "X-Workspace: $WS" "$CORE/modules/spark/v1/me")" = 200 ] && break; sleep 2; done
b=$(v1 "$T_editor")
check "editor: role" "$(echo "$b" | field role)" editor
check "editor: subject is the person's own" "$(echo "$b" | field subject)" "$editor_sub"
check "editor: may submit (submit.minRole editor)" "$(echo "$b" | field canSubmit)" true
check "editor: not a workload caller" "$(echo "$b" | field workload)" false
check "editor: display name from the token" "$(echo "$b" | field displayName)" editor-user
b=$(v1 "$T_viewer")
check "viewer: role" "$(echo "$b" | field role)" viewer
check "viewer: never submits" "$(echo "$b" | field canSubmit)" false
b=$(v1 "$T_owner")
check "owner: may submit" "$(echo "$b" | field canSubmit)" true
b=$(v1 "$T_operator")
check "operator: role is still their workspace role" "$(echo "$b" | field role)" viewer
check "operator: /platform/operator read off the token" "$(echo "$b" | field operator)" true
check "operator: the operator claim grants no submit" "$(echo "$b" | field canSubmit)" false
check "outsider: no role in this workspace (refused by core)" "$(status -H "Authorization: Bearer $T_outsider" -H "X-Workspace: $WS" "$CORE/modules/spark/v1/me")" 403

echo "# /v1 straight to the Service, bypassing the gateway (ADR 0041)"
direct() {
  if [ -n "${3:-}" ]; then status -H "Authorization: Bearer $1" -H "X-Booth-Workspace: $2" -H "X-Booth-Role: $3" "$SPARK/v1/me"
  else status -H "Authorization: Bearer $1" -H "X-Booth-Workspace: $2" "$SPARK/v1/me"; fi
}
check "editor token, its own workspace" "$(direct "$T_editor" $WS)" 200
check "editor token with a forged X-Booth-Role: owner" "$(direct "$T_editor" $WS owner)" 403
check "outsider token naming a workspace it has no role in" "$(direct "$T_outsider" $WS)" 403
check "no token" "$(status -H "X-Booth-Workspace: $WS" "$SPARK/v1/me")" 401
check "a token signed by nobody we trust" "$(direct 'eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwOi8vZXZpbCJ9.c2ln' $WS)" 401

echo "# /v1 with core's real workload token (ADR 0056/0059)"
wl=$(curl -s -X POST -H "Authorization: Bearer $MINT_CRED" -H 'Content-Type: application/json' \
  -d "{\"workspace\":\"$WS\",\"subject\":\"sparkit:1\",\"roleCeiling\":\"editor\",\"owner\":\"$editor_sub\"}" "$MINT_URL")
wltok=$(echo "$wl" | field token)
check "core minted a workload token" "$( [ -n "$wltok" ] && echo yes || echo "no: $wl")" yes
b=$(v1 "$wltok")
check "workload: accepted on /v1" "$(echo "$b" | field subject)" sparkit:1
check "workload: marked as a workload caller" "$(echo "$b" | field workload)" true
check "workload: role is the owner's, within the ceiling" "$(echo "$b" | field role)" editor
check "workload: never an operator" "$(echo "$b" | field operator)" false
check "workload token as an iframe assertion" "$(status -H "X-Booth-Identity: $wltok" -H "X-Booth-Workspace: $WS" "$SPARK/ui/api/me")" 401

echo "# the iframe path through core's real proxy and assertion (ADR 0069)"
session() { # USER-TOKEN: prints core's iframe session cookie for the module
  url=""
  for i in $(seq 1 30); do
    url=$(curl -s -H "Authorization: Bearer $1" -H "X-Workspace: $WS" "$CORE/api/modules/spark/iframe-url" | field url)
    [ -n "$url" ] && break
    sleep 2
  done
  curl -s -o /dev/null -D - "$CORE$url" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p'
}
ui() { curl -s -H "Cookie: booth_iframe_session=$1" "$CORE/iframe/spark$2"; }
c_editor=$(session "$T_editor")
c_viewer=$(session "$T_viewer")
c_operator=$(session "$T_operator")
check "editor got an iframe session" "$( [ -n "$c_editor" ] && echo yes || echo no)" yes
b=$(ui "$c_editor" /ui/api/me)
check "iframe editor: subject from core's assertion" "$(echo "$b" | field subject)" "$editor_sub"
check "iframe editor: role" "$(echo "$b" | field role)" editor
check "iframe editor: may submit" "$(echo "$b" | field canSubmit)" true
check "iframe editor: display name (preferred_username in the assertion)" "$(echo "$b" | field displayName)" editor-user
b=$(ui "$c_viewer" /ui/api/me)
check "iframe viewer: role" "$(echo "$b" | field role)" viewer
check "iframe viewer: never submits" "$(echo "$b" | field canSubmit)" false
b=$(ui "$c_operator" /ui/api/me)
check "iframe operator: /platform/operator carried in core's assertion (ADR 0094)" "$(echo "$b" | field operator)" true
contains "iframe page renders through core" "$(ui "$c_editor" /)" "<title>Spark</title>"
check "the iframe path without a session" "$(status "$CORE/iframe/spark/ui/api/me")" 401

echo "# each credential on its own route group only"
check "a person's bearer token on an iframe route (straight to the Service)" "$(status -H "Authorization: Bearer $T_editor" -H "X-Booth-Workspace: $WS" "$SPARK/ui/api/me")" 401
check "a person's OIDC token presented as an assertion" "$(status -H "X-Booth-Identity: $T_editor" -H "X-Booth-Workspace: $WS" "$SPARK/ui/api/me")" 401
SH

checked identity "PW='$password'; MINT_CRED='$mint_cred'; MINT_URL='$mint_url'
$script"

step "the backend logged its effective issuers once: keys from oidc.jwksUrl, workload issuer trusted"
logs=$(kubectl -n "$ns" logs deployment/booth-spark)
echo "$logs" | grep 'oidc: verifying tokens' || fail "no oidc log line"
test "$(echo "$logs" | grep -c 'oidc: verifying tokens')" = 1 || fail "the verifier was built more than once"
echo "$logs" | grep -q 'keys-from=http://keycloak.keycloak.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs' || fail "keys did not come from oidc.jwksUrl"
echo "$logs" | grep -q 'issuer=http://keycloak.keycloak.svc:8080/realms/booth ' || fail "issuer not the configured one"
echo "$logs" | grep -q 'workload-issuer=http://booth-core.booth-system.svc:8080' || fail "workload issuer not trusted"
if echo "$logs" | grep -q 'eyJ'; then fail "something token-shaped was logged"; fi

kubectl -n booth-spark-it-mint delete boothmodule sparkit --wait=false >/dev/null 2>&1 || true
echo "all identity checks passed"
