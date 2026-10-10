#!/usr/bin/env bash
# Build step 6: the module's own UI (web/), through booth-core's real iframe proxy and assertion:
#   - the page and its assets come only from the module (strict CSP), behind the assertion;
#   - in Chromium (browser/module-ui.mjs): an editor submits an application through the UI's form,
#     and its log, which prints markup, shows as text; a viewer sees the run and no way to submit;
#   - the UI's submission is the editor's own run on /v1, with the editor as its submitter.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
. "$here/api.sh"

read -r -d '' common <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
field() { sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p"; }
tok() { curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$1" -d "password=$PW" | field access_token; }
iframe_url() { # USER WORKSPACE: core's iframe URL for the module (signing in through core first)
  t=$(tok "$1"); curl -s -o /dev/null -H "Authorization: Bearer $t" "$CORE/api/me"
  for i in $(seq 1 30); do
    u=$(curl -s -H "Authorization: Bearer $t" -H "X-Workspace: $2" "$CORE/api/modules/spark/iframe-url" | field url)
    [ -n "$u" ] && { echo "$u"; return; }
    sleep 2
  done
}
session() { # USER WORKSPACE: core's iframe session cookie
  u=$(iframe_url "$1" "$2")
  curl -s -o /dev/null -D - "$CORE$u" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p'
}
SH

step "the module's UI through core (curl): the page, its CSP, its assets, behind the assertion"
out=$(checked module-ui "PW='$password'
$common
c=\$(session viewer-user acme-analytics)
check 'viewer-user got an iframe session' \"\$( [ -n \"\$c\" ] && echo yes || echo no)\" yes
page=\$(curl -s -H \"Cookie: booth_iframe_session=\$c\" \"\$CORE/iframe/spark/\")
contains 'the page is the built UI (its script, relative)' \"\$page\" 'src=\"./assets/index-'
csp=\$(curl -s -o /dev/null -D - -H \"Cookie: booth_iframe_session=\$c\" \"\$CORE/iframe/spark/\" | tr -d '\r' | sed -n 's/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p')
contains 'its CSP: scripts from the module only' \"\$csp\" \"script-src 'self'\"
contains 'its CSP: no other origin for API calls' \"\$csp\" \"connect-src 'self'\"
asset=\$(echo \"\$page\" | sed -n 's/.*src=\"\.\/\(assets\/index-[^\"]*\.js\)\".*/\1/p' | head -1)
check 'its script loads through core' \"\$(status -H \"Cookie: booth_iframe_session=\$c\" \"\$CORE/iframe/spark/\$asset\")\" 200
check 'the UI API answers the viewer' \"\$(status -H \"Cookie: booth_iframe_session=\$c\" \"\$CORE/iframe/spark/ui/api/applications\")\" 200
check 'no session: the page is refused' \"\$(status \"\$CORE/iframe/spark/\")\" 401
check 'straight to the Service, no assertion: refused' \"\$(status http://booth-spark.booth-spark.svc:8080/)\" 401")
echo "$out"

step "fresh iframe URLs for the browser (core's navigation token lives one minute)"
urls=$(probe module-ui-urls "PW='$password'
$common
echo \"editor \$(iframe_url editor-user acme-analytics)\"
echo \"viewer \$(iframe_url viewer-user acme-analytics)\"")
editor_url=$(awk '$1=="editor" {print $2}' <<<"$urls")
viewer_url=$(awk '$1=="viewer" {print $2}' <<<"$urls")
[ -n "$editor_url" ] && [ -n "$viewer_url" ] || fail "could not mint iframe URLs: $(sed 's/\(booth_iframe_token=\)[^ ]*/\1<redacted>/g' <<<"$urls")"

name="ui-made-$(date +%s)"
step "Chromium through core's iframe proxy: submit through the UI, its log as text, the viewer's view"
(cd "$here/browser" && node module-ui.mjs "http://localhost:$core_port" "$editor_url" "$viewer_url" "$name")

step "the UI's submission is the editor's own run"
T_editor=$(tok editor-user)
v1 "$T_editor" GET /applications
row=$(python3 -c "import json,sys; d=json.load(sys.stdin); r=[a for a in d['applications'] if a['name']==sys.argv[1]]; print(r[0]['submitterName'] + ' ' + r[0]['state'] + ' ' + str(r[0]['workload']) if r else '')" "$name" <<<"$body")
[ "$row" = "editor-user succeeded False" ] || fail "the UI's run on /v1: '$row'"
echo "ok: $name on /v1: submitted by editor-user (a person, not a workload), succeeded"
echo "all module UI checks passed"
