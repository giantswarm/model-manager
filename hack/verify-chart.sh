#!/usr/bin/env bash
# Render assertions for helm/model-manager: rules the templates encode that
# `helm lint` and the values schema cannot check. Runs as a pre-commit hook
# (locally and in the pre-commit CI workflow) whenever the chart changes, and
# as `make helm-verify`. Needs helm.
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=helm/model-manager

fail() { echo "verify-chart: FAIL: $*" >&2; exit 1; }

# trusted prints the value of --oauth-trusted-audiences the Deployment passes
# for the given values, or nothing when the flag is not rendered.
trusted() {
  helm template mm "$CHART" --show-only templates/deployment.yaml "$@" \
    | { grep -o -- '--oauth-trusted-audiences=[^ ]*' || true; } | cut -d= -f2-
}

# A Dex install that sets only the platform identity contract.
DEX=(--set oauth.enabled=true --set global.domain=example.test
     --set global.identity.issuerUrl=https://dex.example.test
     --set global.identity.clientId=platform-client
     --set global.identity.existingSecret=idp)

# Trusted audiences = union(oauth.trustedAudiences | default [global.identity.clientId],
#                           muster.mcpServer.auth.requiredAudiences), de-duplicated, order stable.
got=$(trusted "${DEX[@]}" --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes"]')
[ "$got" = "platform-client,kubernetes" ] \
  || fail "global.identity + requiredAudiences: want platform-client,kubernetes, got '$got'"

got=$(trusted "${DEX[@]}")
[ "$got" = "platform-client" ] \
  || fail "global.identity alone: want platform-client, got '$got'"

got=$(trusted "${DEX[@]}" \
  --set-json 'oauth.trustedAudiences=["portal-client","kubernetes"]' \
  --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes","dex-k8s-authenticator"]')
[ "$got" = "portal-client,kubernetes,dex-k8s-authenticator" ] \
  || fail "explicit list + requiredAudiences, de-duplicated, order stable: got '$got'"

# The MCPServer requests the same audiences at login.
helm template mm "$CHART" --show-only templates/mcpserver.yaml "${DEX[@]}" \
  --set muster.mcpServer.enabled=true \
  --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes"]' \
  | grep -q -- '^ *- kubernetes$' \
  || fail "the MCPServer CR does not list requiredAudiences"

# A Google install (no cross-client audiences) trusts the client id alone.
got=$(trusted --set oauth.enabled=true --set oauth.provider=google \
  --set oauth.baseURL=https://mm.example.test --set oauth.google.clientID=g.apps \
  --set oauth.existingSecret=google \
  --set-json 'oauth.trustedAudiences=["g.apps"]' \
  --set-json 'muster.mcpServer.auth.requiredAudiences=[]')
[ "$got" = "g.apps" ] || fail "google shape: want g.apps, got '$got'"

# OAuth off renders no flag.
got=$(trusted)
[ -z "$got" ] || fail "oauth disabled: want no --oauth-trusted-audiences, got '$got'"

echo "verify-chart: ok"
