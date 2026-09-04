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

# The lemonade backend renders its own flags and none of ollama's or kserve's.
args() {
  helm template mm "$CHART" --show-only templates/deployment.yaml "$@" \
    | { grep -o -- '--[a-z-]*=[^ ]*' || true; }
}
got=$(args --set backend=lemonade --set lemonade.endpoint=http://172.21.0.1:13305 \
  --set lemonade.agentHost=http://172.21.0.1:13305)
echo "$got" | grep -q -- '^--lemonade-endpoint=http://172.21.0.1:13305$' \
  || fail "lemonade backend: --lemonade-endpoint not rendered, got '$got'"
echo "$got" | grep -q -- '^--lemonade-agent-host=http://172.21.0.1:13305$' \
  || fail "lemonade backend: --lemonade-agent-host not rendered, got '$got'"
if echo "$got" | grep -q -- '^--ollama-\|^--kserve-'; then
  fail "lemonade backend renders ollama or kserve flags: '$got'"
fi
echo "$got" | grep -q -- '^--in-cluster=true$' \
  || fail "lemonade backend with wiring on: Kubernetes access expected (wiring)"

# Without an agent host only the endpoint flag renders; the default backend
# renders no lemonade flag at all.
got=$(args --set backend=lemonade)
echo "$got" | grep -q -- '^--lemonade-endpoint=http://host.docker.internal:13305$' \
  || fail "lemonade backend: default endpoint not rendered, got '$got'"
if echo "$got" | grep -q -- '^--lemonade-agent-host'; then
  fail "lemonade backend: agent host flag rendered without a value"
fi
got=$(args)
if echo "$got" | grep -q -- '^--lemonade-'; then
  fail "ollama backend renders lemonade flags: '$got'"
fi

# Several backends at once: `backends` renders the list flag plus every listed
# driver's flags, and never the single --backend flag.
got=$(args --set 'backends={ollama,lemonade}' --set ollama.endpoint=http://172.21.0.1:11434 \
  --set lemonade.endpoint=http://172.21.0.1:13305)
echo "$got" | grep -q -- '^--backends=ollama,lemonade$' \
  || fail "backends list: --backends=ollama,lemonade not rendered, got '$got'"
echo "$got" | grep -q -- '^--ollama-endpoint=http://172.21.0.1:11434$' \
  || fail "backends list: the ollama flags must render for a listed ollama"
echo "$got" | grep -q -- '^--lemonade-endpoint=http://172.21.0.1:13305$' \
  || fail "backends list: the lemonade flags must render for a listed lemonade"
if echo "$got" | grep -q -- '^--backend='; then
  fail "backends list: the single --backend flag must not render next to --backends: '$got'"
fi
if echo "$got" | grep -q -- '^--kserve-'; then
  fail "backends list without kserve renders kserve flags: '$got'"
fi

# kserve in the list brings its flags, the Kubernetes access and its Roles even
# with wiring off — as `backend: kserve` alone does.
got=$(args --set 'backends={ollama,kserve}' --set ollama.endpoint=http://172.21.0.1:11434 --set kagent.disableWiring=true)
echo "$got" | grep -q -- '^--kserve-namespace=' \
  || fail "backends list with kserve: the kserve flags must render"
echo "$got" | grep -q -- '^--in-cluster=true$' \
  || fail "backends list with kserve and wiring off: Kubernetes access expected"
helm template mm "$CHART" --show-only templates/rbac.yaml --set 'backends={ollama,kserve}' --set kagent.disableWiring=true \
  | grep -q -- '^  name: mm-model-manager-kserve$' \
  || fail "backends list with kserve: the kserve Role must render"
if helm template mm "$CHART" --show-only templates/rbac.yaml --set 'backends={ollama,lemonade}' --set kagent.disableWiring=true 2>/dev/null \
  | grep -q -- 'kserve'; then
  fail "backends list without kserve renders kserve RBAC"
fi

# The one-backend form is untouched: `backend: ollama` renders --backend=ollama
# and no --backends.
got=$(args)
echo "$got" | grep -q -- '^--backend=ollama$' || fail "single backend: --backend=ollama expected, got '$got'"
if echo "$got" | grep -q -- '^--backends='; then
  fail "single backend renders --backends: '$got'"
fi

echo "verify-chart: ok"
